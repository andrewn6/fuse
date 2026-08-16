package api

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// fakeStreamGuest stands in for fused's /v1/computer/stream route: a real
// HTTP server that answers the fuse-vnc/1 upgrade, sends an RFB-style
// greeting, and echoes one payload back. The fleet dials it over a real
// socket, so this exercises dialGuestStream end to end.
type fakeStreamGuest struct {
	srv      *httptest.Server
	greeting string
}

func newFakeStreamGuest(t *testing.T) *fakeStreamGuest {
	t.Helper()
	g := &fakeStreamGuest{greeting: "RFB 003.008\n"}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/computer/stream" || !strings.EqualFold(r.Header.Get("Upgrade"), orchestrator.VNCProto) {
			http.Error(w, "bad upgrade", http.StatusBadRequest)
			return
		}
		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: " +
			orchestrator.VNCProto + "\r\nConnection: Upgrade\r\n\r\n")
		_ = buf.Flush()
		_, _ = conn.Write([]byte(g.greeting))
		b := make([]byte, 64)
		n, err := buf.Reader.Read(b)
		if err != nil {
			return
		}
		_, _ = conn.Write(append([]byte("echo:"), b[:n]...))
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeStreamGuest) authority() string { return strings.TrimPrefix(g.srv.URL, "http://") }

// dialStream performs the client half of the fuse-vnc/1 upgrade against a
// real listening server, the way dialAttach does for attach.
func dialStream(t *testing.T, srv *httptest.Server, path string) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()

	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", orchestrator.VNCProto)
	if err := req.Write(c); err != nil {
		t.Fatalf("write request: %v", err)
	}

	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return c, br, resp
}

func TestComputerStreamRelaysBothDirections(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)
	srv := httptest.NewServer(r)
	defer srv.Close()

	env := provisionEnv(t, r, p)
	guest := newFakeStreamGuest(t)
	env.url = guest.authority()

	conn, br, resp := dialStream(t, srv, "/v1/environments/fuse-task-1/computer/stream")
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}
	if got := resp.Header.Get("Upgrade"); got != orchestrator.VNCProto {
		t.Errorf("Upgrade = %q, want %q", got, orchestrator.VNCProto)
	}

	// guest → client: the greeting the vnc server sends unprompted
	got := make([]byte, len(guest.greeting))
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatalf("read greeting: %v", err)
	}
	if string(got) != guest.greeting {
		t.Errorf("greeting = %q, want %q", got, guest.greeting)
	}

	// client → guest and back
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatalf("client write: %v", err)
	}
	want := "echo:hello"
	back := make([]byte, len(want))
	if _, err := io.ReadFull(br, back); err != nil {
		t.Fatalf("client read: %v", err)
	}
	if string(back) != want {
		t.Errorf("echo = %q, want %q", back, want)
	}
}

func TestComputerStreamRequiresUpgradeHeader(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)
	provisionEnv(t, r, p)

	rr := doJSON(t, r, http.MethodGet, "/v1/environments/fuse-task-1/computer/stream", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if e := decodeError(t, rr.Body); !strings.Contains(e.Error.Message, orchestrator.VNCProto) {
		t.Fatalf("message = %q, want the required protocol named", e.Error.Message)
	}
}

// the guest's 503 — no display, or the vnc unit is down — must surface as
// unavailable with the guest's reason, not as an opaque upgrade failure.
func TestComputerStreamGuestRefusalMapsToUnavailable(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)
	srv := httptest.NewServer(r)
	defer srv.Close()

	env := provisionEnv(t, r, p)
	guest := newFakeGuest(http.StatusServiceUnavailable, `{"error":"vnc server not reachable at 127.0.0.1:5900; is the fuse-vnc unit running?"}`)
	defer guest.close()
	env.url = guest.authority()

	_, _, resp := dialStream(t, srv, "/v1/environments/fuse-task-1/computer/stream")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "vnc server not reachable") {
		t.Fatalf("body = %s, want the guest's reason", body)
	}
}

func TestComputerStreamUnknownVM(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)
	srv := httptest.NewServer(r)
	defer srv.Close()

	_, _, resp := dialStream(t, srv, "/v1/environments/nope/computer/stream")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestComputerStreamUnsupportedWithoutGuestAddress(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)
	srv := httptest.NewServer(r)
	defer srv.Close()

	env := provisionEnv(t, r, p)
	env.url = "fc://fuse-task-1"

	_, _, resp := dialStream(t, srv, "/v1/environments/fuse-task-1/computer/stream")
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}
}
