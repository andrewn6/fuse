package fusefile

import (
	"encoding/json"
	"reflect"
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
	if c.Spec.CPUs != 0 || c.Spec.RamMB != 0 || c.Spec.StorageGB != 0 || c.Spec.MaxRuntimeSeconds != 0 || c.Spec.Region != "" || c.Spec.GPUs != 0 || c.Spec.GPUKind != "" {
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
			name:        "invalid memory unit words",
			resources:   Resources{Memory: "2 gigabytes"},
			wantContain: `resources.memory: invalid size "2 gigabytes"`,
		},
		{
			name:        "memory missing unit",
			resources:   Resources{Memory: "2"},
			wantContain: `resources.memory: invalid size "2"`,
		},
		{
			name:        "memory missing number",
			resources:   Resources{Memory: "GB"},
			wantContain: `resources.memory: invalid size "GB"`,
		},
		{
			name:        "memory unknown unit",
			resources:   Resources{Memory: "2TB"},
			wantContain: `resources.memory: invalid size "2TB"`,
		},
		{
			name:        "invalid storage",
			resources:   Resources{Storage: "10 gigs"},
			wantContain: `resources.storage: invalid size "10 gigs"`,
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
		Image string `json:"image"`
		Ports []int  `json:"ports"`
		Env   map[string]struct {
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

func TestCompileStartupScript(t *testing.T) {
	// the prelude enables strict mode, then creates and enters the workspace
	// so setup/run execute where the Fusefile says they should.
	const prelude = "set -eu\nif (set -o pipefail) 2>/dev/null; then set -o pipefail; fi\n" +
		"mkdir -p '/workspace'\ncd '/workspace'\n"
	cases := []struct {
		name      string
		workspace string
		setup     []string
		run       string
		want      string
	}{
		{"setup and run", "", []string{"a", "b"}, "./c", prelude + "a\nb\n./c\n"},
		{"run only", "", nil, "./c", prelude + "./c\n"},
		{"setup only", "", []string{"a"}, "", prelude + "a\n"},
		{"neither", "", nil, "", ""},
		{
			"custom workspace",
			"/srv/app",
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
			"./c",
			"set -eu\nif (set -o pipefail) 2>/dev/null; then set -o pipefail; fi\n" +
				`mkdir -p '/tmp/it'\''s here'` + "\n" + `cd '/tmp/it'\''s here'` + "\n./c\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &Fusefile{Version: 1, Workspace: tc.workspace, Setup: tc.setup, Run: tc.run}
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
		setup     []string
		run       string
		wantBuild string
		wantRun   string
	}{
		{"setup and run", []string{"a", "b"}, "./c", prelude + "a\nb\n", prelude + "./c\n"},
		{"setup only", []string{"a"}, "", prelude + "a\n", ""},
		{"run only", nil, "./c", "", prelude + "./c\n"},
		{"neither", nil, "", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &Fusefile{Version: 1, Setup: tc.setup, Run: tc.run}
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
		{"words instead of unit", "2 gigabytes", 0, true},
		{"missing unit", "2", 0, true},
		{"missing number", "GB", 0, true},
		{"unknown unit", "2TB", 0, true},
		{"large but valid gigabytes", "3000GB", 3072000, false},
		{"gigabytes overflows int32", "2097152GB", 0, true},
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
