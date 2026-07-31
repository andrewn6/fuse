package fusefile

import (
	"fmt"
	"strings"
	"testing"
)

// cached builds a Fusefile with caching on and the given steps.
func cached(steps ...Step) *Fusefile {
	return &Fusefile{Version: 1, Image: "base-ubuntu-24", Cache: Cache{Enabled: true}, Setup: steps}
}

// keysOf derives keys and fails the test on error.
func keysOf(t *testing.T, f *Fusefile, inputs InputDigester) []LayerKey {
	t.Helper()
	keys, err := LayerKeys(f, inputs, LayerOptions{Arch: "amd64"})
	if err != nil {
		t.Fatalf("LayerKeys: %v", err)
	}
	return keys
}

func TestLayerKeysAreStable(t *testing.T) {
	f := cached(Step{Run: "a"}, Step{Run: "b"}, Step{Run: "c"})

	first := keysOf(t, f, nil)
	second := keysOf(t, cached(Step{Run: "a"}, Step{Run: "b"}, Step{Run: "c"}), nil)

	if len(first) != 3 {
		t.Fatalf("got %d keys, want 3", len(first))
	}
	for i := range first {
		if !first[i].Cacheable {
			t.Fatalf("step %d: not cacheable, reason %q", i, first[i].Reason)
		}
		if first[i].Key != second[i].Key {
			t.Errorf("step %d: key is not stable across runs: %q vs %q", i, first[i].Key, second[i].Key)
		}
		if len(first[i].Key) != 64 {
			t.Errorf("step %d: key = %q, want a hex sha256", i, first[i].Key)
		}
	}

	// the chain must actually chain: each step's parent is the previous key.
	if first[0].ParentKey != BaseKey("base-ubuntu-24", nil) {
		t.Errorf("step 0 parent = %q, want the base key", first[0].ParentKey)
	}
	if first[1].ParentKey != first[0].Key {
		t.Errorf("step 1 parent = %q, want step 0's key %q", first[1].ParentKey, first[0].Key)
	}
	if first[2].ParentKey != first[1].Key {
		t.Errorf("step 2 parent = %q, want step 1's key %q", first[2].ParentKey, first[1].Key)
	}

	// distinct steps must not collide.
	if first[0].Key == first[1].Key || first[1].Key == first[2].Key {
		t.Errorf("distinct steps produced equal keys: %v", first)
	}
}

func TestLayerKeysEditInvalidatesSuffixOnly(t *testing.T) {
	before := keysOf(t, cached(Step{Run: "a"}, Step{Run: "b"}, Step{Run: "c"}), nil)
	after := keysOf(t, cached(Step{Run: "a"}, Step{Run: "b2"}, Step{Run: "c"}), nil)

	if before[0].Key != after[0].Key {
		t.Errorf("step 0 changed after editing step 1: %q -> %q", before[0].Key, after[0].Key)
	}
	if before[1].Key == after[1].Key {
		t.Errorf("step 1 key unchanged after editing its script: %q", after[1].Key)
	}
	// step 2's script is untouched, but its parent moved, so it must move too.
	if before[2].Key == after[2].Key {
		t.Errorf("step 2 key unchanged after its parent changed: %q", after[2].Key)
	}
}

func TestLayerKeysChangeWithEveryKeyComponent(t *testing.T) {
	base := cached(Step{Run: "a"})
	baseKey := keysOf(t, base, nil)[0].Key

	inserted := keysOf(t, cached(Step{Run: "x"}, Step{Run: "a"}), nil)
	if inserted[1].Key == baseKey {
		t.Errorf("inserting a step before it did not change its key")
	}

	otherImage := cached(Step{Run: "a"})
	otherImage.Image = "base-ubuntu-22"
	if keysOf(t, otherImage, nil)[0].Key == baseKey {
		t.Errorf("changing the base image did not change the key")
	}

	otherWorkspace := cached(Step{Run: "a"})
	otherWorkspace.Workspace = "/srv/app"
	if keysOf(t, otherWorkspace, nil)[0].Key == baseKey {
		t.Errorf("changing the workspace did not change the key")
	}

	// an explicit default workspace must key identically to an omitted one.
	explicitDefault := cached(Step{Run: "a"})
	explicitDefault.Workspace = defaultWorkspace
	if keysOf(t, explicitDefault, nil)[0].Key != baseKey {
		t.Errorf("spelling out the default workspace changed the key")
	}

	otherArch, err := LayerKeys(base, nil, LayerOptions{Arch: "arm64"})
	if err != nil {
		t.Fatal(err)
	}
	if otherArch[0].Key == baseKey {
		t.Errorf("changing the arch did not change the key")
	}

	// resources other than the arch do not change the filesystem.
	moreCPU := cached(Step{Run: "a"})
	moreCPU.Resources.CPUs = 16
	moreCPU.Resources.Memory = "64GB"
	if keysOf(t, moreCPU, nil)[0].Key != baseKey {
		t.Errorf("resources leaked into the key")
	}

	// run: is the entrypoint, not a layer.
	withRun := cached(Step{Run: "a"})
	withRun.Run = "./start.sh"
	if keysOf(t, withRun, nil)[0].Key != baseKey {
		t.Errorf("run: leaked into the key")
	}

	// secrets are excluded: a layer must not be keyed on material it is not
	// allowed to bake in.
	withSecrets := cached(Step{Run: "a"})
	withSecrets.Secrets = []string{"pg_password"}
	if keysOf(t, withSecrets, nil)[0].Key != baseKey {
		t.Errorf("secrets leaked into the key")
	}

	// an explicit cache: true is the same as an unset one.
	yes := true
	if keysOf(t, cached(Step{Run: "a", Cache: &yes}), nil)[0].Key != baseKey {
		t.Errorf("an explicit cache: true changed the key")
	}
}

func TestLayerKeysFoldInInputsDigest(t *testing.T) {
	digest := "deadbeef"
	inputs := func(patterns []string) (string, error) {
		if len(patterns) == 0 {
			t.Fatalf("digester called with no patterns")
		}
		return digest, nil
	}

	withInputs := keysOf(t, cached(Step{Run: "npm ci", Inputs: []string{"package.json"}}), inputs)
	if withInputs[0].InputsDigest != digest {
		t.Errorf("inputs digest = %q, want %q", withInputs[0].InputsDigest, digest)
	}

	without := keysOf(t, cached(Step{Run: "npm ci"}), inputs)
	if without[0].InputsDigest != "" {
		t.Errorf("inputs digest = %q, want empty for a step with no inputs", without[0].InputsDigest)
	}
	if without[0].Key == withInputs[0].Key {
		t.Errorf("declaring inputs did not change the key")
	}

	digest = "cafebabe"
	changed := keysOf(t, cached(Step{Run: "npm ci", Inputs: []string{"package.json"}}), inputs)
	if changed[0].Key == withInputs[0].Key {
		t.Errorf("a changed inputs digest did not change the key")
	}
}

func TestLayerKeysInputsWithoutDigesterFails(t *testing.T) {
	_, err := LayerKeys(cached(Step{Run: "npm ci", Inputs: []string{"package.json"}}), nil, LayerOptions{})
	if err == nil || !strings.Contains(err.Error(), "setup[0].inputs") {
		t.Fatalf("want a setup[0].inputs error, got %v", err)
	}
}

func TestLayerKeysInputDigesterErrorIsAttributed(t *testing.T) {
	inputs := func([]string) (string, error) { return "", fmt.Errorf("no files matched") }
	_, err := LayerKeys(cached(Step{Run: "a"}, Step{Run: "b", Inputs: []string{"nope"}}), inputs, LayerOptions{})
	if err == nil || !strings.Contains(err.Error(), "setup[1].inputs") {
		t.Fatalf("want a setup[1].inputs error, got %v", err)
	}
}

func TestLayerKeysOptOutCascades(t *testing.T) {
	no := false
	keys := keysOf(t, cached(
		Step{Run: "a"},
		Step{Run: "b", Cache: &no},
		Step{Run: "c"},
		Step{Run: "d"},
	), nil)

	if !keys[0].Cacheable {
		t.Errorf("step 0 should still be cacheable, reason %q", keys[0].Reason)
	}
	if keys[1].Cacheable || keys[1].Reason != ReasonStepOptedOut {
		t.Errorf("step 1 = %+v, want uncacheable with the opt-out reason", keys[1])
	}
	for _, i := range []int{2, 3} {
		if keys[i].Cacheable {
			t.Errorf("step %d is cacheable after an uncacheable step", i)
		}
		if keys[i].Reason != ReasonParentUncacheable {
			t.Errorf("step %d reason = %q, want %q", i, keys[i].Reason, ReasonParentUncacheable)
		}
		if keys[i].Key != "" || keys[i].ParentKey != "" {
			t.Errorf("step %d carries a key despite being uncacheable: %+v", i, keys[i])
		}
	}
}

func TestLayerKeysCacheDisabled(t *testing.T) {
	f := &Fusefile{Version: 1, Setup: []Step{{Run: "a"}, {Run: "b"}}}
	keys := keysOf(t, f, nil)
	for i, k := range keys {
		if k.Cacheable || k.Key != "" {
			t.Errorf("step %d is cacheable with caching off: %+v", i, k)
		}
		if k.Reason != ReasonCacheDisabled {
			t.Errorf("step %d reason = %q, want %q", i, k.Reason, ReasonCacheDisabled)
		}
	}
}

func TestLayerKeysGPUIsNeverCached(t *testing.T) {
	f := cached(Step{Run: "a"}, Step{Run: "b"})
	f.Resources.GPU = 1
	for i, k := range keysOf(t, f, nil) {
		if k.Cacheable {
			t.Errorf("step %d is cacheable on a gpu environment", i)
		}
		if k.Reason != ReasonGPU {
			t.Errorf("step %d reason = %q, want %q", i, k.Reason, ReasonGPU)
		}
	}
}

func TestLayerKeysCarryScriptAndIndex(t *testing.T) {
	keys := keysOf(t, cached(Step{Run: "a"}, Step{Run: "b"}), nil)
	for i, k := range keys {
		if k.Index != i {
			t.Errorf("key %d has index %d", i, k.Index)
		}
	}
	// the hashed script is exactly what compileStartupScript emits.
	scripts := setupScripts(cached(Step{Run: "a"}, Step{Run: "b"}))
	for i, s := range scripts {
		if keys[i].Script != s {
			t.Errorf("step %d script = %q, want %q", i, keys[i].Script, s)
		}
	}
}

// a workdir is part of the fragment the step emits, so adding one has to
// invalidate that step's layer: the same command in a different directory
// builds a different rootfs.
func TestLayerKeysWorkdirChangesTheKey(t *testing.T) {
	plain := keysOf(t, cached(Step{Run: "a"}), nil)
	scoped := keysOf(t, cached(Step{Run: "a", Workdir: "web"}), nil)

	if scoped[0].Script != "(cd 'web'; a)" {
		t.Errorf("script = %q, want the subshell form", scoped[0].Script)
	}
	if plain[0].Key == scoped[0].Key {
		t.Errorf("a workdir did not change the layer key")
	}
}

func TestLayerKeysNoSetup(t *testing.T) {
	keys, err := LayerKeys(cached(), nil, LayerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if keys != nil {
		t.Errorf("got %v, want nil for a Fusefile with no setup", keys)
	}
}

func TestLayerKeysExplicitBaseKeyOverridesImage(t *testing.T) {
	f := cached(Step{Run: "a"})
	fromImage := keysOf(t, f, nil)[0].Key

	keys, err := LayerKeys(f, nil, LayerOptions{BaseKey: "rootfs:abc", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if keys[0].ParentKey != "rootfs:abc" {
		t.Errorf("parent = %q, want the supplied base key", keys[0].ParentKey)
	}
	if keys[0].Key == fromImage {
		t.Errorf("supplying a base key did not change the derived key")
	}
}

func TestBaseKeyDistinguishesImages(t *testing.T) {
	if BaseKey("", nil) == BaseKey("base-ubuntu-24", nil) {
		t.Errorf("the baked base rootfs and a named image share a base key")
	}
	if !strings.HasPrefix(BaseKey("", nil), "image:") {
		t.Errorf("base key = %q, want an image: prefix", BaseKey("", nil))
	}
}

// files are written before the first setup step runs, so they are part of the
// state that step builds on: editing one must invalidate the whole chain, or a
// layer baked against the old contents would still be served.
func TestBaseKeyCoversFiles(t *testing.T) {
	img := "base-ubuntu-24"
	none := BaseKey(img, nil)
	one := BaseKey(img, []File{{Path: "app.conf", Content: "a"}})
	edited := BaseKey(img, []File{{Path: "app.conf", Content: "b"}})
	renamed := BaseKey(img, []File{{Path: "other.conf", Content: "a"}})

	if none == one {
		t.Errorf("adding a file did not change the base key")
	}
	if one == edited {
		t.Errorf("editing a file's content did not change the base key")
	}
	if one == renamed {
		t.Errorf("renaming a file did not change the base key")
	}
	if one != BaseKey(img, []File{{Path: "app.conf", Content: "a"}}) {
		t.Errorf("the same file block produced two different base keys")
	}
	// the path/content split is length-prefixed, so no two splits collide.
	if BaseKey(img, []File{{Path: "ab", Content: "c"}}) == BaseKey(img, []File{{Path: "a", Content: "bc"}}) {
		t.Errorf("a different path/content split produced the same base key")
	}
}
