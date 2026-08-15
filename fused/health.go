package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// The environment-level healthcheck runs here, in the guest, rather than in
// the control plane.
//
// fused already sits on the far side of the sandbox boundary, so an in-guest
// probe needs no new network path: it dials localhost or forks a process. An
// orchestrator-side prober would mean outbound HTTP from the control plane to
// every VM, which nothing in the stack does today.
//
// The config arrives as a file because that is the only channel that reaches
// the guest: the firecracker host agent ignores the launch command the
// orchestrator assembles and sends structured paths on a frozen wire, so a new
// flag would never actually be passed. fused reads the config from a fixed
// path instead, and its absence is what means "no probe".
//
// The verdict goes back out the same way. fused writes it to a second file
// that the orchestrator reads over the exec channel every host agent already
// exposes; see internal/orchestrator/health.go.

// Guest-side defaults for any probe field the author omitted. They live here,
// and only here: the wire carries zero for an omitted field precisely so the
// component that executes the probe is the one that decides what unset means.
const (
	defaultProbeInterval = 10 * time.Second
	defaultProbeTimeout  = 2 * time.Second
	defaultProbeRetries  = 3
	defaultProbePath     = "/"
)

// probeConfig is the shape written to the healthcheck file. It mirrors
// orchestrator.HealthcheckSpec field for field; that struct is marshalled
// straight into the file, so the two shapes are the same shape by
// construction.
type probeConfig struct {
	HTTP *probeHTTP `json:"http,omitempty"`
	Exec []string   `json:"exec,omitempty"`

	IntervalSeconds    int64 `json:"interval_seconds,omitempty"`
	TimeoutSeconds     int64 `json:"timeout_seconds,omitempty"`
	Retries            int   `json:"retries,omitempty"`
	StartPeriodSeconds int64 `json:"start_period_seconds,omitempty"`
}

type probeHTTP struct {
	Port int    `json:"port"`
	Path string `json:"path,omitempty"`
}

// Verdicts. They match orchestrator.HealthState byte for byte; the
// orchestrator decodes this file directly.
const (
	healthStarting = "starting"
	healthPassing  = "passing"
	healthFailing  = "failing"
)

// healthStatus is one verdict, both the body served on /health and the
// contents of the state file the orchestrator reads.
type healthStatus struct {
	State    string    `json:"state"`
	Since    time.Time `json:"since,omitempty"`
	Failures int       `json:"failures,omitempty"`
	Message  string    `json:"message,omitempty"`
}

// prober runs one environment-level probe on a loop and publishes its verdict.
//
// A prober is only constructed when a config file was present, so a nil
// *prober is the ordinary "this environment declared no probe" case and every
// method tolerates it.
type prober struct {
	cfg       probeConfig
	statePath string

	interval    time.Duration
	timeout     time.Duration
	retries     int
	startPeriod time.Duration

	mu     sync.Mutex
	status healthStatus
}

// loadProber reads the probe config from path and returns a prober for it.
//
// A missing file is the common case and is not an error: it means the
// environment declared no healthcheck, and (nil, nil) is returned. A file that
// exists but does not parse, or that names neither probe, IS an error: it
// reached the guest because somebody asked for a probe, and quietly running
// none would leave them believing an environment was being checked when it was
// not.
func loadProber(path, statePath string) (*prober, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read healthcheck %s: %w", path, err)
	}

	var cfg probeConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse healthcheck %s: %w", path, err)
	}
	switch {
	case cfg.HTTP != nil && len(cfg.Exec) > 0:
		return nil, fmt.Errorf("healthcheck %s: http and exec are mutually exclusive", path)
	case cfg.HTTP == nil && len(cfg.Exec) == 0:
		return nil, fmt.Errorf("healthcheck %s: http or exec is required", path)
	}

	p := &prober{
		cfg:         cfg,
		statePath:   statePath,
		interval:    secondsOr(cfg.IntervalSeconds, defaultProbeInterval),
		timeout:     secondsOr(cfg.TimeoutSeconds, defaultProbeTimeout),
		retries:     defaultProbeRetries,
		startPeriod: secondsOr(cfg.StartPeriodSeconds, 0),
		status:      healthStatus{State: healthStarting, Since: time.Now().UTC()},
	}
	if cfg.Retries > 0 {
		p.retries = cfg.Retries
	}
	if p.cfg.HTTP != nil && p.cfg.HTTP.Path == "" {
		p.cfg.HTTP.Path = defaultProbePath
	}
	// an attempt that outlived its interval would stretch the interval rather
	// than overlap the next attempt, since attempts run one at a time. the
	// orchestrator rejects that combination, so reaching it here means the
	// file was hand-written; clamp rather than refuse to probe at all.
	if p.timeout > p.interval {
		p.timeout = p.interval
	}
	return p, nil
}

// secondsOr converts a wire duration in seconds to a time.Duration, falling
// back to def when the field was omitted (zero).
func secondsOr(seconds int64, def time.Duration) time.Duration {
	if seconds <= 0 {
		return def
	}
	return time.Duration(seconds) * time.Second
}

// snapshot returns the current verdict, or a zero status for a nil prober.
func (p *prober) snapshot() healthStatus {
	if p == nil {
		return healthStatus{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

// run probes on the configured interval until ctx is cancelled, publishing the
// verdict after every attempt. A nil prober returns immediately, which is what
// makes the whole feature free for an environment that declared no probe.
//
// The first attempt is made immediately rather than after one interval: an
// author who set a 30s interval is asking how often to re-check, not how long
// to wait before the first check.
func (p *prober) run(ctx context.Context) {
	if p == nil {
		return
	}
	p.publish()

	tick := time.NewTicker(p.interval)
	defer tick.Stop()

	started := time.Now()
	for {
		p.record(started, p.attempt(ctx))
		p.publish()

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// attempt runs the probe once and returns nil on success, or the reason it
// failed. It is bounded by the configured timeout, so a probe against a hung
// listener fails on schedule instead of stalling the loop.
func (p *prober) attempt(ctx context.Context) error {
	attemptCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	if p.cfg.HTTP != nil {
		return probeHTTPOnce(attemptCtx, p.cfg.HTTP)
	}
	return probeExecOnce(attemptCtx, p.cfg.Exec)
}

// record folds one attempt's outcome into the verdict.
//
// The start period is a grace window on failures only, measured from the first
// attempt, and it ends for good on the first success: a service that came up
// and then fell over is failing, not still starting. That is why the window is
// only honored while the state is still starting.
func (p *prober) record(started time.Time, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now().UTC()
	if err == nil {
		p.status.Failures = 0
		p.status.Message = ""
		if p.status.State != healthPassing {
			p.status.State = healthPassing
			p.status.Since = now
		}
		return
	}

	p.status.Message = err.Error()
	if p.status.State == healthStarting && time.Since(started) < p.startPeriod {
		// inside the grace window: the failure is recorded as a message so an
		// operator can see what is not up yet, but it is not counted against
		// the retry budget.
		return
	}
	p.status.Failures++
	if p.status.Failures >= p.retries && p.status.State != healthFailing {
		p.status.State = healthFailing
		p.status.Since = now
	}
}

// publish writes the current verdict to the state file the orchestrator reads.
//
// The write goes to a temporary file in the same directory and is renamed into
// place, so a reader never observes a half-written verdict. A write failure is
// not fatal: the probe is still running and still serving its verdict on
// /health, and the next attempt tries again.
func (p *prober) publish() {
	if p.statePath == "" {
		return
	}
	body, err := json.Marshal(p.snapshot())
	if err != nil {
		return
	}
	tmp := p.statePath + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, p.statePath); err != nil {
		_ = os.Remove(tmp)
	}
}

// probeHTTPOnce issues one GET against a port on the guest's loopback. A 2xx
// or 3xx response passes; anything else, including a connection refused,
// fails with a reason an operator can act on.
//
// It dials 127.0.0.1 rather than the address the port is published on: the
// question is whether the app is serving, not whether anything outside the
// guest can reach it.
func probeHTTPOnce(ctx context.Context, cfg *probeHTTP) error {
	path := cfg.Path
	if path == "" {
		path = defaultProbePath
	}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", cfg.Port, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 399 {
		return fmt.Errorf("GET %s returned %d", url, resp.StatusCode)
	}
	return nil
}

// probeExecOnce runs the configured argv and passes on exit status 0. The argv
// is executed directly, with no shell, so no element can be reinterpreted as a
// pipeline, a redirect, or a glob.
func probeExecOnce(ctx context.Context, argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("healthcheck exec: empty command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(argv[0]), err)
	}
	return nil
}
