package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// streamComputer returns a computer whose ready() passes without an X server.
func streamComputer(vncAddr string) *computer {
	c := newComputer(":1")
	c.vncAddr = vncAddr
	c.ready = func() error { return nil }
	return c
}

func TestStreamRequiresUpgradeHeader(t *testing.T) {
	c := streamComputer("127.0.0.1:1")
	req := httptest.NewRequest(http.MethodGet, "/v1/computer/stream", nil)
	rec := httptest.NewRecorder()
	c.handleStream(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if !strings.Contains(body["error"], vncProto) {
		t.Fatalf("error %q does not name the required protocol", body["error"])
	}
}

func TestStreamNoDisplayAnswers503(t *testing.T) {
	c := newComputer(":93")
	req := httptest.NewRequest(http.MethodGet, "/v1/computer/stream", nil)
	req.Header.Set("Upgrade", vncProto)
	rec := httptest.NewRecorder()
	c.handleStream(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rec.Code)
	}
}

func TestStreamVNCUnreachableAnswers503(t *testing.T) {
	// grab a port that is definitely closed by listening and closing it
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	c := streamComputer(addr)
	req := httptest.NewRequest(http.MethodGet, "/v1/computer/stream", nil)
	req.Header.Set("Upgrade", vncProto)
	rec := httptest.NewRecorder()
	c.handleStream(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if !strings.Contains(body["error"], "vnc server not reachable") {
		t.Fatalf("error %q does not name the vnc server", body["error"])
	}
}

// TestStreamSplicesBothDirections runs the full path: a stub vnc server that
// sends an RFB-style greeting and echoes what it receives, the real handler
// behind a real http server (httptest supports hijacking), and a raw client
// performing the upgrade by hand the way the orchestrator will.
func TestStreamSplicesBothDirections(t *testing.T) {
	vnc, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = vnc.Close() }()
	greeting := "RFB 003.008\n"
	go func() {
		conn, err := vnc.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = conn.Write([]byte(greeting))
		// echo one client payload back, prefixed, then keep the conn open
		buf := make([]byte, 64)
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		_, _ = conn.Write(append([]byte("echo:"), buf[:n]...))
	}()

	c := streamComputer(vnc.Addr().String())
	srv := httptest.NewServer(http.HandlerFunc(c.handleStream))
	defer srv.Close()

	raw, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))

	req := "GET /v1/computer/stream HTTP/1.1\r\n" +
		"Host: fused\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: " + vncProto + "\r\n\r\n"
	if _, err := raw.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}

	br := bufio.NewReader(raw)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}
	if got := resp.Header.Get("Upgrade"); got != vncProto {
		t.Fatalf("upgrade header = %q, want %q", got, vncProto)
	}

	got := make([]byte, len(greeting))
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != greeting {
		t.Fatalf("greeting = %q, want %q", got, greeting)
	}

	if _, err := raw.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	want := "echo:hello"
	back := make([]byte, len(want))
	if _, err := io.ReadFull(br, back); err != nil {
		t.Fatal(err)
	}
	if string(back) != want {
		t.Fatalf("echo = %q, want %q", back, want)
	}
}

func TestStreamRouteRequiresToken(t *testing.T) {
	srv := httptest.NewServer(newHandler(config{vmID: "fuse-1", display: ":1"}, "secret-token", nil, 0, false, nil))
	defer srv.Close()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/computer/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Upgrade", vncProto)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", resp.StatusCode)
	}
}
