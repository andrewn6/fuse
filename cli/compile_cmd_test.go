package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	fuse "github.com/folsomintel/fuse/sdks/go"
)

// -update rewrites the golden files instead of comparing against them.
var updateGolden = flag.Bool("update", false, "rewrite compile golden files")

// fixtureFusefile is the checked-in Fusefile the golden tests compile. its
// parent directory name ("compile") is also the default task id.
const fixtureFusefile = "testdata/compile/Fusefile"

// runCompile executes `fuse compile` with args appended, capturing stdout.
// the config path points into a temp dir with no config file, so the command
// runs with no context configured at all.
func runCompile(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(append([]string{"--config", filepath.Join(t.TempDir(), "config.yaml"), "compile"}, args...))
	err := root.Execute()
	return buf.String(), err
}

func TestCompileGolden(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		golden string
	}{
		{"text", []string{"-f", fixtureFusefile}, "testdata/compile/text.golden"},
		{"json", []string{"-f", fixtureFusefile, "--format", "json"}, "testdata/compile/json.golden"},
		{"yaml", []string{"-f", fixtureFusefile, "--format", "yaml"}, "testdata/compile/yaml.golden"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := runCompile(t, c.args...)
			if err != nil {
				t.Fatalf("execute: %v (output %s)", err, out)
			}
			if *updateGolden {
				if err := os.WriteFile(c.golden, []byte(out), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(c.golden)
			if err != nil {
				t.Fatalf("read golden (run with -update to create): %v", err)
			}
			if out != string(want) {
				t.Errorf("output differs from %s\n--- got ---\n%s\n--- want ---\n%s", c.golden, out, want)
			}
			// compiling twice must be byte-identical: the output is meant to
			// be diffed across Fusefile revisions.
			again, err := runCompile(t, c.args...)
			if err != nil {
				t.Fatalf("second execute: %v", err)
			}
			if again != out {
				t.Errorf("output not deterministic\nfirst:\n%s\nsecond:\n%s", out, again)
			}
		})
	}
}

// compile is client-side only: it must work with no context configured and
// must never reach the orchestrator.
func TestCompileMakesNoRequest(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		t.Errorf("unexpected request to %s", r.URL.Path)
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := writeConfig(t, srv.URL)
	var buf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&buf)
	root.SetArgs([]string{"--config", cfg, "compile", "-f", fixtureFusefile})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if hits != 0 {
		t.Errorf("compile made %d http requests, want 0", hits)
	}
	if !strings.Contains(buf.String(), "startup script") {
		t.Errorf("output missing startup script section: %s", buf.String())
	}
}

// the json form is the create body `fuse up` would post, minus secret values.
func TestCompileJSONMatchesWireBody(t *testing.T) {
	out, err := runCompile(t, "-f", fixtureFusefile, "--format", "json")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("json output does not parse: %v\n%s", err, out)
	}
	if got["task_id"] != "compile" {
		t.Errorf("task_id = %v, want compile", got["task_id"])
	}
	spec, ok := got["spec"].(map[string]any)
	if !ok {
		t.Fatalf("spec missing: %v", got)
	}
	for key, want := range map[string]float64{"cpus": 2, "ram_mb": 2048, "storage_gb": 10, "max_runtime_seconds": 3600} {
		if spec[key] != want {
			t.Errorf("spec.%s = %v, want %v", key, spec[key], want)
		}
	}
	// secrets is present and empty: values are supplied at `fuse up`.
	secrets, ok := got["secrets"].(map[string]any)
	if !ok {
		t.Fatalf("secrets missing from wire body: %v", got)
	}
	if len(secrets) != 0 {
		t.Errorf("secrets should be empty, got %v", secrets)
	}
	// required secret names are listed, sorted.
	names, ok := got["required_secrets"].([]any)
	if !ok || len(names) != 2 || names[0] != "api_token" || names[1] != "pg_password" {
		t.Errorf("required_secrets = %v, want [api_token pg_password]", got["required_secrets"])
	}
	// no credential field ever appears in compiled output.
	for _, forbidden := range []string{"gateway_token", "gateway_url"} {
		if _, ok := got[forbidden]; ok {
			t.Errorf("compiled output carries %s", forbidden)
		}
	}
	// manifest_inline decodes to the manifest json the guest receives.
	inline, _ := got["manifest_inline"].(string)
	raw, err := base64.StdEncoding.DecodeString(inline)
	if err != nil {
		t.Fatalf("manifest_inline is not base64: %v", err)
	}
	if !strings.Contains(string(raw), `"postgres"`) {
		t.Errorf("decoded manifest missing service: %s", raw)
	}
	// a secret reference is carried by name only, never a value.
	if !strings.Contains(string(raw), `"secret":"pg_password"`) {
		t.Errorf("decoded manifest missing secret reference: %s", raw)
	}
}

func TestCompileOnlyParts(t *testing.T) {
	manifest, err := runCompile(t, "-f", fixtureFusefile, "--only", "manifest")
	if err != nil {
		t.Fatalf("only manifest: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(manifest), &m); err != nil {
		t.Fatalf("--only manifest is not bare json: %v\n%s", err, manifest)
	}
	if _, ok := m["services"]; !ok {
		t.Errorf("manifest missing services: %s", manifest)
	}

	script, err := runCompile(t, "-f", fixtureFusefile, "--only", "startup-script")
	if err != nil {
		t.Fatalf("only startup-script: %v", err)
	}
	if !strings.HasPrefix(script, "set -eu\n") {
		t.Errorf("--only startup-script should be the bare script: %q", script)
	}
	if !strings.Contains(script, "./start.sh") {
		t.Errorf("script missing run command: %q", script)
	}

	secrets, err := runCompile(t, "-f", fixtureFusefile, "--only", "secrets")
	if err != nil {
		t.Fatalf("only secrets: %v", err)
	}
	if secrets != "api_token\npg_password\n" {
		t.Errorf("--only secrets = %q", secrets)
	}

	expose, err := runCompile(t, "-f", fixtureFusefile, "--only", "expose")
	if err != nil {
		t.Fatalf("only expose: %v", err)
	}
	if expose != "8080 http\n" {
		t.Errorf("--only expose = %q", expose)
	}

	spec, err := runCompile(t, "-f", fixtureFusefile, "--only", "spec")
	if err != nil {
		t.Fatalf("only spec: %v", err)
	}
	if !strings.Contains(spec, `"ram_mb": 2048`) {
		t.Errorf("--only spec = %s", spec)
	}
}

// --from-build must compile to what `fuse up --from-build` posts: the seed
// snapshot set and the run phase alone, since the artifact already carries the
// setup phase's result.
func TestCompileFromBuild(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "Fusefile")
	fusefile := "version: 1\n\nresources:\n  cpus: 1\n  memory: 512MB\n\nsetup:\n  - apt-get update -qq\n\nrun: ./start.sh\n"
	if err := os.WriteFile(target, []byte(fusefile), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runCompile(t, "-f", target, "--task-id", "seeded", "--from-build", "snap-1", "--format", "json")
	if err != nil {
		t.Fatalf("compile --from-build: %v (output %s)", err, out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("json output does not parse: %v\n%s", err, out)
	}
	if got["seed_snapshot_id"] != "snap-1" {
		t.Errorf("seed_snapshot_id = %v, want snap-1", got["seed_snapshot_id"])
	}
	script, _ := got["startup_script"].(string)
	if strings.Contains(script, "apt-get update") {
		t.Errorf("--from-build should skip the setup phase: %q", script)
	}
	if !strings.Contains(script, "./start.sh") {
		t.Errorf("startup script missing run command: %q", script)
	}

	// without the flag the field is omitted entirely and setup is replayed.
	plain, err := runCompile(t, "-f", target, "--task-id", "seeded", "--format", "json")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if strings.Contains(plain, "seed_snapshot_id") {
		t.Errorf("seed_snapshot_id should be omitted without --from-build: %s", plain)
	}

	// the text view names the seed so a preview is not silently ambiguous.
	text, err := runCompile(t, "-f", target, "--task-id", "seeded", "--from-build", "snap-1")
	if err != nil {
		t.Fatalf("compile text: %v", err)
	}
	if !strings.Contains(text, "seed snapshot  snap-1") {
		t.Errorf("text output missing seed snapshot line:\n%s", text)
	}

	// a build artifact and `image` both name the rootfs, so refuse both.
	withImage := filepath.Join(dir, "Fusefile.image")
	if err := os.WriteFile(withImage, []byte("version: 1\n\nimage: ghcr.io/acme/worker:latest\n\nrun: ./start.sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runCompile(t, "-f", withImage, "--from-build", "snap-1"); err == nil ||
		!strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("want mutually-exclusive error, got %v", err)
	}
}

func TestCompileFlagErrors(t *testing.T) {
	if _, err := runCompile(t, "-f", fixtureFusefile, "--only", "bogus"); err == nil ||
		!strings.Contains(err.Error(), "invalid --only") {
		t.Errorf("want invalid --only error, got %v", err)
	}
	if _, err := runCompile(t, "-f", fixtureFusefile, "--format", "toml"); err == nil ||
		!strings.Contains(err.Error(), "invalid --format") {
		t.Errorf("want invalid --format error, got %v", err)
	}
	// two format flags on one command is a footgun, so refuse the combination.
	if _, err := runCompile(t, "-f", fixtureFusefile, "--only", "spec", "--format", "json"); err == nil ||
		!strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("want mutually-exclusive error, got %v", err)
	}
	if _, err := runCompile(t, "-f", filepath.Join(t.TempDir(), "nope")); err == nil ||
		!strings.Contains(err.Error(), "read ") {
		t.Errorf("want read error, got %v", err)
	}
}

// the persistent -o/--output selects the format when --format is absent.
func TestCompileHonoursOutputFlag(t *testing.T) {
	var buf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&buf)
	root.SetArgs([]string{"--config", filepath.Join(t.TempDir(), "config.yaml"), "-o", "json",
		"compile", "-f", fixtureFusefile})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("-o json did not produce json: %v\n%s", err, buf.String())
	}
}

// the `fuse init` scaffold must compile and render.
func TestCompileInitScaffold(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "Fusefile")
	if err := os.WriteFile(target, []byte(initScaffold), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runCompile(t, "-f", target, "--task-id", "scaffold")
	if err != nil {
		t.Fatalf("compile scaffold: %v", err)
	}
	for _, want := range []string{"task id  scaffold", "ram mb", "startup script", "manifest", "expose", "pg_password"} {
		if !strings.Contains(out, want) {
			t.Errorf("scaffold output missing %q:\n%s", want, out)
		}
	}
}

// compiledRequest and compiledSpec exist only to add yaml tags to the sdk wire
// types. this pins them to the originals so a new field on fuse.CreateRequest
// or fuse.Spec cannot silently go unprinted.
func TestCompiledRequestMirrorsWireTypes(t *testing.T) {
	// gateway fields carry a credential and are not compiled from a Fusefile.
	skipped := map[string]bool{"gateway_url": true, "gateway_token": true}
	// required_secrets is compile-only, not part of the wire body.
	extra := map[string]bool{"required_secrets": true}

	assertMirror(t, "spec", reflect.TypeFor[fuse.Spec](), reflect.TypeFor[compiledSpec](), nil, nil)
	assertMirror(t, "request", reflect.TypeFor[fuse.CreateRequest](), reflect.TypeFor[compiledRequest](), skipped, extra)
}

func assertMirror(t *testing.T, label string, want, got reflect.Type, skipped, extra map[string]bool) {
	t.Helper()
	gotTags := map[string]bool{}
	for i := 0; i < got.NumField(); i++ {
		name := jsonName(got.Field(i).Tag.Get("json"))
		gotTags[name] = true
		if got.Field(i).Tag.Get("yaml") == "" {
			t.Errorf("%s: field %s has no yaml tag", label, got.Field(i).Name)
		}
	}
	for i := 0; i < want.NumField(); i++ {
		name := jsonName(want.Field(i).Tag.Get("json"))
		if skipped[name] {
			continue
		}
		if !gotTags[name] {
			t.Errorf("%s: %s.%s (json %q) is missing from the compile output type",
				label, want.Name(), want.Field(i).Name, name)
		}
		delete(gotTags, name)
	}
	for name := range gotTags {
		if !extra[name] {
			t.Errorf("%s: compile output type has unknown field %q", label, name)
		}
	}
}

func jsonName(tag string) string {
	if i := strings.Index(tag, ","); i >= 0 {
		return tag[:i]
	}
	return tag
}
