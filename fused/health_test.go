package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// writeProbeConfig writes a healthcheck config into a temp dir and returns the
// config and state paths, which is what loadProber takes.
func writeProbeConfig(t *testing.T, cfg any) (string, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "healthcheck.json")
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(cfgPath, body, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return cfgPath, filepath.Join(dir, "health.json")
}

// an environment that declared no probe leaves no config file, and that
// absence must be an ordinary "nothing to do" rather than a startup failure.
func TestLoadProberMissingFileIsNotAnError(t *testing.T) {
	p, err := loadProber(filepath.Join(t.TempDir(), "absent.json"), "")
	if err != nil {
		t.Fatalf("loadProber: %v", err)
	}
	if p != nil {
		t.Errorf("prober = %+v, want nil", p)
	}
}

// a config that exists but is unusable reached the guest because somebody
// asked for a probe, so it must fail loudly rather than silently run none.
func TestLoadProberRejectsUnusableConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  probeConfig
	}{
		{name: "both probes", cfg: probeConfig{HTTP: &probeHTTP{Port: 80}, Exec: []string{"/ready"}}},
		{name: "neither probe", cfg: probeConfig{Retries: 3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath, statePath := writeProbeConfig(t, tc.cfg)
			if _, err := loadProber(cfgPath, statePath); err == nil {
				t.Fatalf("loadProber accepted %s", tc.name)
			}
		})
	}
}

func TestLoadProberAppliesDefaults(t *testing.T) {
	cfgPath, statePath := writeProbeConfig(t, probeConfig{HTTP: &probeHTTP{Port: 8080}})
	p, err := loadProber(cfgPath, statePath)
	if err != nil {
		t.Fatalf("loadProber: %v", err)
	}
	if p.interval != defaultProbeInterval || p.timeout != defaultProbeTimeout {
		t.Errorf("interval/timeout = %s/%s, want %s/%s",
			p.interval, p.timeout, defaultProbeInterval, defaultProbeTimeout)
	}
	if p.retries != defaultProbeRetries {
		t.Errorf("retries = %d, want %d", p.retries, defaultProbeRetries)
	}
	if p.cfg.HTTP.Path != defaultProbePath {
		t.Errorf("path = %q, want %q", p.cfg.HTTP.Path, defaultProbePath)
	}
	if got := p.snapshot(); got.State != healthStarting {
		t.Errorf("initial state = %q, want %q", got.State, healthStarting)
	}
}

// authored values win over every default.
func TestLoadProberHonorsAuthoredValues(t *testing.T) {
	cfgPath, statePath := writeProbeConfig(t, probeConfig{
		Exec:               []string{"/app/ready", "--check"},
		IntervalSeconds:    5,
		TimeoutSeconds:     2,
		Retries:            12,
		StartPeriodSeconds: 10,
	})
	p, err := loadProber(cfgPath, statePath)
	if err != nil {
		t.Fatalf("loadProber: %v", err)
	}
	if p.interval != 5*time.Second || p.timeout != 2*time.Second {
		t.Errorf("interval/timeout = %s/%s, want 5s/2s", p.interval, p.timeout)
	}
	if p.retries != 12 || p.startPeriod != 10*time.Second {
		t.Errorf("retries/start period = %d/%s, want 12/10s", p.retries, p.startPeriod)
	}
}

// newProber builds a prober directly, skipping the config file, for the state
// machine tests below.
func newProber(t *testing.T, retries int, startPeriod time.Duration) *prober {
	t.Helper()
	return &prober{
		statePath:   filepath.Join(t.TempDir(), "health.json"),
		interval:    time.Second,
		timeout:     time.Second,
		retries:     retries,
		startPeriod: startPeriod,
		status:      healthStatus{State: healthStarting, Since: time.Now().UTC()},
	}
}

// a single failed attempt is not a verdict: only `retries` in a row flips it.
func TestRecordFlipsToFailingOnlyAfterRetries(t *testing.T) {
	p := newProber(t, 3, 0)
	started := time.Now()

	for i := 1; i < 3; i++ {
		p.record(started, errors.New("connection refused"))
		if got := p.snapshot(); got.State == healthFailing {
			t.Fatalf("failing after %d of 3 attempts", i)
		}
	}
	p.record(started, errors.New("connection refused"))
	got := p.snapshot()
	if got.State != healthFailing {
		t.Fatalf("state = %q, want %q", got.State, healthFailing)
	}
	if got.Failures != 3 || got.Message != "connection refused" {
		t.Errorf("verdict = %+v, want 3 failures and the last reason", got)
	}
}

// a success clears the failure count and the message, so a passing probe never
// reports stale detail from before it recovered.
func TestRecordSuccessClearsFailures(t *testing.T) {
	p := newProber(t, 2, 0)
	started := time.Now()

	p.record(started, errors.New("boom"))
	p.record(started, errors.New("boom"))
	if got := p.snapshot(); got.State != healthFailing {
		t.Fatalf("state = %q, want %q", got.State, healthFailing)
	}

	p.record(started, nil)
	got := p.snapshot()
	if got.State != healthPassing || got.Failures != 0 || got.Message != "" {
		t.Errorf("verdict = %+v, want a clean passing verdict", got)
	}
}

// inside the start period a failure is recorded as detail but not counted, so
// a slow-starting app is not reported as broken while it is still coming up.
func TestRecordStartPeriodSuppressesFailures(t *testing.T) {
	p := newProber(t, 1, time.Hour)
	started := time.Now()

	p.record(started, errors.New("connection refused"))
	got := p.snapshot()
	if got.State != healthStarting {
		t.Errorf("state = %q, want %q while inside the start period", got.State, healthStarting)
	}
	if got.Failures != 0 {
		t.Errorf("failures = %d, want 0 inside the start period", got.Failures)
	}
	if got.Message == "" {
		t.Error("message is empty; the reason should still be visible")
	}
}

// once the probe has passed, the start period is over for good: an app that
// came up and then fell over is failing, not still starting.
func TestRecordStartPeriodEndsOnFirstSuccess(t *testing.T) {
	p := newProber(t, 1, time.Hour)
	started := time.Now()

	p.record(started, nil)
	p.record(started, errors.New("boom"))

	if got := p.snapshot(); got.State != healthFailing {
		t.Errorf("state = %q, want %q after a pass then a failure", got.State, healthFailing)
	}
}

func TestPublishWritesVerdictAtomically(t *testing.T) {
	p := newProber(t, 1, 0)
	p.record(time.Now(), nil)
	p.publish()

	raw, err := os.ReadFile(p.statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var got healthStatus
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("state file is not valid json: %v", err)
	}
	if got.State != healthPassing {
		t.Errorf("state = %q, want %q", got.State, healthPassing)
	}
	// the temp file the rename went through must not be left behind.
	if _, err := os.Stat(p.statePath + ".tmp"); !os.IsNotExist(err) {
		t.Error("the temp file survived the publish")
	}
}

func TestProbeHTTPOnce(t *testing.T) {
	var gotPath string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(status)
	}))
	defer srv.Close()

	port := serverPort(t, srv.URL)
	if err := probeHTTPOnce(context.Background(), &probeHTTP{Port: port, Path: "/healthz"}); err != nil {
		t.Fatalf("probe against a 200: %v", err)
	}
	if gotPath != "/healthz" {
		t.Errorf("path = %q, want /healthz", gotPath)
	}

	// a redirect still means the app is serving.
	status = http.StatusFound
	if err := probeHTTPOnce(context.Background(), &probeHTTP{Port: port, Path: "/healthz"}); err != nil {
		t.Errorf("probe against a 302: %v", err)
	}

	status = http.StatusInternalServerError
	if err := probeHTTPOnce(context.Background(), &probeHTTP{Port: port, Path: "/healthz"}); err == nil {
		t.Error("probe against a 500 passed")
	}
}

// nothing listening is a failure with a reason, not a panic or a hang.
func TestProbeHTTPOnceClosedPort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	port := serverPort(t, srv.URL)
	srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := probeHTTPOnce(ctx, &probeHTTP{Port: port, Path: "/"}); err == nil {
		t.Error("probe against a closed port passed")
	}
}

func TestProbeExecOnce(t *testing.T) {
	if err := probeExecOnce(context.Background(), []string{"true"}); err != nil {
		t.Errorf("exit 0 probe: %v", err)
	}
	if err := probeExecOnce(context.Background(), []string{"false"}); err == nil {
		t.Error("a non-zero exit passed")
	}
	if err := probeExecOnce(context.Background(), nil); err == nil {
		t.Error("an empty argv passed")
	}
}

// /health carries the verdict without changing its status code: it answers "is
// the agent up", which is a different question from "is the workload healthy".
func TestHealthRouteCarriesVerdict(t *testing.T) {
	p := newProber(t, 1, 0)
	p.record(time.Now(), errors.New("connection refused"))

	srv := httptest.NewServer(newHandler(config{vmID: "fuse-1"}, "", nil, 0, false, p))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("get /health: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 even for a failing probe", resp.StatusCode)
	}
	var body struct {
		Status      string       `json:"status"`
		Healthcheck healthStatus `json:"healthcheck"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Status)
	}
	if body.Healthcheck.State != healthFailing {
		t.Errorf("verdict = %q, want %q", body.Healthcheck.State, healthFailing)
	}
}

// with no probe declared the field is absent, so a client can tell "no
// healthcheck" from "a healthcheck that has not reported yet".
func TestHealthRouteOmitsVerdictWithoutAProbe(t *testing.T) {
	srv := httptest.NewServer(newHandler(config{vmID: "fuse-1"}, "", nil, 0, false, nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("get /health: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["healthcheck"]; ok {
		t.Errorf("healthcheck present with no probe declared: %v", body)
	}
}

// serverPort extracts the port an httptest server bound, so a probe can dial
// it on 127.0.0.1 the way it would inside a guest.
func serverPort(t *testing.T, raw string) int {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port of %s: %v", raw, err)
	}
	return port
}
