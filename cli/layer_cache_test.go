package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/folsomintel/fuse/internal/fusefile"
	fuse "github.com/folsomintel/fuse/sdks/go"
)

func testClient(t *testing.T, h http.HandlerFunc) *fuse.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cl, err := fuse.New(srv.URL, "test-token")
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return cl
}

// hostsHandler serves a hosts list and a hosts get from one fixture.
func hostsHandler(t *testing.T, hosts []fuse.Host) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if id := strings.TrimPrefix(r.URL.Path, "/v1/hosts/"); id != r.URL.Path && id != "" {
			for _, h := range hosts {
				if h.ID == id {
					_ = json.NewEncoder(w).Encode(h)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"hosts": hosts})
	}
}

func host(id, arch string) fuse.Host {
	return fuse.Host{ID: id, State: "ready", Capacity: fuse.HostCapacity{Arch: arch}}
}

func TestTargetArch(t *testing.T) {
	tests := []struct {
		name       string
		hosts      []fuse.Host
		activeHost string
		want       string
	}{
		{"single host", []fuse.Host{host("a", "arm64")}, "", "arm64"},
		{"homogeneous fleet", []fuse.Host{host("a", "amd64"), host("b", "amd64")}, "", "amd64"},
		// no answer is safe here: filing a layer under a coin flip means a
		// later lookup is served a rootfs it cannot boot.
		{"mixed fleet", []fuse.Host{host("a", "amd64"), host("b", "arm64")}, "", ""},
		{"no hosts", nil, "", ""},
		// a scoped host is an explicit answer, even when the fleet is mixed.
		{"scoped host wins", []fuse.Host{host("a", "amd64"), host("b", "arm64")}, "b", "arm64"},
		// an unmigrated host declares no arch, and the write path stamps amd64
		// for it, so a lookup has to say amd64 too or it can never hit.
		{"undeclared arch is amd64", []fuse.Host{host("a", "")}, "", "amd64"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cl := testClient(t, hostsHandler(t, tt.hosts))
			if got := targetArch(context.Background(), cl, tt.activeHost); got != tt.want {
				t.Errorf("targetArch = %q, want %q", got, tt.want)
			}
		})
	}
}

// planFor derives a real layer plan so the test exercises the same keys the
// build would.
func planFor(t *testing.T) *layerPlan {
	t.Helper()
	dir := t.TempDir()
	path := writeCachedFusefile(t, dir)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	f, err := fusefile.Parse(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := fusefile.ResolveFiles(f, dir); err != nil {
		t.Fatalf("resolve files: %v", err)
	}
	lp, err := buildLayerPlan(path, f)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return lp
}

// Deepest-first with an early exit is the whole point: a hit at the last step
// means no shallower key needs to be asked about at all, so a warm cache costs
// one round trip rather than one per step.
func TestResolveDeepestLayerStopsAtFirstHit(t *testing.T) {
	lp := planFor(t)
	var probed []string
	cl := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("layer_key")
		probed = append(probed, key)
		w.Header().Set("Content-Type", "application/json")
		// every key hits, so a correct walk asks exactly once.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"found":    true,
			"snapshot": map[string]any{"id": "snap-deep", "layer_key": key},
		})
	})

	hit, err := resolveDeepestLayer(context.Background(), cl, lp, "amd64")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if hit == nil {
		t.Fatal("want a hit")
	}
	if hit.Index != len(lp.Steps)-1 {
		t.Errorf("hit index = %d, want the deepest step %d", hit.Index, len(lp.Steps)-1)
	}
	if len(probed) != 1 {
		t.Errorf("probed %d keys, want 1: %v", len(probed), probed)
	}
	if probed[0] != lp.Steps[len(lp.Steps)-1].Key {
		t.Errorf("probed %q first, want the deepest key", probed[0])
	}
}

func TestResolveDeepestLayerWalksBackOnMisses(t *testing.T) {
	lp := planFor(t)
	var probed []string
	cl := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("layer_key")
		probed = append(probed, key)
		w.Header().Set("Content-Type", "application/json")
		// only the shallowest step is cached.
		if key == lp.Steps[0].Key {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"found":    true,
				"snapshot": map[string]any{"id": "snap-shallow", "layer_key": key},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"found": false})
	})

	hit, err := resolveDeepestLayer(context.Background(), cl, lp, "amd64")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if hit == nil || hit.Index != 0 {
		t.Fatalf("hit = %+v, want index 0", hit)
	}
	if len(probed) != len(lp.Steps) {
		t.Errorf("probed %d keys, want every step %d", len(probed), len(lp.Steps))
	}
}

func TestResolveDeepestLayerMissIsNotAnError(t *testing.T) {
	lp := planFor(t)
	cl := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"found": false})
	})
	hit, err := resolveDeepestLayer(context.Background(), cl, lp, "amd64")
	if err != nil {
		t.Errorf("a cold cache must not be an error: %v", err)
	}
	if hit != nil {
		t.Errorf("hit = %+v, want none", hit)
	}
}

// Without an arch there is nothing safe to ask for, so the lookup must not
// happen at all rather than being asked with a guess.
func TestResolveDeepestLayerSkipsWithoutArch(t *testing.T) {
	lp := planFor(t)
	cl := testClient(t, func(_ http.ResponseWriter, r *http.Request) {
		t.Fatalf("server should not have been called: %s %s", r.Method, r.URL.Path)
	})
	hit, err := resolveDeepestLayer(context.Background(), cl, lp, "")
	if err != nil || hit != nil {
		t.Errorf("got (%+v, %v), want (nil, nil)", hit, err)
	}
}

func TestCacheablePrefixLen(t *testing.T) {
	tests := []struct {
		name string
		plan *layerPlan
		want int
	}{
		{"nil plan", nil, 0},
		{"all cacheable", &layerPlan{Steps: []fusefile.LayerKey{{Cacheable: true}, {Cacheable: true}}}, 2},
		{"none cacheable", &layerPlan{Steps: []fusefile.LayerKey{{}, {}}}, 0},
		// the chain breaks permanently at the first opt-out, so anything after
		// it is uncacheable no matter what it says.
		{"breaks midway", &layerPlan{Steps: []fusefile.LayerKey{{Cacheable: true}, {}, {}}}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cacheablePrefixLen(tt.plan); got != tt.want {
				t.Errorf("cacheablePrefixLen = %d, want %d", got, tt.want)
			}
		})
	}
}

// One artifact satisfies every step leading to it, so a hit at index N marks
// all of 0..N rather than just N.
func TestMarkHitCoversEveryStepBelowIt(t *testing.T) {
	lp := &layerPlan{
		Steps:    []fusefile.LayerKey{{Cacheable: true}, {Cacheable: true}, {Cacheable: true}},
		Statuses: []layerPlanStepStatus{layerStatusMiss, layerStatusMiss, layerStatusMiss},
	}
	lp.markHit(1)
	if lp.hitCount() != 2 {
		t.Errorf("hitCount = %d, want 2", lp.hitCount())
	}
	if lp.Statuses[2] != layerStatusMiss {
		t.Errorf("step above the hit = %q, want miss", lp.Statuses[2])
	}
}
