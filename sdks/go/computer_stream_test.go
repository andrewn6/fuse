package fuse

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestComputerStream_RelaysRawBytes(t *testing.T) {
	greeting := "RFB 003.008\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/environments/fuse-1/computer/stream" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Upgrade"); got != VNCProto {
			t.Errorf("Upgrade = %q, want %q", got, VNCProto)
		}
		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: " + VNCProto + "\r\nConnection: Upgrade\r\n\r\n")
		_ = buf.Flush()
		_, _ = conn.Write([]byte(greeting))
		b := make([]byte, 64)
		n, err := buf.Reader.Read(b)
		if err != nil {
			return
		}
		_, _ = conn.Write(append([]byte("echo:"), b[:n]...))
	}))
	defer srv.Close()

	c, _ := New(srv.URL, "tok")
	stream, err := c.Environments.ComputerStream(context.Background(), "fuse-1")
	if err != nil {
		t.Fatalf("computer stream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	got := make([]byte, len(greeting))
	if _, err := io.ReadFull(stream, got); err != nil {
		t.Fatalf("read greeting: %v", err)
	}
	if string(got) != greeting {
		t.Fatalf("greeting = %q, want %q", got, greeting)
	}

	if _, err := stream.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	want := "echo:hello"
	back := make([]byte, len(want))
	if _, err := io.ReadFull(stream, back); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(back) != want {
		t.Fatalf("echo = %q, want %q", back, want)
	}
}

// An error before the upgrade must surface in the same shape as every other
// call — here, the 503 a non-desktop image answers with.
func TestComputerStream_ServerErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"code":"unavailable","message":"guest surface unavailable: vnc server not reachable"}}`)
	}))
	defer srv.Close()

	c, _ := New(srv.URL, "tok")
	_, err := c.Environments.ComputerStream(context.Background(), "fuse-1")
	if err == nil {
		t.Fatal("want an error for a 503, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %T (%v), want *APIError", err, err)
	}
	if apiErr.Status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", apiErr.Status)
	}
	if !strings.Contains(err.Error(), "vnc server not reachable") {
		t.Errorf("error %q does not carry the server's reason", err)
	}
}

func TestComputerStream_Validation(t *testing.T) {
	c, _ := New("http://example.invalid", "tok")
	if _, err := c.Environments.ComputerStream(context.Background(), ""); err == nil {
		t.Fatal("want an error for an empty vm id, got nil")
	}
}
