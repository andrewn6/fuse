package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile writes body to dir/name and returns the path.
func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// runValidate runs `fuse validate` with args appended, against a config that
// points at a server which fails the test if it is ever called, and returns
// stdout plus the exit code the command set.
func runValidate(t *testing.T, args ...string) (string, int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("validate must not make a request: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	cfg := writeConfig(t, srv.URL)
	app.exitCode = 0
	out, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs(append([]string{"--config", cfg}, args...))
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	return out, app.exitCode
}

const validFusefile = `version: 1
resources:
  cpus: 2
  memory: 2GB
  storage: 10GB
  max_runtime: 1h
services:
  db:
    image: postgres:16
    env:
      PGPASSWORD:
        secret: pg_password
run: ./start.sh
`

func TestValidateAcceptsValidFusefile(t *testing.T) {
	path := writeFile(t, t.TempDir(), "Fusefile", validFusefile)
	out, code := runValidate(t, "validate", path)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stdout: %s)", code, out)
	}
	if out != "" {
		t.Errorf("valid file should print nothing to stdout, got %q", out)
	}
}

// one invalid fixture per error class: yaml syntax, strict-decode (unknown
// field), structural validation, and compile.
func TestValidateReportsErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "yaml syntax",
			body: "version: 1\nresources:\n  cpus: [1\n",
			want: []string{"parse fusefile"},
		},
		{
			name: "unknown field",
			body: "version: 1\nnope: true\n",
			want: []string{"field nope not found"},
		},
		{
			name: "wrong version",
			body: "version: 2\n",
			want: []string{"version: must be 1"},
		},
		{
			name: "service without image",
			body: "version: 1\nservices:\n  db: {}\n",
			want: []string{"services.db: image is required"},
		},
		{
			name: "env value and secret",
			body: "version: 1\nservices:\n  db:\n    image: postgres:16\n    env:\n      A:\n        value: x\n        secret: y\n",
			want: []string{"services.db.env.A: value and secret are mutually exclusive"},
		},
		{
			name: "expose port out of range",
			body: "version: 1\nexpose:\n  - port: 99999\n",
			want: []string{"expose[0].port: must be between 1 and 65535"},
		},
		{
			name: "bad memory size",
			body: "version: 1\nresources:\n  memory: 2 gigs\n",
			want: []string{`resources.memory: invalid size "2 gigs"`},
		},
		{
			name: "bad max_runtime",
			body: "version: 1\nresources:\n  max_runtime: forever\n",
			want: []string{"resources.max_runtime:"},
		},
		{
			name: "invalid gpu profile",
			body: "version: 1\nresources:\n  gpu: 1\n  gpu_profile: half\n",
			want: []string{"resources.gpu_profile: invalid MIG profile"},
		},
		{
			name: "build and setup together",
			body: "version: 1\nbuild:\n  - npm ci\nsetup:\n  - npm test\n",
			want: []string{"setup is a deprecated alias for build"},
		},
		{
			// structural and compile problems are reported together in one
			// pass; `fuse up` stops after the structural batch.
			name: "structural and compile together",
			body: "version: 2\nresources:\n  memory: nope\n",
			want: []string{"version: must be 1", `resources.memory: invalid size "nope"`},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeFile(t, t.TempDir(), "Fusefile", c.body)
			out, code := runValidate(t, "validate", path)
			if code != 1 {
				t.Fatalf("exit code = %d, want 1 (stdout: %s)", code, out)
			}
			for _, want := range c.want {
				if !strings.Contains(out, want) {
					t.Errorf("stdout missing %q:\n%s", want, out)
				}
			}
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				if !strings.HasPrefix(line, path+":") {
					t.Errorf("diagnostic %q is not prefixed with the file path", line)
				}
			}
		})
	}
}

func TestValidateMissingFileExitsTwo(t *testing.T) {
	out, code := runValidate(t, "validate", filepath.Join(t.TempDir(), "absent"))
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if out != "" {
		t.Errorf("io errors belong on stderr, got stdout %q", out)
	}
}

func TestValidateQuietPrintsNothing(t *testing.T) {
	path := writeFile(t, t.TempDir(), "Fusefile", "version: 2\n")
	out, code := runValidate(t, "validate", path, "--quiet")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if out != "" {
		t.Errorf("--quiet should print nothing, got %q", out)
	}
}

func TestValidateJSONOutput(t *testing.T) {
	path := writeFile(t, t.TempDir(), "Fusefile", "version: 2\nresources:\n  memory: nope\n")
	out, code := runValidate(t, "-o", "json", "validate", path)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	var got validateReport
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode json: %v (%s)", err, out)
	}
	if got.Path != path || got.Valid {
		t.Errorf("report = %+v, want path %q and valid false", got, path)
	}
	if len(got.Errors) != 2 {
		t.Fatalf("errors = %+v, want 2", got.Errors)
	}
	if got.Errors[0].Path != "version" || got.Errors[0].Message != "must be 1" {
		t.Errorf("errors[0] = %+v, want path version", got.Errors[0])
	}
	if got.Errors[1].Path != "resources.memory" {
		t.Errorf("errors[1] = %+v, want path resources.memory", got.Errors[1])
	}
}

func TestValidateJSONValidReportsRequiredSecrets(t *testing.T) {
	path := writeFile(t, t.TempDir(), "Fusefile", validFusefile)
	out, code := runValidate(t, "-o", "json", "validate", path)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (%s)", code, out)
	}
	var got validateReport
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode json: %v (%s)", err, out)
	}
	if !got.Valid || len(got.Errors) != 0 {
		t.Errorf("report = %+v, want valid with no errors", got)
	}
	if len(got.RequiredSecrets) != 1 || got.RequiredSecrets[0] != "pg_password" {
		t.Errorf("required_secrets = %v, want [pg_password]", got.RequiredSecrets)
	}
}

// a missing secret is informational by default so validate can run where no
// secrets exist, and an error only under --check-secrets.
func TestValidateCheckSecrets(t *testing.T) {
	path := writeFile(t, t.TempDir(), "Fusefile", validFusefile)

	if _, code := runValidate(t, "validate", path); code != 0 {
		t.Errorf("without --check-secrets: exit code = %d, want 0", code)
	}

	out, code := runValidate(t, "validate", path, "--check-secrets")
	if code != 1 {
		t.Fatalf("with --check-secrets: exit code = %d, want 1", code)
	}
	if !strings.Contains(out, `required secret "pg_password" is not set`) {
		t.Errorf("stdout missing the missing-secret diagnostic:\n%s", out)
	}

	if _, code := runValidate(t, "validate", path, "--check-secrets", "--secret", "pg_password=shh"); code != 0 {
		t.Errorf("with the secret supplied: exit code = %d, want 0", code)
	}
}

// -f, the positional argument, and the ./Fusefile default resolve the same way
// they do for `up` and `init`.
func TestValidateResolvesDefaultPath(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Fusefile", validFusefile)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }()

	if _, code := runValidate(t, "validate"); code != 0 {
		t.Errorf("default path: exit code = %d, want 0", code)
	}
	if _, code := runValidate(t, "validate", "-f", "Fusefile"); code != 0 {
		t.Errorf("-f: exit code = %d, want 0", code)
	}
}

// validate runs before `fuse connect`, so an absent config file is not an error.
func TestValidateWorksWithoutConfig(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "Fusefile", validFusefile)

	app.exitCode = 0
	_, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", filepath.Join(dir, "no-config.yaml"), "validate", path})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if app.exitCode != 0 {
		t.Errorf("exit code = %d, want 0", app.exitCode)
	}
}
