package hostwire

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidateAgentURL_Accepts(t *testing.T) {
	valid := []string{
		"http://10.0.0.1:8090",
		"https://agent.internal:8443",
		"http://127.0.0.1:8090/",
		"https://proxy.example.com/agent", // a path prefix is a supported topology
		"http://[2001:db8::1]:8090",
		"http://host-agent",
	}

	for _, raw := range valid {
		if err := ValidateAgentURL(raw); err != nil {
			t.Errorf("ValidateAgentURL(%q) = %v, want nil", raw, err)
		}
	}
}

func TestValidateAgentURL_Rejects(t *testing.T) {
	invalid := []string{
		"",
		"   ",
		"10.0.0.1:8090",                  // parses as scheme "10.0.0.1"
		"ftp://10.0.0.1:8090",            // wrong scheme
		"file:///etc/passwd",             // reads local state
		"unix:///var/run/agent.sock",     // not http
		"http://user:pass@10.0.0.1:8090", // credentials would be sent every request
		"http://10.0.0.1:8090?x=1",       // query is dropped when paths are appended
		"http://10.0.0.1:8090#frag",
		"http://10.0.0.1:99999", // port out of range
		"http://10.0.0.1:abc",
		"http://",
		"https://",
	}

	for _, raw := range invalid {
		if err := ValidateAgentURL(raw); err == nil {
			t.Errorf("ValidateAgentURL(%q) = nil, want an error", raw)
		}
	}
}

// TestNewClient_HasTimeouts guards the reason this client exists at all: an
// http.Client with no Timeout is what let an unresponsive host pin a call.
func TestNewClient_HasTimeouts(t *testing.T) {
	c := NewClient()
	if c.Timeout <= 0 {
		t.Fatal("client has no overall timeout")
	}

	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport: got %T, want *http.Transport", c.Transport)
	}
	if tr.TLSHandshakeTimeout <= 0 {
		t.Error("transport has no TLS handshake timeout")
	}
	if tr.ResponseHeaderTimeout <= 0 {
		t.Error("transport has no response header timeout")
	}
	if tr.IdleConnTimeout <= 0 {
		t.Error("transport has no idle connection timeout")
	}
}

// TestNewClient_RefusesRedirects verifies a host agent cannot bounce the
// orchestrator (and the Authorization header it carries) somewhere else.
func TestNewClient_RefusesRedirects(t *testing.T) {
	var elsewhereHits int
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhereHits++
		if r.Header.Get("Authorization") != "" {
			t.Error("redirect target received the host agent token")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer elsewhere.Close()

	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusFound)
	}))
	defer agent.Close()

	req, err := http.NewRequest(http.MethodGet, agent.URL+"/v1/capacity", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer host-token")

	res, err := NewClient().Do(req) //nolint:bodyclose // the error path returns no body
	if err == nil {
		_ = res.Body.Close()
		t.Fatal("redirect was followed; want an error")
	}
	if !errors.Is(err, ErrRedirectNotAllowed) {
		t.Fatalf("redirect error: got %v, want ErrRedirectNotAllowed", err)
	}
	if elsewhereHits != 0 {
		t.Fatalf("redirect target was contacted %d times", elsewhereHits)
	}
}

// TestReadErrorBody_IsBounded verifies a host agent cannot make the
// orchestrator hold an arbitrarily large error message in memory.
func TestReadErrorBody_IsBounded(t *testing.T) {
	huge := strings.NewReader(strings.Repeat("x", MaxErrorBodyBytes*3))

	got := ReadErrorBody(huge)

	if len(got) != MaxErrorBodyBytes {
		t.Fatalf("error body length: got %d, want %d", len(got), MaxErrorBodyBytes)
	}
}

func TestReadErrorBody_TrimsShortBodies(t *testing.T) {
	got := ReadErrorBody(strings.NewReader("  vm not found\n"))
	if got != "vm not found" {
		t.Fatalf("error body: got %q, want %q", got, "vm not found")
	}
}
