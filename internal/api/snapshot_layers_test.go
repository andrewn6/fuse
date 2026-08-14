package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// newLayerTestHandler wires a handler over a state store the test keeps a
// reference to, so layer artifacts can be seeded directly rather than by
// booting a VM per fixture. The lookup paths under test read the store, so
// seeding it is the whole setup they need.
func newLayerTestHandler(t *testing.T) (*Handler, orchestrator.StateStore) {
	t.Helper()
	store := orchestrator.NewMemoryStateStore()
	fm := orchestrator.NewFleetManager(orchestrator.FleetConfig{
		Provider:   newFakeProvider(),
		Prefix:     "fuse-",
		StateStore: store,
	})
	return &Handler{Fleet: fm}, store
}

// seedLayer inserts one ready artifact. Timestamps are spread by the caller so
// "newest wins" is deterministic.
func seedLayer(t *testing.T, store orchestrator.StateStore, id, tenant, layerKey, arch string, createdAt time.Time) {
	t.Helper()
	if err := store.UpsertSnapshot(context.Background(), orchestrator.SnapshotRecord{
		SnapshotID: id,
		VMID:       "vm-1",
		TenantID:   tenant,
		Mode:       orchestrator.SnapshotModeBuild,
		LayerKey:   layerKey,
		Arch:       arch,
		Digest:     "sha256-" + id,
		State:      orchestrator.SnapshotStateReady,
		CreatedAt:  createdAt,
		UpdatedAt:  createdAt,
	}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

// getAuthed serves a GET as a specific caller, which is the only way to
// exercise tenant scoping: the scope is read from the credentials, never from
// the request.
func getAuthed(t *testing.T, h http.Handler, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestListSnapshots_layerFilters(t *testing.T) {
	h, store := newLayerTestHandler(t)
	r := mustRouter(t, h)

	base := time.Now().UTC().Truncate(time.Second)
	seedLayer(t, store, "amd-old", "t1", "keyA", "amd64", base)
	seedLayer(t, store, "amd-new", "t1", "keyA", "amd64", base.Add(time.Minute))
	seedLayer(t, store, "arm", "t1", "keyA", "arm64", base.Add(2*time.Minute))
	seedLayer(t, store, "keyb", "t1", "keyB", "amd64", base.Add(3*time.Minute))
	// an ordinary checkpoint: no layer key, no arch.
	seedLayer(t, store, "plain", "t1", "", "", base.Add(4*time.Minute))

	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{
			// no filter at all still returns everything, including the rows
			// with an empty layer key: empty means "do not filter", never
			// "match the empty value".
			name:  "no layer filters returns every snapshot",
			query: "",
			want:  []string{"plain", "keyb", "arm", "amd-new", "amd-old"},
		},
		{
			name:  "layer_key narrows to one step",
			query: "?layer_key=keyA",
			want:  []string{"arm", "amd-new", "amd-old"},
		},
		{
			name:  "arch narrows across steps",
			query: "?arch=arm64",
			want:  []string{"arm"},
		},
		{
			name:  "layer_key and arch combine",
			query: "?layer_key=keyA&arch=amd64",
			want:  []string{"amd-new", "amd-old"},
		},
		{
			// the uname spelling an operator would actually type has to reach
			// the same rows as the stored goarch spelling.
			name:  "arch accepts the uname spelling",
			query: "?layer_key=keyA&arch=aarch64",
			want:  []string{"arm"},
		},
		{
			name:  "an unknown key matches nothing",
			query: "?layer_key=keyZ",
			want:  []string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := getAuthed(t, r, "/v1/snapshots"+tc.query, "")
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d. body: %s", rr.Code, rr.Body.String())
			}
			var list SnapshotList
			if err := json.NewDecoder(rr.Body).Decode(&list); err != nil {
				t.Fatalf("decode: %v", err)
			}
			got := make([]string, 0, len(list.Snapshots))
			for _, s := range list.Snapshots {
				got = append(got, s.ID)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ids = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("ids = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestListSnapshots_exposesLayerFields(t *testing.T) {
	h, store := newLayerTestHandler(t)
	r := mustRouter(t, h)
	seedLayer(t, store, "cp-1", "t1", "keyA", "amd64", time.Now().UTC())

	rr := getAuthed(t, r, "/v1/snapshots?layer_key=keyA&arch=amd64", "")
	var list SnapshotList
	if err := json.NewDecoder(rr.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Snapshots) != 1 {
		t.Fatalf("len = %d, want 1", len(list.Snapshots))
	}
	got := list.Snapshots[0]
	if got.LayerKey != "keyA" || got.Arch != "amd64" || got.Digest != "sha256-cp-1" {
		t.Errorf("layer_key/arch/digest = %q/%q/%q, want keyA/amd64/sha256-cp-1", got.LayerKey, got.Arch, got.Digest)
	}
}

func TestResolveSnapshot(t *testing.T) {
	h, store := newLayerTestHandler(t)
	r := mustRouter(t, h)

	base := time.Now().UTC().Truncate(time.Second)
	seedLayer(t, store, "old", "", "keyA", "amd64", base)
	seedLayer(t, store, "new", "", "keyA", "amd64", base.Add(time.Minute))
	seedLayer(t, store, "arm", "", "keyB", "arm64", base.Add(2*time.Minute))

	cases := []struct {
		name       string
		query      string
		wantStatus int
		wantFound  bool
		wantID     string
	}{
		{
			// two concurrent builds of the same recipe both insert; the newest
			// ready artifact is the answer, not an ambiguity.
			name:       "hit returns the newest artifact for the key",
			query:      "?layer_key=keyA&arch=amd64",
			wantStatus: http.StatusOK, wantFound: true, wantID: "new",
		},
		{
			// a cold cache is the ordinary first-build outcome, not a failure.
			name:       "unknown key is a 200 miss",
			query:      "?layer_key=keyZ&arch=amd64",
			wantStatus: http.StatusOK, wantFound: false,
		},
		{
			// the key exists, just not for this arch. serving it anyway would
			// hand back a rootfs the caller cannot boot.
			name:       "arch mismatch is a miss, not a fallback",
			query:      "?layer_key=keyB&arch=amd64",
			wantStatus: http.StatusOK, wantFound: false,
		},
		{
			name:       "the arch that does exist hits",
			query:      "?layer_key=keyB&arch=arm64",
			wantStatus: http.StatusOK, wantFound: true, wantID: "arm",
		},
		{
			// rejected rather than guessed: a default arch would be a silent
			// wrong answer at the exact moment the caller trusts a hit.
			name:       "missing arch is a 400",
			query:      "?layer_key=keyA",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "an unrecognized arch simply misses",
			query:      "?layer_key=keyA&arch=riscv64",
			wantStatus: http.StatusOK, wantFound: false,
		},
		{
			// an empty key matches nothing by construction, so answering
			// "miss" would hide a caller that failed to derive its keys.
			name:       "missing layer_key is a 400",
			query:      "?arch=amd64",
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := getAuthed(t, r, "/v1/snapshots/resolve"+tc.query, "")
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d. body: %s", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if tc.wantStatus != http.StatusOK {
				return
			}
			var out ResolveSnapshotResponse
			if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if out.Found != tc.wantFound {
				t.Fatalf("found = %v, want %v", out.Found, tc.wantFound)
			}
			if !tc.wantFound {
				if out.Snapshot != nil {
					t.Errorf("snapshot = %+v on a miss, want absent", out.Snapshot)
				}
				return
			}
			if out.Snapshot == nil {
				t.Fatal("found=true with no snapshot")
			}
			if out.Snapshot.ID != tc.wantID {
				t.Errorf("id = %q, want %q", out.Snapshot.ID, tc.wantID)
			}
			// the caller seeds from this record and verifies the transfer
			// against it, so the identity fields have to come back too.
			if out.Snapshot.Digest != "sha256-"+tc.wantID {
				t.Errorf("digest = %q, want it to round-trip", out.Snapshot.Digest)
			}
		})
	}
}

// TestResolveSnapshot_doesNotCrossTenants is the security property: the scope
// comes from the credentials, so no request a caller can compose reaches
// another tenant's artifact.
func TestResolveSnapshot_doesNotCrossTenants(t *testing.T) {
	store := orchestrator.NewMemoryStateStore()
	fm := orchestrator.NewFleetManager(orchestrator.FleetConfig{
		Provider:   newFakeProvider(),
		Prefix:     "fuse-",
		StateStore: store,
	})
	keys := &stubKeyStore{liveKey: "fuse_sk_a", keyID: "ak_a"}
	h := &Handler{Fleet: fm, AuthToken: "master", APIKeys: keys}
	r := mustRouter(t, h)

	now := time.Now().UTC()
	seedLayer(t, store, "theirs", "ak_b", "keyA", "amd64", now)

	// ak_a authenticates fine and the artifact is right there in the store,
	// but it belongs to ak_b.
	rr := getAuthed(t, r, "/v1/snapshots/resolve?layer_key=keyA&arch=amd64", "fuse_sk_a")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", rr.Code, rr.Body.String())
	}
	var out ResolveSnapshotResponse
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Found {
		t.Fatalf("resolved another tenant's artifact %+v", out.Snapshot)
	}

	// and the same caller does see its own, so the miss above is scoping and
	// not a lookup that never works under key auth.
	seedLayer(t, store, "mine", "ak_a", "keyA", "amd64", now.Add(time.Minute))
	rr = getAuthed(t, r, "/v1/snapshots/resolve?layer_key=keyA&arch=amd64", "fuse_sk_a")
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Found || out.Snapshot.ID != "mine" {
		t.Fatalf("resolved %+v, want mine", out.Snapshot)
	}
}

// TestCreateSnapshot_labelsLayer is the write half: the key the caller sends
// is what a later resolve has to be able to find.
func TestCreateSnapshot_labelsLayer(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	_ = doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
	})

	rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-1/snapshots", CreateSnapshotRequest{
		Mode:     "build",
		LayerKey: "layer-abc",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d. body: %s", rr.Code, rr.Body.String())
	}
	var snap Snapshot
	if err := json.NewDecoder(rr.Body).Decode(&snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.LayerKey != "layer-abc" {
		t.Errorf("layer_key = %q, want layer-abc", snap.LayerKey)
	}
	// the fake env does not hash, standing in for a host agent that predates
	// artifact hashing. that must not fail the snapshot.
	if snap.Digest != "" {
		t.Errorf("digest = %q, want empty from a non-hashing backend", snap.Digest)
	}

	// an ordinary snapshot on the same VM stays unlabelled, so it can never be
	// reached by a cache lookup.
	rr = doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-1/snapshots", CreateSnapshotRequest{})
	var plain Snapshot
	if err := json.NewDecoder(rr.Body).Decode(&plain); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if plain.LayerKey != "" {
		t.Errorf("layer_key = %q on an ordinary snapshot, want empty", plain.LayerKey)
	}
}
