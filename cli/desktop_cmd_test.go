package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	fuse "github.com/folsomintel/fuse/sdks/go"
)

// fakeOrchStream stands in for the orchestrator's computer stream route: it
// answers the fuse-vnc/1 upgrade, sends an RFB-style greeting, and echoes
// one payload back, prefixed.
func fakeOrchStream(t *testing.T, greeting string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), fuse.VNCProto) {
			http.Error(w, "bad upgrade", http.StatusBadRequest)
			return
		}
		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: " +
			fuse.VNCProto + "\r\nConnection: Upgrade\r\n\r\n")
		_ = buf.Flush()
		_, _ = conn.Write([]byte(greeting))
		b := make([]byte, 64)
		n, err := buf.Read(b)
		if err != nil {
			return
		}
		_, _ = conn.Write(append([]byte("echo:"), b[:n]...))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The full local bridge: websocket viewer on one side, orchestrator stream on
// the other, with the CLI in the middle. What the browser sends as websocket
// frames must reach the guest as raw bytes, and vice versa.
func TestDesktopBridgeSplicesViewerToStream(t *testing.T) {
	greeting := "RFB 003.008\n"
	orch := fakeOrchStream(t, greeting)

	cl, err := fuse.New(orch.URL, "tok")
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveDesktopBridge(w, r, cl, "fuse-1")
	}))
	defer bridge.Close()

	c := dialWS(t, bridge, "/ws")

	// guest → viewer: the vnc greeting arrives as binary frames
	var got []byte
	for len(got) < len(greeting) {
		op, p := c.recv(t)
		if op != wsOpBinary {
			t.Fatalf("op = %d, want binary", op)
		}
		got = append(got, p...)
	}
	if string(got) != greeting {
		t.Fatalf("greeting = %q, want %q", got, greeting)
	}

	// viewer → guest and back
	c.send(t, wsOpBinary, []byte("hello"))
	want := "echo:hello"
	var back []byte
	for len(back) < len(want) {
		_, p := c.recv(t)
		back = append(back, p...)
	}
	if string(back) != want {
		t.Fatalf("echo = %q, want %q", back, want)
	}
}

// A stream the orchestrator refuses must come back as an HTTP error before
// the websocket handshake, so the page can show a reason.
func TestDesktopBridgeStreamErrorIsHTTP(t *testing.T) {
	orch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"unavailable","message":"guest surface unavailable"}}`))
	}))
	defer orch.Close()

	cl, err := fuse.New(orch.URL, "tok")
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveDesktopBridge(w, r, cl, "fuse-1")
	}))
	defer bridge.Close()

	resp, err := http.Get(bridge.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}
