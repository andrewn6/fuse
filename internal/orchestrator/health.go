package orchestrator

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// This file holds the environment-level healthcheck: the probe an author
// declares in the Fusefile's `healthcheck:` block, the verdict the guest agent
// produces from it, and the reconcile pass that reads that verdict back.
//
// It answers a question nothing else in the stack answered: an environment
// reaching VMStateRunning only ever meant "a process was started on the host".
// Presence in Provider.List keeps a VM Running even after its guest has
// panicked, so a probe is the only signal that the sandbox is doing its job.
//
// The probe itself runs in the guest, in fused, which already sits on the far
// side of the boundary. The orchestrator never dials a VM: it reads the
// verdict fused leaves at GuestHealthStatePath over the exec channel every
// host agent already exposes. That keeps this feature from being the first
// thing in the control plane to make outbound HTTP to a sandbox.

// Guest paths for the healthcheck. Both live under /fuse, which the guest
// mounts on tmpfs, alongside the manifest and secrets (see agent_profile.go).
//
// GuestHealthcheckPath is written by the orchestrator before boot and is how
// the probe reaches the guest at all: the firecracker host agent ignores
// AgentSpec.Command and sends structured paths on a frozen wire, so a new
// flag would never be passed. fused looks for the config at this fixed path
// instead of being told about it.
//
// GuestHealthStatePath is written by fused after every attempt and is what
// reconcileHealth reads back.
const (
	GuestHealthcheckPath = "/fuse/healthcheck.json"
	GuestHealthStatePath = "/fuse/health.json"
)

// HealthcheckSpec is the environment-level readiness probe, as it travels from
// the create request to the guest. Exactly one of HTTP and Exec is set; the
// API layer rejects a request that sets both or neither.
//
// Every duration is in seconds, and a zero means the field was omitted and the
// guest agent's default applies. The orchestrator deliberately does not fill
// those defaults in: the probe is executed in the guest, so the guest owns
// what "unset" means, and having two places invent defaults is how they drift.
//
// The json tags are the guest wire: this struct is marshalled straight into
// GuestHealthcheckPath, so fused's config shape and this one are the same
// shape by construction.
type HealthcheckSpec struct {
	HTTP *HealthcheckHTTP `json:"http,omitempty"`
	Exec []string         `json:"exec,omitempty"`

	IntervalSeconds    int64 `json:"interval_seconds,omitempty"`
	TimeoutSeconds     int64 `json:"timeout_seconds,omitempty"`
	Retries            int   `json:"retries,omitempty"`
	StartPeriodSeconds int64 `json:"start_period_seconds,omitempty"`
}

// HealthcheckHTTP is an HTTP GET against a port inside the guest. The probe
// runs in-guest, so the port needs no expose entry.
type HealthcheckHTTP struct {
	Port int    `json:"port"`
	Path string `json:"path,omitempty"`
}

// HealthState is the verdict of the environment-level probe.
//
// It is deliberately NOT a VMState. VMState is exactly provisioning, running,
// draining and destroying plus the synthetic wire terminals, and every
// consumer of IsSettledState in the CLI and the three SDKs reasons about that
// closed set. A failing probe reports; it does not move an environment
// somewhere new, and nothing tears a VM down for it.
type HealthState string

const (
	// HealthStarting means the probe has not passed yet and is still inside
	// its start period, so failures are not being counted.
	HealthStarting HealthState = "starting"
	// HealthPassing means the most recent attempt succeeded.
	HealthPassing HealthState = "passing"
	// HealthFailing means the probe has failed `retries` times in a row after
	// the start period ended.
	HealthFailing HealthState = "failing"
)

// HealthStatus is one verdict as reported by the guest agent.
//
// It is live state, not durable state: it is never persisted, and an
// orchestrator restart simply re-reads it from the guest on the next tick.
// Persisting a liveness signal would only let a stale one outlive the thing it
// described.
type HealthStatus struct {
	State HealthState `json:"state"`
	// Since is when the probe entered State, so a consumer can tell a
	// long-standing failure from one that just started.
	Since time.Time `json:"since,omitempty"`
	// Failures is the count of consecutive failed attempts. It is reset by
	// any success, so a passing probe always reports zero.
	Failures int `json:"failures,omitempty"`
	// Message is the last attempt's failure detail (a non-2xx status, a
	// connection error, a non-zero exit), empty while passing.
	Message string `json:"message,omitempty"`
}

// healthPollTimeout bounds the single guest read reconcileHealth performs per
// VM. It is short on purpose: the read is a `cat` of a file the guest agent
// has already written, so anything slower than this is a sick guest rather
// than a slow one, and the tick must not be held up by it.
const healthPollTimeout = 5 * time.Second

// reconcileHealth reads each running VM's health verdict back from its guest
// and records it on the vm.
//
// Only VMs whose create request carried a healthcheck are polled, so a fleet
// that declares none pays nothing. The read is one exec per VM per tick,
// issued concurrently because the VMs share no state and paying their round
// trips one after another would make the pass scale with fleet size.
//
// A read that fails is not a failing probe: an unreachable guest could equally
// be a host agent hiccup, and this pass does not have the two-strike structure
// the destructive detectors have. The last known verdict is left in place and
// the next tick tries again.
func (fm *FleetManager) reconcileHealth(ctx context.Context, summary *ReconcileSummary) {
	type candidate struct {
		vmID string
		env  Environment
	}

	fm.mu.RLock()
	candidates := make([]candidate, 0, len(fm.vms))
	for id, v := range fm.vms {
		if v.state != VMStateRunning || v.healthcheck == nil || v.env == nil {
			continue
		}
		candidates = append(candidates, candidate{vmID: id, env: v.env})
	}
	fm.mu.RUnlock()

	if len(candidates) == 0 {
		return
	}

	type verdict struct {
		vmID   string
		status HealthStatus
	}
	results := make(chan verdict, len(candidates))

	var wg sync.WaitGroup
	for _, c := range candidates {
		wg.Add(1)
		go func(c candidate) {
			defer wg.Done()
			status, ok := readGuestHealth(ctx, c.env)
			if !ok {
				return
			}
			results <- verdict{vmID: c.vmID, status: status}
		}(c)
	}
	wg.Wait()
	close(results)

	for r := range results {
		summary.HealthChecked++
		if r.status.State == HealthFailing {
			summary.HealthFailing++
		}

		fm.mu.Lock()
		v, ok := fm.vms[r.vmID]
		if !ok || v.state != VMStateRunning {
			fm.mu.Unlock()
			continue
		}
		changed := v.health == nil || v.health.State != r.status.State
		status := r.status
		v.health = &status
		fm.mu.Unlock()

		if !changed {
			continue
		}
		fm.logger.Info("environment health changed",
			"vm", r.vmID,
			"state", r.status.State,
			"failures", r.status.Failures,
		)
		fm.appendEventBackground("vm", r.vmID, "vm.health_changed", map[string]any{
			"state":    string(r.status.State),
			"failures": r.status.Failures,
			"message":  r.status.Message,
		})
	}
}

// readGuestHealth reads and decodes the verdict fused leaves in the guest.
// The second return is false whenever no usable verdict came back, which
// covers a transport failure, a guest that has not written the file yet, and a
// file that does not parse. None of those are a failing probe, so none of them
// overwrite the last verdict.
func readGuestHealth(ctx context.Context, env Environment) (HealthStatus, bool) {
	res, err := env.Exec(ctx, []string{"cat", GuestHealthStatePath}, ExecOptions{Timeout: healthPollTimeout})
	if err != nil || res.ExitCode != 0 {
		return HealthStatus{}, false
	}
	var status HealthStatus
	if err := json.Unmarshal(res.Stdout, &status); err != nil || status.State == "" {
		return HealthStatus{}, false
	}
	return status, true
}
