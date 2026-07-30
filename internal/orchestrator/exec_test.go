package orchestrator

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// provisionRunning brings up a single VM and returns its id, so the exec tests
// start from the only state exec accepts.
func provisionRunning(t *testing.T, fm *FleetManager) string {
	t.Helper()
	if _, err := fm.ProvisionAndAssign(context.Background(), "task-1", Spec{}, []byte(`{}`), nil, BootOptions{}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	return "fuse-task-1"
}

func TestExec_HappyPath(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-"})
	vmID := provisionRunning(t, fm)

	env := p.envs[vmID]
	env.execResult = ExecResult{ExitCode: 0, Stdout: []byte("hello\n"), Stderr: nil}

	res, err := fm.Exec(context.Background(), vmID, []string{"echo", "hello"}, ExecOptions{})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}
	if string(res.Stdout) != "hello\n" {
		t.Errorf("stdout = %q, want %q", res.Stdout, "hello\n")
	}

	got := env.execArgv
	if len(got) != 1 {
		t.Fatalf("exec called %d times, want 1", len(got))
	}
	if len(got[0]) != 2 || got[0][0] != "echo" || got[0][1] != "hello" {
		t.Errorf("argv = %q, want [echo hello]", got[0])
	}
}

// A guest command that fails is not a transport failure. Exec must report the
// exit code with output intact rather than collapsing it into an error, or the
// caller cannot tell "ran and failed" from "could not run".
func TestExec_NonZeroExitIsNotAnError(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-"})
	vmID := provisionRunning(t, fm)

	p.envs[vmID].execResult = ExecResult{
		ExitCode: 3,
		Stdout:   []byte("partial\n"),
		Stderr:   []byte("boom\n"),
	}

	res, err := fm.Exec(context.Background(), vmID, []string{"false"}, ExecOptions{})
	if err != nil {
		t.Fatalf("exec returned error for a non-zero exit: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}
	if string(res.Stdout) != "partial\n" {
		t.Errorf("stdout = %q, want %q", res.Stdout, "partial\n")
	}
	if string(res.Stderr) != "boom\n" {
		t.Errorf("stderr = %q, want %q", res.Stderr, "boom\n")
	}
}

// stdout and stderr must stay separate all the way up. The old Exec glued them
// together, which is precisely the fidelity this change exists to recover.
func TestExec_StreamsStaySeparate(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-"})
	vmID := provisionRunning(t, fm)

	p.envs[vmID].execResult = ExecResult{Stdout: []byte("out"), Stderr: []byte("err")}

	res, err := fm.Exec(context.Background(), vmID, []string{"sh"}, ExecOptions{})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if string(res.Stdout) != "out" || string(res.Stderr) != "err" {
		t.Errorf("stdout=%q stderr=%q, want out/err kept apart", res.Stdout, res.Stderr)
	}
}

func TestExec_TransportErrorIsReported(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-"})
	vmID := provisionRunning(t, fm)

	p.envs[vmID].execRunErr = errors.New("host agent unreachable")

	if _, err := fm.Exec(context.Background(), vmID, []string{"true"}, ExecOptions{}); err == nil {
		t.Fatal("exec: want error when the transport fails, got nil")
	}
}

// A provider with no real guest (the stub a provider degrades to when its
// BaseURL is unset) must say so rather than fake a clean run.
func TestExec_UnsupportedProviderSurfaces(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-"})
	vmID := provisionRunning(t, fm)

	p.envs[vmID].execRunErr = ErrExecUnsupported

	_, err := fm.Exec(context.Background(), vmID, []string{"true"}, ExecOptions{})
	if !errors.Is(err, ErrExecUnsupported) {
		t.Fatalf("err = %v, want ErrExecUnsupported", err)
	}
}

func TestExec_NotFound(t *testing.T) {
	fm := NewFleetManager(FleetConfig{Provider: newMockProvider(), Prefix: "fuse-"})

	_, err := fm.Exec(context.Background(), "fuse-missing", []string{"true"}, ExecOptions{})
	if !errors.Is(err, ErrVMNotFound) {
		t.Fatalf("err = %v, want ErrVMNotFound", err)
	}
}

// Draining VMs are reachable but off-limits: letting commands in behind a
// drain would undo the quiesce it is performing.
func TestExec_NotRunningRejected(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-"})
	ctx := context.Background()
	vmID := provisionRunning(t, fm)

	if err := fm.Drain(ctx, vmID); err != nil {
		t.Fatalf("drain: %v", err)
	}

	_, err := fm.Exec(ctx, vmID, []string{"true"}, ExecOptions{})
	if !errors.Is(err, ErrVMNotRunning) {
		t.Fatalf("err = %v, want ErrVMNotRunning", err)
	}
}

func TestExec_EmptyCommandRejected(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-"})
	vmID := provisionRunning(t, fm)

	if _, err := fm.Exec(context.Background(), vmID, nil, ExecOptions{}); err == nil {
		t.Fatal("exec: want error for an empty command, got nil")
	}
}

func TestExec_TimeoutDefaultedAndClamped(t *testing.T) {
	tests := []struct {
		name string
		give time.Duration
		want time.Duration
	}{
		{"zero gets the default", 0, DefaultExecTimeout},
		{"negative gets the default", -1 * time.Second, DefaultExecTimeout},
		{"in-range is passed through", 5 * time.Second, 5 * time.Second},
		{"over the ceiling is clamped", MaxExecTimeout + time.Hour, MaxExecTimeout},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newMockProvider()
			fm := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-"})
			vmID := provisionRunning(t, fm)

			if _, err := fm.Exec(context.Background(), vmID, []string{"true"}, ExecOptions{Timeout: tt.give}); err != nil {
				t.Fatalf("exec: %v", err)
			}

			opts := p.envs[vmID].execOpts
			if len(opts) != 1 {
				t.Fatalf("exec called %d times, want 1", len(opts))
			}
			if opts[0].Timeout != tt.want {
				t.Errorf("timeout = %v, want %v", opts[0].Timeout, tt.want)
			}
		})
	}
}

// attachableEnv is a mockEnv that also implements Attacher.
type attachableEnv struct {
	*mockEnv
	stream io.ReadWriteCloser
	spec   AttachSpec
}

func (e *attachableEnv) Attach(_ context.Context, spec AttachSpec) (io.ReadWriteCloser, error) {
	e.spec = spec
	return e.stream, nil
}

func TestAttach_HappyPath(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-"})
	ctx := context.Background()
	vmID := provisionRunning(t, fm)

	guest, caller := net.Pipe()
	t.Cleanup(func() { _ = guest.Close(); _ = caller.Close() })

	// Swap the tracked env for one that can attach.
	att := &attachableEnv{mockEnv: p.envs[vmID], stream: caller}
	fm.mu.Lock()
	fm.vms[vmID].env = att
	fm.mu.Unlock()

	spec := AttachSpec{Cmd: []string{"sh"}, TTY: true, Rows: 24, Cols: 80}
	stream, err := fm.Attach(ctx, vmID, spec)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if stream == nil {
		t.Fatal("attach returned a nil stream")
	}
	if att.spec.TTY != true || att.spec.Rows != 24 || att.spec.Cols != 80 {
		t.Errorf("spec = %+v, want tty with 24x80", att.spec)
	}
	if len(att.spec.Cmd) != 1 || att.spec.Cmd[0] != "sh" {
		t.Errorf("cmd = %q, want [sh]", att.spec.Cmd)
	}
}

// mockEnv does not implement Attacher, so this is the real-world shape of a
// provider that cannot open a shell.
func TestAttach_UnsupportedProvider(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-"})
	vmID := provisionRunning(t, fm)

	_, err := fm.Attach(context.Background(), vmID, AttachSpec{TTY: true})
	if !errors.Is(err, ErrAttachUnsupported) {
		t.Fatalf("err = %v, want ErrAttachUnsupported", err)
	}
}

func TestAttach_NotFound(t *testing.T) {
	fm := NewFleetManager(FleetConfig{Provider: newMockProvider(), Prefix: "fuse-"})

	_, err := fm.Attach(context.Background(), "fuse-missing", AttachSpec{})
	if !errors.Is(err, ErrVMNotFound) {
		t.Fatalf("err = %v, want ErrVMNotFound", err)
	}
}

func TestAttach_NotRunningRejected(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-"})
	ctx := context.Background()
	vmID := provisionRunning(t, fm)

	if err := fm.Drain(ctx, vmID); err != nil {
		t.Fatalf("drain: %v", err)
	}

	_, err := fm.Attach(ctx, vmID, AttachSpec{TTY: true})
	if !errors.Is(err, ErrVMNotRunning) {
		t.Fatalf("err = %v, want ErrVMNotRunning", err)
	}
}

func TestExec_RecordsActivity(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-"})
	vmID := provisionRunning(t, fm)

	// Back-date the activity clock so the bump is unambiguous.
	stale := time.Now().Add(-time.Hour)
	fm.mu.Lock()
	fm.vms[vmID].lastActivityAt = stale
	fm.mu.Unlock()

	if _, err := fm.Exec(context.Background(), vmID, []string{"true"}, ExecOptions{}); err != nil {
		t.Fatalf("exec: %v", err)
	}

	fm.mu.RLock()
	got := fm.vms[vmID].lastActivityAt
	fm.mu.RUnlock()
	if !got.After(stale) {
		t.Errorf("lastActivityAt = %v, want later than %v", got, stale)
	}
}

func TestAttach_TracksSessionUntilStreamClose(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-"})
	ctx := context.Background()
	vmID := provisionRunning(t, fm)

	guest, caller := net.Pipe()
	t.Cleanup(func() { _ = guest.Close() })

	att := &attachableEnv{mockEnv: p.envs[vmID], stream: caller}
	fm.mu.Lock()
	fm.vms[vmID].env = att
	fm.mu.Unlock()

	stream, err := fm.Attach(ctx, vmID, AttachSpec{TTY: true})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}

	fm.mu.RLock()
	open := fm.vms[vmID].attachSessions
	fm.mu.RUnlock()
	if open != 1 {
		t.Fatalf("attachSessions = %d, want 1 while attached", open)
	}

	if err := stream.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Close is idempotent from the fleet's point of view.
	_ = stream.Close()

	fm.mu.RLock()
	open = fm.vms[vmID].attachSessions
	fm.mu.RUnlock()
	if open != 0 {
		t.Errorf("attachSessions = %d, want 0 after close", open)
	}
}
