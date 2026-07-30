package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

// mockEnv implements Environment for testing.
//
// ExecStream records every command (guarded by a mutex) so drain tests can
// assert the configured DrainCommand was actually run. execErr, when set,
// makes ExecStream fail so a test can exercise the drain-failure path.
type mockEnv struct {
	name string
	url  string

	mu        sync.Mutex
	execCalls [][]string
	execErr   error
	execHook  func(ctx context.Context) error

	// Exec (as opposed to ExecStream) fixtures: what it recorded, and what
	// it should hand back.
	execArgv   [][]string
	execOpts   []ExecOptions
	execResult ExecResult
	execRunErr error
}

func (e *mockEnv) Name() string  { return e.name }
func (e *mockEnv) URL() string   { return e.url }
func (e *mockEnv) Token() string { return "" }

// Exec records the argv and options it was called with and replays a
// programmed result, so exec tests can assert both what reached the guest and
// what came back. It deliberately does not implement Attacher: that absence is
// what lets a test exercise the ErrAttachUnsupported path.
func (e *mockEnv) Exec(_ context.Context, cmd []string, opts ExecOptions) (ExecResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.execArgv = append(e.execArgv, cmd)
	e.execOpts = append(e.execOpts, opts)
	return e.execResult, e.execRunErr
}
func (e *mockEnv) ExecStream(ctx context.Context, _, _ io.Writer, name string, args ...string) error {
	e.mu.Lock()
	e.execCalls = append(e.execCalls, append([]string{name}, args...))
	hook := e.execHook
	err := e.execErr
	e.mu.Unlock()
	if hook != nil {
		if hookErr := hook(ctx); hookErr != nil {
			return hookErr
		}
	}
	return err
}

// execCommands returns a copy of the recorded ExecStream calls.
func (e *mockEnv) execCommands() [][]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([][]string, len(e.execCalls))
	copy(out, e.execCalls)
	return out
}

func (e *mockEnv) Upload(_ context.Context, _ []byte, _ string) error      { return nil }
func (e *mockEnv) StartAgent(_ context.Context, _ AgentSpec) error         { return nil }
func (e *mockEnv) Checkpoint(_ context.Context, _ string) (string, error)  { return "cp-1", nil }
func (e *mockEnv) Restore(_ context.Context, _ string) error               { return nil }
func (e *mockEnv) ListCheckpoints(_ context.Context) ([]Checkpoint, error) { return nil, nil }

// mockProvider implements Provider for testing.
type mockProvider struct {
	mu       sync.Mutex
	envs     map[string]*mockEnv
	createFn func(ctx context.Context, spec Spec) (Environment, error)
}

func newMockProvider() *mockProvider {
	return &mockProvider{envs: make(map[string]*mockEnv)}
}

func (p *mockProvider) Create(_ context.Context, spec Spec) (Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.createFn != nil {
		return p.createFn(context.Background(), spec)
	}

	e := &mockEnv{name: spec.Name, url: "http://" + spec.Name + ".test"}
	p.envs[spec.Name] = e
	return e, nil
}

func (p *mockProvider) Get(_ context.Context, name string) (Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	e, ok := p.envs[name]
	if !ok {
		return nil, fmt.Errorf("not found: %s", name)
	}
	return e, nil
}

func (p *mockProvider) Destroy(_ context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.envs, name)
	return nil
}

func (p *mockProvider) List(_ context.Context, _ string) ([]Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([]Environment, 0, len(p.envs))
	for _, e := range p.envs {
		out = append(out, e)
	}
	return out, nil
}

func (p *mockProvider) Close() error { return nil }

func (p *mockProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.envs)
}

type flakyStateStore struct {
	*MemoryStateStore
	mu                sync.Mutex
	failCompletedTask bool
	failFailedTask    bool
}

func newFlakyStateStore() *flakyStateStore {
	return &flakyStateStore{MemoryStateStore: NewMemoryStateStore()}
}

func (s *flakyStateStore) setFailCompletedTask(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failCompletedTask = v
}

func (s *flakyStateStore) setFailFailedTask(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failFailedTask = v
}

func (s *flakyStateStore) UpsertTask(ctx context.Context, task TaskRecord) error {
	s.mu.Lock()
	fail := (s.failCompletedTask && task.RunStatus == TaskRunCompleted) ||
		(s.failFailedTask && task.RunStatus == TaskRunFailed && task.LastError == "vm force destroyed")
	s.mu.Unlock()
	if fail {
		return fmt.Errorf("injected upsert task failure for status %s", task.RunStatus)
	}
	return s.MemoryStateStore.UpsertTask(ctx, task)
}

func TestProvisionAndAssign(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: p,
		Prefix:   "fuse-",
	})

	ctx := context.Background()
	info, err := fm.ProvisionAndAssign(ctx, "task-1", Spec{CPUs: 2, RamMB: 1024}, []byte(`{}`), nil, BootOptions{})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	if info.ID != "fuse-task-1" {
		t.Errorf("got id %q, want fuse-task-1", info.ID)
	}
	if info.State != VMStateRunning {
		t.Errorf("got state %q, want running", info.State)
	}
	if info.TaskID != "task-1" {
		t.Errorf("got task %q, want task-1", info.TaskID)
	}
	if info.URL != "http://fuse-task-1.test" {
		t.Errorf("got url %q, want http://fuse-task-1.test", info.URL)
	}
}

func TestDuplicateTaskRejected(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: p,
		Prefix:   "fuse-",
	})

	ctx := context.Background()
	_, err := fm.ProvisionAndAssign(ctx, "task-1", Spec{}, []byte(`{}`), nil, BootOptions{})
	if err != nil {
		t.Fatalf("first provision: %v", err)
	}

	_, err = fm.ProvisionAndAssign(ctx, "task-1", Spec{}, []byte(`{}`), nil, BootOptions{})
	if err == nil {
		t.Fatal("expected error for duplicate task, got nil")
	}
}

func TestCompleteTaskDestroysVM(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: p,
		Prefix:   "fuse-",
	})

	ctx := context.Background()
	_, err := fm.ProvisionAndAssign(ctx, "task-1", Spec{}, []byte(`{}`), nil, BootOptions{})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	if err := fm.CompleteTask("task-1"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// Wait for async destroy.
	time.Sleep(100 * time.Millisecond)

	if p.count() != 0 {
		t.Error("expected provider to have 0 envs after complete")
	}
	if len(fm.ListFleet()) != 0 {
		t.Error("expected fleet to be empty after complete")
	}
}

func TestCompleteTaskNotFound(t *testing.T) {
	fm := NewFleetManager(FleetConfig{
		Provider: newMockProvider(),
		Prefix:   "fuse-",
	})

	if err := fm.CompleteTask("nonexistent"); err == nil {
		t.Fatal("expected error for nonexistent task")
	}
}

func TestCompleteTaskRollbackOnPersistFailure(t *testing.T) {
	p := newMockProvider()
	store := newFlakyStateStore()
	fm := NewFleetManager(FleetConfig{
		Provider:   p,
		Prefix:     "fuse-",
		StateStore: store,
	})

	ctx := context.Background()
	_, err := fm.ProvisionAndAssign(ctx, "task-1", Spec{}, []byte(`{}`), nil, BootOptions{})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	store.setFailCompletedTask(true)
	if err := fm.CompleteTask("task-1"); err == nil {
		t.Fatal("expected complete task to fail when task persistence fails")
	}

	info, ok := fm.GetVMByTask("task-1")
	if !ok {
		t.Fatal("expected task assignment to be rolled back in memory")
	}
	if info.State != VMStateRunning {
		t.Fatalf("expected vm state rolled back to running, got %s", info.State)
	}

	store.setFailCompletedTask(false)
	if err := fm.CompleteTask("task-1"); err != nil {
		t.Fatalf("retry complete should succeed: %v", err)
	}

	waitFor(t, 2*time.Second, func() bool {
		return p.count() == 0 && len(fm.ListFleet()) == 0
	}, "expected vm destroyed after retrying complete")
}

func TestListFleet(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: p,
		Prefix:   "fuse-",
	})

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_, err := fm.ProvisionAndAssign(ctx, fmt.Sprintf("task-%d", i), Spec{}, []byte(`{}`), nil, BootOptions{})
		if err != nil {
			t.Fatalf("provision %d: %v", i, err)
		}
	}

	fleet := fm.ListFleet()
	if len(fleet) != 3 {
		t.Fatalf("got %d vms, want 3", len(fleet))
	}
}

func TestGetVM(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: p,
		Prefix:   "fuse-",
	})

	ctx := context.Background()
	_, _ = fm.ProvisionAndAssign(ctx, "task-1", Spec{}, []byte(`{}`), nil, BootOptions{})

	info, ok := fm.GetVM("fuse-task-1")
	if !ok {
		t.Fatal("expected to find vm")
	}
	if info.TaskID != "task-1" {
		t.Errorf("got task %q, want task-1", info.TaskID)
	}

	_, ok = fm.GetVM("nonexistent")
	if ok {
		t.Error("expected not to find nonexistent vm")
	}
}

func TestGetVMByTask(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: p,
		Prefix:   "fuse-",
	})

	ctx := context.Background()
	_, _ = fm.ProvisionAndAssign(ctx, "task-1", Spec{}, []byte(`{}`), nil, BootOptions{})

	info, ok := fm.GetVMByTask("task-1")
	if !ok {
		t.Fatal("expected to find vm by task")
	}
	if info.ID != "fuse-task-1" {
		t.Errorf("got id %q, want fuse-task-1", info.ID)
	}
}

func TestDestroyVM(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: p,
		Prefix:   "fuse-",
	})

	ctx := context.Background()
	_, _ = fm.ProvisionAndAssign(ctx, "task-1", Spec{}, []byte(`{}`), nil, BootOptions{})

	if err := fm.DestroyVM(ctx, "fuse-task-1"); err != nil {
		t.Fatalf("destroy: %v", err)
	}

	if p.count() != 0 {
		t.Error("expected provider to have 0 envs")
	}
	if len(fm.ListFleet()) != 0 {
		t.Error("expected fleet to be empty")
	}
}

func TestDestroyVMRollbackOnPersistFailure(t *testing.T) {
	p := newMockProvider()
	store := newFlakyStateStore()
	fm := NewFleetManager(FleetConfig{
		Provider:   p,
		Prefix:     "fuse-",
		StateStore: store,
	})

	ctx := context.Background()
	_, err := fm.ProvisionAndAssign(ctx, "task-1", Spec{}, []byte(`{}`), nil, BootOptions{})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	store.setFailFailedTask(true)
	if err := fm.DestroyVM(ctx, "fuse-task-1"); err == nil {
		t.Fatal("expected destroy to fail when task persistence fails")
	}

	info, ok := fm.GetVM("fuse-task-1")
	if !ok {
		t.Fatal("expected vm to remain tracked after failed destroy persistence")
	}
	if info.State != VMStateRunning {
		t.Fatalf("expected vm state rolled back to running, got %s", info.State)
	}
	if info.TaskID != "task-1" {
		t.Fatalf("expected task binding rolled back, got %q", info.TaskID)
	}

	store.setFailFailedTask(false)
	if err := fm.DestroyVM(ctx, "fuse-task-1"); err != nil {
		t.Fatalf("retry destroy should succeed: %v", err)
	}

	if p.count() != 0 {
		t.Error("expected provider envs to be empty after retry destroy")
	}
	if len(fm.ListFleet()) != 0 {
		t.Error("expected fleet to be empty after retry destroy")
	}
}

func TestReconcileOrphans(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{
		Provider:          p,
		Prefix:            "fuse-",
		ReconcileInterval: 50 * time.Millisecond,
	})

	// Inject an orphan directly into the provider (not via fleet manager).
	p.mu.Lock()
	p.envs["fuse-orphan"] = &mockEnv{name: "fuse-orphan", url: "http://orphan.test"}
	p.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fm.Start(ctx)

	// Wait for reconcile tick.
	time.Sleep(200 * time.Millisecond)
	fm.Stop()

	if p.count() != 0 {
		t.Errorf("expected orphan destroyed, got %d envs", p.count())
	}
}

func TestReconcileDeadVM(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{
		Provider:          p,
		Prefix:            "fuse-",
		ReconcileInterval: 50 * time.Millisecond,
	})

	ctx := context.Background()
	_, _ = fm.ProvisionAndAssign(ctx, "task-1", Spec{}, []byte(`{}`), nil, BootOptions{})

	// Simulate provider losing the VM.
	p.mu.Lock()
	delete(p.envs, "fuse-task-1")
	p.mu.Unlock()

	ctx2, cancel := context.WithCancel(context.Background())
	defer cancel()
	fm.Start(ctx2)

	time.Sleep(200 * time.Millisecond)
	fm.Stop()

	if len(fm.ListFleet()) != 0 {
		t.Error("expected dead vm removed from fleet")
	}
}

func TestProvisionFailureCleanup(t *testing.T) {
	p := newMockProvider()
	p.createFn = func(_ context.Context, _ Spec) (Environment, error) {
		return nil, fmt.Errorf("provider unavailable")
	}

	fm := NewFleetManager(FleetConfig{
		Provider: p,
		Prefix:   "fuse-",
	})

	ctx := context.Background()
	_, err := fm.ProvisionAndAssign(ctx, "task-fail", Spec{}, []byte(`{}`), nil, BootOptions{})
	if err == nil {
		t.Fatal("expected error on provision failure")
	}

	if len(fm.ListFleet()) != 0 {
		t.Error("expected failed vm removed from fleet")
	}
}

func TestStateRecoveryLoadsPersistedVMs(t *testing.T) {
	p := newMockProvider()
	store := NewMemoryStateStore()

	fm := NewFleetManager(FleetConfig{
		Provider:   p,
		Prefix:     "fuse-",
		StateStore: store,
	})

	ctx := context.Background()
	_, err := fm.ProvisionAndAssign(ctx, "task-1", Spec{}, []byte(`{}`), nil, BootOptions{})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	recovered := NewFleetManager(FleetConfig{
		Provider:   p,
		Prefix:     "fuse-",
		StateStore: store,
	})
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recovered.Start(runCtx)
	defer recovered.Stop()

	waitFor(t, 2*time.Second, func() bool {
		info, ok := recovered.GetVM("fuse-task-1")
		return ok && info.State == VMStateRunning && info.TaskID == "task-1"
	}, "expected vm to be recovered from persisted state")
}

func TestStateRecoveryMarksMissingProviderVMTaskFailed(t *testing.T) {
	p := newMockProvider()
	store := NewMemoryStateStore()

	fm := NewFleetManager(FleetConfig{
		Provider:   p,
		Prefix:     "fuse-",
		StateStore: store,
	})

	ctx := context.Background()
	_, err := fm.ProvisionAndAssign(ctx, "task-1", Spec{}, []byte(`{}`), nil, BootOptions{})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	p.mu.Lock()
	delete(p.envs, "fuse-task-1")
	p.mu.Unlock()

	recovered := NewFleetManager(FleetConfig{
		Provider:   p,
		Prefix:     "fuse-",
		StateStore: store,
	})
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recovered.Start(runCtx)
	defer recovered.Stop()

	waitFor(t, 2*time.Second, func() bool {
		tasks, listErr := store.ListTasks(context.Background())
		if listErr != nil {
			return false
		}
		for _, task := range tasks {
			if task.TaskID == "task-1" {
				return task.RunStatus == TaskRunFailed
			}
		}
		return false
	}, "expected missing recovered vm task to be marked failed")
}

func waitFor(t *testing.T, timeout time.Duration, fn func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

// TestProvisionAndAssign_placementOnNoHostsPathRejected covers the legacy
// fallback: with no hosts registered, ProvisionAndAssign boots on the default
// provider without calling Schedule, so a pin or a selector there could never
// be honored. It must fail instead of silently booting somewhere else.
func TestProvisionAndAssign_placementOnNoHostsPathRejected(t *testing.T) {
	cases := []struct {
		name string
		spec Spec
	}{
		{"host pin", Spec{CPUs: 2, RamMB: 1024, HostID: "build-3"}},
		{"label selector", Spec{CPUs: 2, RamMB: 1024, Labels: map[string]string{"disk": "nvme"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newMockProvider()
			fm := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-"})
			_, err := fm.ProvisionAndAssign(context.Background(), "task-1", tc.spec, []byte(`{}`), nil, BootOptions{})
			if !errors.Is(err, ErrNoHosts) {
				t.Fatalf("err = %v, want ErrNoHosts", err)
			}
			if vms := fm.ListFleet(); len(vms) != 0 {
				t.Errorf("fleet = %+v, want no vm left behind", vms)
			}
		})
	}
}

// TestProvisionAndAssign_noPlacementOnNoHostsPathUnchanged is the control for
// the guard above: an empty placement still boots on the default provider.
func TestProvisionAndAssign_noPlacementOnNoHostsPathUnchanged(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-"})
	info, err := fm.ProvisionAndAssign(context.Background(), "task-1", Spec{CPUs: 2, RamMB: 1024}, []byte(`{}`), nil, BootOptions{})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if info.State != VMStateRunning {
		t.Errorf("state = %q, want running", info.State)
	}
}
