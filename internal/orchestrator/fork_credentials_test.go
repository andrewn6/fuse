package orchestrator

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/folsomintel/fuse/internal/secrets"
)

// credForkEnv records the things a fork must do to a guest that the plain
// snapshotTestEnv cannot observe: the per-vm token installed via TokenSetter,
// and whether StartAgent was called (the only step that actually replaces the
// source's token in a live fused, since fused reads its token file once at
// startup).
type credForkEnv struct {
	name string
	url  string

	mu          sync.Mutex
	files       map[string][]byte
	checkpoints []Checkpoint

	authToken       string
	startAgentCalls []AgentSpec

	// onUpload runs before each Upload records its file, so a test can drive
	// something concurrent-looking (a reconcile tick) from inside the fork's
	// credential-upload window.
	onUpload func()
}

var (
	_ Environment     = (*credForkEnv)(nil)
	_ TokenSetter     = (*credForkEnv)(nil)
	_ SnapshotCapable = (*credForkEnv)(nil)
)

func (e *credForkEnv) Name() string  { return e.name }
func (e *credForkEnv) URL() string   { return e.url }
func (e *credForkEnv) Token() string { return e.authToken }
func (e *credForkEnv) SetToken(token string) {
	e.authToken = token
}

func (e *credForkEnv) Exec(context.Context, []string, ExecOptions) (ExecResult, error) {
	return ExecResult{}, nil
}
func (e *credForkEnv) ExecStream(context.Context, io.Writer, io.Writer, string, ...string) error {
	return nil
}

func (e *credForkEnv) Upload(_ context.Context, data []byte, path string) error {
	if e.onUpload != nil {
		e.onUpload()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.files == nil {
		e.files = make(map[string][]byte)
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	e.files[path] = cp
	return nil
}

func (e *credForkEnv) StartAgent(_ context.Context, spec AgentSpec) error {
	e.startAgentCalls = append(e.startAgentCalls, spec)
	return nil
}

func (e *credForkEnv) Checkpoint(_ context.Context, comment string) (string, error) {
	id := "cp-" + comment
	e.checkpoints = append(e.checkpoints, Checkpoint{ID: id, Comment: comment})
	return id, nil
}

func (e *credForkEnv) Restore(context.Context, string) error { return nil }

func (e *credForkEnv) ListCheckpoints(context.Context) ([]Checkpoint, error) {
	return e.checkpoints, nil
}

// credForkProvider is a forkable provider whose envs are credForkEnv, so a test
// can inspect what the fork did to the new guest.
type credForkProvider struct {
	envs map[string]*credForkEnv

	// forkUploadHook is installed as onUpload on envs born from
	// CreateFromCheckpoint, so only the fork's uploads trigger it.
	forkUploadHook func()
}

func newCredForkProvider() *credForkProvider {
	return &credForkProvider{envs: make(map[string]*credForkEnv)}
}

func (p *credForkProvider) Create(_ context.Context, spec Spec) (Environment, error) {
	env := &credForkEnv{name: spec.Name, url: "http://" + spec.Name + ".test"}
	p.envs[spec.Name] = env
	return env, nil
}

func (p *credForkProvider) Get(_ context.Context, name string) (Environment, error) {
	env, ok := p.envs[name]
	if !ok {
		return nil, ErrVMNotFound
	}
	return env, nil
}

func (p *credForkProvider) Destroy(_ context.Context, name string) error {
	delete(p.envs, name)
	return nil
}

func (p *credForkProvider) List(_ context.Context, _ string) ([]Environment, error) {
	out := make([]Environment, 0, len(p.envs))
	for _, env := range p.envs {
		out = append(out, env)
	}
	return out, nil
}

func (*credForkProvider) Close() error { return nil }

// CreateFromCheckpoint models the real host: the new guest is a byte copy of the
// source's disk, so it starts life holding the SOURCE's credential files.
func (p *credForkProvider) CreateFromCheckpoint(_ context.Context, spec Spec, srcVMID, checkpointID string) (Environment, error) {
	src, ok := p.envs[srcVMID]
	if !ok {
		return nil, ErrVMNotFound
	}
	env := &credForkEnv{
		name:     spec.Name,
		url:      "http://" + spec.Name + ".test",
		files:    make(map[string][]byte, len(src.files)),
		onUpload: p.forkUploadHook,
	}
	for path, data := range src.files {
		cp := make([]byte, len(data))
		copy(cp, data)
		env.files[path] = cp
	}
	env.checkpoints = append(env.checkpoints, Checkpoint{ID: checkpointID})
	p.envs[spec.Name] = env
	return env, nil
}

func testEncryptionKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return key
}

// TestForkEnvironment_mintsItsOwnCredentials pins the security property that
// makes fork safe: the fork inherits the source's disk (and therefore the
// source's auth token file), so it must be issued its own credentials and have
// its guest agent RESTARTED with them. Without the restart, fused keeps serving
// the source's token, because it reads the token file only at startup.
func TestForkEnvironment_mintsItsOwnCredentials(t *testing.T) {
	provider := newCredForkProvider()
	fm := NewFleetManager(FleetConfig{
		Provider:           provider,
		Prefix:             "fuse-",
		TokenEncryptionKey: testEncryptionKey(),
	})
	srcID := provisionSnapshotTestVM(t, fm, "task-1")

	srcEnv := provider.envs[srcID]
	srcTokenFile := string(srcEnv.files[fuseAuthTokenPath])
	if srcTokenFile == "" {
		t.Fatal("source guest should have an auth token file after provisioning")
	}

	newID, err := fm.ForkEnvironment(context.Background(), srcID, ForkOptions{})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}

	forkEnv := provider.envs[newID]
	if forkEnv == nil {
		t.Fatalf("provider has no env for fork %s", newID)
	}

	// the fork's guest must no longer be holding the source's token.
	forkTokenFile := string(forkEnv.files[fuseAuthTokenPath])
	if forkTokenFile == "" {
		t.Fatal("fork guest has no auth token file")
	}
	if forkTokenFile == srcTokenFile {
		t.Fatal("fork guest is still holding the SOURCE's auth token; two live vms would share one secret")
	}

	// and the agent must have been restarted with it, or the running fused
	// would keep serving the token it read at boot (the source's).
	if len(forkEnv.startAgentCalls) != 1 {
		t.Fatalf("StartAgent calls on fork = %d, want 1 (fused must be restarted to pick up the new token)", len(forkEnv.startAgentCalls))
	}
	if got := forkEnv.startAgentCalls[0].AuthToken; got != forkTokenFile {
		t.Fatalf("StartAgent auth token = %q, want the fork's own token %q", got, forkTokenFile)
	}
	if forkEnv.startAgentCalls[0].DrainCommand == "" {
		t.Fatal("fork's agent spec has no drain command, so Drain would skip graceful shutdown")
	}

	// the orchestrator's own record must carry the fork's token, decryptable
	// and distinct from the source's.
	fm.mu.RLock()
	forkVM := fm.vms[newID]
	srcVM := fm.vms[srcID]
	forkEnc := forkVM.authTokenEncrypted
	srcEnc := srcVM.authTokenEncrypted
	forkDrain := forkVM.drainCommand
	fm.mu.RUnlock()

	if len(forkEnc) == 0 {
		t.Fatal("fork vm record has no encrypted auth token")
	}
	plain, err := secrets.DecryptToken(forkEnc, testEncryptionKey())
	if err != nil {
		t.Fatalf("decrypt fork token: %v", err)
	}
	if plain != forkTokenFile {
		t.Fatalf("fork record token = %q, want the token on its guest %q", plain, forkTokenFile)
	}
	if string(forkEnc) == string(srcEnc) {
		t.Fatal("fork and source have the same encrypted token")
	}
	if forkDrain == "" {
		t.Fatal("fork vm record has no drain command")
	}
}

// TestForkEnvironment_survivesReconcileDuringCredentialUpload pins the fix for
// the orphan-destroy race: the forked microVM exists on the host from
// CreateFromCheckpoint onward, but the fork is not finished until its own
// credentials are uploaded and fused is restarted. reconcileOrphans destroys
// any provider vm that is not in fm.vms, so a reconcile tick landing in that
// window used to delete the fork out from under itself.
//
// the test reproduces the race deterministically by running a full reconcile
// pass from inside the fork's first credential upload, which is exactly the
// unprotected window.
func TestForkEnvironment_survivesReconcileDuringCredentialUpload(t *testing.T) {
	provider := newCredForkProvider()
	fm := NewFleetManager(FleetConfig{
		Provider:           provider,
		Prefix:             "fuse-",
		TokenEncryptionKey: testEncryptionKey(),
	})
	srcID := provisionSnapshotTestVM(t, fm, "task-1")

	// uploadFiles uploads the credential files concurrently, so run exactly one
	// reconcile pass: the point is that a tick landed in the window, not how
	// many did, and the fake provider's env map is not safe for concurrent use.
	var once sync.Once
	ticked := false
	provider.forkUploadHook = func() {
		once.Do(func() {
			ticked = true
			fm.reconcile(context.Background())
		})
	}

	newID, err := fm.ForkEnvironment(context.Background(), srcID, ForkOptions{})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if !ticked {
		t.Fatal("reconcile never ran during the credential upload; the test is not exercising the race")
	}

	// the fork must still exist on the provider: reconcile must not have
	// treated the half-finished fork as an orphan.
	if provider.envs[newID] == nil {
		t.Fatalf("fork %s was destroyed by reconcile during its credential-upload window", newID)
	}
	// and it must have completed normally, running and tracked.
	fm.mu.RLock()
	forkVM, tracked := fm.vms[newID]
	var state VMState
	if tracked {
		state = forkVM.state
	}
	fm.mu.RUnlock()
	if !tracked {
		t.Fatalf("fork %s is not tracked in the fleet after ForkEnvironment returned", newID)
	}
	if state != VMStateRunning {
		t.Fatalf("fork %s state = %s, want %s", newID, state, VMStateRunning)
	}
}

// TestForkEnvironment_chargesSourceHost pins the capacity invariant. A fork is a
// real vm on the source's host, so it must be allocated there. If it were not,
// DestroyVM (which deallocates unconditionally by the vm's spec) would credit
// back capacity the host never charged for, permanently under-counting the host
// and letting the scheduler overcommit it.
func TestForkEnvironment_chargesSourceHost(t *testing.T) {
	provider := newCredForkProvider()
	fm := NewFleetManager(FleetConfig{Provider: provider, Prefix: "fuse-"})

	host := Host{
		ID:       "host-1",
		URL:      "http://host-1.test",
		Backend:  BackendFirecracker,
		Capacity: HostCapacity{CPUs: 8, RamMB: 8192, StorageGB: 100, VMCount: 10},
	}
	if err := fm.RegisterHost(context.Background(), host, provider); err != nil {
		t.Fatalf("register host: %v", err)
	}

	spec := Spec{CPUs: 2, RamMB: 1024, StorageGB: 10}
	if _, err := fm.ProvisionAndAssign(context.Background(), "task-1", spec, []byte(`{"version":"1","services":{}}`), nil, BootOptions{}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	srcID := "fuse-task-1"

	afterProvision := findHost(t, fm, "host-1").Allocated
	if afterProvision.CPUs != 2 || afterProvision.RamMB != 1024 {
		t.Fatalf("allocated after provision = %+v, want 2 cpus / 1024 MB", afterProvision)
	}

	newID, err := fm.ForkEnvironment(context.Background(), srcID, ForkOptions{})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}

	afterFork := findHost(t, fm, "host-1").Allocated
	if afterFork.CPUs != 4 || afterFork.RamMB != 2048 {
		t.Fatalf("allocated after fork = %+v, want the fork charged too (4 cpus / 2048 MB)", afterFork)
	}
	if afterFork.VMCount != 2 {
		t.Fatalf("vm count after fork = %d, want 2", afterFork.VMCount)
	}

	// destroying the fork must give back exactly the fork's share, leaving the
	// source's allocation intact.
	if err := fm.DestroyVM(context.Background(), newID); err != nil {
		t.Fatalf("destroy fork: %v", err)
	}
	afterDestroy := findHost(t, fm, "host-1").Allocated
	if afterDestroy.CPUs != 2 || afterDestroy.RamMB != 1024 {
		t.Fatalf("allocated after destroying fork = %+v, want the source's 2 cpus / 1024 MB still charged", afterDestroy)
	}
	if afterDestroy.VMCount != 1 {
		t.Fatalf("vm count after destroying fork = %d, want 1", afterDestroy.VMCount)
	}
}
