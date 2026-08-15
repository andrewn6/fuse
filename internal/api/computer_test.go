package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGuest stands in for fused on the far side of the DNAT: a real HTTP
// server the fleet's computer proxy dials, recording what arrived.
type fakeGuest struct {
	srv *httptest.Server

	gotMethod string
	gotPath   string
	gotBody   []byte
	gotAuth   string

	status int
	body   string
}

func newFakeGuest(status int, body string) *fakeGuest {
	g := &fakeGuest{status: status, body: body}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.gotMethod = r.Method
		g.gotPath = r.URL.Path
		g.gotAuth = r.Header.Get("Authorization")
		g.gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(g.status)
		_, _ = w.Write([]byte(g.body))
	}))
	return g
}

// authority is the host:port the fleet dials — the same bare-authority shape
// fc-agent reports for the per-VM DNAT.
func (g *fakeGuest) authority() string { return strings.TrimPrefix(g.srv.URL, "http://") }
func (g *fakeGuest) close()            { g.srv.Close() }

func TestComputerActionProxiesToGuest(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)
	env := provisionEnv(t, r, p)

	guest := newFakeGuest(http.StatusOK, `{"output":"","screenshot":"cGl4ZWxz"}`)
	defer guest.close()
	env.url = guest.authority()

	// the unknown field must survive the proxy: the action schema belongs to
	// the guest agent, which may be newer than this server.
	body := `{"action":"left_click","coordinate":[100,200],"future_field":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/environments/fuse-task-1/computer", strings.NewReader(body))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if guest.gotMethod != http.MethodPost || guest.gotPath != "/v1/computer/action" {
		t.Fatalf("guest saw %s %s, want POST /v1/computer/action", guest.gotMethod, guest.gotPath)
	}
	if string(guest.gotBody) != body {
		t.Fatalf("guest body = %s, want the request body verbatim", guest.gotBody)
	}
	// this fake env has no guest token, so no Authorization header may be
	// invented for it.
	if guest.gotAuth != "" {
		t.Fatalf("guest saw Authorization %q, want none", guest.gotAuth)
	}
	if !strings.Contains(rr.Body.String(), "cGl4ZWxz") {
		t.Fatalf("guest response not relayed: %s", rr.Body.String())
	}
}

func TestComputerDisplayProxiesToGuest(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)
	env := provisionEnv(t, r, p)

	guest := newFakeGuest(http.StatusOK, `{"display":":1","up":true,"width":1280,"height":800}`)
	defer guest.close()
	env.url = guest.authority()

	rr := doJSON(t, r, http.MethodGet, "/v1/environments/fuse-task-1/computer", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if guest.gotPath != "/v1/computer/display" {
		t.Fatalf("guest saw %s, want /v1/computer/display", guest.gotPath)
	}
	if !strings.Contains(rr.Body.String(), `"width":1280`) {
		t.Fatalf("display report not relayed: %s", rr.Body.String())
	}
}

func TestComputerActionGuestErrorRewrapped(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)
	env := provisionEnv(t, r, p)

	guest := newFakeGuest(http.StatusServiceUnavailable, `{"error":"display :1 is not up (no socket at /tmp/.X11-unix/X1)"}`)
	defer guest.close()
	env.url = guest.authority()

	req := httptest.NewRequest(http.MethodPost, "/v1/environments/fuse-task-1/computer",
		strings.NewReader(`{"action":"screenshot"}`))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	e := decodeError(t, rr.Body)
	if e.Error.Code != CodeUnavailable {
		t.Fatalf("code = %s, want %s", e.Error.Code, CodeUnavailable)
	}
	if !strings.Contains(e.Error.Message, "display :1 is not up") {
		t.Fatalf("message = %q, want the guest's reason", e.Error.Message)
	}
}

func TestComputerActionValidatesShape(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)
	env := provisionEnv(t, r, p)
	env.url = "127.0.0.1:1" // must never be dialed for these

	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing action", `{}`, "action is required"},
		{"malformed json", `{nope`, "malformed action"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/environments/fuse-task-1/computer", strings.NewReader(tc.body))
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rr.Code, rr.Body.String())
			}
			if e := decodeError(t, rr.Body); !strings.Contains(e.Error.Message, tc.want) {
				t.Fatalf("message = %q, want %q", e.Error.Message, tc.want)
			}
		})
	}
}

func TestComputerActionUnknownVM(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)
	req := httptest.NewRequest(http.MethodPost, "/v1/environments/nope/computer",
		strings.NewReader(`{"action":"screenshot"}`))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

// an environment whose provider reports a placeholder URL (the in-memory
// stub) has no guest agent to dial, which is a capability gap, not a fault.
func TestComputerActionUnsupportedWithoutGuestAddress(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)
	env := provisionEnv(t, r, p)
	env.url = "fc://fuse-task-1"

	req := httptest.NewRequest(http.MethodPost, "/v1/environments/fuse-task-1/computer",
		strings.NewReader(`{"action":"screenshot"}`))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 (body %s)", rr.Code, rr.Body.String())
	}
	if e := decodeError(t, rr.Body); e.Error.Code != CodeUnimplemented {
		t.Fatalf("code = %s, want %s", e.Error.Code, CodeUnimplemented)
	}
}

func TestCreateEnvironmentValidatesDesktop(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-desk",
		ManifestInline: encodeManifest(t),
		Desktop:        &DesktopSpec{Width: 10, Height: 800},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rr.Code, rr.Body.String())
	}
	if e := decodeError(t, rr.Body); !strings.Contains(e.Error.Message, "desktop.width") {
		t.Fatalf("message = %q, want a desktop.width complaint", e.Error.Message)
	}
}
