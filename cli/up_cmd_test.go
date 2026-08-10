package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFusefile writes a minimal valid Fusefile with a required secret (via
// both a service env reference and the top-level secrets list) to dir and
// returns its path.
func writeFusefile(t *testing.T, dir string) string {
	t.Helper()
	src := `version: 1
resources:
  memory: 2GB
services:
  db:
    image: postgres:16
    env:
      PGPASSWORD:
        secret: pg_password
run: ./start.sh
secrets:
  - pg_password
`
	path := filepath.Join(dir, "Fusefile")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUpCreatesEnvironmentFromFusefile(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/environments" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("auth header = %q, want Bearer test-token", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		fmt.Fprint(w, `{"id":"vm1","state":"pending","task_id":"t","url":"","spec":{}}`)
	}))
	defer srv.Close()

	fusefilePath := writeFusefile(t, t.TempDir())
	cfg := writeConfig(t, srv.URL)

	out, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{
			"--config", cfg, "-o", "json",
			"up", "-f", fusefilePath,
			"--task-id", "t",
			"--secret", "pg_password=shh",
			"--no-wait",
		})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, `"vm1"`) {
		t.Errorf("output missing environment id: %s", out)
	}

	if gotBody == nil {
		t.Fatalf("server was never called")
	}

	spec, ok := gotBody["spec"].(map[string]any)
	if !ok {
		t.Fatalf("spec missing or wrong type: %v", gotBody["spec"])
	}
	if ramMB, _ := spec["ram_mb"].(float64); ramMB != 2048 {
		t.Errorf("spec.ram_mb = %v, want 2048", spec["ram_mb"])
	}

	manifestInline, _ := gotBody["manifest_inline"].(string)
	manifestJSON, err := base64.StdEncoding.DecodeString(manifestInline)
	if err != nil {
		t.Fatalf("manifest_inline is not valid base64: %v", err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(manifestJSON, &manifest); err != nil {
		t.Fatalf("manifest_inline did not decode to json: %v", err)
	}
	if manifest["version"] != "1" {
		t.Errorf("manifest version = %v, want %q", manifest["version"], "1")
	}
	machine, ok := manifest["machine"].(map[string]any)
	if !ok || machine["workspace"] != "/workspace" {
		t.Errorf("manifest missing machine.workspace: %v", manifest["machine"])
	}

	secrets, ok := gotBody["secrets"].(map[string]any)
	if !ok {
		t.Fatalf("secrets missing or wrong type: %v", gotBody["secrets"])
	}
	if secrets["pg_password"] != "shh" {
		t.Errorf("secrets.pg_password = %v, want shh", secrets["pg_password"])
	}
}

// writeGPUFusefile writes a minimal Fusefile requesting a gpu (no secrets) and
// returns its path.
func writeGPUFusefile(t *testing.T, dir string) string {
	t.Helper()
	src := `version: 1
resources:
  memory: 2GB
  gpu: 1
  gpu_kind: a100
run: ./start.sh
`
	path := filepath.Join(dir, "Fusefile")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUpSendsGPUSpec(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/environments" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		fmt.Fprint(w, `{"id":"vm1","state":"pending","task_id":"t","url":"","spec":{}}`)
	}))
	defer srv.Close()

	fusefilePath := writeGPUFusefile(t, t.TempDir())
	cfg := writeConfig(t, srv.URL)

	_, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{
			"--config", cfg, "-o", "json",
			"up", "-f", fusefilePath,
			"--task-id", "t",
			"--no-wait",
		})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if gotBody == nil {
		t.Fatalf("server was never called")
	}

	spec, ok := gotBody["spec"].(map[string]any)
	if !ok {
		t.Fatalf("spec missing or wrong type: %v", gotBody["spec"])
	}
	if gpus, _ := spec["gpus"].(float64); gpus != 1 {
		t.Errorf("spec.gpus = %v, want 1", spec["gpus"])
	}
	if spec["gpu_kind"] != "a100" {
		t.Errorf("spec.gpu_kind = %v, want a100", spec["gpu_kind"])
	}
}

// a startup script that blows its ceiling arrives as a bare invalid_argument,
// which said nothing about what to do next.
func TestUpStartupScriptTimeoutIsExplained(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"code":"invalid_argument","message":"startup script did not complete in time after 30s: a startup script must terminate; background long-lived processes or declare them under `+"`services`"+`"}}`)
	}))
	defer srv.Close()

	fusefilePath := writeGPUFusefile(t, t.TempDir())
	cfg := writeConfig(t, srv.URL)

	root := newRootCmd()
	root.SetArgs([]string{"--config", cfg, "up", "-f", fusefilePath, "--task-id", "t", "--no-wait"})
	err := root.Execute()
	if err == nil {
		t.Fatal("want an error for a startup script timeout")
	}
	// the ceiling the orchestrator actually applied is carried in its own
	// message; the cli adds what to do about it.
	for _, want := range []string{"after 30s", "startup_timeout", "fuse build", "--from-build"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

func TestUpStartupScriptTimeoutTooLargeIsExplained(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"code":"invalid_argument","message":"startup script timeout exceeds the configured maximum: requested 5m0s, maximum 55s"}}`)
	}))
	defer srv.Close()

	fusefilePath := writeGPUFusefile(t, t.TempDir())
	cfg := writeConfig(t, srv.URL)

	root := newRootCmd()
	root.SetArgs([]string{"--config", cfg, "up", "-f", fusefilePath, "--task-id", "t", "--no-wait"})
	err := root.Execute()
	if err == nil {
		t.Fatal("want an error for an over-large startup timeout")
	}
	if !strings.Contains(err.Error(), "maximum 55s") || !strings.Contains(err.Error(), "fuse build") {
		t.Errorf("error missing the ceiling or the remediation:\n%v", err)
	}
}

// --dry-run prints the request and makes no request of its own.
func TestUpDryRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("--dry-run must not call the orchestrator: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	fusefilePath := writeFusefile(t, t.TempDir())
	cfg := writeConfig(t, srv.URL)

	out, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{
			"--config", cfg,
			"up", "-f", fusefilePath,
			"--task-id", "t",
			"--secret", "pg_password=shh",
			"--dry-run",
		})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	// the shared `fuse compile` renderer, so the two cannot drift.
	if !strings.Contains(out, "task id  t") || !strings.Contains(out, "ram mb") {
		t.Errorf("dry run did not print the compiled request:\n%s", out)
	}
}

// -o json makes the dry run emit the wire body itself, so it can be diffed or
// posted by hand.
func TestUpDryRunJSON(t *testing.T) {
	fusefilePath := writeFusefile(t, t.TempDir())
	cfg := writeConfig(t, "http://127.0.0.1:1")

	out, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{
			"--config", cfg, "-o", "json",
			"up", "-f", fusefilePath,
			"--task-id", "t",
			"--secret", "pg_password=shh",
			"--dry-run",
		})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var req map[string]any
	if err := json.Unmarshal([]byte(out), &req); err != nil {
		t.Fatalf("dry run output is not json: %v\n%s", err, out)
	}
	if req["task_id"] != "t" {
		t.Errorf("task_id = %v, want t", req["task_id"])
	}
	// secret values are never printed, only the names the create would need.
	secrets, _ := req["secrets"].(map[string]any)
	if len(secrets) != 0 {
		t.Errorf("secrets = %v, want it empty", secrets)
	}
}

// the secret gate is client-side, so a dry run still catches a missing secret.
func TestUpDryRunAppliesTheSecretGate(t *testing.T) {
	fusefilePath := writeFusefile(t, t.TempDir())
	cfg := writeConfig(t, "http://127.0.0.1:1")

	root := newRootCmd()
	root.SetArgs([]string{"--config", cfg, "up", "-f", fusefilePath, "--task-id", "t", "--dry-run"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "missing required secrets") {
		t.Fatalf("want missing required secrets error, got %v", err)
	}
}

func TestUpDryRunRejectsPlan(t *testing.T) {
	fusefilePath := writeFusefile(t, t.TempDir())
	cfg := writeConfig(t, "http://127.0.0.1:1")

	root := newRootCmd()
	root.SetArgs([]string{"--config", cfg, "up", "-f", fusefilePath, "--dry-run", "--plan"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("want a mutual-exclusion error, got %v", err)
	}
}

// a derived task id is announced before the create, since the Fusefile has no
// task id field and the derivation is not obvious from the command line.
func TestUpReportsDerivedTaskID(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		fmt.Fprint(w, `{"id":"vm1","state":"pending","task_id":"t","url":"","spec":{}}`)
	}))
	defer srv.Close()

	dir := filepath.Join(t.TempDir(), "my-service")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fusefilePath := writeGPUFusefile(t, dir)
	cfg := writeConfig(t, srv.URL)

	_, stderr, err := captureBoth(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", cfg, "-o", "json", "up", "-f", fusefilePath, "--no-wait"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if gotBody["task_id"] != "my-service" {
		t.Errorf("task_id = %v, want my-service", gotBody["task_id"])
	}
	if !strings.Contains(stderr, `using "my-service"`) {
		t.Errorf("stderr does not report the derived task id:\n%s", stderr)
	}

	// an explicit --task-id was not derived, so there is nothing to report.
	_, stderr, err = captureBoth(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", cfg, "-o", "json", "up", "-f", fusefilePath, "--task-id", "explicit", "--no-wait"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if strings.Contains(stderr, "derived from") {
		t.Errorf("stderr reports a derived task id despite --task-id:\n%s", stderr)
	}
}

// up reaches parity with `fuse environment create`: the gateway is settable
// from the Fusefile path too.
func TestUpSendsGatewayFlags(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		fmt.Fprint(w, `{"id":"vm1","state":"pending","task_id":"t","url":"","spec":{}}`)
	}))
	defer srv.Close()

	fusefilePath := writeGPUFusefile(t, t.TempDir())
	cfg := writeConfig(t, srv.URL)

	_, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{
			"--config", cfg, "-o", "json",
			"up", "-f", fusefilePath,
			"--task-id", "t",
			"--gateway-url", "https://gw.internal",
			"--gateway-token", "gw-token",
			"--no-wait",
		})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if gotBody["gateway_url"] != "https://gw.internal" {
		t.Errorf("gateway_url = %v, want https://gw.internal", gotBody["gateway_url"])
	}
	if gotBody["gateway_token"] != "gw-token" {
		t.Errorf("gateway_token = %v, want gw-token", gotBody["gateway_token"])
	}
}

// with no gateway flags the fields stay off the wire, so a Fusefile that never
// mentioned a gateway posts exactly what it posted before.
func TestUpWithoutGatewayOmitsTheFields(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		fmt.Fprint(w, `{"id":"vm1","state":"pending","task_id":"t","url":"","spec":{}}`)
	}))
	defer srv.Close()

	fusefilePath := writeGPUFusefile(t, t.TempDir())
	cfg := writeConfig(t, srv.URL)

	_, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{
			"--config", cfg, "-o", "json",
			"up", "-f", fusefilePath, "--task-id", "t", "--no-wait",
		})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if _, present := gotBody["gateway_url"]; present {
		t.Errorf("gateway_url present (%v), want it omitted", gotBody["gateway_url"])
	}
	if _, present := gotBody["gateway_token"]; present {
		t.Errorf("gateway_token present (%v), want it omitted", gotBody["gateway_token"])
	}
}

func TestUpMissingRequiredSecretFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("server should not have been called: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	fusefilePath := writeFusefile(t, t.TempDir())
	cfg := writeConfig(t, srv.URL)

	root := newRootCmd()
	root.SetArgs([]string{
		"--config", cfg,
		"up", "-f", fusefilePath,
		"--task-id", "t",
		"--no-wait",
	})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "missing required secrets") {
		t.Fatalf("want missing required secrets error, got %v", err)
	}
}

// an empty value does not satisfy a required secret, so a typo'd
// `--secret name=` fails the gate rather than booting with a blank credential.
func TestUpEmptySecretValueFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("server should not have been called: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	fusefilePath := writeFusefile(t, t.TempDir())
	cfg := writeConfig(t, srv.URL)

	root := newRootCmd()
	root.SetArgs([]string{
		"--config", cfg,
		"up", "-f", fusefilePath,
		"--task-id", "t",
		"--secret", "pg_password=",
		"--no-wait",
	})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "missing required secrets: pg_password") {
		t.Fatalf("want missing required secrets error, got %v", err)
	}
	if !strings.Contains(err.Error(), "--allow-empty-secrets") {
		t.Errorf("error does not mention the opt-out flag: %v", err)
	}
}

// --allow-empty-secrets is the escape hatch for a genuinely empty credential.
func TestUpAllowEmptySecrets(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		fmt.Fprint(w, `{"id":"vm1","state":"pending","task_id":"t","url":"","spec":{}}`)
	}))
	defer srv.Close()

	fusefilePath := writeFusefile(t, t.TempDir())
	cfg := writeConfig(t, srv.URL)

	_, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{
			"--config", cfg, "-o", "json",
			"up", "-f", fusefilePath,
			"--task-id", "t",
			"--secret", "pg_password=",
			"--allow-empty-secrets",
			"--no-wait",
		})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	secrets, ok := gotBody["secrets"].(map[string]any)
	if !ok {
		t.Fatalf("secrets missing or wrong type: %v", gotBody["secrets"])
	}
	if secrets["pg_password"] != "" {
		t.Errorf("secrets.pg_password = %v, want the empty string", secrets["pg_password"])
	}
}

func TestUpSecretsFile(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/environments" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		fmt.Fprint(w, `{"id":"vm1","state":"pending","task_id":"t","url":"","spec":{}}`)
	}))
	defer srv.Close()

	// Write secrets file with comment line, blank line, and real entry.
	tmpdir := t.TempDir()
	secretsPath := filepath.Join(tmpdir, "secrets.txt")
	secretsContent := "# database password\n\npg_password=fromfile\n"
	if err := os.WriteFile(secretsPath, []byte(secretsContent), 0o600); err != nil {
		t.Fatal(err)
	}

	fusefilePath := writeFusefile(t, tmpdir)
	cfg := writeConfig(t, srv.URL)

	out, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{
			"--config", cfg, "-o", "json",
			"up", "-f", fusefilePath,
			"--task-id", "t",
			"--secrets-file", secretsPath,
			"--no-wait",
		})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, `"vm1"`) {
		t.Errorf("output missing environment id: %s", out)
	}

	if gotBody == nil {
		t.Fatalf("server was never called")
	}

	secrets, ok := gotBody["secrets"].(map[string]any)
	if !ok {
		t.Fatalf("secrets missing or wrong type: %v", gotBody["secrets"])
	}
	if secrets["pg_password"] != "fromfile" {
		t.Errorf("secrets.pg_password = %v, want fromfile", secrets["pg_password"])
	}
}

func TestUpSecretFlagOverridesFile(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/environments" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		fmt.Fprint(w, `{"id":"vm1","state":"pending","task_id":"t","url":"","spec":{}}`)
	}))
	defer srv.Close()

	// Write secrets file with pg_password=fromfile.
	tmpdir := t.TempDir()
	secretsPath := filepath.Join(tmpdir, "secrets.txt")
	if err := os.WriteFile(secretsPath, []byte("pg_password=fromfile\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	fusefilePath := writeFusefile(t, tmpdir)
	cfg := writeConfig(t, srv.URL)

	out, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{
			"--config", cfg, "-o", "json",
			"up", "-f", fusefilePath,
			"--task-id", "t",
			"--secrets-file", secretsPath,
			"--secret", "pg_password=fromflag",
			"--no-wait",
		})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, `"vm1"`) {
		t.Errorf("output missing environment id: %s", out)
	}

	if gotBody == nil {
		t.Fatalf("server was never called")
	}

	secrets, ok := gotBody["secrets"].(map[string]any)
	if !ok {
		t.Fatalf("secrets missing or wrong type: %v", gotBody["secrets"])
	}
	if secrets["pg_password"] != "fromflag" {
		t.Errorf("secrets.pg_password = %v, want fromflag (flag should override file)", secrets["pg_password"])
	}
}

// writePlacementFusefile writes a Fusefile with a placement block and no
// secrets, so `up` sends the spec straight through.
func writePlacementFusefile(t *testing.T, dir string) string {
	t.Helper()
	src := `version: 1
resources:
  cpus: 4
  memory: 8GB
placement:
  host: build-3
  labels:
    disk: nvme
    tier: build
run: ./start.sh
`
	path := filepath.Join(dir, "Fusefile")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUpSendsPlacementSpec(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/environments" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		fmt.Fprint(w, `{"id":"vm1","state":"pending","task_id":"t","url":"","spec":{}}`)
	}))
	defer srv.Close()

	fusefilePath := writePlacementFusefile(t, t.TempDir())
	cfg := writeConfig(t, srv.URL)

	_, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{
			"--config", cfg, "-o", "json",
			"up", "-f", fusefilePath,
			"--task-id", "t",
			"--no-wait",
		})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if gotBody == nil {
		t.Fatalf("server was never called")
	}

	spec, ok := gotBody["spec"].(map[string]any)
	if !ok {
		t.Fatalf("spec missing or wrong type: %v", gotBody["spec"])
	}
	if spec["host_id"] != "build-3" {
		t.Errorf("spec.host_id = %v, want build-3", spec["host_id"])
	}
	labels, ok := spec["labels"].(map[string]any)
	if !ok {
		t.Fatalf("spec.labels missing or wrong type: %v", spec["labels"])
	}
	if labels["disk"] != "nvme" || labels["tier"] != "build" {
		t.Errorf("spec.labels = %v, want disk=nvme and tier=build", labels)
	}
}

func TestUpWithoutPlacementOmitsTheFields(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/environments" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		fmt.Fprint(w, `{"id":"vm1","state":"pending","task_id":"t","url":"","spec":{}}`)
	}))
	defer srv.Close()

	fusefilePath := writeFusefile(t, t.TempDir())
	cfg := writeConfig(t, srv.URL)

	_, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{
			"--config", cfg, "-o", "json",
			"up", "-f", fusefilePath,
			"--task-id", "t",
			"--secret", "pg_password=pw",
			"--no-wait",
		})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	spec, ok := gotBody["spec"].(map[string]any)
	if !ok {
		t.Fatalf("spec missing or wrong type: %v", gotBody["spec"])
	}
	// omitempty on both fields: a Fusefile with no placement block must send
	// exactly the wire it sent before placement existed.
	if _, present := spec["host_id"]; present {
		t.Errorf("spec.host_id present (%v), want it omitted", spec["host_id"])
	}
	if _, present := spec["labels"]; present {
		t.Errorf("spec.labels present (%v), want it omitted", spec["labels"])
	}
}

func TestFindFusefilePath(t *testing.T) {
	// an explicit -f or positional path is used as given, even when it does
	// not exist: the caller gets an error naming what they typed.
	if got, err := findFusefilePath("custom.yaml", nil); err != nil || got != "custom.yaml" {
		t.Errorf("findFusefilePath(-f custom.yaml) = %q, %v, want custom.yaml, nil", got, err)
	}
	if got, err := findFusefilePath("", []string{"other/Fusefile"}); err != nil || got != "other/Fusefile" {
		t.Errorf("findFusefilePath(positional) = %q, %v, want other/Fusefile, nil", got, err)
	}

	for _, name := range []string{"Fusefile", "Fusefile.yaml", "Fusefile.yml", "fusefile.yaml", "fusefile.yml"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, name, "version: 1\nrun: ./start.sh\n")
			t.Chdir(dir)

			got, err := findFusefilePath("", nil)
			if err != nil {
				t.Fatalf("findFusefilePath: %v", err)
			}
			// case-insensitively: on a case-insensitive filesystem the
			// candidate that matches first differs only in case from the name
			// on disk, and still opens the same file.
			if !strings.EqualFold(got, name) {
				t.Errorf("findFusefilePath = %q, want %q", got, name)
			}
		})
	}

	t.Run("prefers Fusefile when several exist", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "fusefile.yml", "version: 1\nrun: ./start.sh\n")
		writeFile(t, dir, "Fusefile", "version: 1\nrun: ./start.sh\n")
		t.Chdir(dir)

		got, err := findFusefilePath("", nil)
		if err != nil {
			t.Fatalf("findFusefilePath: %v", err)
		}
		if got != "Fusefile" {
			t.Errorf("findFusefilePath = %q, want Fusefile", got)
		}
	})

	t.Run("not found names every candidate", func(t *testing.T) {
		t.Chdir(t.TempDir())

		_, err := findFusefilePath("", nil)
		if err == nil {
			t.Fatal("want an error in a directory with no Fusefile")
		}
		for _, name := range fusefileNames {
			if !strings.Contains(err.Error(), name) {
				t.Errorf("error does not name %q: %v", name, err)
			}
		}
	})

	// a directory named Fusefile is not a Fusefile, and must not shadow the
	// other candidates.
	t.Run("skips a directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "Fusefile"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, dir, "Fusefile.yaml", "version: 1\nrun: ./start.sh\n")
		t.Chdir(dir)

		got, err := findFusefilePath("", nil)
		if err != nil {
			t.Fatalf("findFusefilePath: %v", err)
		}
		if got != "Fusefile.yaml" {
			t.Errorf("findFusefilePath = %q, want Fusefile.yaml", got)
		}
	})
}

func TestParseHostLabels(t *testing.T) {
	got, err := parseHostLabels([]string{"disk=nvme", "tier=build"})
	if err != nil {
		t.Fatal(err)
	}
	if got["disk"] != "nvme" || got["tier"] != "build" {
		t.Errorf("labels = %v, want disk=nvme and tier=build", got)
	}
	if nilMap, err := parseHostLabels(nil); err != nil || nilMap != nil {
		t.Errorf("parseHostLabels(nil) = %v, %v, want nil, nil", nilMap, err)
	}
	for _, bad := range []string{"disk", "=nvme", "disk="} {
		if _, err := parseHostLabels([]string{bad}); err == nil {
			t.Errorf("parseHostLabels(%q): expected an error", bad)
		}
	}
}
