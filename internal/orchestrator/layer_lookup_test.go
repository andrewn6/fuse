package orchestrator

import (
	"context"
	"testing"
	"time"
)

// TestFindSnapshotByLayerKey runs one conformance suite against both store
// implementations. The layer lookup is the only place the two stores answer a
// "which artifact wins" question, and they answer it in completely different
// ways (a SQL ORDER BY versus a Go scan), so a shared suite is what stops a
// developer on the in-memory default from seeing a different cache hit than
// production does.
func TestFindSnapshotByLayerKey(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		assertLayerLookup(t, NewMemoryStateStore())
	})
	t.Run("postgres", func(t *testing.T) {
		// skips when DATABASE_URL is unset, so `go test ./...` needs no db.
		assertLayerLookup(t, openTestPostgres(t))
	})
}

func assertLayerLookup(t *testing.T, store StateStore) {
	t.Helper()
	ctx := context.Background()

	base := time.Now().UTC().Truncate(time.Second)
	at := func(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute) }

	// ids are prefixed so a shared postgres instance cannot collide with
	// leftovers from another test, and every one of them is deleted after.
	const p = "snap-layerkey-test-"
	seed := func(id, tenant, layerKey, arch string, state SnapshotState, createdAt time.Time) {
		t.Helper()
		id = p + id
		t.Cleanup(func() { _ = store.DeleteSnapshot(ctx, id) })
		if err := store.UpsertSnapshot(ctx, SnapshotRecord{
			SnapshotID: id,
			VMID:       "vm-layerkey-test",
			TenantID:   tenant,
			Mode:       SnapshotModeBuild,
			LayerKey:   layerKey,
			Arch:       arch,
			Digest:     "sha256-of-" + id,
			State:      state,
			CreatedAt:  createdAt,
			UpdatedAt:  createdAt,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	// (tenant-a, keyA, amd64): an older and a newer ready artifact, plus a
	// still-creating one that is newer than both. two concurrent builds of the
	// same recipe legitimately produce the first two, so the lookup has to
	// break the tie rather than treat it as an error.
	seed("old", "tenant-a", "keyA", "amd64", SnapshotStateReady, at(0))
	seed("new", "tenant-a", "keyA", "amd64", SnapshotStateReady, at(1))
	seed("creating", "tenant-a", "keyA", "amd64", SnapshotStateCreating, at(9))

	// same key and tenant, other arch. newer than every amd64 row, so if arch
	// were not part of the lookup this would win and hand back a rootfs the
	// guest cannot boot.
	seed("arm", "tenant-a", "keyA", "arm64", SnapshotStateReady, at(8))

	// same key and arch, another tenant, newest of all. serving this would leak
	// whatever that tenant's build baked in.
	seed("other-tenant", "tenant-b", "keyA", "amd64", SnapshotStateReady, at(10))

	// keyB exists only on arm64.
	seed("keyb-arm", "tenant-a", "keyB", "arm64", SnapshotStateReady, at(2))

	// a plain (non-layer) build artifact: no layer key at all.
	seed("no-key", "tenant-a", "", "amd64", SnapshotStateReady, at(3))

	// two ready artifacts with the same timestamp, to pin the tiebreak.
	seed("tie-a", "tenant-a", "keyC", "amd64", SnapshotStateReady, at(4))
	seed("tie-b", "tenant-a", "keyC", "amd64", SnapshotStateReady, at(4))

	cases := []struct {
		name     string
		tenantID string
		layerKey string
		arch     string
		wantID   string // "" means the lookup must miss
	}{
		{
			name:     "newest ready artifact wins",
			tenantID: "tenant-a", layerKey: "keyA", arch: "amd64",
			wantID: p + "new",
		},
		{
			name:     "an unknown key misses",
			tenantID: "tenant-a", layerKey: "keyZ", arch: "amd64",
			wantID: "",
		},
		{
			name:     "an arch mismatch misses",
			tenantID: "tenant-a", layerKey: "keyB", arch: "amd64",
			wantID: "",
		},
		{
			name:     "the arch that does exist hits",
			tenantID: "tenant-a", layerKey: "keyB", arch: "arm64",
			wantID: p + "keyb-arm",
		},
		{
			// proves the cross-tenant row above is genuinely present, so the
			// tenant-a miss is scoping and not an absent artifact.
			name:     "another tenant sees its own artifact",
			tenantID: "tenant-b", layerKey: "keyA", arch: "amd64",
			wantID: p + "other-tenant",
		},
		{
			name:     "an unknown tenant misses",
			tenantID: "tenant-c", layerKey: "keyA", arch: "amd64",
			wantID: "",
		},
		{
			// every non-layer snapshot in the fleet has an empty layer key, so
			// an empty query must never match one.
			name:     "an empty layer key misses",
			tenantID: "tenant-a", layerKey: "", arch: "amd64",
			wantID: "",
		},
		{
			name:     "equal timestamps break on snapshot id descending",
			tenantID: "tenant-a", layerKey: "keyC", arch: "amd64",
			wantID: p + "tie-b",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := store.FindSnapshotByLayerKey(ctx, tc.tenantID, tc.layerKey, tc.arch)
			if err != nil {
				t.Fatalf("FindSnapshotByLayerKey: %v", err)
			}
			if tc.wantID == "" {
				if ok {
					t.Fatalf("got hit %s, want a miss", got.SnapshotID)
				}
				return
			}
			if !ok {
				t.Fatalf("got a miss, want %s", tc.wantID)
			}
			if got.SnapshotID != tc.wantID {
				t.Fatalf("got %s, want %s", got.SnapshotID, tc.wantID)
			}
			// the record is returned whole, not just its id: the caller needs
			// the host and the digest to seed and verify from it.
			if got.LayerKey != tc.layerKey || got.Arch != tc.arch {
				t.Errorf("got layer_key=%q arch=%q, want %q/%q", got.LayerKey, got.Arch, tc.layerKey, tc.arch)
			}
			if got.Digest != "sha256-of-"+tc.wantID {
				t.Errorf("digest = %q, want it to round-trip", got.Digest)
			}
		})
	}
}
