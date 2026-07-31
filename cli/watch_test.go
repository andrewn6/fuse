package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	fuse "github.com/folsomintel/fuse/sdks/go"
)

// captureWithDeadline runs fn with os.Stdout redirected and fails the test if
// fn has not returned within d. stdout is always restored, even on timeout, so
// a hang here cannot wedge the rest of the suite.
func captureWithDeadline(t *testing.T, d time.Duration, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	done := make(chan error, 1)
	go func() { done <- fn() }()

	// drain concurrently so a full pipe buffer can never be the thing that
	// blocks fn.
	out := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(r)
		out <- string(data)
	}()

	var err error
	timedOut := false
	select {
	case err = <-done:
	case <-time.After(d):
		timedOut = true
	}
	os.Stdout = old
	_ = w.Close()
	s := <-out
	if timedOut {
		t.Fatalf("did not return within %s (hang); output so far:\n%s", d, s)
	}
	return s, err
}

func TestStreamPlainStopsAtRunning(t *testing.T) {
	// the channel is never closed, mirroring a live stream that stays open
	// after the environment comes up.
	ch := make(chan fuse.Event, 2)
	ch <- fuse.Event{State: fuse.StateProvisioning}
	ch <- fuse.Event{State: fuse.StateRunning}

	var state string
	_, err := captureWithDeadline(t, 5*time.Second, func() error {
		var err error
		state, err = streamPlain(ch, fuse.IsSettledState, nil)
		return err
	})
	if err != nil {
		t.Fatalf("streamPlain: %v", err)
	}
	if state != fuse.StateRunning {
		t.Errorf("state = %q, want %q", state, fuse.StateRunning)
	}
}

func TestStreamPlainTerminalPredicateIgnoresRunning(t *testing.T) {
	// watch keeps its old behavior: running is not a stopping point, so the
	// loop keeps going until the environment is gone.
	ch := make(chan fuse.Event, 3)
	ch <- fuse.Event{State: fuse.StateRunning}
	ch <- fuse.Event{State: fuse.StateDestroying}
	ch <- fuse.Event{State: fuse.StateDestroyed}

	var state string
	_, err := captureWithDeadline(t, 5*time.Second, func() error {
		var err error
		state, err = streamPlain(ch, fuse.IsTerminalState, nil)
		return err
	})
	if err != nil {
		t.Fatalf("streamPlain: %v", err)
	}
	if state != fuse.StateDestroyed {
		t.Errorf("state = %q, want %q", state, fuse.StateDestroyed)
	}
}

// sseEnvServer serves a create response plus an event stream that emits the
// given states and then holds the connection open, like the orchestrator does
// for an environment that is up and staying up.
func sseEnvServer(t *testing.T, states ...string) *httptest.Server {
	t.Helper()
	return newSSEEnvServer(t, true, states...)
}

// sseEnvDropServer emits the given states and then closes the stream, like a
// proxy timeout or an orchestrator restart mid-provision. the sdk reports a
// clean eof by closing the channel with no error, so this is the shape that
// used to look like success.
func sseEnvDropServer(t *testing.T, states ...string) *httptest.Server {
	t.Helper()
	return newSSEEnvServer(t, false, states...)
}

func newSSEEnvServer(t *testing.T, hold bool, states ...string) *httptest.Server {
	t.Helper()
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/environments" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"vm1","state":"pending","task_id":"t","url":"","spec":{}}`)
		case r.URL.Path == "/v1/environments/vm1/events":
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, ok := w.(http.Flusher)
			if !ok {
				t.Error("response writer is not a flusher")
				return
			}
			w.WriteHeader(http.StatusOK)
			flusher.Flush()
			for _, st := range states {
				fmt.Fprintf(w, "data: {\"vm_id\":\"vm1\",\"state\":%q}\n\n", st)
				flusher.Flush()
			}
			if !hold {
				// returning ends the response body, which the sdk sees as
				// a clean eof.
				return
			}
			// holding the stream open is what makes the hang reproducible.
			select {
			case <-r.Context().Done():
			case <-stop:
			}
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	// cleanups run last-registered-first, so stop is closed before Close
	// waits on outstanding requests. otherwise a failing test would block
	// in srv.Close forever.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(stop) })
	return srv
}

// runUp executes `fuse up` against srv with the wait path enabled.
func runUp(t *testing.T, srv *httptest.Server) (string, error) {
	t.Helper()
	fusefilePath := writeFusefile(t, t.TempDir())
	cfg := writeConfig(t, srv.URL)
	return captureWithDeadline(t, 20*time.Second, func() error {
		root := newRootCmd()
		root.SetArgs([]string{
			"--config", cfg, "-o", "json",
			"up", "-f", fusefilePath,
			"--task-id", "t",
			"--secret", "pg_password=shh",
		})
		return root.Execute()
	})
}

func TestUpReturnsOnceRunning(t *testing.T) {
	srv := sseEnvServer(t, fuse.StateProvisioning, fuse.StateRunning)
	out, err := runUp(t, srv)
	if err != nil {
		t.Fatalf("up returned an error: %v", err)
	}
	if !strings.Contains(out, `"running"`) {
		t.Errorf("output missing the running event: %s", out)
	}
}

func TestUpFailsWhenStreamDropsMidProvision(t *testing.T) {
	// the stream ends on provisioning without ever reaching running. a dropped
	// stream is not a successful provision.
	srv := sseEnvDropServer(t, fuse.StateProvisioning)
	_, err := runUp(t, srv)
	if err == nil {
		t.Fatal("up returned nil for a dropped stream, want an error")
	}
	if !strings.Contains(err.Error(), fuse.StateProvisioning) {
		t.Errorf("error = %v, want it to name the last observed state", err)
	}
}

func TestUpFailsWhenStreamEndsWithNoEvents(t *testing.T) {
	// the stream closes before a single event arrives, so there is no state to
	// report at all.
	srv := sseEnvDropServer(t)
	_, err := runUp(t, srv)
	if err == nil {
		t.Fatal("up returned nil for an empty stream, want an error")
	}
	if !strings.Contains(err.Error(), "no events") {
		t.Errorf("error = %v, want it to say the stream carried no events", err)
	}
}

func TestUpFailsWhenEnvironmentFails(t *testing.T) {
	srv := sseEnvServer(t, fuse.StateProvisioning, fuse.StateFailed)
	_, err := runUp(t, srv)
	if err == nil {
		t.Fatal("up returned nil for a failed environment, want an error")
	}
	if !strings.Contains(err.Error(), "failed to provision") {
		t.Errorf("error = %v, want it to mention the provisioning failure", err)
	}
}
