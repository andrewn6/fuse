package fusefile

import (
	"reflect"
	"strings"
	"testing"
)

// parseProbe parses a Fusefile whose only interesting field is the
// healthcheck block, so each case below is just the yaml under test.
func parseProbe(t *testing.T, body string) (*Fusefile, error) {
	t.Helper()
	return Parse([]byte("version: 1\n" + body))
}

func TestParseHealthcheckHTTP(t *testing.T) {
	f, err := parseProbe(t, `healthcheck:
  http:
    port: 8080
    path: /healthz
  interval: 5s
  timeout: 2s
  retries: 12
  start_period: 10s
`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := &HealthProbe{
		HTTP:        &HealthProbeHTTP{Port: 8080, Path: "/healthz"},
		Interval:    "5s",
		Timeout:     "2s",
		Retries:     12,
		StartPeriod: "10s",
	}
	if !reflect.DeepEqual(f.Healthcheck, want) {
		t.Errorf("healthcheck = %+v, want %+v", f.Healthcheck, want)
	}
}

func TestParseHealthcheckExec(t *testing.T) {
	f, err := parseProbe(t, `healthcheck:
  exec: ["/app/ready", "--check"]
`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := f.Healthcheck.Exec; !reflect.DeepEqual(got, []string{"/app/ready", "--check"}) {
		t.Errorf("exec = %v", got)
	}
	if f.Healthcheck.HTTP != nil {
		t.Errorf("http = %+v, want nil", f.Healthcheck.HTTP)
	}
}

// an absent block must stay absent: nil is what tells every layer below that
// the author asked for no probe, and a zero-valued struct would not.
func TestParseHealthcheckAbsent(t *testing.T) {
	f, err := parseProbe(t, "run: ./serve.sh\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.Healthcheck != nil {
		t.Errorf("healthcheck = %+v, want nil", f.Healthcheck)
	}
}

func TestValidateHealthcheckRejects(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "both probes",
			body: "healthcheck:\n  http:\n    port: 80\n  exec: [\"/ready\"]\n",
			want: "http and exec are mutually exclusive",
		},
		{
			name: "neither probe",
			body: "healthcheck:\n  interval: 5s\n",
			want: "http or exec is required",
		},
		{
			name: "port too low",
			body: "healthcheck:\n  http:\n    port: 0\n",
			want: "healthcheck.http.port: must be between 1 and 65535",
		},
		{
			name: "port too high",
			body: "healthcheck:\n  http:\n    port: 70000\n",
			want: "healthcheck.http.port: must be between 1 and 65535",
		},
		{
			name: "path without leading slash",
			body: "healthcheck:\n  http:\n    port: 80\n    path: healthz\n",
			want: "healthcheck.http.path: must start with a slash",
		},
		{
			name: "empty exec argument",
			body: "healthcheck:\n  exec: [\"/ready\", \"  \"]\n",
			want: "healthcheck.exec[1]: must not be empty",
		},
		{
			name: "negative retries",
			body: "healthcheck:\n  http:\n    port: 80\n  retries: -1\n",
			want: "healthcheck.retries: must not be negative",
		},
		{
			name: "unknown field",
			body: "healthcheck:\n  http:\n    port: 80\n  grace: 5s\n",
			want: "field grace not found",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseProbe(t, tc.body)
			if err == nil {
				t.Fatalf("Parse accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestCompileHealthcheck(t *testing.T) {
	f, err := parseProbe(t, `healthcheck:
  http:
    port: 8080
    path: /healthz
  interval: 5s
  timeout: 2s
  retries: 12
  start_period: 10s
`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	want := &HealthcheckSpec{
		HTTP:               &HealthcheckHTTP{Port: 8080, Path: "/healthz"},
		IntervalSeconds:    5,
		TimeoutSeconds:     2,
		Retries:            12,
		StartPeriodSeconds: 10,
	}
	if !reflect.DeepEqual(c.Healthcheck, want) {
		t.Errorf("healthcheck = %+v, want %+v", c.Healthcheck, want)
	}
}

// an omitted duration compiles to zero, which is what the wire reads as
// "unset" and the guest agent replaces with its own default.
func TestCompileHealthcheckOmittedDurations(t *testing.T) {
	f, err := parseProbe(t, "healthcheck:\n  exec: [\"/app/ready\"]\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	want := &HealthcheckSpec{Exec: []string{"/app/ready"}}
	if !reflect.DeepEqual(c.Healthcheck, want) {
		t.Errorf("healthcheck = %+v, want %+v", c.Healthcheck, want)
	}
}

// a sub-second duration rounds up rather than flooring to zero, which the wire
// would read as "unset".
func TestCompileHealthcheckRoundsUp(t *testing.T) {
	f, err := parseProbe(t, "healthcheck:\n  exec: [\"/ready\"]\n  interval: 1500ms\n  timeout: 250ms\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if got := c.Healthcheck.IntervalSeconds; got != 2 {
		t.Errorf("interval seconds = %d, want 2", got)
	}
	if got := c.Healthcheck.TimeoutSeconds; got != 1 {
		t.Errorf("timeout seconds = %d, want 1", got)
	}
}

func TestCompileHealthcheckRejects(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "unparseable interval",
			body: "healthcheck:\n  exec: [\"/ready\"]\n  interval: soon\n",
			want: "healthcheck.interval",
		},
		{
			name: "zero timeout",
			body: "healthcheck:\n  exec: [\"/ready\"]\n  timeout: 0s\n",
			want: "healthcheck.timeout: must be positive",
		},
		{
			name: "negative start period",
			body: "healthcheck:\n  exec: [\"/ready\"]\n  start_period: -5s\n",
			want: "healthcheck.start_period: must be positive",
		},
		{
			name: "timeout above interval",
			body: "healthcheck:\n  exec: [\"/ready\"]\n  interval: 2s\n  timeout: 5s\n",
			want: "must not exceed healthcheck.interval",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := parseProbe(t, tc.body)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if _, err := Compile(f); err == nil {
				t.Fatalf("Compile accepted %s", tc.name)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// a Fusefile with no healthcheck must compile to a nil spec, so nothing
// downstream has to tell "no probe" apart from "an empty probe".
func TestCompileHealthcheckAbsent(t *testing.T) {
	f, err := parseProbe(t, "run: ./serve.sh\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if c.Healthcheck != nil {
		t.Errorf("healthcheck = %+v, want nil", c.Healthcheck)
	}
}
