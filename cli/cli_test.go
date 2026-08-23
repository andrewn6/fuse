package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fuse "github.com/folsomintel/fuse/sdks/go"
)

func TestParseKeyVals(t *testing.T) {
	m, err := parseKeyVals([]string{"a=1", "b=x=y"})
	if err != nil {
		t.Fatal(err)
	}
	if m["a"] != "1" || m["b"] != "x=y" {
		t.Errorf("got %v", m)
	}
	if _, err := parseKeyVals([]string{"bad"}); err == nil {
		t.Error("expected error for missing =")
	}
	if m, err := parseKeyVals(nil); err != nil || m != nil {
		t.Errorf("nil input should be nil map, got %v %v", m, err)
	}
}

func TestMaybeFile(t *testing.T) {
	if got, _ := maybeFile("plain"); got != "plain" {
		t.Errorf("plain string changed: %q", got)
	}
	p := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(p, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := maybeFile("@" + p)
	if err != nil || got != "hello" {
		t.Errorf("file read = %q, %v", got, err)
	}
	if _, err := maybeFile("@/no/such/file"); err == nil {
		t.Error("expected error for missing file")
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{0: "-", 512: "512 B", 1024: "1.0 KB", 1536: "1.5 KB", 1048576: "1.0 MB"}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestFriendlyError(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{&fuse.APIError{Status: 404, Code: "not_found", Message: "host gone"}, "not found"},
		{&fuse.APIError{Status: 403, Code: "forbidden", Message: "nope"}, "forbidden"},
		{&fuse.APIError{Status: 401, Code: "unauthorized", Message: "bad token"}, "unauthorized"},
		{&fuse.APIError{Status: 409, Code: "conflict", Message: "vm not running"}, "conflict"},
		{&fuse.APIError{Status: 400, Code: "invalid_argument", Message: "task_id required"}, "invalid argument"},
		{fmt.Errorf("plain error"), "plain error"},
	}
	for _, c := range cases {
		got := friendly(c.err)
		if got == nil || !strings.Contains(got.Error(), c.want) {
			t.Errorf("friendly(%v) = %v, want contains %q", c.err, got, c.want)
		}
	}
	if friendly(nil) != nil {
		t.Error("friendly(nil) should be nil")
	}
}

// writeConfig writes a minimal config pointing at baseURL and returns its path.
func writeConfig(t *testing.T, baseURL string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := fmt.Sprintf("current_context: t\ncontexts:\n  - name: t\n    base_url: %s\n    token: test-token\n", baseURL)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func capture(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	err := fn()
	_ = w.Close()
	os.Stdout = old
	data, _ := io.ReadAll(r)
	return string(data), err
}

// captureBoth is capture for commands whose interesting output is on stderr:
// infof, successf, and warnf all write there so that -o json keeps stdout
// machine-readable. It returns stdout and stderr separately.
func captureBoth(t *testing.T, fn func() error) (string, string, error) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	os.Stdout, os.Stderr = outW, errW
	err := fn()
	_ = outW.Close()
	_ = errW.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	outData, _ := io.ReadAll(outR)
	errData, _ := io.ReadAll(errR)
	return string(outData), string(errData), err
}

func TestHostsListJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/hosts" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("auth header = %q, want Bearer test-token", got)
		}
		fmt.Fprint(w, `{"hosts":[{"id":"h1","url":"http://h1","state":"active","capacity":{"cpus":4,"ram_mb":8192,"storage_gb":100,"vm_count":10},"allocated":{"cpus":1,"ram_mb":1024,"storage_gb":10,"vm_count":2}}]}`)
	}))
	defer srv.Close()

	cfg := writeConfig(t, srv.URL)
	out, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", cfg, "-o", "json", "hosts", "list"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, `"h1"`) || !strings.Contains(out, `"active"`) {
		t.Errorf("output missing host fields: %s", out)
	}
}

// TestEnvGetRendersEndpoints checks that the detail view renders one row per
// published endpoint, labelled by the author's `as` name when there is one and
// by the guest port otherwise, and that the agent address is labelled as such
// rather than as the environment's url.
func TestEnvGetRendersEndpoints(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":"vm1","state":"running","task_id":"t","host_id":"h1",
			"url":"10.0.0.4:19551","spec":{"cpus":2,"ram_mb":2048},
			"endpoints":[
				{"as":"http","url":"10.0.0.4:41337","port":8080},
				{"url":"10.0.0.4:41338","port":5432}
			]}`)
	}))
	defer srv.Close()

	cfg := writeConfig(t, srv.URL)
	out, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", cfg, "environment", "get", "vm1"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "endpoint http") || !strings.Contains(out, "10.0.0.4:41337  ->  guest :8080") {
		t.Errorf("output missing the named endpoint row:\n%s", out)
	}
	// no `as`, so the guest port labels the row.
	if !strings.Contains(out, "endpoint 5432") || !strings.Contains(out, "10.0.0.4:41338  ->  guest :5432") {
		t.Errorf("output missing the unnamed endpoint row:\n%s", out)
	}
	if !strings.Contains(out, "agent") || !strings.Contains(out, "10.0.0.4:19551") {
		t.Errorf("output missing the agent row:\n%s", out)
	}
}

// an environment with no expose block renders exactly the rows it did before
// endpoints existed.
func TestEnvGetWithoutEndpoints(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":"vm1","state":"running","task_id":"t","url":"10.0.0.4:19551","spec":{}}`)
	}))
	defer srv.Close()

	cfg := writeConfig(t, srv.URL)
	out, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", cfg, "environment", "get", "vm1"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if strings.Contains(out, "endpoint") {
		t.Errorf("output has an endpoint row for an environment with none:\n%s", out)
	}
}

func TestHostGetNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"not_found","message":"host missing"}}`)
	}))
	defer srv.Close()

	cfg := writeConfig(t, srv.URL)
	root := newRootCmd()
	root.SetArgs([]string{"--config", cfg, "host", "get", "missing"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want not-found error, got %v", err)
	}
}

// TestHostGetMIGInstanceDetail checks that `host get` renders one row per
// carved MIG instance with its profile, parent gpu, and free/in-use state
// when the host reports per-instance inventory.
func TestHostGetMIGInstanceDetail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":"mig-1","url":"http://m1","state":"active","backend":"qemu",
			"capacity":{"cpus":8,"ram_mb":4096,"storage_gb":100,"vm_count":10,"gpu_kind":"a100",
				"mig_profiles":{"1g.10gb":2},
				"mig_instances":[
					{"uuid":"MIG-aa-00","profile":"1g.10gb","kind":"a100","parent_gpu_uuid":"GPU-aaa"},
					{"uuid":"MIG-bb-11","profile":"1g.10gb","kind":"a100","parent_gpu_uuid":"GPU-aaa"}
				]},
			"allocated":{"cpus":2,"ram_mb":512,"storage_gb":10,"vm_count":1,
				"mig_profiles":{"1g.10gb":1},"mig_instance_uuids":["MIG-aa-00"]}}`)
	}))
	defer srv.Close()

	cfg := writeConfig(t, srv.URL)
	out, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", cfg, "host", "get", "mig-1"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	// the bound instance is in-use; the other is free. uuids render in their
	// 8-char short form.
	if !strings.Contains(out, "MIG-aa-0") || !strings.Contains(out, "in-use") {
		t.Errorf("output missing bound instance row: %s", out)
	}
	if !strings.Contains(out, "MIG-bb-1") || !strings.Contains(out, "free") {
		t.Errorf("output missing free instance row: %s", out)
	}
	if !strings.Contains(out, "parent=GPU-aaa") {
		t.Errorf("output missing parent gpu: %s", out)
	}
}

func TestEnvListScopedToActiveHost(t *testing.T) {
	var gotHostQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHostQuery = r.URL.Query().Get("host_id")
		fmt.Fprint(w, `{"environments":[]}`)
	}))
	defer srv.Close()

	// config with an active host set; env list should default to it.
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := fmt.Sprintf("current_context: t\ncontexts:\n  - name: t\n    base_url: %s\n    token: test-token\n    active_host: prod-1\n", srv.URL)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", path, "-o", "json", "environment", "list"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if gotHostQuery != "prod-1" {
		t.Errorf("host_id query = %q, want prod-1", gotHostQuery)
	}
}

func TestEnvForkJSON(t *testing.T) {
	var (
		gotPath   string
		gotAction string
		gotBody   string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAction = r.URL.Query().Get("action")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"fuse-fork-abc","state":"running","task_id":"fork-abc","url":"http://fuse-fork-abc.test","spec":{}}`)
	}))
	defer srv.Close()

	cfg := writeConfig(t, srv.URL)
	out, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", cfg, "-o", "json", "environment", "fork", "fuse-task-1", "--reuse-snapshot", "cp-1", "--comment", "clone"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if gotPath != "/v1/environments/fuse-task-1" {
		t.Errorf("path = %q, want /v1/environments/fuse-task-1", gotPath)
	}
	if gotAction != "fork" {
		t.Errorf("action = %q, want fork", gotAction)
	}
	if !strings.Contains(gotBody, `"reuse_snapshot_id":"cp-1"`) || !strings.Contains(gotBody, `"comment":"clone"`) {
		t.Errorf("request body missing fork options: %s", gotBody)
	}
	if !strings.Contains(out, `"fuse-fork-abc"`) {
		t.Errorf("output missing new env id: %s", out)
	}
}

func TestConnectAndContextCurrent(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	root := newRootCmd()
	// --no-verify: this test exercises context persistence, not
	// connectivity, and there is no orchestrator listening on :9999.
	root.SetArgs([]string{"--config", cfg, "connect", "http://localhost:9999", "--token", "abc", "--name", "dev", "--no-verify"})
	if err := root.Execute(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	out, err := capture(t, func() error {
		r := newRootCmd()
		r.SetArgs([]string{"--config", cfg, "-o", "json", "context", "current"})
		return r.Execute()
	})
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if !strings.Contains(out, `"dev"`) || strings.Contains(out, "abc") {
		t.Errorf("context current json wrong (token must not leak): %s", out)
	}
}

func TestEnvCreateGPUFlags(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"fuse-1","state":"provisioning","task_id":"t1","spec":{"gpus":2,"gpu_kind":"a100","gpu_profile":"1g.10gb"}}`)
	}))
	defer srv.Close()

	cfg := writeConfig(t, srv.URL)
	_, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", cfg, "-o", "json", "environment", "create",
			"--task-id", "t1", "--gpus", "2", "--gpu-kind", "a100", "--gpu-profile", "1g.10gb"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	for _, want := range []string{`"gpus":2`, `"gpu_kind":"a100"`, `"gpu_profile":"1g.10gb"`} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("request body missing %s: %s", want, gotBody)
		}
	}
}

// a create with no gpu flags must not send gpu fields at all, so the server
// keeps treating it as a plain cpu request.
func TestEnvCreateOmitsGPUWhenUnset(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"fuse-1","state":"provisioning","task_id":"t1","spec":{}}`)
	}))
	defer srv.Close()

	cfg := writeConfig(t, srv.URL)
	_, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", cfg, "-o", "json", "environment", "create", "--task-id", "t1"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	for _, unwanted := range []string{"gpus", "gpu_kind", "gpu_profile"} {
		if strings.Contains(gotBody, unwanted) {
			t.Errorf("request body should omit %s: %s", unwanted, gotBody)
		}
	}
}

// the gpu column carries counts for gpu hosts and a dash for cpu-only ones.
func TestHostsListGPUColumn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"hosts":[
			{"id":"gpu-1","url":"http://g1","state":"active","capacity":{"cpus":16,"ram_mb":65536,"vm_count":20,"gpus":4,"gpu_kind":"a100"},"allocated":{"cpus":4,"ram_mb":8192,"vm_count":1,"gpus":2}},
			{"id":"cpu-1","url":"http://c1","state":"active","capacity":{"cpus":16,"ram_mb":65536,"vm_count":20},"allocated":{"cpus":0,"ram_mb":0,"vm_count":0}}
		]}`)
	}))
	defer srv.Close()

	cfg := writeConfig(t, srv.URL)
	out, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", cfg, "hosts", "list"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "GPUS") {
		t.Errorf("output missing GPUS header: %s", out)
	}
	if !strings.Contains(out, "2/4 (a100)") {
		t.Errorf("output missing gpu allocation: %s", out)
	}
	// the cpu-only row must render a dash, not 0/0.
	if strings.Contains(out, "0/0") {
		t.Errorf("cpu-only host should render a dash, not 0/0: %s", out)
	}
}

func TestHostsListGPUCell(t *testing.T) {
	cases := []struct {
		name string
		host fuse.Host
		want string
	}{
		{"no gpus", fuse.Host{}, "-"},
		{"gpus without kind", fuse.Host{
			Capacity:  fuse.HostCapacity{GPUs: 4},
			Allocated: fuse.HostCapacity{GPUs: 1},
		}, "1/4"},
		{"gpus with kind", fuse.Host{
			Capacity:  fuse.HostCapacity{GPUs: 8, GPUKind: "h100"},
			Allocated: fuse.HostCapacity{GPUs: 8},
		}, "8/8 (h100)"},
	}
	for _, c := range cases {
		if got := gpuCell(c.host); got != c.want {
			t.Errorf("%s: gpuCell = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestHostMetricsGPURows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":"gpu-1","url":"http://g1","state":"active","backend":"qemu",
			"capacity":{"cpus":16,"ram_mb":65536,"storage_gb":500,"vm_count":20,"gpus":4,"gpu_kind":"a100","mig_profiles":{"2g.20gb":2,"1g.10gb":4}},
			"allocated":{"cpus":4,"ram_mb":8192,"storage_gb":100,"vm_count":1,"gpus":2,"mig_profiles":{"1g.10gb":1}}}`)
	}))
	defer srv.Close()

	cfg := writeConfig(t, srv.URL)
	out, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", cfg, "host", "metrics", "gpu-1"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "2 / 4 (2 free, a100)") {
		t.Errorf("output missing gpu row: %s", out)
	}
	// profiles render in sorted order regardless of map iteration order.
	if !strings.Contains(out, "1g.10gb 1 / 4 (3 free), 2g.20gb 0 / 2 (2 free)") {
		t.Errorf("output missing mig row: %s", out)
	}
}

// a cpu-only host must not grow empty gpu/mig rows.
func TestHostMetricsOmitsGPURowsForCPUHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":"cpu-1","url":"http://c1","state":"active",
			"capacity":{"cpus":16,"ram_mb":65536,"storage_gb":500,"vm_count":20},
			"allocated":{"cpus":4,"ram_mb":8192,"storage_gb":100,"vm_count":1}}`)
	}))
	defer srv.Close()

	cfg := writeConfig(t, srv.URL)
	out, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", cfg, "host", "metrics", "cpu-1"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if strings.Contains(out, "gpus") || strings.Contains(out, "mig") {
		t.Errorf("cpu-only metrics should omit gpu/mig rows: %s", out)
	}
}

// `fuse up` must carry a Fusefile's files and startup_timeout onto the wire:
// files as base64 blocks inside the startup script, the timeout as its own
// field. Sources resolve relative to the Fusefile, not the process cwd.
func TestUpSendsFilesAndStartupTimeout(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"fuse-1","state":"provisioning","task_id":"t1","spec":{}}`)
	}))
	defer srv.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.yaml"), []byte("from disk\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fusefilePath := filepath.Join(dir, "Fusefile")
	if err := os.WriteFile(fusefilePath, []byte(
		"version: 1\n"+
			"startup_timeout: 45s\n"+
			"files:\n"+
			"  - path: app.yaml\n"+
			"    source: ./app.yaml\n"+
			"run: cat app.yaml\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := writeConfig(t, srv.URL)
	_, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", cfg, "-o", "json", "up", "-f", fusefilePath,
			"--task-id", "t1", "--no-wait"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if !strings.Contains(gotBody, `"startup_script_timeout_seconds":45`) {
		t.Errorf("request body missing the startup timeout: %s", gotBody)
	}
	// The file's content reached the wire base64'd inside the startup script.
	// The script itself is JSON-escaped in the body, so assert on the encoded
	// payload rather than on shell syntax.
	if want := base64.StdEncoding.EncodeToString([]byte("from disk\n")); !strings.Contains(gotBody, want) {
		t.Errorf("request body missing the resolved file content (%s): %s", want, gotBody)
	}
}

// TestSnapCreateLiveFlag checks --live reaches the wire and that the kind the
// server reports back is what gets rendered, not the flag that was passed.
func TestSnapCreateLiveFlag(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"snap-1","vm_id":"fuse-task-1","kind":"live","state":"ready","created_at":"2024-01-01T00:00:00Z"}`)
	}))
	defer srv.Close()

	cfg := writeConfig(t, srv.URL)
	out, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", cfg, "snapshot", "create", "fuse-task-1", "--live", "--comment", "warmed"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(gotBody, `"live":true`) {
		t.Errorf("request body missing live flag: %s", gotBody)
	}
	if !strings.Contains(out, "live") {
		t.Errorf("detail output missing kind:\n%s", out)
	}
}

// TestSnapCreateOmitsLiveWhenUnset pins the default: an ordinary create must
// not start sending live at all, and the rendered kind falls back to disk when
// the server is old enough not to send one.
func TestSnapCreateOmitsLiveWhenUnset(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"snap-1","vm_id":"fuse-task-1","state":"ready","created_at":"2024-01-01T00:00:00Z"}`)
	}))
	defer srv.Close()

	cfg := writeConfig(t, srv.URL)
	out, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", cfg, "snapshot", "create", "fuse-task-1", "--comment", "plain"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if strings.Contains(gotBody, `"live"`) {
		t.Errorf("live sent when unset: %s", gotBody)
	}
	if !strings.Contains(out, "disk") {
		t.Errorf("detail output should default kind to disk:\n%s", out)
	}
}
