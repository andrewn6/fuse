package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// forkTestProvider mirrors snapshotTestProvider (reusing snapshotTestEnv) but
// additionally implements SnapshotForkable, exercising the happy path a real
// firecracker provider cannot yet.
type forkTestProvider struct {
	envs map[string]*snapshotTestEnv
}

func newForkTestProvider() *forkTestProvider {
	return &forkTestProvider{envs: make(map[string]*snapshotTestEnv)}
}

func (p *forkTestProvider) Create(_ context.Context, spec Spec) (Environment, error) {
	env := &snapshotTestEnv{name: spec.Name, url: "http://" + spec.Name + ".test"}
	p.envs[spec.Name] = env
	return env, nil
}
func (p *forkTestProvider) Get(_ context.Context, name string) (Environment, error) {
	env, ok := p.envs[name]
	if !ok {
		return nil, fmt.Errorf("env %s not found", name)
	}
	return env, nil
}
func (p *forkTestProvider) Destroy(_ context.Context, name string) error {
	delete(p.envs, name)
	return nil
}
func (p *forkTestProvider) List(_ context.Context, _ string) ([]Environment, error) {
	out := make([]Environment, 0, len(p.envs))
	for _, env := range p.envs {
		out = append(out, env)
	}
	return out, nil
}
func (*forkTestProvider) Close() error { return nil }

// CreateFromCheckpoint copies the source env's matching checkpoint into a new
// env keyed by the new vm id (spec.Name) and returns it.
func (p *forkTestProvider) CreateFromCheckpoint(_ context.Context, spec Spec, srcVMID, checkpointID string) (Environment, error) {
	srcEnv, ok := p.envs[srcVMID]
	if !ok {
		return nil, fmt.Errorf("source env %s not found", srcVMID)
	}
	var (
		seed  Checkpoint
		found bool
	)
	for _, cp := range srcEnv.checkpoints {
		if cp.ID == checkpointID {
			seed = cp
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("checkpoint %s not found on %s", checkpointID, srcVMID)
	}
	env := &snapshotTestEnv{name: spec.Name, url: "http://" + spec.Name + ".test"}
	env.checkpoints = append(env.checkpoints, seed)
	p.envs[spec.Name] = env
	return env, nil
}

func TestForkEnvironment_snapshotsSourceAndRegistersRunning(t *testing.T) {
	provider := newForkTestProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: provider,
		Prefix:   "fuse-",
	})
	srcID := provisionSnapshotTestVM(t, fm, "task-1")

	newID, err := fm.ForkEnvironment(context.Background(), srcID, ForkOptions{Comment: "forked"})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if newID == srcID {
		t.Fatalf("new vm id %q must differ from source", newID)
	}

	info, ok := fm.GetVM(newID)
	if !ok {
		t.Fatalf("GetVM(%s) not found", newID)
	}
	if info.State != VMStateRunning {
		t.Fatalf("new vm state = %s, want running", info.State)
	}
	if _, err := provider.Get(context.Background(), newID); err != nil {
		t.Fatalf("provider missing forked env: %v", err)
	}

	// the seed snapshot is the sole snapshot recorded for the source.
	srcSnaps, err := fm.ListSnapshots(context.Background(), srcID)
	if err != nil {
		t.Fatalf("list source snapshots: %v", err)
	}
	if len(srcSnaps) != 1 {
		t.Fatalf("source snapshots = %d, want 1", len(srcSnaps))
	}
	seedID := srcSnaps[0].SnapshotID

	// the lineage record for the new vm references the seed snapshot.
	forkSnaps, err := fm.ListSnapshots(context.Background(), newID)
	if err != nil {
		t.Fatalf("list fork snapshots: %v", err)
	}
	if len(forkSnaps) != 1 {
		t.Fatalf("fork lineage records = %d, want 1", len(forkSnaps))
	}
	if forkSnaps[0].ParentSnapshotID != seedID {
		t.Fatalf("lineage parent = %q, want %q", forkSnaps[0].ParentSnapshotID, seedID)
	}
}

func TestForkEnvironment_reuseSnapshotDoesNotCreateExtra(t *testing.T) {
	provider := newForkTestProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: provider,
		Prefix:   "fuse-",
	})
	srcID := provisionSnapshotTestVM(t, fm, "task-1")

	seed, err := fm.CreateSnapshot(context.Background(), srcID, SnapshotOptions{Comment: "seed"})
	if err != nil {
		t.Fatalf("create seed snapshot: %v", err)
	}
	before, err := fm.ListSnapshots(context.Background(), srcID)
	if err != nil {
		t.Fatalf("list source snapshots: %v", err)
	}

	newID, err := fm.ForkEnvironment(context.Background(), srcID, ForkOptions{ReuseSnapshotID: seed.SnapshotID})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}

	after, err := fm.ListSnapshots(context.Background(), srcID)
	if err != nil {
		t.Fatalf("list source snapshots after: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("source snapshot count changed: before=%d after=%d, fork must not snapshot the source when reusing", len(before), len(after))
	}

	if info, ok := fm.GetVM(newID); !ok || info.State != VMStateRunning {
		t.Fatalf("forked vm not running: ok=%v info=%+v", ok, info)
	}
	forkSnaps, err := fm.ListSnapshots(context.Background(), newID)
	if err != nil {
		t.Fatalf("list fork snapshots: %v", err)
	}
	if len(forkSnaps) != 1 || forkSnaps[0].ParentSnapshotID != seed.SnapshotID {
		t.Fatalf("lineage record wrong: %+v", forkSnaps)
	}
}

func TestForkEnvironment_rejectsNonRunning(t *testing.T) {
	provider := newForkTestProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: provider,
		Prefix:   "fuse-",
	})
	srcID := provisionSnapshotTestVM(t, fm, "task-1")

	// flip the source vm out of the running state.
	fm.mu.Lock()
	fm.vms[srcID].state = VMStateDraining
	fm.mu.Unlock()

	if _, err := fm.ForkEnvironment(context.Background(), srcID, ForkOptions{}); err == nil {
		t.Fatal("expected fork of non-running vm to fail")
	}
}

func TestForkEnvironment_providerNotForkable(t *testing.T) {
	// snapshotTestProvider is SnapshotCapable at the env level but does not
	// implement SnapshotForkable, matching the real firecracker provider.
	provider := newSnapshotTestProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: provider,
		Prefix:   "fuse-",
	})
	srcID := provisionSnapshotTestVM(t, fm, "task-1")

	_, err := fm.ForkEnvironment(context.Background(), srcID, ForkOptions{})
	if err == nil {
		t.Fatal("expected fork to fail when provider does not implement SnapshotForkable")
	}
	if !strings.Contains(err.Error(), "does not support fork") {
		t.Fatalf("err = %v, want provider does not support fork", err)
	}
}

// liveForkTestProvider is forkTestProvider whose envs also carry the live
// capability, so a fork test can produce a genuine live snapshot record
// instead of hand-editing a kind into the store.
type liveForkTestProvider struct {
	*forkTestProvider
	live map[string]*liveSnapshotTestEnv
}

func newLiveForkTestProvider() *liveForkTestProvider {
	return &liveForkTestProvider{
		forkTestProvider: newForkTestProvider(),
		live:             make(map[string]*liveSnapshotTestEnv),
	}
}

func (p *liveForkTestProvider) Create(ctx context.Context, spec Spec) (Environment, error) {
	env, err := p.forkTestProvider.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	wrapped := &liveSnapshotTestEnv{snapshotTestEnv: env.(*snapshotTestEnv)}
	p.live[spec.Name] = wrapped
	return wrapped, nil
}

func (p *liveForkTestProvider) Get(ctx context.Context, name string) (Environment, error) {
	if env, ok := p.live[name]; ok {
		return env, nil
	}
	return p.forkTestProvider.Get(ctx, name)
}

// TestForkEnvironment_rejectsLiveSeed asserts fork refuses a live snapshot up
// front. A live snapshot's rootfs is copied without quiescing the guest
// filesystem, so booting it alone gets a filesystem missing everything still
// in page cache; against a real host that surfaced as a 500 from deep inside
// the agent ("agent start failed: activating") after a vm had already been
// provisioned and had to be rolled back.
func TestForkEnvironment_rejectsLiveSeed(t *testing.T) {
	provider := newLiveForkTestProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: provider,
		Prefix:   "fuse-",
	})
	srcID := provisionSnapshotTestVM(t, fm, "task-1")

	seed, err := fm.CreateSnapshot(context.Background(), srcID, SnapshotOptions{Comment: "live seed", Live: true})
	if err != nil {
		t.Fatalf("create live seed snapshot: %v", err)
	}
	if seed.Kind != SnapshotKindLive {
		t.Fatalf("seed kind = %q, want live; the test provider did not take a live snapshot", seed.Kind)
	}

	_, err = fm.ForkEnvironment(context.Background(), srcID, ForkOptions{ReuseSnapshotID: seed.SnapshotID})
	if !errors.Is(err, ErrSnapshotNotSeedable) {
		t.Fatalf("err = %v, want ErrSnapshotNotSeedable", err)
	}

	// the rejection must happen before anything is provisioned, or the caller
	// pays for a vm that gets torn down again.
	if got := len(fm.ListFleet()); got != 1 {
		t.Fatalf("vm count = %d, want 1; a rejected fork must not leave an environment behind", got)
	}
}

// TestForkEnvironment_diskSeedStillWorks pins the other half of the guard: the
// kind check must reject live only, not every reused snapshot.
func TestForkEnvironment_diskSeedStillWorks(t *testing.T) {
	provider := newLiveForkTestProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: provider,
		Prefix:   "fuse-",
	})
	srcID := provisionSnapshotTestVM(t, fm, "task-1")

	seed, err := fm.CreateSnapshot(context.Background(), srcID, SnapshotOptions{Comment: "disk seed"})
	if err != nil {
		t.Fatalf("create disk seed snapshot: %v", err)
	}
	if seed.Kind == SnapshotKindLive {
		t.Fatalf("seed kind = live, want disk")
	}

	if _, err := fm.ForkEnvironment(context.Background(), srcID, ForkOptions{ReuseSnapshotID: seed.SnapshotID}); err != nil {
		t.Fatalf("fork from a disk snapshot: %v", err)
	}
}
