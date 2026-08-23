package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// ── Test doubles ──────────────────────────────────────────────────
//
// plainEnv is a minimal Environment that deliberately does NOT implement
// orchestrator.SnapshotCapable, mirroring the real qemu env. It lets the
// handler layer exercise the "provider cannot snapshot" and gpu guardrail
// paths without importing internal/qemu.

type plainEnv struct {
	name string
	url  string
}

func (e *plainEnv) Name() string  { return e.name }
func (e *plainEnv) URL() string   { return e.url }
func (e *plainEnv) Token() string { return "" }
func (e *plainEnv) Exec(context.Context, []string, orchestrator.ExecOptions) (orchestrator.ExecResult, error) {
	return orchestrator.ExecResult{}, nil
}
func (e *plainEnv) ExecStream(context.Context, io.Writer, io.Writer, string, ...string) error {
	return nil
}
func (e *plainEnv) Upload(context.Context, []byte, string) error             { return nil }
func (e *plainEnv) StartAgent(context.Context, orchestrator.AgentSpec) error { return nil }

// plainProvider hands out plainEnv handles.
type plainProvider struct {
	envs map[string]*plainEnv
}

func newPlainProvider() *plainProvider {
	return &plainProvider{envs: make(map[string]*plainEnv)}
}

func (p *plainProvider) Create(_ context.Context, spec orchestrator.Spec) (orchestrator.Environment, error) {
	e := &plainEnv{name: spec.Name, url: "http://" + spec.Name + ".test"}
	p.envs[spec.Name] = e
	return e, nil
}

func (p *plainProvider) Get(_ context.Context, name string) (orchestrator.Environment, error) {
	e, ok := p.envs[name]
	if !ok {
		return nil, orchestrator.ErrVMNotFound
	}
	return e, nil
}

func (p *plainProvider) Destroy(_ context.Context, name string) error {
	delete(p.envs, name)
	return nil
}

func (p *plainProvider) List(_ context.Context, _ string) ([]orchestrator.Environment, error) {
	out := make([]orchestrator.Environment, 0, len(p.envs))
	for _, e := range p.envs {
		out = append(out, e)
	}
	return out, nil
}

func (*plainProvider) Close() error { return nil }

// newPlainHandler wires a handler whose envs are not SnapshotCapable.
func newPlainHandler(t *testing.T) (*Handler, *orchestrator.FleetManager, *plainProvider) {
	t.Helper()
	p := newPlainProvider()
	fm := orchestrator.NewFleetManager(orchestrator.FleetConfig{
		Provider: p,
		Prefix:   "fuse-",
	})
	return &Handler{Fleet: fm}, fm, p
}

// ── Helpers ───────────────────────────────────────────────────────

// createEnv posts an environment and fails the test unless it is created.
func createEnv(t *testing.T, r http.Handler, req CreateEnvironmentRequest) Environment {
	t.Helper()
	rr := doJSON(t, r, http.MethodPost, "/v1/environments", req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create env status = %d, want 201. body: %s", rr.Code, rr.Body.String())
	}
	var env Environment
	if err := json.NewDecoder(rr.Body).Decode(&env); err != nil {
		t.Fatalf("decode environment: %v", err)
	}
	return env
}

// registerGPUHost registers a qemu-backed host advertising gpus, which the
// scheduler requires before it will place a gpu workload.
func registerGPUHost(t *testing.T, fm *orchestrator.FleetManager, p orchestrator.Provider) {
	t.Helper()
	host := orchestrator.Host{
		ID:      "gpu-host",
		URL:     "http://gpu-host.test",
		Backend: orchestrator.BackendQEMU,
		Capacity: orchestrator.HostCapacity{
			CPUs: 8, RamMB: 4096, StorageGB: 100, VMCount: 10,
			GPUs: 1, GPUKind: "a100",
		},
	}
	if err := fm.RegisterHost(context.Background(), host, p); err != nil {
		t.Fatalf("register gpu host: %v", err)
	}
}

// drainEnv drains an environment so it leaves the Running state.
func drainEnv(t *testing.T, r http.Handler, vmID string) {
	t.Helper()
	rr := doJSON(t, r, http.MethodPost, "/v1/environments/"+vmID+"?action=drain", nil)
	if rr.Code != http.StatusOK && rr.Code != http.StatusNoContent {
		t.Fatalf("drain status = %d, want 200 or 204. body: %s", rr.Code, rr.Body.String())
	}
}

// ── Snapshot preconditions ────────────────────────────────────────

// TestCreateSnapshot_nonRunningVMReturns409 covers snapshots.go:61. Before the
// sentinel fix this returned 500/internal because the bare fmt.Errorf fell
// through to the default arm of classifyFleetError.
func TestCreateSnapshot_nonRunningVMReturns409(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	env := createEnv(t, r, CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
	})
	drainEnv(t, r, env.ID)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments/"+env.ID+"/snapshots", nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409. body: %s", rr.Code, rr.Body.String())
	}
	if e := decodeError(t, rr.Body); e.Error.Code != CodeConflict {
		t.Errorf("error code = %q, want %q", e.Error.Code, CodeConflict)
	}
}

// TestCreateSnapshot_gpuEnvReturns409 covers snapshots.go:99. A gpu vm holds a
// vfio passthrough device that cannot be checkpointed (d4), a permanent
// property of the environment, so 409 rather than 500.
func TestCreateSnapshot_gpuEnvReturns409(t *testing.T) {
	h, fm, p := newPlainHandler(t)
	r := mustRouter(t, h)
	registerGPUHost(t, fm, p)

	env := createEnv(t, r, CreateEnvironmentRequest{
		TaskID:         "task-gpu",
		Spec:           ResourceSpec{GPUs: 1, GPUKind: "a100"},
		ManifestInline: encodeManifest(t),
	})

	rr := doJSON(t, r, http.MethodPost, "/v1/environments/"+env.ID+"/snapshots", nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409. body: %s", rr.Code, rr.Body.String())
	}
	if e := decodeError(t, rr.Body); e.Error.Code != CodeConflict {
		t.Errorf("error code = %q, want %q", e.Error.Code, CodeConflict)
	}
}

// TestCreateSnapshot_providerUnsupportedReturns501 covers snapshots.go:101.
// This follows the existing ErrExecUnsupported/ErrAttachUnsupported convention:
// a provider capability gap is 501/unimplemented, not 500/internal.
func TestCreateSnapshot_providerUnsupportedReturns501(t *testing.T) {
	h, _, _ := newPlainHandler(t)
	r := mustRouter(t, h)

	env := createEnv(t, r, CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
	})

	rr := doJSON(t, r, http.MethodPost, "/v1/environments/"+env.ID+"/snapshots", nil)
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501. body: %s", rr.Code, rr.Body.String())
	}
	if e := decodeError(t, rr.Body); e.Error.Code != CodeUnimplemented {
		t.Errorf("error code = %q, want %q", e.Error.Code, CodeUnimplemented)
	}
}

// TestCreateSnapshot_liveOnDiskOnlyProviderReturns501 covers the second,
// narrower capability assertion. A provider that can checkpoint but not
// capture memory is a capability gap, not a conflict: nothing about the VM's
// state is wrong, and the same call without live succeeds. The docs and all
// three SDKs promise 501/unimplemented here, and specifically promise that a
// conflict check does not match it.
func TestCreateSnapshot_liveOnDiskOnlyProviderReturns501(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	env := createEnv(t, r, CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
	})

	// the same env takes a plain disk snapshot happily, which is what makes
	// this a live-specific rejection rather than a provider with no snapshot
	// support at all.
	if rr := doJSON(t, r, http.MethodPost, "/v1/environments/"+env.ID+"/snapshots", CreateSnapshotRequest{}); rr.Code != http.StatusCreated {
		t.Fatalf("disk snapshot status = %d, want 201. body: %s", rr.Code, rr.Body.String())
	}

	rr := doJSON(t, r, http.MethodPost, "/v1/environments/"+env.ID+"/snapshots", CreateSnapshotRequest{Live: true})
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501. body: %s", rr.Code, rr.Body.String())
	}
	e := decodeError(t, rr.Body)
	if e.Error.Code != CodeUnimplemented {
		t.Errorf("error code = %q, want %q", e.Error.Code, CodeUnimplemented)
	}
	// the message has to name the way out, otherwise 501 reads as "never
	// snapshot this vm" rather than "drop the flag".
	if !strings.Contains(e.Error.Message, "retry without live") {
		t.Errorf("message = %q, want it to suggest retrying without live", e.Error.Message)
	}
}

// TestCreateSnapshot_gpuEnvWithLiveStillReturns409 pins the ordering of the
// two capability checks. A GPU env implements neither snapshot interface, so
// a naive implementation would report the live gap; the useful answer is that
// a vfio device cannot be checkpointed at all, which no flag change fixes.
// The api layer must not flatten the two into one status.
func TestCreateSnapshot_gpuEnvWithLiveStillReturns409(t *testing.T) {
	h, fm, p := newPlainHandler(t)
	r := mustRouter(t, h)
	registerGPUHost(t, fm, p)

	env := createEnv(t, r, CreateEnvironmentRequest{
		TaskID:         "task-gpu",
		Spec:           ResourceSpec{GPUs: 1, GPUKind: "a100"},
		ManifestInline: encodeManifest(t),
	})

	rr := doJSON(t, r, http.MethodPost, "/v1/environments/"+env.ID+"/snapshots", CreateSnapshotRequest{Live: true})
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (gpu reason wins over the live gap). body: %s", rr.Code, rr.Body.String())
	}
	e := decodeError(t, rr.Body)
	if e.Error.Code != CodeConflict {
		t.Errorf("error code = %q, want %q", e.Error.Code, CodeConflict)
	}
	if !strings.Contains(e.Error.Message, "gpu") {
		t.Errorf("message = %q, want the gpu-specific explanation", e.Error.Message)
	}
}

// TestRestoreSnapshot_nonRunningVMReturns409 covers snapshots.go:301. The
// snapshot is taken while the vm is running, then the vm is drained so the
// restore hits the state guard rather than a missing-snapshot path.
func TestRestoreSnapshot_nonRunningVMReturns409(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	env := createEnv(t, r, CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
	})

	snapRR := doJSON(t, r, http.MethodPost, "/v1/environments/"+env.ID+"/snapshots", nil)
	if snapRR.Code != http.StatusCreated && snapRR.Code != http.StatusOK {
		t.Fatalf("create snapshot status = %d. body: %s", snapRR.Code, snapRR.Body.String())
	}
	var snap Snapshot
	if err := json.NewDecoder(snapRR.Body).Decode(&snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}

	drainEnv(t, r, env.ID)

	rr := doJSON(t, r, http.MethodPost, "/v1/snapshots/"+snap.ID+"?action=restore", nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409. body: %s", rr.Code, rr.Body.String())
	}
	if e := decodeError(t, rr.Body); e.Error.Code != CodeConflict {
		t.Errorf("error code = %q, want %q", e.Error.Code, CodeConflict)
	}
}

// ── Fork precondition ─────────────────────────────────────────────

// TestForkEnvironment_gpuEnvReturns409 covers fork.go:100.
func TestForkEnvironment_gpuEnvReturns409(t *testing.T) {
	h, fm, p := newPlainHandler(t)
	r := mustRouter(t, h)
	registerGPUHost(t, fm, p)

	env := createEnv(t, r, CreateEnvironmentRequest{
		TaskID:         "task-gpu",
		Spec:           ResourceSpec{GPUs: 1, GPUKind: "a100"},
		ManifestInline: encodeManifest(t),
	})

	rr := doJSON(t, r, http.MethodPost, "/v1/environments/"+env.ID+"?action=fork", nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409. body: %s", rr.Code, rr.Body.String())
	}
	if e := decodeError(t, rr.Body); e.Error.Code != CodeConflict {
		t.Errorf("error code = %q, want %q", e.Error.Code, CodeConflict)
	}
}

// ── Host precondition ─────────────────────────────────────────────

// TestRemoveHost_withVMsReturns409 covers fleet_hosts.go:104. Removing a host
// that still has VMs assigned is a caller sequencing error (cordon and drain
// first), not a server fault.
func TestRemoveHost_withVMsReturns409(t *testing.T) {
	h, fm, p := newTestHandler(t)
	r := mustRouter(t, h)

	host := orchestrator.Host{
		ID:  "host-1",
		URL: "http://host-1.test",
		Capacity: orchestrator.HostCapacity{
			CPUs: 8, RamMB: 4096, StorageGB: 100, VMCount: 10,
		},
	}
	if err := fm.RegisterHost(context.Background(), host, p); err != nil {
		t.Fatalf("register host: %v", err)
	}

	createEnv(t, r, CreateEnvironmentRequest{
		TaskID:         "task-1",
		Spec:           ResourceSpec{CPUs: 2, RamMB: 1024},
		ManifestInline: encodeManifest(t),
	})

	rr := doJSON(t, r, http.MethodDelete, "/v1/hosts/host-1", nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409. body: %s", rr.Code, rr.Body.String())
	}
	if e := decodeError(t, rr.Body); e.Error.Code != CodeConflict {
		t.Errorf("error code = %q, want %q", e.Error.Code, CodeConflict)
	}
}

// ── Startup script preconditions ──────────────────────────────────

// A startup script that never returns must fail the create with 400 rather
// than blocking until the HTTP write timeout truncates the response, which
// surfaced to callers as an opaque 500 with a VM left running.
func TestCreateEnvironment_startupScriptTimeoutReturns400(t *testing.T) {
	p := newFakeProvider()
	p.blockExec = true
	fm := orchestrator.NewFleetManager(orchestrator.FleetConfig{
		Provider:             p,
		Prefix:               "fuse-",
		StartupScriptTimeout: 50 * time.Millisecond,
	})
	r := mustRouter(t, &Handler{Fleet: fm})

	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-hang",
		ManifestInline: encodeManifest(t),
		StartupScript:  "sleep infinity",
	})

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body: %s", rr.Code, rr.Body.String())
	}
	if got := decodeError(t, rr.Body); got.Error.Code != CodeInvalidArgument {
		t.Fatalf("code = %q, want %q", got.Error.Code, CodeInvalidArgument)
	}
}
