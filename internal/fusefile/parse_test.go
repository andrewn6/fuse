package fusefile

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestParseValidFusefile(t *testing.T) {
	src := `version: 1
image: img:tag
resources:
  cpus: 2
  memory: 2GB
  storage: 10GB
  max_runtime: 1h
  idle_timeout: 15m
setup:
  - echo hi
services:
  db:
    image: postgres:16
    ports:
      - 5432
    env:
      PGPASSWORD:
        secret: pg_password
      PGUSER:
        value: admin
run: ./start.sh
workspace: /workspace
expose:
  - port: 8080
    as: http
secrets:
  - pg_password
`

	f, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// these assertions decode through the real yaml tags, closing the gap
	// left by schema_test.go's struct-literal test (which never exercised
	// yaml unmarshaling at all).
	if f.Resources.Memory != "2GB" {
		t.Errorf("resources.memory: got %q, want %q", f.Resources.Memory, "2GB")
	}
	if f.Resources.MaxRuntime != "1h" {
		t.Errorf("resources.max_runtime: got %q, want %q", f.Resources.MaxRuntime, "1h")
	}
	if f.Resources.IdleTimeout != "15m" {
		t.Errorf("resources.idle_timeout: got %q, want %q", f.Resources.IdleTimeout, "15m")
	}

	db, ok := f.Services["db"]
	if !ok {
		t.Fatalf("services.db: not found")
	}
	if len(db.Ports) != 1 || db.Ports[0] != 5432 {
		t.Errorf("services.db.ports: got %v, want [5432]", db.Ports)
	}
	if db.Env["PGPASSWORD"].Secret != "pg_password" {
		t.Errorf("services.db.env.PGPASSWORD.secret: got %q, want %q", db.Env["PGPASSWORD"].Secret, "pg_password")
	}

	if len(f.Expose) != 1 || f.Expose[0].Port != 8080 {
		t.Errorf("expose[0].port: got %v, want 8080", f.Expose)
	}
	if f.Expose[0].As != "http" {
		t.Errorf("expose[0].as: got %q, want %q", f.Expose[0].As, "http")
	}

	if f.Workspace != "/workspace" {
		t.Errorf("workspace: got %q, want %q", f.Workspace, "/workspace")
	}
	if len(f.Setup) != 1 || f.Setup[0].Run != "echo hi" {
		t.Errorf("setup: got %v, want [echo hi]", f.Setup)
	}
}

func TestParseRejectsBadVersion(t *testing.T) {
	_, err := Parse([]byte("version: 2\n"))
	if err == nil {
		t.Fatalf("expected unsupported version error")
	}
}

func TestParseRejectsAmbiguousEnv(t *testing.T) {
	src := "version: 1\nservices:\n  db:\n    image: x\n    env:\n      K: { value: a, secret: b }\n"
	if _, err := Parse([]byte(src)); err == nil {
		t.Fatalf("expected ambiguous env value error")
	}
}

func TestParseEmptyInput(t *testing.T) {
	cases := []struct {
		name  string
		input []byte
	}{
		{
			name:  "empty byte slice",
			input: []byte(""),
		},
		{
			name:  "nil input",
			input: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.input)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), "version: must be 1") {
				t.Errorf("error %q does not contain %q", err.Error(), "version: must be 1")
			}
		})
	}
}

func TestParsePortBoundaries(t *testing.T) {
	cases := []struct {
		name        string
		port        int
		shouldError bool
		wantContain string
	}{
		{
			name:        "port 1 (minimum valid)",
			port:        1,
			shouldError: false,
		},
		{
			name:        "port 65535 (maximum valid)",
			port:        65535,
			shouldError: false,
		},
		{
			name:        "port 0 (below minimum)",
			port:        0,
			shouldError: true,
			wantContain: "expose[0].port: must be between 1 and 65535",
		},
		{
			name:        "port 65536 (above maximum)",
			port:        65536,
			shouldError: true,
			wantContain: "expose[0].port: must be between 1 and 65535",
		},
		{
			name:        "port -1 (negative)",
			port:        -1,
			shouldError: true,
			wantContain: "expose[0].port: must be between 1 and 65535",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "version: 1\nimage: x\nexpose:\n  - port: " + fmt.Sprintf("%d", tc.port) + "\n"
			_, err := Parse([]byte(src))
			if tc.shouldError && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.shouldError && err != nil {
				t.Fatalf("expected success, got error: %v", err)
			}
			if tc.shouldError && tc.wantContain != "" && !strings.Contains(err.Error(), tc.wantContain) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantContain)
			}
		})
	}
}

func TestParseInvalid(t *testing.T) {
	cases := []struct {
		name        string
		src         string
		wantContain string
	}{
		{
			name:        "missing version",
			src:         "services:\n  db:\n    image: x\n",
			wantContain: "version: must be 1",
		},
		{
			name:        "unsupported version",
			src:         "version: 2\n",
			wantContain: "version: must be 1",
		},
		{
			name:        "missing image",
			src:         "version: 1\nservices:\n  db: {}\n",
			wantContain: "services.db: image is required",
		},
		{
			name:        "ambiguous env",
			src:         "version: 1\nservices:\n  db:\n    image: x\n    env:\n      K: { value: a, secret: b }\n",
			wantContain: "services.db.env.K: value and secret are mutually exclusive",
		},
		{
			name:        "missing env",
			src:         "version: 1\nservices:\n  db:\n    image: x\n    env:\n      K: {}\n",
			wantContain: "services.db.env.K: value or secret is required",
		},
		{
			name:        "port zero",
			src:         "version: 1\nimage: x\nexpose:\n  - port: 0\n",
			wantContain: "expose[0].port: must be between 1 and 65535",
		},
		{
			name:        "unknown top-level field",
			src:         "version: 1\nbogus: x\n",
			wantContain: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.src))
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if tc.wantContain != "" && !strings.Contains(err.Error(), tc.wantContain) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantContain)
			}
		})
	}
}

func TestParseValidationErrorsSortedAndJoined(t *testing.T) {
	src := "version: 1\nservices:\n  zebra: {}\n  apple: {}\n"

	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatalf("expected error for missing images")
	}

	msg := err.Error()
	appleIdx := strings.Index(msg, "services.apple: image is required")
	zebraIdx := strings.Index(msg, "services.zebra: image is required")
	if appleIdx == -1 || zebraIdx == -1 {
		t.Fatalf("expected both service errors present, got: %s", msg)
	}
	if appleIdx > zebraIdx {
		t.Fatalf("expected sorted (apple before zebra) order, got: %s", msg)
	}
}

func TestParsePlacementBlock(t *testing.T) {
	src := `version: 1
resources:
  cpus: 4
  memory: 8GB
placement:
  host: build-3
  labels:
    disk: nvme
    tier: build
`
	f, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.Placement.Host != "build-3" {
		t.Errorf("placement.host: got %q, want %q", f.Placement.Host, "build-3")
	}
	if got := f.Placement.Labels["disk"]; got != "nvme" {
		t.Errorf("placement.labels.disk: got %q, want %q", got, "nvme")
	}
	if got := f.Placement.Labels["tier"]; got != "build" {
		t.Errorf("placement.labels.tier: got %q, want %q", got, "build")
	}
}

func TestParseNoPlacementBlockIsEmpty(t *testing.T) {
	f, err := Parse([]byte("version: 1\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.Placement.Host != "" || f.Placement.Labels != nil {
		t.Errorf("placement = %+v, want the zero value", f.Placement)
	}
}

func TestParseRejectsBadPlacementLabels(t *testing.T) {
	src := `version: 1
placement:
  labels:
    "bad key": nvme
    disk: "bad value"
`
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("expected an error for malformed label key and value")
	}
	// both violations are reported in one pass, keys sorted for stability.
	msg := err.Error()
	for _, want := range []string{`invalid label key "bad key"`, `invalid label value "bad value"`} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q, want it to contain %q", msg, want)
		}
	}
	if got := strings.Index(msg, "bad key"); got > strings.Index(msg, "bad value") {
		t.Errorf("error %q: want the sorted key order (disk after the bad key)", msg)
	}
}

func TestParseSetupStepForms(t *testing.T) {
	src := `version: 1
cache:
  enabled: true
setup:
  - apt-get update -qq
  - run: npm ci
    inputs:
      - package.json
      - package-lock.json
  - run: ./scripts/register.sh
    cache: false
`
	f, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !f.Cache.Enabled {
		t.Errorf("cache.enabled: got false, want true")
	}
	if len(f.Setup) != 3 {
		t.Fatalf("setup: got %d steps, want 3", len(f.Setup))
	}

	// the bare scalar form keeps working and carries no extra fields.
	if f.Setup[0].Run != "apt-get update -qq" {
		t.Errorf("setup[0].run: got %q", f.Setup[0].Run)
	}
	if f.Setup[0].Inputs != nil || f.Setup[0].Cache != nil {
		t.Errorf("setup[0]: bare scalar picked up extra fields: %+v", f.Setup[0])
	}

	if f.Setup[1].Run != "npm ci" {
		t.Errorf("setup[1].run: got %q", f.Setup[1].Run)
	}
	if len(f.Setup[1].Inputs) != 2 || f.Setup[1].Inputs[0] != "package.json" {
		t.Errorf("setup[1].inputs: got %v", f.Setup[1].Inputs)
	}
	if f.Setup[1].Cache != nil {
		t.Errorf("setup[1].cache: got %v, want unset", *f.Setup[1].Cache)
	}

	if f.Setup[2].Cache == nil || *f.Setup[2].Cache {
		t.Errorf("setup[2].cache: got %v, want an explicit false", f.Setup[2].Cache)
	}
}

// the mapping form's workdir scopes one step; a relative path is accepted (it
// resolves against the workspace) and so is an absolute one.
func TestParseSetupStepWorkdir(t *testing.T) {
	src := `version: 1
setup:
  - npm ci
  - workdir: web
    run: npm run build
  - workdir: /opt/tools
    run: ./install.sh
`
	f, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.Setup[0].Workdir != "" {
		t.Errorf("setup[0].workdir: got %q, want empty", f.Setup[0].Workdir)
	}
	if f.Setup[1].Workdir != "web" || f.Setup[1].Run != "npm run build" {
		t.Errorf("setup[1]: got %+v", f.Setup[1])
	}
	if f.Setup[2].Workdir != "/opt/tools" {
		t.Errorf("setup[2].workdir: got %q", f.Setup[2].Workdir)
	}
}

// a custom UnmarshalYAML does not inherit the decoder's KnownFields(true), so
// the step mapping needs its own unknown-key check or a typo parses as an
// empty step.
func TestParseRejectsUnknownStepField(t *testing.T) {
	_, err := Parse([]byte("version: 1\nsetup:\n  - ruh: npm ci\n"))
	if err == nil {
		t.Fatalf("expected an error for an unknown step field")
	}
	if !strings.Contains(err.Error(), "ruh") {
		t.Errorf("error %q does not name the unknown field", err.Error())
	}
}

func TestParseRejectsBadSetupSteps(t *testing.T) {
	cases := []struct {
		name        string
		src         string
		wantContain string
	}{
		{
			name:        "empty run",
			src:         "version: 1\nsetup:\n  - run: \"\"\n",
			wantContain: "setup[0].run: is required",
		},
		{
			name:        "blank scalar step",
			src:         "version: 1\nsetup:\n  - \"   \"\n",
			wantContain: "setup[0].run: is required",
		},
		{
			name:        "inputs on an uncacheable step",
			src:         "version: 1\nsetup:\n  - run: x\n    cache: false\n    inputs: [a]\n",
			wantContain: "setup[0].inputs: not allowed on a step with cache: false",
		},
		{
			name:        "absolute input",
			src:         "version: 1\nsetup:\n  - run: x\n    inputs: [/etc/passwd]\n",
			wantContain: "setup[0].inputs[0]: must be relative",
		},
		{
			name:        "traversing input",
			src:         "version: 1\nsetup:\n  - run: x\n    inputs: [\"../../etc/passwd\"]\n",
			wantContain: "setup[0].inputs[0]: must not traverse outside",
		},
		{
			name:        "traversing input via a subdirectory",
			src:         "version: 1\nsetup:\n  - run: x\n    inputs: [\"a/../../b\"]\n",
			wantContain: "setup[0].inputs[0]: must not traverse outside",
		},
		{
			name:        "empty input",
			src:         "version: 1\nsetup:\n  - run: x\n    inputs: [\"\"]\n",
			wantContain: "setup[0].inputs[0]: must not be empty",
		},
		{
			name:        "step is a list",
			src:         "version: 1\nsetup:\n  - [a, b]\n",
			wantContain: "must be a string or a mapping",
		},
		{
			name:        "traversing workdir",
			src:         "version: 1\nsetup:\n  - run: x\n    workdir: ../etc\n",
			wantContain: `setup[0].workdir: must not contain ".." segments`,
		},
		{
			name:        "workdir with a newline",
			src:         "version: 1\nsetup:\n  - run: x\n    workdir: \"web\\nrm -rf /\"\n",
			wantContain: "setup[0].workdir: must not contain newlines",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.src))
			if err == nil {
				t.Fatalf("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantContain) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantContain)
			}
		})
	}
}

// a step inside a subdirectory is fine; only escaping the Fusefile's directory
// is rejected.
func TestParseAcceptsNestedInputPaths(t *testing.T) {
	f, err := Parse([]byte("version: 1\nsetup:\n  - run: x\n    inputs: [\"src/**/*.go\", \"./package.json\", \"a/../b\"]\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(f.Setup[0].Inputs) != 3 {
		t.Errorf("inputs: got %v", f.Setup[0].Inputs)
	}
}

// `build:` is the first-class spelling of the phase and takes exactly the same
// step forms `setup:` always did.
func TestParseBuildSteps(t *testing.T) {
	src := `version: 1
build:
  - apt-get update -qq
  - run: npm ci
    inputs:
      - package.json
  - workdir: web
    run: npm run build
run: ./start.sh
`
	f, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(f.Build) != 3 {
		t.Fatalf("build: got %d steps, want 3", len(f.Build))
	}
	if f.Build[0].Run != "apt-get update -qq" {
		t.Errorf("build[0].run: got %q", f.Build[0].Run)
	}
	if len(f.Build[1].Inputs) != 1 || f.Build[1].Inputs[0] != "package.json" {
		t.Errorf("build[1].inputs: got %v", f.Build[1].Inputs)
	}
	if f.Build[2].Workdir != "web" {
		t.Errorf("build[2].workdir: got %q", f.Build[2].Workdir)
	}
	if len(f.Setup) != 0 {
		t.Errorf("setup: got %v, want empty", f.Setup)
	}
}

// BuildSteps is the single place the alias is resolved, so both spellings have
// to arrive at the same slice.
func TestBuildStepsResolvesTheSetupAlias(t *testing.T) {
	build, err := Parse([]byte("version: 1\nbuild:\n  - npm ci\n"))
	if err != nil {
		t.Fatalf("build: unexpected error: %v", err)
	}
	setup, err := Parse([]byte("version: 1\nsetup:\n  - npm ci\n"))
	if err != nil {
		t.Fatalf("setup: unexpected error: %v", err)
	}
	if !reflect.DeepEqual(build.BuildSteps(), setup.BuildSteps()) {
		t.Errorf("BuildSteps: build %v, setup %v", build.BuildSteps(), setup.BuildSteps())
	}
	if steps := (&Fusefile{Version: 1}).BuildSteps(); len(steps) != 0 {
		t.Errorf("BuildSteps of a file with neither key: got %v, want none", steps)
	}
}

// the two keys are one field, so a file that sets both is rejected rather than
// silently resolved in one direction.
func TestParseRejectsBuildAndSetupTogether(t *testing.T) {
	_, err := Parse([]byte("version: 1\nbuild:\n  - npm ci\nsetup:\n  - npm test\n"))
	if err == nil {
		t.Fatalf("expected an error for a file setting both build and setup")
	}
	if !strings.Contains(err.Error(), "setup is a deprecated alias for build") {
		t.Errorf("error %q does not explain the conflict", err.Error())
	}
}

// a step error names the key the author wrote, so the index points into a list
// they can actually see in their file.
func TestParseStepErrorsNameTheAuthoredKey(t *testing.T) {
	cases := []struct {
		name        string
		src         string
		wantContain string
	}{
		{
			name:        "build",
			src:         "version: 1\nbuild:\n  - run: \"\"\n",
			wantContain: "build[0].run: is required",
		},
		{
			name:        "setup",
			src:         "version: 1\nsetup:\n  - run: \"\"\n",
			wantContain: "setup[0].run: is required",
		},
		{
			name:        "build workdir",
			src:         "version: 1\nbuild:\n  - run: x\n    workdir: ../etc\n",
			wantContain: `build[0].workdir: must not contain ".." segments`,
		},
		{
			name:        "build inputs",
			src:         "version: 1\nbuild:\n  - run: x\n    inputs: [/etc/passwd]\n",
			wantContain: "build[0].inputs[0]: must be relative",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.src))
			if err == nil {
				t.Fatalf("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantContain) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantContain)
			}
		})
	}
}

// one case per structural rule added for the gaps every one of these used to
// fall through: service ports, expose names and duplicates, workspace, empty
// secret and env names.
func TestParseStructuralRules(t *testing.T) {
	cases := []struct {
		name        string
		src         string
		wantContain string
	}{
		{
			name:        "service port above range",
			src:         "version: 1\nservices:\n  db:\n    image: postgres:16\n    ports: [70000]\n",
			wantContain: "services.db.ports[0]: must be between 1 and 65535",
		},
		{
			name:        "service port negative",
			src:         "version: 1\nservices:\n  db:\n    image: postgres:16\n    ports: [5432, -1]\n",
			wantContain: "services.db.ports[1]: must be between 1 and 65535",
		},
		{
			name:        "empty env name",
			src:         "version: 1\nservices:\n  db:\n    image: postgres:16\n    env:\n      \"\": { value: x }\n",
			wantContain: "services.db.env: environment variable name must not be empty",
		},
		{
			name:        "duplicate expose port",
			src:         "version: 1\nexpose:\n  - port: 8080\n  - port: 8080\n",
			wantContain: "expose[1].port: 8080 is already exposed by expose[0]",
		},
		{
			name:        "duplicate expose name",
			src:         "version: 1\nexpose:\n  - port: 8080\n    as: http\n  - port: 8081\n    as: http\n",
			wantContain: `expose[1].as: "http" is already used by expose[0]`,
		},
		{
			name:        "expose name charset",
			src:         "version: 1\nexpose:\n  - port: 8080\n    as: \"HTTP api\"\n",
			wantContain: "expose[0].as: invalid name",
		},
		{
			name:        "expose name too long",
			src:         "version: 1\nexpose:\n  - port: 8080\n    as: " + strings.Repeat("a", 64) + "\n",
			wantContain: "expose[0].as: invalid name",
		},
		{
			name:        "relative workspace",
			src:         "version: 1\nworkspace: ../../etc\n",
			wantContain: `workspace: must be an absolute path, got "../../etc"`,
		},
		{
			name:        "traversing absolute workspace",
			src:         "version: 1\nworkspace: /workspace/../etc\n",
			wantContain: `workspace: must not contain ".." segments`,
		},
		{
			name:        "workspace with a newline",
			src:         "version: 1\nworkspace: \"/a\\nb\"\n",
			wantContain: "workspace: must not contain newlines or NUL bytes",
		},
		{
			name:        "empty secret name",
			src:         "version: 1\nsecrets:\n  - \"\"\n",
			wantContain: "secrets[0]: must not be empty",
		},
		{
			name:        "blank secret name",
			src:         "version: 1\nsecrets:\n  - \"   \"\n",
			wantContain: "secrets[0]: must not be empty",
		},
		{
			name:        "second yaml document",
			src:         "version: 1\n---\nversion: 1\n",
			wantContain: "must contain exactly one yaml document",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.src))
			if err == nil {
				t.Fatalf("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantContain) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantContain)
			}
		})
	}
}

func TestParseAcceptsValidStructuralValues(t *testing.T) {
	src := `version: 1
workspace: /srv/app
services:
  db:
    image: postgres:16
    ports: [1, 65535]
    env:
      PGDATA: { value: /var/lib/postgresql/data }
expose:
  - port: 8080
    as: http
  - port: 8081
    as: metrics-2
secrets:
  - pg_password
`
	if _, err := Parse([]byte(src)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// every violation is reported in one pass, not just the first.
func TestParseReportsEveryStructuralViolation(t *testing.T) {
	src := `version: 1
workspace: ../../etc
services:
  db:
    image: postgres:16
    ports: [70000, -1]
    env:
      "": { value: x }
expose:
  - port: 8080
    as: http
  - port: 8080
    as: http
secrets:
  - ""
`
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{
		"workspace: must be an absolute path",
		"services.db.ports[0]: must be between 1 and 65535",
		"services.db.ports[1]: must be between 1 and 65535",
		"services.db.env: environment variable name must not be empty",
		"expose[1].port: 8080 is already exposed by expose[0]",
		`expose[1].as: "http" is already used by expose[0]`,
		"secrets[0]: must not be empty",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error is missing %q; got: %s", want, msg)
		}
	}
}

func TestValidExposeName(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"http", true},
		{"api-v2", true},
		{"a", true},
		{strings.Repeat("a", 63), true},
		{strings.Repeat("a", 64), false},
		{"", false},
		{"HTTP", false},
		{"-http", false},
		{"http-", false},
		{"http_api", false},
		{"http.api", false},
		{"web app", false},
	}
	for _, tc := range cases {
		if got := ValidExposeName(tc.in); got != tc.want {
			t.Errorf("ValidExposeName(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// cpus is a whole vcpu count, so a whole-valued float is accepted and a
// genuine fraction is rejected with a reason.
func TestParseCPUs(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		want    VCPUs
		wantErr string
	}{
		{name: "integer", src: "version: 1\nresources:\n  cpus: 2\n", want: 2},
		{name: "whole float", src: "version: 1\nresources:\n  cpus: 2.0\n", want: 2},
		{
			name:    "fraction",
			src:     "version: 1\nresources:\n  cpus: 0.5\n",
			wantErr: "is not a whole number of vcpus",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := Parse([]byte(tc.src))
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error, got nil")
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if f.Resources.CPUs != tc.want {
				t.Errorf("resources.cpus: got %d, want %d", f.Resources.CPUs, tc.want)
			}
		})
	}
}

// disk is the preferred spelling for the root disk size; storage stays valid.
func TestParseDiskAndStorage(t *testing.T) {
	f, err := Parse([]byte("version: 1\nresources:\n  disk: 10GB\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.Resources.Disk != "10GB" {
		t.Errorf("resources.disk: got %q, want %q", f.Resources.Disk, "10GB")
	}

	f, err = Parse([]byte("version: 1\nresources:\n  storage: 10GB\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.Resources.Storage != "10GB" {
		t.Errorf("resources.storage: got %q, want %q", f.Resources.Storage, "10GB")
	}
}

// TestParseRunCommandForm covers the polymorphic top-level `run` field. A
// scalar keeps its shell semantics (and compiles byte-identical, see
// TestCompileStartupScript); a list becomes an argv; a mapping, an empty list,
// or a non-string element is rejected at decode time, before validate runs.
func TestParseRunCommandForm(t *testing.T) {
	t.Run("scalar is the shell form", func(t *testing.T) {
		f, err := Parse([]byte("version: 1\nrun: ./start.sh\n"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if f.Run.Shell != "./start.sh" || f.Run.Argv != nil {
			t.Errorf("Run = %+v, want Shell=./start.sh, Argv=nil", f.Run)
		}
	})

	t.Run("multi-token scalar stays one shell string", func(t *testing.T) {
		f, err := Parse([]byte("version: 1\nrun: ./serve.sh --port 8080\n"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if f.Run.Shell != "./serve.sh --port 8080" || f.Run.Argv != nil {
			t.Errorf("Run = %+v, want the whole line as Shell", f.Run)
		}
	})

	t.Run("list is the exec form", func(t *testing.T) {
		// an argument with a space and a slash keeps its integrity; exec form
		// is an argv, not a shell string.
		f, err := Parse([]byte("version: 1\nrun: [\"python\", \"app.py\", \"--config\", \"/etc/my app/config.json\"]\n"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{"python", "app.py", "--config", "/etc/my app/config.json"}
		if f.Run.Shell != "" || !reflect.DeepEqual(f.Run.Argv, want) {
			t.Errorf("Run = %+v, want Shell=\"\", Argv=%v", f.Run, want)
		}
	})

	cases := []struct {
		name        string
		src         string
		wantContain string
	}{
		{name: "empty list", src: "version: 1\nrun: []\n", wantContain: "run: an empty list is not a valid command"},
		{name: "mapping form is rejected", src: "version: 1\nrun: {shell: ./x}\n", wantContain: "run: must be a string or a list of strings"},
		{name: "non-string element (int)", src: "version: 1\nrun: [\"a\", 5]\n", wantContain: "run: argument 1 is not a string"},
		{name: "non-string element (bool)", src: "version: 1\nrun: [\"a\", true]\n", wantContain: "run: argument 1 is not a string"},
		{name: "non-string element (null)", src: "version: 1\nrun: [\"a\", ~]\n", wantContain: "run: argument 1 is not a string"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.src)); err == nil {
				t.Fatalf("expected an error, got nil")
			} else if !strings.Contains(err.Error(), tc.wantContain) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantContain)
			}
		})
	}
}
