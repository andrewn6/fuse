package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/folsomintel/fuse/internal/fusefile"
)

// writeCachedFusefile writes a Fusefile with caching enabled and a step that
// declares inputs, plus the input files themselves, and returns its path.
func writeCachedFusefile(t *testing.T, dir string) string {
	t.Helper()
	src := `version: 1
image: base-ubuntu-24
cache:
  enabled: true
setup:
  - apt-get update -qq
  - run: npm ci
    inputs:
      - package.json
run: ./start.sh
`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "Fusefile")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// planJSON runs `up --plan` in json mode and decodes the result.
func planJSON(t *testing.T, fusefilePath string, extra ...string) layerPlanJSON {
	t.Helper()
	return cmdPlanJSON(t, "up", fusefilePath, extra...)
}

// cmdPlanJSON runs `<cmd> --plan` in json mode and decodes the result. --plan
// must not reach the orchestrator on any command, so the server fails the test
// if it is called at all.
func cmdPlanJSON(t *testing.T, cmd, fusefilePath string, extra ...string) layerPlanJSON {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("server should not have been called: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	cfg := writeConfig(t, srv.URL)
	args := append([]string{"--config", cfg, "-o", "json", cmd, "-f", fusefilePath, "--plan"}, extra...)
	out, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs(args)
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var plan layerPlanJSON
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatalf("decode plan %q: %v", out, err)
	}
	return plan
}

func TestUpPlanPrintsKeysAndReportsMisses(t *testing.T) {
	plan := planJSON(t, writeCachedFusefile(t, t.TempDir()))

	if !plan.CacheEnabled {
		t.Errorf("cache_enabled = false, want true")
	}
	if plan.BaseKey != fusefile.BaseKey("base-ubuntu-24") {
		t.Errorf("base_key = %q", plan.BaseKey)
	}
	if plan.Arch == "" {
		t.Errorf("arch is empty")
	}
	if len(plan.Steps) != 2 {
		t.Fatalf("got %d steps, want 2", len(plan.Steps))
	}
	for i, s := range plan.Steps {
		if !s.Cacheable {
			t.Errorf("step %d not cacheable: %s", i, s.Reason)
		}
		// there is no layer store to look a key up in yet.
		if s.Status != "miss" {
			t.Errorf("step %d status = %q, want miss", i, s.Status)
		}
		if len(s.Key) != 64 {
			t.Errorf("step %d key = %q, want a hex sha256", i, s.Key)
		}
	}
	if plan.Steps[0].ParentKey != plan.BaseKey {
		t.Errorf("step 0 parent = %q, want the base key", plan.Steps[0].ParentKey)
	}
	if plan.Steps[1].ParentKey != plan.Steps[0].Key {
		t.Errorf("step 1 parent = %q, want step 0's key", plan.Steps[1].ParentKey)
	}
	if plan.Steps[1].InputsDigest == "" {
		t.Errorf("step 1 has no inputs digest")
	}
}

func TestUpPlanNoCacheDisablesCaching(t *testing.T) {
	plan := planJSON(t, writeCachedFusefile(t, t.TempDir()), "--no-cache")
	if plan.CacheEnabled {
		t.Errorf("cache_enabled = true with --no-cache")
	}
	for i, s := range plan.Steps {
		if s.Cacheable || s.Key != "" {
			t.Errorf("step %d is cacheable with --no-cache: %+v", i, s)
		}
		if s.Status != "skip" {
			t.Errorf("step %d status = %q, want skip", i, s.Status)
		}
	}
}

// editing an input file must move that step's key and every key after it.
func TestUpPlanInputEditInvalidatesSuffix(t *testing.T) {
	dir := t.TempDir()
	path := writeCachedFusefile(t, dir)
	before := planJSON(t, path)

	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"y"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	after := planJSON(t, path)

	if before.Steps[0].Key != after.Steps[0].Key {
		t.Errorf("step 0 key moved after an unrelated input changed")
	}
	if before.Steps[1].Key == after.Steps[1].Key {
		t.Errorf("step 1 key unchanged after its input changed")
	}
}

func TestUpPlanMissingInputFails(t *testing.T) {
	dir := t.TempDir()
	src := `version: 1
cache:
  enabled: true
setup:
  - run: npm ci
    inputs:
      - package.json
`
	path := filepath.Join(dir, "Fusefile")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := writeConfig(t, "http://127.0.0.1:1")

	root := newRootCmd()
	root.SetArgs([]string{"--config", cfg, "up", "-f", path, "--plan"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "no files matched") {
		t.Fatalf("want a no-files-matched error, got %v", err)
	}
}

func TestInputDigesterIsStableAndContentSensitive(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("a.txt", "a", 0o600)
	write("b.txt", "b", 0o600)

	digest := inputDigester(dir)
	first, err := digest([]string{"*.txt"})
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	second, err := digest([]string{"*.txt"})
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if first != second {
		t.Errorf("digest is not stable: %q vs %q", first, second)
	}

	// pattern order and overlap must not change the digest: the file set is
	// sorted and deduped.
	reordered, err := digest([]string{"b.txt", "a.txt", "*.txt"})
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if reordered != first {
		t.Errorf("digest depends on pattern order: %q vs %q", reordered, first)
	}

	write("a.txt", "a2", 0o600)
	changed, err := digest([]string{"*.txt"})
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if changed == first {
		t.Errorf("digest unchanged after a file's contents changed")
	}

	// the executable bit is part of the digest; the rest of the mode is not.
	write("a.txt", "a2", 0o600)
	baseline, _ := digest([]string{"*.txt"})
	if err := os.Chmod(filepath.Join(dir, "a.txt"), 0o644); err != nil {
		t.Fatal(err)
	}
	sameExec, _ := digest([]string{"*.txt"})
	if sameExec != baseline {
		t.Errorf("digest changed on a non-executable mode change")
	}
	if err := os.Chmod(filepath.Join(dir, "a.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	execSet, _ := digest([]string{"*.txt"})
	if execSet == baseline {
		t.Errorf("digest unchanged after the executable bit was set")
	}
}

func TestInputDigesterWalksDirectories(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(dir, "src", "nested", "x.go")
	if err := os.WriteFile(deep, []byte("package x"), 0o600); err != nil {
		t.Fatal(err)
	}

	digest := inputDigester(dir)
	before, err := digest([]string{"src"})
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if err := os.WriteFile(deep, []byte("package y"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := digest([]string{"src"})
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if before == after {
		t.Errorf("digest unchanged after a nested file changed")
	}
}

func TestInputDigesterRejectsUnmatchedAndNonRegular(t *testing.T) {
	dir := t.TempDir()
	digest := inputDigester(dir)

	if _, err := digest([]string{"nope.json"}); err == nil || !strings.Contains(err.Error(), "no files matched") {
		t.Errorf("want a no-files-matched error, got %v", err)
	}

	// a symlink can point outside the Fusefile's directory, so it is rejected
	// rather than silently resolved.
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := digest([]string{"link.txt"}); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("want a not-a-regular-file error, got %v", err)
	}
}

// `fuse build` is the command that actually runs the setup phase, so its plan
// must be the same plan `fuse up` derives from the same Fusefile: one algorithm,
// not two that can drift.
func TestBuildPlanMatchesUpPlan(t *testing.T) {
	path := writeCachedFusefile(t, t.TempDir())

	up := planJSON(t, path)
	build := cmdPlanJSON(t, "build", path)

	if !reflect.DeepEqual(up, build) {
		t.Errorf("build plan differs from up plan:\n build %+v\n up    %+v", build, up)
	}
}

func TestBuildPlanNoCacheDisablesCaching(t *testing.T) {
	plan := cmdPlanJSON(t, "build", writeCachedFusefile(t, t.TempDir()), "--no-cache")
	if plan.CacheEnabled {
		t.Errorf("cache_enabled = true with --no-cache")
	}
	for i, s := range plan.Steps {
		if s.Cacheable || s.Key != "" {
			t.Errorf("step %d is cacheable with --no-cache: %+v", i, s)
		}
	}
}

// a bad `inputs` entry must fail before a builder vm is booted, since a builder
// holds host capacity and the failure is knowable client-side.
func TestBuildFailsOnBadInputsBeforeCreating(t *testing.T) {
	dir := t.TempDir()
	src := `version: 1
cache:
  enabled: true
setup:
  - run: npm ci
    inputs:
      - package.json
`
	path := filepath.Join(dir, "Fusefile")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("server should not have been called: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	cfg := writeConfig(t, srv.URL)

	root := newRootCmd()
	root.SetArgs([]string{"--config", cfg, "build", "-f", path})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "no files matched") {
		t.Fatalf("want a no-files-matched error, got %v", err)
	}
}

// a --from-build boot runs no setup steps, so the two flags that govern the
// setup layer cache have nothing to act on and are rejected rather than
// silently ignored.
func TestUpFromBuildRejectsCacheFlags(t *testing.T) {
	path := writeCachedFusefile(t, t.TempDir())
	cfg := writeConfig(t, "http://127.0.0.1:1")

	for _, flag := range []string{"--plan", "--no-cache"} {
		t.Run(flag, func(t *testing.T) {
			root := newRootCmd()
			root.SetArgs([]string{"--config", cfg, "up", "-f", path, "--from-build", "snap-1", flag})
			err := root.Execute()
			// the message must name the flag: the Fusefile also sets `image`,
			// which is separately incompatible with --from-build, and passing
			// that check off as this one would hide a regression.
			want := flag + " and --from-build are mutually exclusive"
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("want %q, got %v", want, err)
			}
		})
	}
}
