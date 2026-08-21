package orchestrator

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"testing"
)

// gpuTestEnv is a minimal Environment that, like the real qemu env,
// deliberately does NOT implement SnapshotCapable. It exists so the snapshot
// and fork guardrails for gpu environments can be exercised without importing
// internal/qemu (which would create an import cycle).
type gpuTestEnv struct {
	name string
	url  string
}

func (e *gpuTestEnv) Name() string  { return e.name }
func (e *gpuTestEnv) URL() string   { return e.url }
func (e *gpuTestEnv) Token() string { return "" }
func (e *gpuTestEnv) Exec(context.Context, []string, ExecOptions) (ExecResult, error) {
	return ExecResult{}, nil
}
func (e *gpuTestEnv) ExecStream(context.Context, io.Writer, io.Writer, string, ...string) error {
	return nil
}
func (e *gpuTestEnv) Upload(context.Context, []byte, string) error { return nil }
func (e *gpuTestEnv) StartAgent(context.Context, AgentSpec) error  { return nil }

// gpuTestProvider hands out gpuTestEnv handles. It is NOT SnapshotForkable, so
// the fork guardrail's reuse-snapshot path is reached for gpu envs.
type gpuTestProvider struct {
	envs map[string]*gpuTestEnv
}

func newGPUTestProvider() *gpuTestProvider {
	return &gpuTestProvider{envs: make(map[string]*gpuTestEnv)}
}

func (p *gpuTestProvider) Create(_ context.Context, spec Spec) (Environment, error) {
	env := &gpuTestEnv{name: spec.Name, url: "http://" + spec.Name + ".test"}
	p.envs[spec.Name] = env
	return env, nil
}
func (p *gpuTestProvider) Get(_ context.Context, name string) (Environment, error) {
	env, ok := p.envs[name]
	if !ok {
		return nil, fmt.Errorf("env %s not found", name)
	}
	return env, nil
}
func (p *gpuTestProvider) Destroy(_ context.Context, name string) error {
	delete(p.envs, name)
	return nil
}
func (p *gpuTestProvider) List(_ context.Context, _ string) ([]Environment, error) {
	out := make([]Environment, 0, len(p.envs))
	for _, env := range p.envs {
		out = append(out, env)
	}
	return out, nil
}
func (*gpuTestProvider) Close() error { return nil }

// provisionGPUTestVM provisions a running vm with a gpu spec on the given fleet.
func provisionGPUTestVM(t *testing.T, fm *FleetManager, taskID string) string {
	t.Helper()
	manifest := base64.StdEncoding.EncodeToString([]byte(`{"version":"1","services":{}}`))
	_, err := fm.ProvisionAndAssign(context.Background(), taskID, Spec{GPUs: 1, GPUKind: "a100"},
		mustDecodeBase64(t, manifest), nil, BootOptions{})
	if err != nil {
		t.Fatalf("provision gpu vm: %v", err)
	}
	return "fuse-" + taskID
}

// TestCreateSnapshot_gpuEnvUnsupported asserts CreateSnapshot on a gpu env
// fails with a gpu-specific message (via the non-SnapshotCapable assertion),
// not the generic "provider does not support snapshots" wording.
func TestCreateSnapshot_gpuEnvUnsupported(t *testing.T) {
	provider := newGPUTestProvider()
	fm := NewFleetManager(FleetConfig{Provider: provider, Prefix: "fuse-"})
	if err := fm.RegisterHost(context.Background(), gpuFleetHost("gpu-host", 1, "a100"), provider); err != nil {
		t.Fatal(err)
	}
	vmID := provisionGPUTestVM(t, fm, "task-1")

	_, err := fm.CreateSnapshot(context.Background(), vmID, SnapshotOptions{})
	if err == nil {
		t.Fatal("expected snapshot of gpu env to fail")
	}
	if !strings.Contains(err.Error(), "gpu") {
		t.Fatalf("err = %v, want a gpu-specific unsupported message", err)
	}
}

// TestForkEnvironment_gpuEnvUnsupported asserts fork on a gpu env fails at the
// snapshot step (reuse_snapshot_id empty => CreateSnapshot is called first)
// with the same class of gpu-specific error.
func TestForkEnvironment_gpuEnvUnsupported(t *testing.T) {
	provider := newGPUTestProvider()
	fm := NewFleetManager(FleetConfig{Provider: provider, Prefix: "fuse-"})
	if err := fm.RegisterHost(context.Background(), gpuFleetHost("gpu-host", 1, "a100"), provider); err != nil {
		t.Fatal(err)
	}
	vmID := provisionGPUTestVM(t, fm, "task-1")

	_, err := fm.ForkEnvironment(context.Background(), vmID, ForkOptions{})
	if err == nil {
		t.Fatal("expected fork of gpu env to fail")
	}
	if !strings.Contains(err.Error(), "gpu") {
		t.Fatalf("err = %v, want a gpu-specific unsupported message", err)
	}
}

// TestCreateSnapshot_gpuEnvLiveStillGPUError asserts the gpu explanation wins
// over the live one. A gpu env implements neither snapshot interface, and
// "your gpu cannot be checkpointed" is the answer that tells the caller
// dropping --live will not help; the generic live-unsupported message would
// send them to retry something that cannot work.
func TestCreateSnapshot_gpuEnvLiveStillGPUError(t *testing.T) {
	provider := newGPUTestProvider()
	fm := NewFleetManager(FleetConfig{Provider: provider, Prefix: "fuse-"})
	if err := fm.RegisterHost(context.Background(), gpuFleetHost("gpu-host", 1, "a100"), provider); err != nil {
		t.Fatal(err)
	}
	vmID := provisionGPUTestVM(t, fm, "task-1")

	_, err := fm.CreateSnapshot(context.Background(), vmID, SnapshotOptions{Live: true})
	if err == nil {
		t.Fatal("expected live snapshot of gpu env to fail")
	}
	if !strings.Contains(err.Error(), "gpu") {
		t.Fatalf("err = %v, want a gpu-specific unsupported message", err)
	}
}
