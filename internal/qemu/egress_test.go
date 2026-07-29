package qemu

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/folsomintel/fuse/internal/hostwire"
	"github.com/folsomintel/fuse/internal/orchestrator"
)

// TestProvider_DefaultClientIsBounded pins the provider off http.DefaultClient,
// which has no timeout at all.
func TestProvider_DefaultClientIsBounded(t *testing.T) {
	p := New(Config{BaseURL: "http://127.0.0.1:1"})

	if p.client == http.DefaultClient {
		t.Fatal("provider uses http.DefaultClient, which has no timeout")
	}
	if p.client.Timeout <= 0 {
		t.Fatal("provider client has no overall timeout")
	}
}

// TestProvider_BoundsOversizedErrorBody verifies a host agent cannot make the
// orchestrator hold an unbounded error string just by answering an error with
// a very long body.
func TestProvider_BoundsOversizedErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		// Written in chunks so the handler does not have to hold it all either.
		chunk := strings.Repeat("x", 64<<10)
		for range 8 {
			_, _ = w.Write([]byte(chunk))
		}
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL, Token: "token"})

	_, err := p.Get(context.Background(), "vm-1")
	if err == nil {
		t.Fatal("want an error from a 500 response")
	}

	var statusErr *orchestrator.HTTPStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("error type: got %T, want *orchestrator.HTTPStatusError", err)
	}
	if len(statusErr.Body) > hostwire.MaxErrorBodyBytes {
		t.Fatalf("error body: got %d bytes, want at most %d",
			len(statusErr.Body), hostwire.MaxErrorBodyBytes)
	}
}

// TestProvider_DoesNotFollowRedirects verifies a host agent cannot redirect a
// provider call to another origin, which would take the host's bearer token
// somewhere the operator never registered.
func TestProvider_DoesNotFollowRedirects(t *testing.T) {
	var elsewhereHits int
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhereHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"vm_id":"vm-1","url":""}`))
	}))
	defer elsewhere.Close()

	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusFound)
	}))
	defer agent.Close()

	p := New(Config{BaseURL: agent.URL, Token: "token"})

	if _, err := p.Get(context.Background(), "vm-1"); err == nil {
		t.Fatal("redirect was followed; want an error")
	}
	if elsewhereHits != 0 {
		t.Fatalf("redirect target was contacted %d times", elsewhereHits)
	}
}
