package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// asKey returns req authenticated as a non-master API key.
func asKey(req *http.Request) *http.Request {
	return req.WithContext(withPrincipal(req.Context(), Principal{Master: false, KeyID: "key-1"}))
}

// asMaster returns req authenticated as the master operator.
func asMaster(req *http.Request) *http.Request {
	return req.WithContext(withPrincipal(req.Context(), Principal{Master: true}))
}

// hostAuthzHandler is a Handler whose provider factory records whether it was
// ever asked to build a provider. Nothing that a non-master caller sends
// should reach it.
func hostAuthzHandler(t *testing.T, built *bool) *Handler {
	t.Helper()
	return &Handler{
		NewProvider: func(string, string, orchestrator.HostBackend) orchestrator.Provider {
			*built = true
			return nil
		},
	}
}

// TestHostMutations_RejectNonMasterKey is the core of the boundary: an API key
// must not be able to register, cordon, uncordon, or remove a host. Host
// registration in particular decides where the orchestrator sends every later
// VM operation.
func TestHostMutations_RejectNonMasterKey(t *testing.T) {
	tests := []struct {
		name    string
		request func() *http.Request
		call    func(*Handler, http.ResponseWriter, *http.Request)
	}{
		{
			name: "register",
			request: func() *http.Request {
				return httptest.NewRequest(http.MethodPost, "/v1/hosts",
					strings.NewReader(`{"id":"h1","url":"http://10.0.0.1:8090"}`))
			},
			call: (*Handler).registerHost,
		},
		{
			name: "cordon",
			request: func() *http.Request {
				return httptest.NewRequest(http.MethodPost, "/v1/hosts/h1?action=cordon", nil)
			},
			call: (*Handler).hostAction,
		},
		{
			name: "uncordon",
			request: func() *http.Request {
				return httptest.NewRequest(http.MethodPost, "/v1/hosts/h1?action=uncordon", nil)
			},
			call: (*Handler).hostAction,
		},
		{
			name: "remove",
			request: func() *http.Request {
				return httptest.NewRequest(http.MethodDelete, "/v1/hosts/h1", nil)
			},
			call: (*Handler).removeHost,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var built bool
			h := hostAuthzHandler(t, &built)
			rec := httptest.NewRecorder()

			tt.call(h, rec, asKey(tt.request()))

			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s as api key: got %d, want 403 (%s)", tt.name, rec.Code, rec.Body.String())
			}
			if built {
				t.Fatalf("%s as api key reached the provider factory", tt.name)
			}
		})
	}
}

// TestRegisterHost_MasterTokenPassesAuthz verifies the gate does not also
// block the master operator. It gets past authorization and fails later, on
// the URL policy, which is what proves it was not stopped at the gate.
func TestRegisterHost_MasterTokenPassesAuthz(t *testing.T) {
	var built bool
	h := hostAuthzHandler(t, &built)
	req := asMaster(httptest.NewRequest(http.MethodPost, "/v1/hosts",
		strings.NewReader(`{"id":"h1","url":"ftp://10.0.0.1:8090"}`)))
	rec := httptest.NewRecorder()

	h.registerHost(rec, req)

	if rec.Code == http.StatusForbidden {
		t.Fatalf("master token was rejected by the authz gate: %s", rec.Body.String())
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("register with a bad scheme: got %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
}

// TestRegisterHost_RejectsDisallowedURLsBeforeDialing walks the URL policy.
// Every one of these must be refused before a provider is constructed, since
// constructing one is what leads to the capacity probe going out on the wire.
func TestRegisterHost_RejectsDisallowedURLsBeforeDialing(t *testing.T) {
	urls := []string{
		"ftp://10.0.0.1:8090",
		"file:///etc/passwd",
		"gopher://10.0.0.1:70",
		"10.0.0.1:8090",                  // no scheme
		"http://user:pass@10.0.0.1:8090", // embedded credentials
		"http://10.0.0.1:8090?x=1",       // query string
		"http://10.0.0.1:8090#frag",      // fragment
		"http://10.0.0.1:not-a-port",     // invalid port
		"http://",                        // no host
	}

	for _, raw := range urls {
		t.Run(raw, func(t *testing.T) {
			var built bool
			h := hostAuthzHandler(t, &built)
			body := `{"id":"h1","url":"` + raw + `"}`
			req := asMaster(httptest.NewRequest(http.MethodPost, "/v1/hosts", strings.NewReader(body)))
			rec := httptest.NewRecorder()

			h.registerHost(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("register %q: got %d, want 400 (%s)", raw, rec.Code, rec.Body.String())
			}
			if built {
				t.Fatalf("register %q constructed a provider for a rejected URL", raw)
			}
		})
	}
}
