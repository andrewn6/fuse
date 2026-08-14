package orchestrator

import (
	"context"
	"io"
	"sync"
	"testing"
)

// layerEnv is a snapshot-capable environment whose checkpoint reports a
// digest, standing in for a host agent that hashes the artifact as it writes
// it. digest is set to "" to model the other case that has to keep working: an
// agent build that predates artifact hashing and simply omits the field.
type layerEnv struct {
	name   string
	digest string

	mu          sync.Mutex
	checkpoints []Checkpoint
}

func (e *layerEnv) Name() string  { return e.name }
func (e *layerEnv) URL() string   { return "http://" + e.name + ".test" }
func (e *layerEnv) Token() string { return "" }

func (e *layerEnv) Exec(context.Context, []string, ExecOptions) (ExecResult, error) {
	return ExecResult{}, nil
}
func (e *layerEnv) ExecStream(context.Context, io.Writer, io.Writer, string, ...string) error {
	return nil
}
func (e *layerEnv) Upload(context.Context, []byte, string) error { return nil }
func (e *layerEnv) StartAgent(context.Context, AgentSpec) error  { return nil }

func (e *layerEnv) Checkpoint(ctx context.Context, comment string) (string, error) {
	cp, err := e.CheckpointWithDigest(ctx, comment)
	return cp.ID, err
}

func (e *layerEnv) CheckpointWithDigest(_ context.Context, comment string) (Checkpoint, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cp := Checkpoint{ID: "cp-1", Comment: comment, SizeBytes: 4096, Digest: e.digest}
	e.checkpoints = append(e.checkpoints, cp)
	return cp, nil
}

func (e *layerEnv) Restore(context.Context, string) error { return nil }

func (e *layerEnv) ListCheckpoints(context.Context) ([]Checkpoint, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Checkpoint, len(e.checkpoints))
	copy(out, e.checkpoints)
	return out, nil
}

type layerProvider struct {
	mu  sync.Mutex
	env Environment
}

func (p *layerProvider) Create(_ context.Context, spec Spec) (Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.env.(*layerEnv); ok {
		e.name = spec.Name
	}
	return p.env, nil
}

func (p *layerProvider) Get(context.Context, string) (Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.env, nil
}

func (p *layerProvider) Destroy(context.Context, string) error { return nil }

func (p *layerProvider) List(context.Context, string) ([]Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return []Environment{p.env}, nil
}

func (*layerProvider) Close() error { return nil }

// layerHost is a plain (non-gpu) firecracker host with a declared arch. arch
// is passed in the operator's spelling on purpose in one of the cases below,
// to prove the stored value is normalized.
func layerHost(id, arch string) Host {
	return Host{
		ID:      id,
		URL:     "http://" + id + ".test",
		Backend: BackendFirecracker,
		Capacity: HostCapacity{
			CPUs:      8,
			RamMB:     8192,
			StorageGB: 100,
			VMCount:   10,
			Arch:      arch,
		},
	}
}

// TestCreateSnapshot_recordsLayerIdentity covers what a build artifact has to
// carry to be findable later: the caller's layer key, the arch of the host
// that actually built it, and the digest the agent computed.
func TestCreateSnapshot_recordsLayerIdentity(t *testing.T) {
	cases := []struct {
		name       string
		hostArch   string // as the operator declared it at registration
		digest     string // as the agent reported it
		layerKey   string
		wantArch   string
		wantDigest string
	}{
		{
			name:     "layer key, host arch and digest all land on the record",
			hostArch: "amd64", digest: "abc123", layerKey: "layer-1",
			wantArch: "amd64", wantDigest: "abc123",
		},
		{
			// the operator registered the host with the uname spelling. it has
			// to be stored in the scheduler's vocabulary, or a lookup for
			// "arm64" would miss an artifact that is in fact arm64.
			name:     "an aarch64 host is normalized to arm64",
			hostArch: "aarch64", digest: "def456", layerKey: "layer-1",
			wantArch: "arm64", wantDigest: "def456",
		},
		{
			// an agent that predates artifact hashing sends no digest. the
			// snapshot still has to succeed: the digest verifies a later
			// transfer, it is not something the artifact needs to exist.
			name:     "a host agent that does not hash yields an empty digest",
			hostArch: "amd64", digest: "", layerKey: "layer-1",
			wantArch: "amd64", wantDigest: "",
		},
		{
			// an ordinary checkpoint. arch is still recorded (it costs
			// nothing and is true), but with no layer key it can never be
			// reached by a cache lookup.
			name:     "an ordinary snapshot carries no layer key",
			hostArch: "amd64", digest: "abc123", layerKey: "",
			wantArch: "amd64", wantDigest: "abc123",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			env := &layerEnv{digest: tc.digest}
			hostProvider := &layerProvider{env: env}
			fm := NewFleetManager(FleetConfig{Provider: &layerProvider{env: env}, Prefix: "b-"})
			if err := fm.RegisterHost(ctx, layerHost("h1", tc.hostArch), hostProvider); err != nil {
				t.Fatal(err)
			}

			info, err := fm.ProvisionAndAssign(ctx, "build-task", Spec{CPUs: 1, RamMB: 256}, []byte(`{}`), nil, BootOptions{})
			if err != nil {
				t.Fatal(err)
			}

			record, err := fm.CreateSnapshot(ctx, info.ID, SnapshotOptions{
				Mode:     SnapshotModeBuild,
				LayerKey: tc.layerKey,
			})
			if err != nil {
				t.Fatalf("CreateSnapshot: %v", err)
			}

			if record.LayerKey != tc.layerKey {
				t.Errorf("layer_key = %q, want %q", record.LayerKey, tc.layerKey)
			}
			if record.Arch != tc.wantArch {
				t.Errorf("arch = %q, want %q", record.Arch, tc.wantArch)
			}
			if record.Digest != tc.wantDigest {
				t.Errorf("digest = %q, want %q", record.Digest, tc.wantDigest)
			}

			// and it survives the round trip through the store, which is what
			// a later build actually reads.
			stored, err := fm.GetSnapshotByID(ctx, record.SnapshotID)
			if err != nil {
				t.Fatalf("GetSnapshotByID: %v", err)
			}
			if stored.LayerKey != tc.layerKey || stored.Arch != tc.wantArch || stored.Digest != tc.wantDigest {
				t.Errorf("stored layer_key/arch/digest = %q/%q/%q, want %q/%q/%q",
					stored.LayerKey, stored.Arch, stored.Digest, tc.layerKey, tc.wantArch, tc.wantDigest)
			}
		})
	}
}

// TestResolveLayer_findsWhatCreateSnapshotWrote is the end-to-end shape of the
// cache: a build writes an artifact, and a lookup by the same key and arch
// finds it. It is the one test that would catch the write path and the read
// path disagreeing about how either value is spelled.
func TestResolveLayer_findsWhatCreateSnapshotWrote(t *testing.T) {
	ctx := context.Background()
	env := &layerEnv{digest: "abc123"}
	hostProvider := &layerProvider{env: env}
	fm := NewFleetManager(FleetConfig{Provider: &layerProvider{env: env}, Prefix: "b-"})
	if err := fm.RegisterHost(ctx, layerHost("h1", "x86_64"), hostProvider); err != nil {
		t.Fatal(err)
	}
	info, err := fm.ProvisionAndAssign(ctx, "build-task", Spec{CPUs: 1, RamMB: 256}, []byte(`{}`), nil, BootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	record, err := fm.CreateSnapshot(ctx, info.ID, SnapshotOptions{Mode: SnapshotModeBuild, LayerKey: "layer-1"})
	if err != nil {
		t.Fatal(err)
	}

	got, ok, err := fm.ResolveLayer(ctx, record.TenantID, "layer-1", "amd64")
	if err != nil {
		t.Fatalf("ResolveLayer: %v", err)
	}
	if !ok {
		t.Fatalf("got a miss, want %s", record.SnapshotID)
	}
	if got.SnapshotID != record.SnapshotID {
		t.Errorf("resolved %s, want %s", got.SnapshotID, record.SnapshotID)
	}

	// the arch filter is not decoration: the same key on the other arch is a
	// miss, because that rootfs would not boot.
	if _, ok, err := fm.ResolveLayer(ctx, record.TenantID, "layer-1", "arm64"); err != nil || ok {
		t.Errorf("arm64 lookup ok = %v (err %v), want a miss", ok, err)
	}
	if _, ok, err := fm.ResolveLayer(ctx, "someone-else", "layer-1", "amd64"); err != nil || ok {
		t.Errorf("cross-tenant lookup ok = %v (err %v), want a miss", ok, err)
	}
}
