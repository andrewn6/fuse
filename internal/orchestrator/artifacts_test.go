package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// artifactHost is a plain firecracker host with an agent url and token, which
// is what a peer transfer needs: the orchestrator mints the grant with the
// serving host's token and posts the pull to the receiving host's own.
func artifactHost(id string, cpus int) Host {
	return Host{
		ID:      id,
		URL:     "http://" + id + ".test",
		Token:   "token-" + id,
		Backend: BackendFirecracker,
		State:   HostActive,
		Capacity: HostCapacity{
			CPUs:      cpus,
			RamMB:     cpus * 1024,
			StorageGB: 100,
			VMCount:   10,
		},
	}
}

// recordingMover stands in for the composition root's hostwire-backed mover. It
// records what it was asked to do rather than moving anything, so a test can
// assert on the direction of the transfer, which is the part that matters:
// getting From and To the wrong way round would overwrite the source.
type recordingMover struct {
	mu    sync.Mutex
	moves []ArtifactMove
	err   error
}

func (m *recordingMover) MoveArtifact(_ context.Context, move ArtifactMove) (ArtifactMoved, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return ArtifactMoved{}, m.err
	}
	m.moves = append(m.moves, move)
	return ArtifactMoved{SnapshotID: move.SnapshotID, SizeBytes: 4096}, nil
}

func (m *recordingMover) calls() []ArtifactMove {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]ArtifactMove(nil), m.moves...)
}

// seedArtifactRecord writes a ready build-layer artifact into the store, the
// way CreateSnapshot would have.
func seedArtifactRecord(t *testing.T, fm *FleetManager, id, tenant, hostID, digest string) SnapshotRecord {
	t.Helper()
	now := time.Now()
	record := SnapshotRecord{
		SnapshotID: id,
		VMID:       "",
		HostID:     hostID,
		TenantID:   tenant,
		Mode:       SnapshotModeBuild,
		LayerKey:   "layer-1",
		Arch:       ArchAMD64,
		Digest:     digest,
		State:      SnapshotStateReady,
		SizeBytes:  4096,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := fm.upsertSnapshotRecord(context.Background(), record); err != nil {
		t.Fatalf("seed artifact %s: %v", id, err)
	}
	return record
}

// TestSchedulePreferring_artifactLocality pins the one property that makes
// locality safe: it decides between hosts that could all have taken the
// workload, and never admits or excludes one on its own.
func TestSchedulePreferring_artifactLocality(t *testing.T) {
	host := func(id string, cpus, allocatedCPUs int) *Host {
		h := artifactHost(id, cpus)
		h.Allocated.CPUs = allocatedCPUs
		h.Allocated.RamMB = allocatedCPUs * 1024
		return &h
	}

	cases := []struct {
		name      string
		hosts     []*Host
		holders   map[string]bool
		spec      Spec
		policy    PlacementPolicy
		wantHost  string
		wantLocal bool
	}{
		{
			// spread would take h-big (most headroom). The holder wins anyway,
			// because copying the artifact costs more than any packing choice.
			name:      "a holder beats the spread policy's pick",
			hosts:     []*Host{host("h-big", 16, 0), host("h-small", 8, 4)},
			holders:   map[string]bool{"h-small": true},
			spec:      Spec{CPUs: 2, RamMB: 1024},
			policy:    PlacementSpread,
			wantHost:  "h-small",
			wantLocal: true,
		},
		{
			// and the same in the other direction, so this is not an accident of
			// which way one policy happens to sort.
			name:      "a holder beats the binpack policy's pick",
			hosts:     []*Host{host("h-big", 16, 0), host("h-small", 8, 4)},
			holders:   map[string]bool{"h-big": true},
			spec:      Spec{CPUs: 2, RamMB: 1024},
			policy:    PlacementBinpack,
			wantHost:  "h-big",
			wantLocal: true,
		},
		{
			// the whole point. A host holding the artifact that cannot fit the
			// workload must lose to one that can, or a cache hit becomes a
			// scheduling failure.
			name:      "a holder that does not fit loses to a host that does",
			hosts:     []*Host{host("h-holder", 2, 1), host("h-roomy", 16, 0)},
			holders:   map[string]bool{"h-holder": true},
			spec:      Spec{CPUs: 4, RamMB: 4096},
			policy:    PlacementSpread,
			wantHost:  "h-roomy",
			wantLocal: false,
		},
		{
			name: "a cordoned holder loses to an active host",
			hosts: func() []*Host {
				held := host("h-holder", 16, 0)
				held.State = HostCordoned
				return []*Host{held, host("h-active", 8, 0)}
			}(),
			holders:   map[string]bool{"h-holder": true},
			spec:      Spec{CPUs: 2, RamMB: 1024},
			policy:    PlacementSpread,
			wantHost:  "h-active",
			wantLocal: false,
		},
		{
			name: "an arch-mismatched holder loses",
			hosts: func() []*Host {
				held := host("h-holder", 16, 0)
				held.Capacity.Arch = ArchARM64
				return []*Host{held, host("h-amd", 8, 0)}
			}(),
			holders:   map[string]bool{"h-holder": true},
			spec:      Spec{CPUs: 2, RamMB: 1024, Arch: ArchAMD64},
			policy:    PlacementSpread,
			wantHost:  "h-amd",
			wantLocal: false,
		},
		{
			name: "a label-mismatched holder loses",
			hosts: func() []*Host {
				held := host("h-holder", 16, 0)
				tagged := host("h-tagged", 8, 0)
				tagged.Labels = map[string]string{"pool": "build"}
				return []*Host{held, tagged}
			}(),
			holders:   map[string]bool{"h-holder": true},
			spec:      Spec{CPUs: 2, RamMB: 1024, Labels: map[string]string{"pool": "build"}},
			policy:    PlacementSpread,
			wantHost:  "h-tagged",
			wantLocal: false,
		},
		{
			// with no hint, placement is exactly what it was before locality
			// existed. this is the regression guard for every unseeded create.
			name:     "no holders leaves the policy in charge",
			hosts:    []*Host{host("h-big", 16, 0), host("h-small", 8, 4)},
			holders:  nil,
			spec:     Spec{CPUs: 2, RamMB: 1024},
			policy:   PlacementSpread,
			wantHost: "h-big",
		},
		{
			// when everything is local, locality says nothing and the policy
			// decides, rather than the first candidate winning by accident.
			name:      "the policy still decides among holders",
			hosts:     []*Host{host("h-big", 16, 0), host("h-small", 8, 4)},
			holders:   map[string]bool{"h-big": true, "h-small": true},
			spec:      Spec{CPUs: 2, RamMB: 1024},
			policy:    PlacementBinpack,
			wantHost:  "h-small",
			wantLocal: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, decision, err := SchedulePreferring(tc.spec, tc.hosts, tc.policy,
				PlacementHints{ArtifactHosts: tc.holders})
			if err != nil {
				t.Fatalf("SchedulePreferring: %v", err)
			}
			if got.ID != tc.wantHost {
				t.Fatalf("scheduled on %s, want %s", got.ID, tc.wantHost)
			}
			if decision.ArtifactLocal != tc.wantLocal {
				t.Errorf("decision.ArtifactLocal = %v, want %v", decision.ArtifactLocal, tc.wantLocal)
			}
		})
	}
}

// TestProvisionAndAssign_seedFallbackLadder walks every rung of the fallback,
// in order. The rungs are the contract: a cache hit must never be able to turn
// a working create into a failing one, so each step down has to be observable.
func TestProvisionAndAssign_seedFallbackLadder(t *testing.T) {
	const digest = "d1"

	// hostSpec describes a host to register: how many cpus it has, and whether
	// it already holds a copy of the artifact.
	type hostSpec struct {
		id    string
		cpus  int
		holds bool
	}

	cases := []struct {
		name      string
		hosts     []hostSpec
		pinned    string
		specCPUs  int
		wantHost  string
		wantMoves int
		wantErr   error
	}{
		{
			// rung one: nothing moves when the artifact is already where the
			// workload can run.
			name:      "the pinned host takes it when it can",
			hosts:     []hostSpec{{"h-a", 8, true}, {"h-b", 8, false}},
			pinned:    "h-a",
			specCPUs:  2,
			wantHost:  "h-a",
			wantMoves: 0,
		},
		{
			// rung two: another host already has a copy, so the workload goes
			// there rather than paying for a transfer.
			name:      "another holder takes it when the pinned host is full",
			hosts:     []hostSpec{{"h-a", 1, true}, {"h-b", 8, true}, {"h-c", 16, false}},
			pinned:    "h-a",
			specCPUs:  4,
			wantHost:  "h-b",
			wantMoves: 0,
		},
		{
			// rung three: no holder can take it, so the artifact follows the
			// workload to a host that can.
			name:      "the artifact is pulled when no holder can take it",
			hosts:     []hostSpec{{"h-a", 1, true}, {"h-c", 16, false}},
			pinned:    "h-a",
			specCPUs:  4,
			wantHost:  "h-c",
			wantMoves: 1,
		},
		{
			// rung four: only now is it a failure, and it says so as a capacity
			// outcome so the api answers 503 rather than 500.
			name:     "nothing fits anywhere and the error names the artifact",
			hosts:    []hostSpec{{"h-a", 1, true}, {"h-c", 2, false}},
			pinned:   "h-a",
			specCPUs: 8,
			wantErr:  ErrSeedUnplaceable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			stub := newStubProvider()
			mover := &recordingMover{}
			fm := NewFleetManager(FleetConfig{Provider: stub, Prefix: "fuse-", ArtifactMover: mover})

			for _, h := range tc.hosts {
				if err := fm.RegisterHost(ctx, artifactHost(h.id, h.cpus), stub); err != nil {
					t.Fatalf("register %s: %v", h.id, err)
				}
			}
			// The tenant of a snapshot is its task id (see snapshotTenantID), and
			// the artifact index is scoped to it, so the seeded records have to
			// carry the same tenant the create will be charged to.
			const taskID = "task-seed"
			origin := ""
			for _, h := range tc.hosts {
				if !h.holds {
					continue
				}
				id := "art-" + h.id
				seedArtifactRecord(t, fm, id, taskID, h.id, digest)
				if h.id == tc.pinned {
					origin = id
				}
			}

			spec := Spec{
				CPUs:           tc.specCPUs,
				RamMB:          1024,
				SeedSnapshotID: origin,
				PinnedHostID:   tc.pinned,
			}
			info, err := fm.ProvisionAndAssign(ctx, taskID, spec, nil, nil, BootOptions{})
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				// Wrapping ErrNoCapacity is what makes the api answer 503; a
				// bare new sentinel would fall through to a 500.
				if !errors.Is(err, ErrNoCapacity) {
					t.Errorf("err = %v, want it to read as a capacity outcome", err)
				}
				if !strings.Contains(err.Error(), origin) {
					t.Errorf("err = %v, want it to name the artifact %s", err, origin)
				}
				if _, tracked := fm.GetVM("fuse-" + taskID); tracked {
					t.Error("a failed seeded provision left the vm tracked")
				}
				return
			}
			if err != nil {
				t.Fatalf("provision: %v", err)
			}
			if info.HostID != tc.wantHost {
				t.Fatalf("vm landed on %q, want %q", info.HostID, tc.wantHost)
			}
			moves := mover.calls()
			if len(moves) != tc.wantMoves {
				t.Fatalf("%d artifact copies, want %d (%+v)", len(moves), tc.wantMoves, moves)
			}
			if tc.wantMoves == 0 {
				return
			}
			// Direction is the thing worth asserting: From and To reversed would
			// overwrite the only good copy of the artifact.
			move := moves[0]
			if move.To.HostID != tc.wantHost {
				t.Errorf("copied to %s, want %s", move.To.HostID, tc.wantHost)
			}
			if move.From.HostID == tc.wantHost {
				t.Errorf("copied from the destination host %s", move.From.HostID)
			}
			if move.From.Token != "token-"+move.From.HostID || move.To.Token != "token-"+tc.wantHost {
				t.Errorf("endpoints carry the wrong tokens: %+v", move)
			}
			if move.Digest != digest {
				t.Errorf("copied digest %q, want %q", move.Digest, digest)
			}
			// The vm must boot from the copy that landed here, not from the id
			// the caller resolved, which names a file on another host.
			if info.Spec.SeedSnapshotID != move.SnapshotID {
				t.Errorf("vm seeds from %q, want the local copy %q", info.Spec.SeedSnapshotID, move.SnapshotID)
			}
			if _, err := fm.GetSnapshotByID(ctx, move.SnapshotID); err != nil {
				t.Errorf("pulled copy %s was not recorded: %v", move.SnapshotID, err)
			}
		})
	}
}

// TestProvisionAndAssign_immovableArtifactKeepsThePin covers the degraded
// cases. An artifact with no digest cannot be verified after a transfer, so it
// must not be transferred; the workload stays pinned to it exactly as it did
// before any of this existed, rather than being scheduled somewhere the bytes
// can never arrive.
func TestProvisionAndAssign_immovableArtifactKeepsThePin(t *testing.T) {
	cases := []struct {
		name   string
		digest string
		mover  ArtifactMover
	}{
		{name: "an artifact with no digest", digest: "", mover: &recordingMover{}},
		{name: "an orchestrator with no mover", digest: "d1", mover: nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			stub := newStubProvider()
			fm := NewFleetManager(FleetConfig{Provider: stub, Prefix: "fuse-", ArtifactMover: tc.mover})
			// h-a holds the artifact and cannot fit the workload; h-b could.
			if err := fm.RegisterHost(ctx, artifactHost("h-a", 1), stub); err != nil {
				t.Fatal(err)
			}
			if err := fm.RegisterHost(ctx, artifactHost("h-b", 16), stub); err != nil {
				t.Fatal(err)
			}
			seedArtifactRecord(t, fm, "art-1", "task-seed", "h-a", tc.digest)

			_, err := fm.ProvisionAndAssign(ctx, "task-seed", Spec{
				CPUs: 4, RamMB: 1024, SeedSnapshotID: "art-1", PinnedHostID: "h-a",
			}, nil, nil, BootOptions{})
			if !errors.Is(err, ErrNoCapacity) {
				t.Fatalf("err = %v, want a capacity outcome", err)
			}
			// It must fail on the pinned host rather than quietly booting the
			// wrong rootfs on h-b.
			if _, tracked := fm.GetVM("fuse-task-seed"); tracked {
				t.Error("failed provision left the vm tracked")
			}
		})
	}
}

// TestProvisionAndAssign_failedPullLeavesNoHalfState asserts the cleanup
// contract. A transfer that does not complete must leave no vm, no reservation,
// and no record of a copy that never landed.
func TestProvisionAndAssign_failedPullLeavesNoHalfState(t *testing.T) {
	ctx := context.Background()
	stub := newStubProvider()
	mover := &recordingMover{err: errors.New("peer refused")}
	fm := NewFleetManager(FleetConfig{Provider: stub, Prefix: "fuse-", ArtifactMover: mover})
	if err := fm.RegisterHost(ctx, artifactHost("h-a", 1), stub); err != nil {
		t.Fatal(err)
	}
	if err := fm.RegisterHost(ctx, artifactHost("h-b", 16), stub); err != nil {
		t.Fatal(err)
	}
	seedArtifactRecord(t, fm, "art-1", "task-seed", "h-a", "d1")

	_, err := fm.ProvisionAndAssign(ctx, "task-seed", Spec{
		CPUs: 4, RamMB: 1024, SeedSnapshotID: "art-1", PinnedHostID: "h-a",
	}, nil, nil, BootOptions{})
	if !errors.Is(err, ErrArtifactTransferFailed) {
		t.Fatalf("err = %v, want ErrArtifactTransferFailed", err)
	}

	if _, tracked := fm.GetVM("fuse-task-seed"); tracked {
		t.Error("failed pull left the vm tracked")
	}
	// The reservation has to be given back, or the host stays permanently
	// short of the capacity a vm that never existed reserved.
	h, ok := fm.GetHost("h-b")
	if !ok {
		t.Fatal("h-b vanished")
	}
	if h.Allocated.CPUs != 0 || h.Allocated.VMCount != 0 {
		t.Errorf("h-b still holds a reservation: %+v", h.Allocated)
	}
	// And no record may claim h-b holds the artifact.
	holders, err := fm.HostsHoldingArtifact(ctx, "task-seed", "d1")
	if err != nil {
		t.Fatal(err)
	}
	if _, claimed := holders["h-b"]; claimed {
		t.Error("a failed pull recorded h-b as holding the artifact")
	}
	if fm.artifactPulls[replicaSnapshotID("d1", "h-b")] != 0 {
		t.Error("a failed pull left an in-flight marker behind")
	}
}

// TestEnsureArtifactOnHost_isIdempotent covers the second create against a host
// that already pulled the artifact: it must reuse the copy rather than pay for
// the transfer again.
func TestEnsureArtifactOnHost_isIdempotent(t *testing.T) {
	ctx := context.Background()
	stub := newStubProvider()
	mover := &recordingMover{}
	fm := NewFleetManager(FleetConfig{Provider: stub, Prefix: "fuse-", ArtifactMover: mover})
	for _, id := range []string{"h-a", "h-b"} {
		if err := fm.RegisterHost(ctx, artifactHost(id, 8), stub); err != nil {
			t.Fatal(err)
		}
	}
	record := seedArtifactRecord(t, fm, "art-1", "tenant-1", "h-a", "d1")

	first, err := fm.ensureArtifactOnHost(ctx, record, "h-b")
	if err != nil {
		t.Fatalf("first copy: %v", err)
	}
	second, err := fm.ensureArtifactOnHost(ctx, record, "h-b")
	if err != nil {
		t.Fatalf("second copy: %v", err)
	}
	if first != second {
		t.Errorf("second call returned %q, want the existing copy %q", second, first)
	}
	if n := len(mover.calls()); n != 1 {
		t.Errorf("%d transfers, want 1: the second call must reuse the copy", n)
	}
	// The artifact's own host needs no transfer at all.
	if got, err := fm.ensureArtifactOnHost(ctx, record, "h-a"); err != nil || got != record.SnapshotID {
		t.Errorf("ensureArtifactOnHost on the origin host = (%q, %v), want (%q, nil)", got, err, record.SnapshotID)
	}
	if n := len(mover.calls()); n != 1 {
		t.Errorf("%d transfers, want 1: the origin host must not copy to itself", n)
	}
}

// digestForkEnv is a forkable environment whose checkpoints report a digest,
// which is what makes its snapshots eligible to be copied to another host.
type digestForkEnv struct {
	snapshotTestEnv
}

func (e *digestForkEnv) CheckpointWithDigest(ctx context.Context, comment string) (Checkpoint, error) {
	id, err := e.Checkpoint(ctx, comment)
	if err != nil {
		return Checkpoint{}, err
	}
	return Checkpoint{ID: id, Comment: comment, SizeBytes: 4096, Digest: "fork-digest"}, nil
}

type digestForkProvider struct {
	mu   sync.Mutex
	envs map[string]*digestForkEnv
}

func newDigestForkProvider() *digestForkProvider {
	return &digestForkProvider{envs: make(map[string]*digestForkEnv)}
}

func (p *digestForkProvider) Create(_ context.Context, spec Spec) (Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	env := &digestForkEnv{snapshotTestEnv{name: spec.Name, url: "http://" + spec.Name + ".test"}}
	p.envs[spec.Name] = env
	return env, nil
}

func (p *digestForkProvider) Get(_ context.Context, name string) (Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	env, ok := p.envs[name]
	if !ok {
		return nil, fmt.Errorf("env %s not found", name)
	}
	return env, nil
}

func (p *digestForkProvider) Destroy(_ context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.envs, name)
	return nil
}

func (p *digestForkProvider) List(_ context.Context, _ string) ([]Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Environment, 0, len(p.envs))
	for _, env := range p.envs {
		out = append(out, env)
	}
	return out, nil
}

func (*digestForkProvider) Close() error { return nil }

func (p *digestForkProvider) CreateFromCheckpoint(ctx context.Context, spec Spec, _, _ string) (Environment, error) {
	return p.Create(ctx, spec)
}

// TestForkEnvironment_fallsBackOffTheSourceHost covers the other place a
// host-local artifact used to be a hard pin. A fork has always been charged to
// the source's host without asking whether that host could still take it; now
// the seed follows the fork when it cannot.
func TestForkEnvironment_fallsBackOffTheSourceHost(t *testing.T) {
	ctx := context.Background()
	provider := newDigestForkProvider()
	mover := &recordingMover{}
	fm := NewFleetManager(FleetConfig{Provider: provider, Prefix: "fuse-", ArtifactMover: mover})

	// h-a has exactly enough room for the source and nothing more, so the fork
	// cannot be born there.
	if err := fm.RegisterHost(ctx, artifactHost("h-a", 1), provider); err != nil {
		t.Fatal(err)
	}
	if err := fm.RegisterHost(ctx, artifactHost("h-b", 16), provider); err != nil {
		t.Fatal(err)
	}
	if _, err := fm.ProvisionAndAssign(ctx, "task-src",
		Spec{CPUs: 1, RamMB: 512, HostID: "h-a"}, []byte(`{}`), nil, BootOptions{}); err != nil {
		t.Fatalf("provision source: %v", err)
	}

	forkID, err := fm.ForkEnvironment(ctx, "fuse-task-src", ForkOptions{})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}

	info, ok := fm.GetVM(forkID)
	if !ok {
		t.Fatalf("fork %s is not tracked", forkID)
	}
	if info.HostID != "h-b" {
		t.Fatalf("fork landed on %q, want h-b", info.HostID)
	}
	moves := mover.calls()
	if len(moves) != 1 {
		t.Fatalf("%d artifact copies, want 1", len(moves))
	}
	if moves[0].From.HostID != "h-a" || moves[0].To.HostID != "h-b" {
		t.Errorf("copied %s -> %s, want h-a -> h-b", moves[0].From.HostID, moves[0].To.HostID)
	}
	if info.Spec.SeedSnapshotID != moves[0].SnapshotID {
		t.Errorf("fork seeds from %q, want the local copy %q", info.Spec.SeedSnapshotID, moves[0].SnapshotID)
	}
	// The fork is a real vm on h-b, so h-b has to be charged for it: DestroyVM
	// deallocates unconditionally, and an uncharged fork would credit capacity
	// back to a host that never reserved it.
	h, _ := fm.GetHost("h-b")
	if h.Allocated.VMCount != 1 {
		t.Errorf("h-b allocated = %+v, want the fork charged there", h.Allocated)
	}
}

// TestForkEnvironment_staysHomeWhenTheArtifactCannotMove is the guard against
// the fallback becoming a new failure mode. Forking has never consulted
// capacity, so an artifact that cannot be copied must leave the old behaviour
// exactly as it was rather than failing a fork that works today.
func TestForkEnvironment_staysHomeWhenTheArtifactCannotMove(t *testing.T) {
	ctx := context.Background()
	provider := newForkTestProvider() // its checkpoints report no digest
	fm := NewFleetManager(FleetConfig{Provider: provider, Prefix: "fuse-", ArtifactMover: &recordingMover{}})
	if err := fm.RegisterHost(ctx, artifactHost("h-a", 1), provider); err != nil {
		t.Fatal(err)
	}
	if err := fm.RegisterHost(ctx, artifactHost("h-b", 16), provider); err != nil {
		t.Fatal(err)
	}
	if _, err := fm.ProvisionAndAssign(ctx, "task-src",
		Spec{CPUs: 1, RamMB: 512, HostID: "h-a"}, []byte(`{}`), nil, BootOptions{}); err != nil {
		t.Fatalf("provision source: %v", err)
	}

	forkID, err := fm.ForkEnvironment(ctx, "fuse-task-src", ForkOptions{})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	info, ok := fm.GetVM(forkID)
	if !ok {
		t.Fatalf("fork %s is not tracked", forkID)
	}
	if info.HostID != "h-a" {
		t.Fatalf("fork landed on %q, want the source's host h-a", info.HostID)
	}
}

// TestReconcileArtifacts_safety is the eviction safety suite. Every case here
// describes an artifact that must survive a sweep, plus the one that must not,
// so a future change that widens the collector has to break one of these.
func TestReconcileArtifacts_safety(t *testing.T) {
	const tenant = "tenant-1"
	stale := time.Now().Add(-24 * time.Hour)

	cases := []struct {
		name string
		// arrange mutates the fleet after the artifact has been seeded.
		arrange   func(t *testing.T, fm *FleetManager, record SnapshotRecord)
		wantAlive bool
	}{
		{
			name:      "an artifact nothing references and nothing has used is collected",
			arrange:   func(*testing.T, *FleetManager, SnapshotRecord) {},
			wantAlive: false,
		},
		{
			// destroying the rootfs a running guest booted from is the one
			// failure nothing recovers from.
			name: "an artifact a vm is seeding from survives",
			arrange: func(_ *testing.T, fm *FleetManager, record SnapshotRecord) {
				fm.mu.Lock()
				fm.vms["vm-1"] = &vm{
					id:    "vm-1",
					state: VMStateRunning,
					spec:  Spec{SeedSnapshotID: record.SnapshotID},
				}
				fm.mu.Unlock()
			},
			wantAlive: true,
		},
		{
			name: "an artifact at either end of an in-flight copy survives",
			arrange: func(_ *testing.T, fm *FleetManager, record SnapshotRecord) {
				fm.beginArtifactPull(record.SnapshotID)
			},
			wantAlive: true,
		},
		{
			// the resolve-then-create window: a client has looked this up and
			// is about to create from it, so for that moment nothing references
			// it and it is nonetheless the least collectable thing in the fleet.
			name: "an artifact a cache lookup just resolved survives",
			arrange: func(t *testing.T, fm *FleetManager, record SnapshotRecord) {
				got, ok, err := fm.ResolveLayer(context.Background(), tenant, record.LayerKey, record.Arch)
				if err != nil || !ok {
					t.Fatalf("ResolveLayer = (%v, %v), want a hit", ok, err)
				}
				if got.SnapshotID != record.SnapshotID {
					t.Fatalf("resolved %s, want %s", got.SnapshotID, record.SnapshotID)
				}
			},
			wantAlive: true,
		},
		{
			name: "an artifact with a child snapshot survives",
			arrange: func(t *testing.T, fm *FleetManager, record SnapshotRecord) {
				child := record
				child.SnapshotID = "child-1"
				child.ParentSnapshotID = record.SnapshotID
				child.LayerKey = ""
				child.Mode = SnapshotModeAuto
				if err := fm.upsertSnapshotRecord(context.Background(), child); err != nil {
					t.Fatal(err)
				}
			},
			wantAlive: true,
		},
		{
			// a named `fuse build` artifact is operator-created and referenced
			// by name, so it keeps its explicit-delete-only contract.
			name: "a build artifact that is not a cache layer survives",
			arrange: func(t *testing.T, fm *FleetManager, record SnapshotRecord) {
				record.LayerKey = ""
				if err := fm.upsertSnapshotRecord(context.Background(), record); err != nil {
					t.Fatal(err)
				}
			},
			wantAlive: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			fm := NewFleetManager(FleetConfig{
				Provider:        newStubProvider(),
				Prefix:          "fuse-",
				ArtifactIdleTTL: time.Minute,
			})
			record := seedArtifactRecord(t, fm, "art-1", tenant, "h-a", "d1")
			// Age the record past both the idle ttl and the use grace, so the
			// only thing keeping it alive is whatever the case arranges.
			record.CreatedAt, record.UpdatedAt = stale, stale
			if err := fm.upsertSnapshotRecord(ctx, record); err != nil {
				t.Fatal(err)
			}

			tc.arrange(t, fm, record)
			fm.reconcileArtifacts(ctx)

			_, err := fm.GetSnapshotByID(ctx, record.SnapshotID)
			alive := err == nil
			if alive != tc.wantAlive {
				t.Fatalf("artifact alive = %v, want %v (err %v)", alive, tc.wantAlive, err)
			}
		})
	}
}

// TestReconcileArtifacts_perTenantCap covers the cheap ceiling behind the idle
// sweep: a tenant that builds constantly keeps every layer inside the idle
// window, so without a cap its artifacts grow without bound.
func TestReconcileArtifacts_perTenantCap(t *testing.T) {
	ctx := context.Background()
	fm := NewFleetManager(FleetConfig{
		Provider:             newStubProvider(),
		Prefix:               "fuse-",
		ArtifactMaxPerTenant: 2,
	})

	// Five artifacts for one tenant and one for another, all used long enough
	// ago to be past the grace, with strictly ordered use times.
	base := time.Now().Add(-24 * time.Hour)
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("art-%d", i)
		record := seedArtifactRecord(t, fm, id, "tenant-1", "h-a", fmt.Sprintf("d%d", i))
		record.CreatedAt, record.UpdatedAt = base, base
		if err := fm.upsertSnapshotRecord(ctx, record); err != nil {
			t.Fatal(err)
		}
		fm.mu.Lock()
		// art-0 is the least recently used, art-4 the most.
		fm.artifactUse[id] = base.Add(time.Duration(i) * time.Hour)
		fm.mu.Unlock()
	}
	other := seedArtifactRecord(t, fm, "art-other", "tenant-2", "h-a", "dx")
	other.CreatedAt, other.UpdatedAt = base, base
	if err := fm.upsertSnapshotRecord(ctx, other); err != nil {
		t.Fatal(err)
	}

	fm.reconcileArtifacts(ctx)

	alive := func(id string) bool {
		_, err := fm.GetSnapshotByID(ctx, id)
		return err == nil
	}
	// Least recently used go first, newest survive.
	for _, id := range []string{"art-0", "art-1", "art-2"} {
		if alive(id) {
			t.Errorf("%s survived, want the least recently used evicted", id)
		}
	}
	for _, id := range []string{"art-3", "art-4"} {
		if !alive(id) {
			t.Errorf("%s was evicted, want the most recently used kept", id)
		}
	}
	// The cap is per tenant, so a tenant under it is untouched.
	if !alive("art-other") {
		t.Error("another tenant's artifact was evicted by tenant-1's overflow")
	}
}

// TestReconcileArtifacts_capNeverEvictsAProtectedArtifact is the cap's own
// version of the resolve-then-create guard. Overshooting a soft ceiling for one
// reconcile tick is strictly better than deleting an artifact somebody is about
// to boot.
func TestReconcileArtifacts_capNeverEvictsAProtectedArtifact(t *testing.T) {
	ctx := context.Background()
	fm := NewFleetManager(FleetConfig{
		Provider:             newStubProvider(),
		Prefix:               "fuse-",
		ArtifactMaxPerTenant: 1,
	})

	base := time.Now().Add(-24 * time.Hour)
	for _, id := range []string{"art-cold", "art-hot"} {
		record := seedArtifactRecord(t, fm, id, "tenant-1", "h-a", "d-"+id)
		record.CreatedAt, record.UpdatedAt = base, base
		if err := fm.upsertSnapshotRecord(ctx, record); err != nil {
			t.Fatal(err)
		}
		fm.mu.Lock()
		fm.artifactUse[id] = base
		fm.mu.Unlock()
	}
	// art-cold sorts first as least recently used, but a client has just
	// resolved it, so it is the one thing that must not go.
	fm.touchArtifact("art-cold")

	fm.reconcileArtifacts(ctx)

	if _, err := fm.GetSnapshotByID(ctx, "art-cold"); err != nil {
		t.Fatalf("the just-resolved artifact was evicted by the cap: %v", err)
	}
	if _, err := fm.GetSnapshotByID(ctx, "art-hot"); err != nil {
		t.Fatalf("the cap evicted more than the overflow: %v", err)
	}
}

// TestReconcileArtifacts_offByDefault guards the upgrade contract: turning this
// on by default would mean installing a new orchestrator silently deleted
// artifacts the fleet was relying on.
func TestReconcileArtifacts_offByDefault(t *testing.T) {
	ctx := context.Background()
	fm := NewFleetManager(FleetConfig{Provider: newStubProvider(), Prefix: "fuse-"})
	record := seedArtifactRecord(t, fm, "art-1", "tenant-1", "h-a", "d1")
	record.CreatedAt = time.Now().Add(-365 * 24 * time.Hour)
	record.UpdatedAt = record.CreatedAt
	if err := fm.upsertSnapshotRecord(ctx, record); err != nil {
		t.Fatal(err)
	}

	fm.reconcileArtifacts(ctx)

	if _, err := fm.GetSnapshotByID(ctx, "art-1"); err != nil {
		t.Fatalf("an unconfigured collector evicted an artifact: %v", err)
	}
}

// TestListSnapshotsByDigest runs one conformance suite against both stores.
// The artifact index is the second question the two answer in completely
// different ways (a partial-index SQL probe versus a Go scan), and a developer
// on the in-memory default seeing a different holder set than production is
// exactly how a pull ends up aimed at a host that does not have the bytes.
func TestListSnapshotsByDigest(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		assertDigestIndex(t, NewMemoryStateStore())
	})
	t.Run("postgres", func(t *testing.T) {
		// skips when DATABASE_URL is unset, so `go test ./...` needs no db.
		assertDigestIndex(t, openTestPostgres(t))
	})
}

func assertDigestIndex(t *testing.T, store StateStore) {
	t.Helper()
	ctx := context.Background()

	const p = "snap-digest-test-"
	seed := func(id, tenant, hostID, digest string, state SnapshotState) {
		t.Helper()
		id = p + id
		t.Cleanup(func() { _ = store.DeleteSnapshot(ctx, id) })
		if err := store.UpsertSnapshot(ctx, SnapshotRecord{
			SnapshotID: id,
			VMID:       "vm-digest-test",
			HostID:     hostID,
			TenantID:   tenant,
			Mode:       SnapshotModeBuild,
			Digest:     digest,
			State:      state,
			CreatedAt:  time.Now().UTC().Truncate(time.Second),
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	seed("a", "tenant-a", "h-a", "dd", SnapshotStateReady)
	seed("b", "tenant-a", "h-b", "dd", SnapshotStateReady)
	// not ready: the bytes may not be complete on that host yet, so it is not
	// somewhere anything may be told to pull from.
	seed("creating", "tenant-a", "h-c", "dd", SnapshotStateCreating)
	// another tenant, same bytes. serving it would cross the boundary that
	// makes the whole cache per-tenant.
	seed("other", "tenant-b", "h-d", "dd", SnapshotStateReady)
	// an agent that does not hash. '' must never read as a digest.
	seed("nodigest", "tenant-a", "h-e", "", SnapshotStateReady)

	cases := []struct {
		name      string
		tenantID  string
		digest    string
		wantHosts []string
	}{
		{
			name:     "every ready holder of this tenant's copies",
			tenantID: "tenant-a", digest: "dd",
			wantHosts: []string{"h-a", "h-b"},
		},
		{
			name:     "another tenant sees only its own",
			tenantID: "tenant-b", digest: "dd",
			wantHosts: []string{"h-d"},
		},
		{
			name:     "an empty digest matches nothing",
			tenantID: "tenant-a", digest: "",
			wantHosts: nil,
		},
		{
			name:     "an unknown digest misses",
			tenantID: "tenant-a", digest: "nope",
			wantHosts: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := store.ListSnapshotsByDigest(ctx, tc.tenantID, tc.digest)
			if err != nil {
				t.Fatalf("ListSnapshotsByDigest: %v", err)
			}
			hosts := make([]string, 0, len(got))
			for _, record := range got {
				hosts = append(hosts, record.HostID)
			}
			if strings.Join(hosts, ",") != strings.Join(tc.wantHosts, ",") {
				t.Fatalf("hosts = %v, want %v", hosts, tc.wantHosts)
			}
		})
	}
}

// TestHostsHoldingArtifact_isTenantScoped pins the index's security boundary.
// An artifact carries whatever its build baked in, so another tenant holding
// byte-identical bytes is not a source this tenant may pull from.
func TestHostsHoldingArtifact_isTenantScoped(t *testing.T) {
	ctx := context.Background()
	fm := NewFleetManager(FleetConfig{Provider: newStubProvider(), Prefix: "fuse-"})
	seedArtifactRecord(t, fm, "art-mine", "tenant-1", "h-a", "d1")
	seedArtifactRecord(t, fm, "art-theirs", "tenant-2", "h-b", "d1")
	// An agent that does not hash leaves the digest empty. Empty must never be
	// a value, or every such snapshot reads as a holder of one phantom blob.
	seedArtifactRecord(t, fm, "art-nodigest", "tenant-1", "h-c", "")

	holders, err := fm.HostsHoldingArtifact(ctx, "tenant-1", "d1")
	if err != nil {
		t.Fatal(err)
	}
	if len(holders) != 1 {
		t.Fatalf("holders = %+v, want only h-a", holders)
	}
	if holders["h-a"].SnapshotID != "art-mine" {
		t.Errorf("holders[h-a] = %+v, want art-mine", holders["h-a"])
	}

	if empty, err := fm.HostsHoldingArtifact(ctx, "tenant-1", ""); err != nil || len(empty) != 0 {
		t.Errorf("an empty digest resolved to %+v (err %v), want nothing", empty, err)
	}
}
