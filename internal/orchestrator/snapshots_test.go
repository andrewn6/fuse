package orchestrator

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

type snapshotTestEnv struct {
	name string
	url  string

	mu          sync.Mutex
	files       map[string][]byte
	checkpoints []Checkpoint

	// deleteCheckpointErr, when set, makes DeleteCheckpoint fail with it.
	// Lets a test drive the non-404 provider-failure branch.
	deleteCheckpointErr error
}

func (e *snapshotTestEnv) Name() string  { return e.name }
func (e *snapshotTestEnv) URL() string   { return e.url }
func (e *snapshotTestEnv) Token() string { return "" }

func (e *snapshotTestEnv) Exec(context.Context, []string, ExecOptions) (ExecResult, error) {
	return ExecResult{}, nil
}
func (e *snapshotTestEnv) ExecStream(context.Context, io.Writer, io.Writer, string, ...string) error {
	return nil
}
func (e *snapshotTestEnv) Upload(_ context.Context, data []byte, path string) error {
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
func (e *snapshotTestEnv) StartAgent(context.Context, AgentSpec) error { return nil }
func (e *snapshotTestEnv) Checkpoint(_ context.Context, comment string) (string, error) {
	var sizeBytes int64
	for _, file := range e.files {
		sizeBytes += int64(len(file))
	}
	id := fmt.Sprintf("cp-%d", len(e.checkpoints)+1)
	e.checkpoints = append(e.checkpoints, Checkpoint{
		ID:        id,
		Comment:   comment,
		SizeBytes: sizeBytes,
		CreatedAt: time.Now(),
	})
	return id, nil
}
func (e *snapshotTestEnv) Restore(_ context.Context, checkpointID string) error {
	for _, checkpoint := range e.checkpoints {
		if checkpoint.ID == checkpointID {
			return nil
		}
	}
	return fmt.Errorf("checkpoint %s not found", checkpointID)
}
func (e *snapshotTestEnv) DeleteCheckpoint(_ context.Context, checkpointID string) error {
	e.mu.Lock()
	stubErr := e.deleteCheckpointErr
	e.mu.Unlock()
	if stubErr != nil {
		return stubErr
	}
	for i, checkpoint := range e.checkpoints {
		if checkpoint.ID != checkpointID {
			continue
		}
		e.checkpoints = append(e.checkpoints[:i], e.checkpoints[i+1:]...)
		return nil
	}
	return fmt.Errorf("checkpoint %s not found", checkpointID)
}
func (e *snapshotTestEnv) ListCheckpoints(context.Context) ([]Checkpoint, error) {
	out := make([]Checkpoint, len(e.checkpoints))
	copy(out, e.checkpoints)
	return out, nil
}

type snapshotTestProvider struct {
	envs map[string]*snapshotTestEnv
}

func newSnapshotTestProvider() *snapshotTestProvider {
	return &snapshotTestProvider{envs: make(map[string]*snapshotTestEnv)}
}

func (p *snapshotTestProvider) Create(_ context.Context, spec Spec) (Environment, error) {
	env := &snapshotTestEnv{name: spec.Name, url: "http://" + spec.Name + ".test"}
	p.envs[spec.Name] = env
	return env, nil
}
func (p *snapshotTestProvider) Get(_ context.Context, name string) (Environment, error) {
	env, ok := p.envs[name]
	if !ok {
		// Mirror the real providers, which surface an agent 404 as
		// *HTTPStatusError; cleanup paths key off that to tell a vanished
		// VM apart from a provider failure.
		return nil, &HTTPStatusError{Code: http.StatusNotFound, Body: "vm not found"}
	}
	return env, nil
}
func (p *snapshotTestProvider) Destroy(_ context.Context, name string) error {
	delete(p.envs, name)
	return nil
}
func (p *snapshotTestProvider) List(_ context.Context, _ string) ([]Environment, error) {
	out := make([]Environment, 0, len(p.envs))
	for _, env := range p.envs {
		out = append(out, env)
	}
	return out, nil
}
func (*snapshotTestProvider) Close() error { return nil }

func provisionSnapshotTestVM(t *testing.T, fm *FleetManager, taskID string) string {
	t.Helper()
	manifest := base64.StdEncoding.EncodeToString([]byte(`{"version":"1","services":{}}`))
	_, err := fm.ProvisionAndAssign(context.Background(), taskID, Spec{}, []byte(mustDecodeBase64(t, manifest)), nil, BootOptions{})
	if err != nil {
		t.Fatalf("provision vm: %v", err)
	}
	return "fuse-" + taskID
}

func mustDecodeBase64(t *testing.T, encoded string) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	return raw
}

func TestCreateSnapshot_persistsLineageAndMetadata(t *testing.T) {
	provider := newSnapshotTestProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: provider,
		Prefix:   "fuse-",
	})
	vmID := provisionSnapshotTestVM(t, fm, "task-1")

	first, err := fm.CreateSnapshot(context.Background(), vmID, SnapshotOptions{Comment: "first"})
	if err != nil {
		t.Fatalf("create first snapshot: %v", err)
	}
	second, err := fm.CreateSnapshot(context.Background(), vmID, SnapshotOptions{Comment: "second"})
	if err != nil {
		t.Fatalf("create second snapshot: %v", err)
	}

	if first.State != SnapshotStateReady {
		t.Fatalf("first state = %s, want ready", first.State)
	}
	if first.TaskID != "task-1" {
		t.Fatalf("first task_id = %q, want task-1", first.TaskID)
	}
	if first.TenantID != "task-1" {
		t.Fatalf("first tenant_id = %q, want task-1", first.TenantID)
	}
	if first.SizeBytes <= 0 {
		t.Fatalf("first size_bytes = %d, want > 0", first.SizeBytes)
	}
	if second.ParentSnapshotID != first.SnapshotID {
		t.Fatalf("second parent = %q, want %q", second.ParentSnapshotID, first.SnapshotID)
	}
}

func TestDeleteSnapshot_rejectsParentWithChildren(t *testing.T) {
	provider := newSnapshotTestProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: provider,
		Prefix:   "fuse-",
	})
	vmID := provisionSnapshotTestVM(t, fm, "task-1")

	first, _ := fm.CreateSnapshot(context.Background(), vmID, SnapshotOptions{})
	_, _ = fm.CreateSnapshot(context.Background(), vmID, SnapshotOptions{})

	err := fm.DeleteSnapshot(context.Background(), vmID, first.SnapshotID)
	if err == nil {
		t.Fatal("expected delete to fail for parent snapshot")
	}
	if !errors.Is(err, ErrSnapshotHasChildren) {
		t.Fatalf("err = %v, want ErrSnapshotHasChildren", err)
	}
}

// A snapshot whose VM has been destroyed must still be deletable. The host
// reclaimed the checkpoint along with the VM, so only the metadata row is
// left. Before this was handled, the delete failed on the provider's 404 and
// the record was stranded in "error" forever, which in turn pinned every
// ancestor snapshot (a child blocks its parent) and left an undeletable chain.
func TestDeleteSnapshot_dropsRecordWhenVMIsGone(t *testing.T) {
	provider := newSnapshotTestProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: provider,
		Prefix:   "fuse-",
	})
	vmID := provisionSnapshotTestVM(t, fm, "task-1")

	snap, err := fm.CreateSnapshot(context.Background(), vmID, SnapshotOptions{})
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}

	// Destroy the VM out from under the snapshot, the way `environment
	// destroy` on a fork does to its auto fork-seed snapshot.
	if err := provider.Destroy(context.Background(), vmID); err != nil {
		t.Fatalf("destroy vm: %v", err)
	}
	fm.mu.Lock()
	delete(fm.vms, vmID)
	fm.mu.Unlock()

	// DeleteSnapshotByID is the path the API and CLI use (DELETE
	// /v1/snapshots/{id}); it resolves the record globally rather than via
	// the VM, so it reaches the artifact delete even once the VM is gone.
	if err := fm.DeleteSnapshotByID(context.Background(), snap.SnapshotID); err != nil {
		t.Fatalf("delete snapshot with missing vm: %v", err)
	}

	remaining, err := fm.loadSnapshots(context.Background())
	if err != nil {
		t.Fatalf("load snapshots: %v", err)
	}
	for _, s := range remaining {
		if s.SnapshotID == snap.SnapshotID {
			t.Fatalf("snapshot %s still present after delete (state=%s)", s.SnapshotID, s.State)
		}
	}
}

// A non-404 provider failure must still strand the record in "error" rather
// than silently dropping metadata for an artifact that may still exist.
func TestDeleteSnapshot_keepsRecordOnNonNotFoundError(t *testing.T) {
	provider := newSnapshotTestProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: provider,
		Prefix:   "fuse-",
	})
	vmID := provisionSnapshotTestVM(t, fm, "task-1")

	snap, err := fm.CreateSnapshot(context.Background(), vmID, SnapshotOptions{})
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}

	env := provider.envs[vmID]
	env.mu.Lock()
	env.deleteCheckpointErr = &HTTPStatusError{Code: http.StatusInternalServerError, Body: "boom"}
	env.mu.Unlock()

	if err := fm.DeleteSnapshot(context.Background(), vmID, snap.SnapshotID); err == nil {
		t.Fatal("expected delete to fail on a 500 from the provider")
	}

	remaining, err := fm.loadSnapshots(context.Background())
	if err != nil {
		t.Fatalf("load snapshots: %v", err)
	}
	var found bool
	for _, s := range remaining {
		if s.SnapshotID == snap.SnapshotID {
			found = true
			if s.State != SnapshotStateError {
				t.Fatalf("state = %s, want %s", s.State, SnapshotStateError)
			}
		}
	}
	if !found {
		t.Fatalf("snapshot %s was dropped despite a non-404 failure", snap.SnapshotID)
	}
}

func TestReconcileSnapshots_deletesExpiredLeaf(t *testing.T) {
	provider := newSnapshotTestProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: provider,
		Prefix:   "fuse-",
	})
	vmID := provisionSnapshotTestVM(t, fm, "task-1")

	first, _ := fm.CreateSnapshot(context.Background(), vmID, SnapshotOptions{})
	second, _ := fm.CreateSnapshot(context.Background(), vmID, SnapshotOptions{})
	third, _ := fm.CreateSnapshot(context.Background(), vmID, SnapshotOptions{})

	second.ParentSnapshotID = first.SnapshotID
	expired := time.Now().Add(-time.Hour)
	second.RetentionUntil = &expired
	if err := fm.upsertSnapshotRecord(context.Background(), second); err != nil {
		t.Fatalf("update second snapshot: %v", err)
	}

	third.ParentSnapshotID = first.SnapshotID
	if err := fm.upsertSnapshotRecord(context.Background(), third); err != nil {
		t.Fatalf("update third snapshot: %v", err)
	}

	fm.reconcileSnapshots(context.Background())

	if _, err := fm.GetSnapshot(context.Background(), vmID, second.SnapshotID); err == nil {
		t.Fatal("expected expired leaf snapshot to be deleted")
	}
	if _, err := fm.GetSnapshot(context.Background(), vmID, third.SnapshotID); err != nil {
		t.Fatalf("latest branch snapshot should remain: %v", err)
	}
}

// TestReconcileSnapshots_keepsExpiredBuildArtifact is the counterpart to
// deletesExpiredLeaf: an identically expired leaf survives gc purely because
// its mode is build. Sweeping one would break every `fuse up --from-build`
// referencing it.
func TestReconcileSnapshots_keepsExpiredBuildArtifact(t *testing.T) {
	provider := newSnapshotTestProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: provider,
		Prefix:   "fuse-",
	})
	vmID := provisionSnapshotTestVM(t, fm, "task-1")

	// a later snapshot exists so the artifact is not the vm's latest, which is
	// otherwise exempt on its own and would mask the mode check.
	artifact, _ := fm.CreateSnapshot(context.Background(), vmID, SnapshotOptions{Mode: SnapshotModeBuild})
	if _, err := fm.CreateSnapshot(context.Background(), vmID, SnapshotOptions{}); err != nil {
		t.Fatalf("create trailing snapshot: %v", err)
	}

	expired := time.Now().Add(-time.Hour)
	artifact.RetentionUntil = &expired
	if err := fm.upsertSnapshotRecord(context.Background(), artifact); err != nil {
		t.Fatalf("update artifact: %v", err)
	}

	fm.reconcileSnapshots(context.Background())

	if _, err := fm.GetSnapshot(context.Background(), vmID, artifact.SnapshotID); err != nil {
		t.Fatalf("expired build artifact should survive gc: %v", err)
	}
}

// TestEnforceSnapshotQuota_ignoresBuildArtifacts pins the other half of the
// exemption: build artifacts do not consume the per-tenant checkpoint quota.
func TestEnforceSnapshotQuota_ignoresBuildArtifacts(t *testing.T) {
	fm := NewFleetManager(FleetConfig{
		Provider:              newSnapshotTestProvider(),
		Prefix:                "fuse-",
		SnapshotQuotaMaxCount: 1,
	})

	ctx := context.Background()
	for _, rec := range []SnapshotRecord{
		{SnapshotID: "s-build", TenantID: "t1", State: SnapshotStateReady, Mode: SnapshotModeBuild},
		{SnapshotID: "s-build-2", TenantID: "t1", State: SnapshotStateReady, Mode: SnapshotModeBuild},
	} {
		if err := fm.upsertSnapshotRecord(ctx, rec); err != nil {
			t.Fatalf("seed snapshot %s: %v", rec.SnapshotID, err)
		}
	}
	if err := fm.enforceSnapshotQuota(ctx, "t1"); err != nil {
		t.Fatalf("build artifacts should not count against the quota: %v", err)
	}

	// a single ordinary checkpoint still trips the same quota of 1.
	manual := SnapshotRecord{SnapshotID: "s-manual", TenantID: "t1", State: SnapshotStateReady, Mode: SnapshotModeManual}
	if err := fm.upsertSnapshotRecord(ctx, manual); err != nil {
		t.Fatalf("seed manual snapshot: %v", err)
	}
	if err := fm.enforceSnapshotQuota(ctx, "t1"); !errors.Is(err, ErrSnapshotQuotaExceeded) {
		t.Fatalf("manual snapshot should still be charged: got %v", err)
	}
}

// TestListSnapshotsFiltered_modeAndName covers the lookup key a build artifact
// is found by when its random id is not known.
func TestListSnapshotsFiltered_modeAndName(t *testing.T) {
	provider := newSnapshotTestProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: provider,
		Prefix:   "fuse-",
	})
	vmID := provisionSnapshotTestVM(t, fm, "task-1")

	artifact, err := fm.CreateSnapshot(context.Background(), vmID, SnapshotOptions{
		Mode:     SnapshotModeBuild,
		Metadata: map[string]string{"name": "api"},
	})
	if err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	if _, err := fm.CreateSnapshot(context.Background(), vmID, SnapshotOptions{
		Mode:     SnapshotModeBuild,
		Metadata: map[string]string{"name": "worker"},
	}); err != nil {
		t.Fatalf("create second artifact: %v", err)
	}
	if _, err := fm.CreateSnapshot(context.Background(), vmID, SnapshotOptions{}); err != nil {
		t.Fatalf("create manual snapshot: %v", err)
	}

	got, _, err := fm.ListSnapshotsFiltered(context.Background(), SnapshotFilter{
		Mode: SnapshotModeBuild,
		Name: "api",
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].SnapshotID != artifact.SnapshotID {
		t.Fatalf("mode+name filter = %+v, want just %s", got, artifact.SnapshotID)
	}

	// mode alone excludes the manual snapshot but keeps both artifacts.
	got, _, err = fm.ListSnapshotsFiltered(context.Background(), SnapshotFilter{Mode: SnapshotModeBuild})
	if err != nil {
		t.Fatalf("list by mode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("mode filter returned %d snapshots, want 2", len(got))
	}

	// an unknown name matches nothing rather than erroring.
	got, _, err = fm.ListSnapshotsFiltered(context.Background(), SnapshotFilter{Name: "nope"})
	if err != nil {
		t.Fatalf("list by unknown name: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("unknown name returned %d snapshots, want 0", len(got))
	}
}

func TestRestoreSnapshot_marksMissingProviderSnapshotError(t *testing.T) {
	provider := newSnapshotTestProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: provider,
		Prefix:   "fuse-",
	})
	vmID := provisionSnapshotTestVM(t, fm, "task-1")

	snapshot, _ := fm.CreateSnapshot(context.Background(), vmID, SnapshotOptions{})
	env, err := provider.Get(context.Background(), vmID)
	if err != nil {
		t.Fatalf("get env: %v", err)
	}
	if deleter, ok := env.(SnapshotDeleter); ok {
		if err := deleter.DeleteCheckpoint(context.Background(), snapshot.SnapshotID); err != nil {
			t.Fatalf("delete checkpoint: %v", err)
		}
	} else {
		t.Fatal("test env does not implement SnapshotDeleter")
	}

	err = fm.RestoreSnapshot(context.Background(), vmID, snapshot.SnapshotID)
	if err == nil {
		t.Fatal("expected restore to fail for missing provider snapshot")
	}
	if !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("err = %v, want ErrSnapshotNotFound", err)
	}

	record, err := fm.GetSnapshot(context.Background(), vmID, snapshot.SnapshotID)
	if err != nil {
		t.Fatalf("reload snapshot: %v", err)
	}
	if record.State != SnapshotStateError {
		t.Fatalf("state = %s, want error", record.State)
	}
}

// liveTestMemBytes stands in for the guest memory image a real live snapshot
// writes. It is what makes a live artifact a different order of size from a
// disk one, which is the property the quota path has to see.
const liveTestMemBytes int64 = 64 << 20

// liveSnapshotTestEnv is snapshotTestEnv plus the live capability. It is a
// separate type rather than a flag on snapshotTestEnv because the whole
// mechanism under test is a type assertion: an env that implements the method
// and refuses at runtime would not exercise the branch that matters.
type liveSnapshotTestEnv struct {
	*snapshotTestEnv
}

func (e *liveSnapshotTestEnv) CheckpointLive(ctx context.Context, comment string) (Checkpoint, error) {
	id, err := e.Checkpoint(ctx, comment)
	if err != nil {
		return Checkpoint{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range e.checkpoints {
		if e.checkpoints[i].ID != id {
			continue
		}
		e.checkpoints[i].Kind = SnapshotKindLive
		e.checkpoints[i].SizeBytes += liveTestMemBytes
		return e.checkpoints[i], nil
	}
	return Checkpoint{}, fmt.Errorf("checkpoint %s vanished", id)
}

type liveSnapshotTestProvider struct {
	*snapshotTestProvider
	envs map[string]*liveSnapshotTestEnv
}

func newLiveSnapshotTestProvider() *liveSnapshotTestProvider {
	return &liveSnapshotTestProvider{
		snapshotTestProvider: newSnapshotTestProvider(),
		envs:                 make(map[string]*liveSnapshotTestEnv),
	}
}

func (p *liveSnapshotTestProvider) Create(ctx context.Context, spec Spec) (Environment, error) {
	env, err := p.snapshotTestProvider.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	wrapped := &liveSnapshotTestEnv{snapshotTestEnv: env.(*snapshotTestEnv)}
	p.envs[spec.Name] = wrapped
	return wrapped, nil
}

func (p *liveSnapshotTestProvider) Get(ctx context.Context, name string) (Environment, error) {
	if env, ok := p.envs[name]; ok {
		return env, nil
	}
	return p.snapshotTestProvider.Get(ctx, name)
}

// TestCreateSnapshot_liveRecordsKindAndSize asserts a live snapshot is filed
// as live and accounted at its real (memory-inclusive) size. The size is not
// incidental: quota is enforced on it deliberately, so under-reporting it
// would let a tenant fill a disk it was accounted as not filling.
func TestCreateSnapshot_liveRecordsKindAndSize(t *testing.T) {
	provider := newLiveSnapshotTestProvider()
	fm := NewFleetManager(FleetConfig{Provider: provider, Prefix: "fuse-"})
	vmID := provisionSnapshotTestVM(t, fm, "task-1")

	rec, err := fm.CreateSnapshot(context.Background(), vmID, SnapshotOptions{Comment: "live", Live: true})
	if err != nil {
		t.Fatalf("create live snapshot: %v", err)
	}
	if rec.Kind != SnapshotKindLive {
		t.Fatalf("kind = %q, want live", rec.Kind)
	}
	if rec.SizeBytes < liveTestMemBytes {
		t.Fatalf("size_bytes = %d, want the memory image counted", rec.SizeBytes)
	}

	// the same provider, asked for an ordinary snapshot, must still file disk:
	// the kind follows the request through to what was actually written, it is
	// not a property of the backend.
	disk, err := fm.CreateSnapshot(context.Background(), vmID, SnapshotOptions{Comment: "disk"})
	if err != nil {
		t.Fatalf("create disk snapshot: %v", err)
	}
	if disk.Kind != SnapshotKindDisk {
		t.Fatalf("kind = %q, want disk", disk.Kind)
	}
}

// TestCreateSnapshot_liveUnsupportedProvider asserts a provider that can
// snapshot but cannot capture memory refuses the live request explicitly,
// rather than silently handing back a disk snapshot the caller would treat as
// resumable.
func TestCreateSnapshot_liveUnsupportedProvider(t *testing.T) {
	provider := newSnapshotTestProvider()
	fm := NewFleetManager(FleetConfig{Provider: provider, Prefix: "fuse-"})
	vmID := provisionSnapshotTestVM(t, fm, "task-1")

	_, err := fm.CreateSnapshot(context.Background(), vmID, SnapshotOptions{Live: true})
	if !errors.Is(err, ErrSnapshotUnsupported) {
		t.Fatalf("err = %v, want ErrSnapshotUnsupported", err)
	}
	// the ordinary path on the same provider still works, which is what makes
	// the error a per-request refusal rather than a broken provider.
	if _, err := fm.CreateSnapshot(context.Background(), vmID, SnapshotOptions{}); err != nil {
		t.Fatalf("create disk snapshot: %v", err)
	}
}

// TestRestoreSnapshot_liveNeedsNoExtraCapability asserts restore of a live
// snapshot goes through the same Restore as a disk one. The host agent records
// the kind and branches on it internally, so there is deliberately no
// RestoreLive for a caller to get wrong.
func TestRestoreSnapshot_liveNeedsNoExtraCapability(t *testing.T) {
	provider := newLiveSnapshotTestProvider()
	fm := NewFleetManager(FleetConfig{Provider: provider, Prefix: "fuse-"})
	vmID := provisionSnapshotTestVM(t, fm, "task-1")

	rec, err := fm.CreateSnapshot(context.Background(), vmID, SnapshotOptions{Live: true})
	if err != nil {
		t.Fatalf("create live snapshot: %v", err)
	}
	if err := fm.RestoreSnapshot(context.Background(), vmID, rec.SnapshotID); err != nil {
		t.Fatalf("restore live snapshot: %v", err)
	}
}
