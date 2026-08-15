package fusefile

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestCompileMemoryGB(t *testing.T) {
	f := &Fusefile{Version: 1, Resources: Resources{Memory: "2GB"}}
	c, err := Compile(f)
	if err != nil {
		t.Fatal(err)
	}
	if c.Spec.RamMB != 2048 {
		t.Fatalf("ram_mb = %d, want 2048", c.Spec.RamMB)
	}
}

func TestCompileMemoryMB(t *testing.T) {
	f := &Fusefile{Version: 1, Resources: Resources{Memory: "512MB"}}
	c, err := Compile(f)
	if err != nil {
		t.Fatal(err)
	}
	if c.Spec.RamMB != 512 {
		t.Fatalf("ram_mb = %d, want 512", c.Spec.RamMB)
	}
}

func TestCompileStorageGB(t *testing.T) {
	f := &Fusefile{Version: 1, Resources: Resources{Storage: "10GB"}}
	c, err := Compile(f)
	if err != nil {
		t.Fatal(err)
	}
	if c.Spec.StorageGB != 10 {
		t.Fatalf("storage_gb = %d, want 10", c.Spec.StorageGB)
	}
}

func TestCompileStorageRoundsUp(t *testing.T) {
	// sub-1GB (and non-whole-GB) storage must round up, never floor to a
	// smaller value than requested.
	cases := []struct {
		name  string
		input string
		want  int32
	}{
		{"sub gb rounds up to 1", "512MB", 1},
		{"non whole gb rounds up", "1536MB", 2},
		{"whole gb unchanged", "10GB", 10},
		{"int32 max storage rounds up positive", "2147483647MB", 2097152},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &Fusefile{Version: 1, Resources: Resources{Storage: tc.input}}
			c, err := Compile(f)
			if err != nil {
				t.Fatal(err)
			}
			if c.Spec.StorageGB != tc.want {
				t.Fatalf("storage_gb = %d, want %d", c.Spec.StorageGB, tc.want)
			}
		})
	}
}

func TestCompileDiskAndStorage(t *testing.T) {
	// disk is the preferred spelling, storage the permanent alias. either
	// alone compiles to storage_gb, and both together are fine as long as
	// they mean the same size.
	cases := []struct {
		name    string
		disk    string
		storage string
		want    int32
	}{
		{"disk alone", "10GB", "", 10},
		{"storage alone", "", "10GB", 10},
		{"both agreeing", "10GB", "10GB", 10},
		{"both agreeing through different spellings", "10GB", "10240MB", 10},
		{"storage still rounds up", "", "512MB", 1},
		{"disk rounds up", "512MB", "", 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &Fusefile{Version: 1, Resources: Resources{Disk: tc.disk, Storage: tc.storage}}
			c, err := Compile(f)
			if err != nil {
				t.Fatal(err)
			}
			if c.Spec.StorageGB != tc.want {
				t.Fatalf("storage_gb = %d, want %d", c.Spec.StorageGB, tc.want)
			}
		})
	}
}

func TestCompileMaxRuntime(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  int64
	}{
		{"one hour", "1h", 3600},
		{"ninety minutes", "90m", 5400},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &Fusefile{Version: 1, Resources: Resources{MaxRuntime: tc.input}}
			c, err := Compile(f)
			if err != nil {
				t.Fatal(err)
			}
			if c.Spec.MaxRuntimeSeconds != tc.want {
				t.Fatalf("max_runtime_seconds = %d, want %d", c.Spec.MaxRuntimeSeconds, tc.want)
			}
		})
	}
}

func TestCompileIdleTimeout(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  int64
	}{
		{"unset", "", 0},
		{"one minute", "1m", 60},
		{"fifteen minutes", "15m", 900},
		{"two hours", "2h", 7200},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &Fusefile{Version: 1, Resources: Resources{IdleTimeout: tc.input}}
			c, err := Compile(f)
			if err != nil {
				t.Fatal(err)
			}
			if c.Spec.IdleTimeoutSeconds != tc.want {
				t.Fatalf("idle_timeout_seconds = %d, want %d", c.Spec.IdleTimeoutSeconds, tc.want)
			}
		})
	}
}

// idle_timeout and max_runtime are independent knobs: one is a ceiling from
// create, the other a window from the last exec or attach.
func TestCompileIdleTimeoutAndMaxRuntimeCoexist(t *testing.T) {
	f := &Fusefile{Version: 1, Resources: Resources{MaxRuntime: "4h", IdleTimeout: "15m"}}
	c, err := Compile(f)
	if err != nil {
		t.Fatal(err)
	}
	if c.Spec.MaxRuntimeSeconds != 14400 {
		t.Errorf("max_runtime_seconds = %d, want 14400", c.Spec.MaxRuntimeSeconds)
	}
	if c.Spec.IdleTimeoutSeconds != 900 {
		t.Errorf("idle_timeout_seconds = %d, want 900", c.Spec.IdleTimeoutSeconds)
	}
}

func TestCompileRegion(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"unset", "", ""},
		{"region set", "us-east", "us-east"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &Fusefile{Version: 1, Resources: Resources{Region: tc.input}}
			c, err := Compile(f)
			if err != nil {
				t.Fatal(err)
			}
			if c.Spec.Region != tc.want {
				t.Fatalf("region = %q, want %q", c.Spec.Region, tc.want)
			}
		})
	}
}

func TestCompileCPUsPassthrough(t *testing.T) {
	f := &Fusefile{Version: 1, Resources: Resources{CPUs: 4}}
	c, err := Compile(f)
	if err != nil {
		t.Fatal(err)
	}
	if c.Spec.CPUs != 4 {
		t.Fatalf("cpus = %d, want 4", c.Spec.CPUs)
	}
}

func TestCompileGPU(t *testing.T) {
	f := &Fusefile{Version: 1, Resources: Resources{GPU: 1, GPUKind: "a100"}}
	c, err := Compile(f)
	if err != nil {
		t.Fatal(err)
	}
	if c.Spec.GPUs != 1 {
		t.Fatalf("gpus = %d, want 1", c.Spec.GPUs)
	}
	if c.Spec.GPUKind != "a100" {
		t.Fatalf("gpu_kind = %q, want a100", c.Spec.GPUKind)
	}
	if c.Spec.GPUProfile != "" {
		t.Fatalf("gpu_profile = %q, want empty", c.Spec.GPUProfile)
	}
}

func TestCompileGPUProfile(t *testing.T) {
	f := &Fusefile{Version: 1, Resources: Resources{GPU: 2, GPUKind: "a100", GPUProfile: "1G.10GB"}}
	c, err := Compile(f)
	if err != nil {
		t.Fatal(err)
	}
	if c.Spec.GPUs != 2 {
		t.Fatalf("gpus = %d, want 2", c.Spec.GPUs)
	}
	if c.Spec.GPUProfile != "1g.10gb" {
		t.Fatalf("gpu_profile = %q, want 1g.10gb (lowercased)", c.Spec.GPUProfile)
	}
}

func TestKindSupportsMIG(t *testing.T) {
	cases := []struct {
		kind string
		want bool
	}{
		{"", true},                      // unknown at request time, defer to scheduler
		{"a100", true},                  // known mig-capable
		{"NVIDIA A100-SXM4-40GB", true}, // full model string still resolves
		{"h100", true},
		{"someunreleasedgpu", true}, // unrecognized passes (no stale allowlist block)
		{"v100", false},             // known non-mig-capable
		{"NVIDIA V100-SXM2", false}, // substring match, case-insensitive
		{"t4", false},
		{"rtx4090", false},
		{"l40s", false},
	}
	for _, tc := range cases {
		if got := KindSupportsMIG(tc.kind); got != tc.want {
			t.Errorf("KindSupportsMIG(%q) = %v, want %v", tc.kind, got, tc.want)
		}
	}
}

func TestCompileGPUAbsentIsZero(t *testing.T) {
	f := &Fusefile{Version: 1, Resources: Resources{CPUs: 2}}
	c, err := Compile(f)
	if err != nil {
		t.Fatal(err)
	}
	if c.Spec.GPUs != 0 {
		t.Fatalf("gpus = %d, want 0", c.Spec.GPUs)
	}
	if c.Spec.GPUKind != "" {
		t.Fatalf("gpu_kind = %q, want empty", c.Spec.GPUKind)
	}
}

func TestCompileEmptyResources(t *testing.T) {
	f := &Fusefile{Version: 1}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Spec.CPUs != 0 || c.Spec.RamMB != 0 || c.Spec.StorageGB != 0 || c.Spec.MaxRuntimeSeconds != 0 || c.Spec.IdleTimeoutSeconds != 0 || c.Spec.Region != "" || c.Spec.GPUs != 0 || c.Spec.GPUKind != "" {
		t.Fatalf("expected zero spec, got %+v", c.Spec)
	}
}

func TestCompileInvalid(t *testing.T) {
	cases := []struct {
		name        string
		resources   Resources
		wantContain string
	}{
		{
			// the size error names the accepted forms so a rejected string
			// tells the author what to write instead.
			name:      "invalid memory unit words",
			resources: Resources{Memory: "2 gigabytes"},
			wantContain: `resources.memory: invalid size "2 gigabytes" ` +
				`(expected a number and a unit, e.g. "512MB", "2GB", "1.5GiB")`,
		},
		{
			name:      "memory missing unit",
			resources: Resources{Memory: "2048"},
			wantContain: `resources.memory: invalid size "2048" ` +
				`(expected a number and a unit, e.g. "512MB", "2GB", "1.5GiB")`,
		},
		{
			name:        "memory missing number",
			resources:   Resources{Memory: "GB"},
			wantContain: `resources.memory: invalid size "GB"`,
		},
		{
			name:        "memory unknown unit",
			resources:   Resources{Memory: "2PB"},
			wantContain: `resources.memory: invalid size "2PB"`,
		},
		{
			name:        "memory below a whole mib",
			resources:   Resources{Memory: "0.25MB"},
			wantContain: `resources.memory: invalid size "0.25MB": must be a whole number of MiB`,
		},
		{
			name:        "invalid storage",
			resources:   Resources{Storage: "10 gigs"},
			wantContain: `resources.storage: invalid size "10 gigs"`,
		},
		{
			name:        "invalid disk",
			resources:   Resources{Disk: "10 gigs"},
			wantContain: `resources.disk: invalid size "10 gigs"`,
		},
		{
			name:        "disk and storage disagree",
			resources:   Resources{Disk: "10GB", Storage: "20GB"},
			wantContain: `resources.disk and resources.storage are the same field, but were set to "10GB" and "20GB"`,
		},
		{
			name:        "memory gb value overflows int32",
			resources:   Resources{Memory: "2097152GB"},
			wantContain: `resources.memory: invalid size "2097152GB": value too large`,
		},
		{
			// the underlying time.ParseDuration error is wrapped with %w, so
			// only assert the field path prefix rather than the exact
			// stdlib wording.
			name:        "invalid duration",
			resources:   Resources{MaxRuntime: "1 hour"},
			wantContain: `resources.max_runtime: `,
		},
		{
			name:        "negative max_runtime",
			resources:   Resources{MaxRuntime: "-1h"},
			wantContain: `resources.max_runtime: must not be negative`,
		},
		{
			name:        "invalid idle_timeout duration",
			resources:   Resources{IdleTimeout: "15 minutes"},
			wantContain: `resources.idle_timeout: `,
		},
		{
			name:        "negative idle_timeout",
			resources:   Resources{IdleTimeout: "-15m"},
			wantContain: `resources.idle_timeout: must not be negative`,
		},
		{
			name:        "sub-minute idle_timeout",
			resources:   Resources{IdleTimeout: "10s"},
			wantContain: `resources.idle_timeout: must be at least 1m0s`,
		},
		{
			name:        "negative cpu count",
			resources:   Resources{CPUs: -4},
			wantContain: `resources.cpus: must not be negative`,
		},
		{
			name:        "negative gpu count",
			resources:   Resources{GPU: -1},
			wantContain: `resources.gpu: must not be negative`,
		},
		{
			name:        "invalid gpu profile",
			resources:   Resources{GPU: 1, GPUProfile: "half"},
			wantContain: `resources.gpu_profile: invalid MIG profile`,
		},
		{
			name:        "gpu profile without count",
			resources:   Resources{GPUProfile: "1g.10gb"},
			wantContain: `resources.gpu_profile: requires resources.gpu >= 1`,
		},
		{
			name:        "gpu profile on non-mig-capable kind",
			resources:   Resources{GPU: 1, GPUKind: "v100", GPUProfile: "1g.10gb"},
			wantContain: `does not support MIG`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &Fusefile{Version: 1, Resources: tc.resources}
			_, err := Compile(f)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantContain) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantContain)
			}
		})
	}
}

// manifestDoc is a local unmarshal target for asserting the ManifestJSON
// shape without depending on internal/orchestrator or internal/secrets.
type manifestDoc struct {
	Version string `json:"version"`
	Machine struct {
		Workspace string `json:"workspace"`
	} `json:"machine"`
	Services map[string]struct {
		Image   string   `json:"image"`
		Ports   []int    `json:"ports"`
		Command []string `json:"command"`
		Restart string   `json:"restart"`
		Health  *struct {
			Test     []string `json:"test"`
			Interval string   `json:"interval"`
			Timeout  string   `json:"timeout"`
			Retries  int      `json:"retries"`
		} `json:"healthcheck"`
		DependsOn []string `json:"depends_on"`
		Env       map[string]struct {
			Value  string `json:"value"`
			Secret string `json:"secret"`
		} `json:"env"`
	} `json:"services"`
}

// an empty Fusefile must compile to a manifest byte-for-byte equal to
// internal/orchestrator/agent_profile.go's DefaultFusedManifest.
func TestCompileManifestEmptyMatchesDefaultFusedManifest(t *testing.T) {
	f := &Fusefile{Version: 1}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := `{"version":"1","machine":{"workspace":"/workspace"},"services":{}}`
	if string(c.ManifestJSON) != want {
		t.Fatalf("manifest json = %s, want %s", c.ManifestJSON, want)
	}
}

func TestCompileManifestWorkspaceCustom(t *testing.T) {
	f := &Fusefile{Version: 1, Workspace: "/ws"}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var m manifestDoc
	if err := json.Unmarshal(c.ManifestJSON, &m); err != nil {
		t.Fatal(err)
	}
	if m.Machine.Workspace != "/ws" {
		t.Fatalf("workspace = %q, want /ws", m.Machine.Workspace)
	}
}

func TestCompileManifestServices(t *testing.T) {
	f := &Fusefile{Version: 1, Workspace: "/ws",
		Services: map[string]Service{"db": {Image: "postgres:16",
			Ports: []int{5432},
			Env:   map[string]EnvValue{"P": {Secret: "pg"}}}}}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m manifestDoc
	if err := json.Unmarshal(c.ManifestJSON, &m); err != nil {
		t.Fatal(err)
	}

	svc, ok := m.Services["db"]
	if !ok {
		t.Fatalf("services.db missing")
	}
	if svc.Image != "postgres:16" {
		t.Fatalf("services.db.image = %q, want postgres:16", svc.Image)
	}
	if len(svc.Ports) != 1 || svc.Ports[0] != 5432 {
		t.Fatalf("services.db.ports = %v, want [5432]", svc.Ports)
	}
	env, ok := svc.Env["P"]
	if !ok {
		t.Fatalf("services.db.env.P missing")
	}
	if env.Secret != "pg" {
		t.Fatalf("services.db.env.P.secret = %q, want pg", env.Secret)
	}
	if env.Value != "" {
		t.Fatalf("services.db.env.P.value = %q, want empty", env.Value)
	}

	if !reflect.DeepEqual(c.RequiredSecrets, []string{"pg"}) {
		t.Fatalf("required secrets = %v, want [pg]", c.RequiredSecrets)
	}
}

func TestCompileManifestServiceEnvValue(t *testing.T) {
	f := &Fusefile{Version: 1,
		Services: map[string]Service{"api": {Image: "app:latest",
			Env: map[string]EnvValue{"MODE": {Value: "prod"}}}}}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m manifestDoc
	if err := json.Unmarshal(c.ManifestJSON, &m); err != nil {
		t.Fatal(err)
	}

	env := m.Services["api"].Env["MODE"]
	if env.Value != "prod" {
		t.Fatalf("services.api.env.MODE.value = %q, want prod", env.Value)
	}
	if env.Secret != "" {
		t.Fatalf("services.api.env.MODE.secret = %q, want empty", env.Secret)
	}
}

func TestCompileManifestServiceComposeFields(t *testing.T) {
	f := &Fusefile{Version: 1,
		Services: map[string]Service{"api": {
			Image:   "ghcr.io/acme/api:1",
			Command: []string{"/app/api", "serve"},
			Restart: "on-failure",
			HealthCheck: &HealthCheck{
				Test:     []string{"CMD", "curl", "-fsS", "http://localhost:8080/health"},
				Interval: "10s",
				Timeout:  "2s",
				Retries:  3,
			},
			DependsOn: []string{"db"},
		}}}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m manifestDoc
	if err := json.Unmarshal(c.ManifestJSON, &m); err != nil {
		t.Fatal(err)
	}

	svc, ok := m.Services["api"]
	if !ok {
		t.Fatalf("services.api missing")
	}
	if !reflect.DeepEqual(svc.Command, []string{"/app/api", "serve"}) {
		t.Fatalf("services.api.command = %v, want [/app/api serve]", svc.Command)
	}
	if svc.Restart != "on-failure" {
		t.Fatalf("services.api.restart = %q, want on-failure", svc.Restart)
	}
	if !reflect.DeepEqual(svc.DependsOn, []string{"db"}) {
		t.Fatalf("services.api.depends_on = %v, want [db]", svc.DependsOn)
	}
	if svc.Health == nil {
		t.Fatalf("services.api.healthcheck missing")
	}
	if !reflect.DeepEqual(svc.Health.Test, []string{"CMD", "curl", "-fsS", "http://localhost:8080/health"}) {
		t.Fatalf("services.api.healthcheck.test = %v", svc.Health.Test)
	}
	if svc.Health.Interval != "10s" || svc.Health.Timeout != "2s" || svc.Health.Retries != 3 {
		t.Fatalf("services.api.healthcheck = %+v, want interval=10s timeout=2s retries=3", svc.Health)
	}
}

func TestCompileServiceRestartInvalid(t *testing.T) {
	f := &Fusefile{Version: 1,
		Services: map[string]Service{"api": {Image: "app:latest", Restart: "sometimes"}}}
	_, err := Compile(f)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	wantContain := `services.api.restart: invalid "sometimes"`
	if !strings.Contains(err.Error(), wantContain) {
		t.Errorf("error %q does not contain %q", err.Error(), wantContain)
	}
}

func TestCompileServiceRestartValidValues(t *testing.T) {
	for _, r := range []string{"no", "always", "on-failure", "unless-stopped", ""} {
		t.Run(r, func(t *testing.T) {
			f := &Fusefile{Version: 1,
				Services: map[string]Service{"api": {Image: "app:latest", Restart: r}}}
			if _, err := Compile(f); err != nil {
				t.Fatalf("unexpected error for restart %q: %v", r, err)
			}
		})
	}
}

// the phase markers the startup script wraps the build steps in, written out
// literally rather than borrowed from the compiler: these bytes run in a guest
// shell, so a test that reused the constants would agree with any typo in them.
const (
	buildOpen  = "# fuse: build phase (a failure here exits 90)\ntrap '[ $? -eq 0 ] || exit 90' EXIT\n"
	buildClose = "trap - EXIT\n"
	runMarker  = "# fuse: run phase\n"
)

func TestCompileStartupScript(t *testing.T) {
	// the prelude enables strict mode, then creates and enters the workspace
	// so build/run execute where the Fusefile says they should.
	const prelude = "set -eu\nif (set -o pipefail) 2>/dev/null; then set -o pipefail; fi\n" +
		"mkdir -p '/workspace'\ncd '/workspace'\n"
	cases := []struct {
		name      string
		workspace string
		env       map[string]EnvValue
		build     []Step
		run       string
		want      string
	}{
		{
			"build and run", "", nil, []Step{{Run: "a"}, {Run: "b"}}, "./c",
			prelude + buildOpen + "a\nb\n" + buildClose + runMarker + "./c\n",
		},
		{"run only", "", nil, nil, "./c", prelude + "./c\n"},
		{"build only", "", nil, []Step{{Run: "a"}}, "", prelude + buildOpen + "a\n" + buildClose},
		{"neither", "", nil, nil, "", ""},
		{
			// the env block is sourced by path, before the build phase, so both
			// build and run see it. no key and no value is written into the script.
			"env is sourced, never interpolated",
			"",
			map[string]EnvValue{"NODE_ENV": {Value: "production"}, "TOKEN": {Secret: "api_token"}},
			[]Step{{Run: "a"}},
			"./c",
			prelude + "set -a\n. /fuse/env\nset +a\n" + buildOpen + "a\n" + buildClose + runMarker + "./c\n",
		},
		{
			// an env block on its own is not something to boot, so the empty
			// script short circuit still wins.
			"env alone emits no script",
			"",
			map[string]EnvValue{"NODE_ENV": {Value: "production"}},
			nil,
			"",
			"",
		},
		{
			"custom workspace",
			"/srv/app",
			nil,
			nil,
			"./c",
			"set -eu\nif (set -o pipefail) 2>/dev/null; then set -o pipefail; fi\n" +
				"mkdir -p '/srv/app'\ncd '/srv/app'\n./c\n",
		},
		{
			// an author-supplied path must not be able to break out of the
			// generated script.
			"workspace with a quote is escaped",
			"/tmp/it's here",
			nil,
			nil,
			"./c",
			"set -eu\nif (set -o pipefail) 2>/dev/null; then set -o pipefail; fi\n" +
				`mkdir -p '/tmp/it'\''s here'` + "\n" + `cd '/tmp/it'\''s here'` + "\n./c\n",
		},
		{
			// a per-step workdir is scoped to its own subshell, so the step
			// after it still starts in the workspace.
			"per-step workdir is a subshell",
			"",
			nil,
			[]Step{{Run: "a"}, {Workdir: "web", Run: "b"}, {Run: "c"}},
			"./d",
			prelude + buildOpen + "a\n(cd 'web'; b)\nc\n" + buildClose + runMarker + "./d\n",
		},
		{
			"absolute per-step workdir",
			"",
			nil,
			[]Step{{Workdir: "/opt/x", Run: "b"}},
			"",
			prelude + buildOpen + "(cd '/opt/x'; b)\n" + buildClose,
		},
		{
			"per-step workdir with a quote is escaped",
			"",
			nil,
			[]Step{{Workdir: "it's here", Run: "b"}},
			"",
			prelude + buildOpen + `(cd 'it'\''s here'; b)` + "\n" + buildClose,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &Fusefile{Version: 1, Workspace: tc.workspace, Env: tc.env, Build: tc.build, Run: Command{Shell: tc.run}}
			c, err := Compile(f)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.StartupScript != tc.want {
				t.Fatalf("startup script = %q, want %q", c.StartupScript, tc.want)
			}
		})
	}
}

// TestCompileBuildAndRunScripts pins the split `fuse build` and
// `fuse up --from-build` rely on: BuildScript is setup without run, RunScript
// is run without setup, and each is empty when its phase is.
func TestCompileBuildAndRunScripts(t *testing.T) {
	const prelude = "set -eu\nif (set -o pipefail) 2>/dev/null; then set -o pipefail; fi\n" +
		"mkdir -p '/workspace'\ncd '/workspace'\n"
	cases := []struct {
		name      string
		setup     []Step
		run       string
		wantBuild string
		wantRun   string
	}{
		{"setup and run", []Step{{Run: "a"}, {Run: "b"}}, "./c", prelude + "a\nb\n", prelude + "./c\n"},
		{"setup only", []Step{{Run: "a"}}, "", prelude + "a\n", ""},
		{"run only", nil, "./c", "", prelude + "./c\n"},
		{"neither", nil, "", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &Fusefile{Version: 1, Setup: tc.setup, Run: Command{Shell: tc.run}}
			c, err := Compile(f)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.BuildScript != tc.wantBuild {
				t.Fatalf("build script = %q, want %q", c.BuildScript, tc.wantBuild)
			}
			if c.RunScript != tc.wantRun {
				t.Fatalf("run script = %q, want %q", c.RunScript, tc.wantRun)
			}
			// the run command must never appear in the build phase: that is the
			// whole point of running setup separately.
			if tc.run != "" && strings.Contains(c.BuildScript, tc.run) {
				t.Fatalf("build script %q contains the run command %q", c.BuildScript, tc.run)
			}
		})
	}
}

// the mapping form of a build step must emit the same fragment the bare scalar
// form does, in every generated script. layer keys are derived from that same
// fragment, so a build that ran something else than the key covered would serve
// a stale layer.
func TestCompileMappingStepsEmitRunOnly(t *testing.T) {
	cacheOff := false
	f := &Fusefile{Version: 1, Build: []Step{
		{Run: "apt-get update -qq"},
		{Run: "npm ci", Inputs: []string{"package.json"}},
		{Run: "./register.sh", Cache: &cacheOff},
	}}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	const prelude = "set -eu\nif (set -o pipefail) 2>/dev/null; then set -o pipefail; fi\n" +
		"mkdir -p '/workspace'\ncd '/workspace'\n"
	const steps = "apt-get update -qq\nnpm ci\n./register.sh\n"
	if want := prelude + steps; c.BuildScript != want {
		t.Errorf("build script = %q, want %q", c.BuildScript, want)
	}
	if want := prelude + buildOpen + steps + buildClose; c.StartupScript != want {
		t.Errorf("startup script = %q, want %q", c.StartupScript, want)
	}
}

// build and setup are one field under two names, so a Fusefile written either
// way has to compile to the same bytes, keys included.
func TestCompileBuildAndSetupAreInterchangeable(t *testing.T) {
	steps := []Step{{Run: "apt-get update -qq"}, {Workdir: "web", Run: "npm ci"}}
	build, err := Compile(&Fusefile{Version: 1, Build: steps, Run: Command{Shell: "./start.sh"}})
	if err != nil {
		t.Fatalf("build: unexpected error: %v", err)
	}
	setup, err := Compile(&Fusefile{Version: 1, Setup: steps, Run: Command{Shell: "./start.sh"}})
	if err != nil {
		t.Fatalf("setup: unexpected error: %v", err)
	}
	if !reflect.DeepEqual(build, setup) {
		t.Errorf("build compiles to %+v, setup to %+v", build, setup)
	}
}

// the startup script carries both phases, so the seam between them has to be
// visible: a build failure is trapped into a fixed status, and the trap is
// cleared before the run command so a run failure still reports its own.
func TestCompileStartupScriptSeparatesThePhases(t *testing.T) {
	c, err := Compile(&Fusefile{Version: 1,
		Build: []Step{{Run: "npm ci"}},
		Run:   Command{Shell: "./start.sh"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	script := c.StartupScript
	openAt := strings.Index(script, buildOpen)
	closeAt := strings.Index(script, buildClose)
	stepAt := strings.Index(script, "npm ci")
	runAt := strings.Index(script, "./start.sh")
	if openAt < 0 || closeAt < 0 {
		t.Fatalf("startup script carries no phase markers:\n%s", script)
	}
	if openAt >= stepAt || stepAt >= closeAt || closeAt >= runAt {
		t.Errorf("phases are out of order in:\n%s", script)
	}
	if !strings.Contains(script, strconv.Itoa(BuildPhaseExitStatus)) {
		t.Errorf("startup script does not name the build failure status:\n%s", script)
	}
}

// a Fusefile with no build steps has only one phase, so it keeps compiling to
// exactly the script it always did: no trap, no markers.
func TestCompileStartupScriptRunOnlyCarriesNoMarkers(t *testing.T) {
	c, err := Compile(&Fusefile{Version: 1, Run: Command{Shell: "./start.sh"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(c.StartupScript, "trap") || strings.Contains(c.StartupScript, "# fuse:") {
		t.Errorf("run-only startup script carries phase markers:\n%s", c.StartupScript)
	}
}

func TestCompileRequiredSecretsUnion(t *testing.T) {
	f := &Fusefile{Version: 1,
		Secrets: []string{"pg_password"},
		Services: map[string]Service{"db": {Image: "postgres:16",
			Env: map[string]EnvValue{"P": {Secret: "pg"}}}}}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(c.RequiredSecrets, []string{"pg", "pg_password"}) {
		t.Fatalf("required secrets = %v, want [pg pg_password]", c.RequiredSecrets)
	}
}

func TestCompileRequiredSecretsDedupesOverlap(t *testing.T) {
	f := &Fusefile{Version: 1,
		Secrets: []string{"pg"},
		Services: map[string]Service{"db": {Image: "postgres:16",
			Env: map[string]EnvValue{"P": {Secret: "pg"}}}}}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(c.RequiredSecrets, []string{"pg"}) {
		t.Fatalf("required secrets = %v, want [pg]", c.RequiredSecrets)
	}
}

func TestCompileManifestSecretEnvExactBytes(t *testing.T) {
	f := &Fusefile{Version: 1, Services: map[string]Service{"db": {Image: "postgres:16",
		Ports: []int{5432}, Env: map[string]EnvValue{"PGPASSWORD": {Secret: "pg"}}}}}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := `{"version":"1","machine":{"workspace":"/workspace"},"services":{"db":{"image":"postgres:16","ports":[5432],"env":{"PGPASSWORD":{"secret":"pg"}}}}}`
	if string(c.ManifestJSON) != want {
		t.Fatalf("manifest json =\n%s\nwant\n%s", string(c.ManifestJSON), want)
	}
}

func TestCompileManifestValueEnvExactBytes(t *testing.T) {
	f := &Fusefile{Version: 1, Services: map[string]Service{"x": {Image: "x",
		Env: map[string]EnvValue{"FOO": {Value: "bar"}}}}}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := `{"version":"1","machine":{"workspace":"/workspace"},"services":{"x":{"image":"x","env":{"FOO":{"value":"bar"}}}}}`
	if string(c.ManifestJSON) != want {
		t.Fatalf("manifest json =\n%s\nwant\n%s", string(c.ManifestJSON), want)
	}
}

// the top-level env block rides in machine.env, and its exact bytes are the
// contract the orchestrator's /fuse/env renderer and
// internal/secrets.ExtractRequiredSecrets both read.
func TestCompileManifestTopLevelEnvExactBytes(t *testing.T) {
	f := &Fusefile{Version: 1, Env: map[string]EnvValue{
		"NODE_ENV":     {Value: "production"},
		"DATABASE_URL": {Secret: "db_url"},
	}}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := `{"version":"1","machine":{"workspace":"/workspace","env":{"DATABASE_URL":{"secret":"db_url"},"NODE_ENV":{"value":"production"}}},"services":{}}`
	if string(c.ManifestJSON) != want {
		t.Fatalf("manifest json =\n%s\nwant\n%s", string(c.ManifestJSON), want)
	}
}

// a secret referenced from the top-level env block is required transitively,
// exactly as a service's env ref is, so `fuse up` fails before any network call
// when it was not supplied.
func TestCompileManifestTopLevelEnvRequiredSecretsUnion(t *testing.T) {
	f := &Fusefile{Version: 1, Secrets: []string{"s0"},
		Env: map[string]EnvValue{
			"A": {Secret: "s1"},
			"B": {Value: "literal"},
			// the same secret from two places must still be one requirement.
			"C": {Secret: "s0"},
		},
		Services: map[string]Service{
			"db": {Image: "d", Env: map[string]EnvValue{"D": {Secret: "s2"}}}}}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []string{"s0", "s1", "s2"}
	if !reflect.DeepEqual(c.RequiredSecrets, want) {
		t.Fatalf("required secrets = %v, want %v", c.RequiredSecrets, want)
	}
}

// the security property this whole block exists for: neither a secret name nor
// anything that could carry its value reaches the generated script. The script
// is handed to ssh as one argv element, so its text is public on the host for
// the length of the boot; only the path to the resolved file appears in it.
func TestCompileTopLevelEnvSecretNeverReachesTheScript(t *testing.T) {
	f := &Fusefile{Version: 1,
		Env:   map[string]EnvValue{"DATABASE_URL": {Secret: "db_url"}},
		Setup: []Step{{Run: "npm ci"}},
		Run:   Command{Shell: "node server.js"}}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, script := range []string{c.StartupScript, c.RunScript, StartupScriptFrom(f, 1)} {
		if strings.Contains(script, "db_url") {
			t.Errorf("script names the secret db_url:\n%s", script)
		}
		if strings.Contains(script, "DATABASE_URL=") {
			t.Errorf("script assigns DATABASE_URL inline:\n%s", script)
		}
		if !strings.Contains(script, ". /fuse/env") {
			t.Errorf("script does not source the env file:\n%s", script)
		}
	}

	// the build path is deliberately left alone: it runs through exec, and its
	// fragments are what layer keys are derived from.
	if strings.Contains(c.BuildScript, "/fuse/env") {
		t.Errorf("build script sources the env file:\n%s", c.BuildScript)
	}
}

func TestCompileManifestMultiServiceRequiredSecretsUnion(t *testing.T) {
	f := &Fusefile{Version: 1, Secrets: []string{"s0", "s1"},
		Services: map[string]Service{
			"db":    {Image: "d", Env: map[string]EnvValue{"A": {Secret: "s1"}}},
			"cache": {Image: "c", Env: map[string]EnvValue{"B": {Secret: "s2"}}}}}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []string{"s0", "s1", "s2"}
	if !reflect.DeepEqual(c.RequiredSecrets, want) {
		t.Fatalf("required secrets = %v, want %v", c.RequiredSecrets, want)
	}
}

func TestParseSize(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantMB  int32
		wantErr bool
	}{
		{"empty is not an error", "", 0, false},
		{"megabytes", "512MB", 512, false},
		{"gigabytes", "2GB", 2048, false},
		{"lowercase unit", "2gb", 2048, false},
		// the widened grammar. every unit is binary, so G, GB and GiB all
		// mean 1024 MiB.
		{"short gigabyte unit", "2G", 2048, false},
		{"short megabyte unit", "512M", 512, false},
		{"space before unit", "2 GB", 2048, false},
		{"gibibyte synonym", "2GiB", 2048, false},
		{"mebibyte synonym", "512MiB", 512, false},
		{"decimal gigabytes", "1.5GB", 1536, false},
		{"decimal gibibytes", "1.5GiB", 1536, false},
		{"terabytes", "2TB", 2097152, false},
		{"short terabyte unit", "1T", 1048576, false},
		{"tebibyte synonym", "1TiB", 1048576, false},
		{"words instead of unit", "2 gigabytes", 0, true},
		{"missing unit", "2048", 0, true},
		{"missing number", "GB", 0, true},
		{"unknown unit", "2PB", 0, true},
		{"not a whole mib", "0.25MB", 0, true},
		{"large but valid gigabytes", "3000GB", 3072000, false},
		{"gigabytes overflows int32", "2097152GB", 0, true},
		{"terabytes overflow int32", "2048TB", 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mb, err := parseSize(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if mb != tc.wantMB {
				t.Fatalf("mb = %d, want %d", mb, tc.wantMB)
			}
		})
	}
}

func TestCompilePlacement(t *testing.T) {
	f := &Fusefile{
		Version: 1,
		Placement: Placement{
			Host:   "build-3",
			Labels: map[string]string{"disk": "nvme"},
		},
	}
	c, err := Compile(f)
	if err != nil {
		t.Fatal(err)
	}
	if c.Spec.HostID != "build-3" {
		t.Errorf("spec.HostID = %q, want build-3", c.Spec.HostID)
	}
	if !reflect.DeepEqual(c.Spec.Labels, map[string]string{"disk": "nvme"}) {
		t.Errorf("spec.Labels = %v, want disk=nvme", c.Spec.Labels)
	}
	// the compiled spec must not alias the Fusefile's map.
	c.Spec.Labels["disk"] = "ssd"
	if f.Placement.Labels["disk"] != "nvme" {
		t.Errorf("compile aliased the fusefile label map")
	}
}

func TestCompileEmptyPlacementLeavesSpecEmpty(t *testing.T) {
	c, err := Compile(&Fusefile{Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	if c.Spec.HostID != "" || c.Spec.Labels != nil {
		t.Errorf("spec placement = %q/%v, want empty (nil labels keep the scheduler fast path)",
			c.Spec.HostID, c.Spec.Labels)
	}
}

func TestValidLabel(t *testing.T) {
	valid := []string{"a", "nvme", "disk", "tier-1", "a.b_c-d", "A1", strings.Repeat("x", 63)}
	for _, s := range valid {
		if !ValidLabel(s) {
			t.Errorf("ValidLabel(%q) = false, want true", s)
		}
	}
	invalid := []string{"", " ", "-lead", "trail-", "has space", "has/slash", "has=eq", strings.Repeat("x", 64)}
	for _, s := range invalid {
		if ValidLabel(s) {
			t.Errorf("ValidLabel(%q) = true, want false", s)
		}
	}
}

func TestCompileStartupTimeout(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"", 0},       // unset: the orchestrator's default applies
		{"45s", 45},   //
		{"2m", 120},   //
		{"1500ms", 2}, // rounded up, never floored to the "unset" zero
	}
	for _, tc := range cases {
		c, err := Compile(&Fusefile{Version: 1, StartupTimeout: tc.in})
		if err != nil {
			t.Fatalf("Compile(%q): %v", tc.in, err)
		}
		if c.StartupTimeoutSeconds != tc.want {
			t.Errorf("Compile(%q) = %d seconds, want %d", tc.in, c.StartupTimeoutSeconds, tc.want)
		}
	}
}

func TestCompileStartupTimeoutErrors(t *testing.T) {
	cases := []struct {
		in          string
		wantContain string
	}{
		{"1 minute", "startup_timeout: "},
		{"0s", `startup_timeout: must be positive`},
		{"-5s", `startup_timeout: must be positive`},
	}
	for _, tc := range cases {
		_, err := Compile(&Fusefile{Version: 1, StartupTimeout: tc.in})
		if err == nil {
			t.Fatalf("Compile(%q): expected error, got nil", tc.in)
		}
		if !strings.Contains(err.Error(), tc.wantContain) {
			t.Errorf("Compile(%q) error %q does not contain %q", tc.in, err.Error(), tc.wantContain)
		}
	}
}

// zero is the "omitted" value and keeps meaning "let the host agent pick its
// default vCPU count"; only a negative count is a compile error.
func TestCompileCPUsZeroIsHostDefault(t *testing.T) {
	c, err := Compile(&Fusefile{Version: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Spec.CPUs != 0 {
		t.Errorf("cpus: got %d, want 0", c.Spec.CPUs)
	}
}

// resource errors are joined, so one Compile reports all of them.
func TestCompileReportsEveryResourceViolation(t *testing.T) {
	f := &Fusefile{Version: 1, Resources: Resources{
		CPUs:       -4,
		GPU:        -1,
		Memory:     "2 gigabytes",
		MaxRuntime: "-1h",
	}}
	_, err := Compile(f)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{
		"resources.cpus: must not be negative",
		"resources.gpu: must not be negative",
		`resources.memory: invalid size "2 gigabytes"`,
		"resources.max_runtime: must not be negative",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error is missing %q; got: %s", want, msg)
		}
	}
}

// TestCompileStartupScriptExecForm pins the exec form of `run`: a list argv is
// shell-quoted per element into a single command line, so an argument
// containing a single quote, a space, $HOME, or a glob cannot alter the
// generated script. The shell form stays byte-identical (covered by
// TestCompileStartupScript). The generated fragment runs under sh -lc, so exec
// form protects arguments from word splitting and metacharacter
// reinterpretation but is not a shell-less execve.
func TestCompileStartupScriptExecForm(t *testing.T) {
	const prelude = "set -eu\nif (set -o pipefail) 2>/dev/null; then set -o pipefail; fi\n" +
		"mkdir -p '/workspace'\ncd '/workspace'\n"
	cases := []struct {
		name    string
		argv    []string
		wantRun string // the single run line emitted after the prelude
	}{
		{name: "simple argv", argv: []string{"python", "app.py"}, wantRun: `'python' 'app.py'`},
		{
			name:    "argument with a single quote",
			argv:    []string{"python", "it's hot.py"},
			wantRun: `'python' 'it'\''s hot.py'`,
		},
		{
			name:    "argument with a space",
			argv:    []string{"python", "my app.py"},
			wantRun: `'python' 'my app.py'`,
		},
		{
			name:    "argument with $HOME and a glob",
			argv:    []string{"python", "$HOME", "*.py"},
			wantRun: `'python' '$HOME' '*.py'`,
		},
		{
			name:    "argument with quote, space, $ and glob together",
			argv:    []string{"sh", "-c", "it's $HOME *.py"},
			wantRun: `'sh' '-c' 'it'\''s $HOME *.py'`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &Fusefile{Version: 1, Run: Command{Argv: tc.argv}}
			c, err := Compile(f)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			want := prelude + tc.wantRun + "\n"
			if c.StartupScript != want {
				t.Errorf("StartupScript = %q, want %q", c.StartupScript, want)
			}
			// RunScript is the setup-free StartupScript; the run line is shared.
			if c.RunScript != want {
				t.Errorf("RunScript = %q, want %q", c.RunScript, want)
			}
			// the exec-form argv must never leak into the bake/build phase
			if c.BuildScript != "" {
				t.Errorf("BuildScript = %q, want empty", c.BuildScript)
			}
		})
	}
}
