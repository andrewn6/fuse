package fusefile

import (
	"fmt"
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
