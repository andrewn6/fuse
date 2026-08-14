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
		case r.URL.Path == "/v1/environments/vm1" && r.Method == http.MethodGet:
			// the settled environment `up` re-fetches to build its summary,
			// since the event stream carries a state and nothing else.
			fmt.Fprint(w, `{"id":"vm1","state":"running","task_id":"t","host_id":"h1",
				"url":"10.0.0.4:19551","spec":{"cpus":2,"ram_mb":2048},
				"endpoints":[{"as":"http","url":"10.0.0.4:41337","port":8080}]}`)
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

// runUp executes `fuse up` against srv with the wait path enabled, in json
// mode so the assertions read the raw events.
func runUp(t *testing.T, srv *httptest.Server) (string, error) {
	t.Helper()
	return runUpWith(t, srv, "-o", "json")
}

// runUpWith is runUp with extra root-level arguments, for tests that need a
// different output mode.
func runUpWith(t *testing.T, srv *httptest.Server, rootArgs ...string) (string, error) {
	t.Helper()
	fusefilePath := writeFusefile(t, t.TempDir())
	cfg := writeConfig(t, srv.URL)
	args := append([]string{"--config", cfg}, rootArgs...)
	args = append(args,
		"up", "-f", fusefilePath,
		"--task-id", "t",
		"--secret", "pg_password=shh",
	)
	return captureWithDeadline(t, 20*time.Second, func() error {
		root := newRootCmd()
		root.SetArgs(args)
		return root.Execute()
	})
}

// the waiting path used to print nothing at all: the stream settled, the TUI
// wrote a faint "done", and the process exited leaving the author with no
// address to curl.
func TestUpPrintsSummaryWhenReady(t *testing.T) {
	srv := sseEnvServer(t, fuse.StateProvisioning, fuse.StateRunning)
	out, err := runUpWith(t, srv)
	if err != nil {
		t.Fatalf("up returned an error: %v", err)
	}
	for _, want := range []string{
		"environment",
		"vm1",
		"agent",
		"10.0.0.4:19551",
		"endpoint http",
		"10.0.0.4:41337  ->  guest :8080",
		"fuse environment shell vm1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
}

// in json mode the summary is the environment object, so stdout stays
// machine-readable.
func TestUpPrintsEnvironmentJSONWhenReady(t *testing.T) {
	srv := sseEnvServer(t, fuse.StateProvisioning, fuse.StateRunning)
	out, err := runUp(t, srv)
	if err != nil {
		t.Fatalf("up returned an error: %v", err)
	}
	if !strings.Contains(out, `"endpoints"`) || !strings.Contains(out, `"10.0.0.4:41337"`) {
		t.Errorf("json output missing the settled environment:\n%s", out)
	}
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

// healthServer replies to environment reads with the given verdicts in order,
// repeating the last one once they run out.
func healthServer(t *testing.T, bodies ...string) *fuse.Client {
	t.Helper()
	var i int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body := bodies[i]
		if i < len(bodies)-1 {
			i++
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)

	cl, err := fuse.New(srv.URL, "test-token")
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return cl
}

const envBase = `"id":"vm1","state":"running","task_id":"t","url":"","spec":{}`

// the wait ends as soon as the verdict turns passing, and it tolerates the
// window before the guest has reported anything at all.
func TestWaitForEnvironmentHealthyPasses(t *testing.T) {
	cl := healthServer(t,
		`{`+envBase+`}`,
		`{`+envBase+`,"health":{"state":"starting"}}`,
		`{`+envBase+`,"health":{"state":"passing"}}`,
	)
	if err := waitForEnvironmentHealthy(t.Context(), cl, "vm1", 10*time.Second); err != nil {
		t.Fatalf("waitForEnvironmentHealthy: %v", err)
	}
}

// a probe that never passes ends on the timeout, and the error carries the
// last verdict so the author knows why rather than just that time ran out.
func TestWaitForEnvironmentHealthyTimesOut(t *testing.T) {
	cl := healthServer(t, `{`+envBase+`,"health":{"state":"failing","message":"connection refused"}}`)

	err := waitForEnvironmentHealthy(t.Context(), cl, "vm1", time.Millisecond)
	if err == nil {
		t.Fatal("wait returned success for a probe that never passed")
	}
	for _, want := range []string{"did not report healthy", "failing", "connection refused"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}
}
