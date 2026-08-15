package orchestrator

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// healthFleet builds a fleet with one Running VM whose guest replies to the
// health read with body, so the reconcile pass can be exercised without a real
// guest. A nil probe means the environment declared no healthcheck.
func healthFleet(t *testing.T, probe *HealthcheckSpec, body string, exitCode int) (*FleetManager, string, *mockEnv) {
	t.Helper()
	metrics := &captureMetrics{}
	fm, vmID := fleetWithRunningVM(t, FleetConfig{
		TaskStuckTimeout: 24 * time.Hour,
		Metrics:          metrics,
	}, "task-1", time.Minute)

	env := &mockEnv{
		name:       vmID,
		url:        "http://" + vmID + ".test",
		execResult: ExecResult{ExitCode: exitCode, Stdout: []byte(body)},
	}
	fm.mu.Lock()
	fm.vms[vmID].env = env
	fm.vms[vmID].healthcheck = probe
	fm.mu.Unlock()
	return fm, vmID, env
}

// vmHealth returns the verdict recorded on the vm record, or nil.
func vmHealth(fm *FleetManager, vmID string) *HealthStatus {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	return fm.vms[vmID].health
}

func TestReconcileHealth_recordsVerdict(t *testing.T) {
	since := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	body, err := json.Marshal(HealthStatus{State: HealthPassing, Since: since})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	fm, vmID, env := healthFleet(t, &HealthcheckSpec{Exec: []string{"/ready"}}, string(body), 0)

	summary := ReconcileSummary{}
	fm.reconcileHealth(context.Background(), &summary)

	got := vmHealth(fm, vmID)
	if got == nil {
		t.Fatal("no verdict recorded")
	}
	if got.State != HealthPassing {
		t.Errorf("state = %q, want %q", got.State, HealthPassing)
	}
	if !got.Since.Equal(since) {
		t.Errorf("since = %s, want %s", got.Since, since)
	}
	if summary.HealthChecked != 1 || summary.HealthFailing != 0 {
		t.Errorf("summary = %+v, want 1 checked and 0 failing", summary)
	}

	// the read must be a plain cat of the guest's verdict file: anything that
	// went out over the network would be the first control-plane-to-sandbox
	// dial in the codebase.
	env.mu.Lock()
	argv := env.execArgv
	env.mu.Unlock()
	if len(argv) != 1 || argv[0][0] != "cat" || argv[0][1] != GuestHealthStatePath {
		t.Errorf("exec argv = %v, want [cat %s]", argv, GuestHealthStatePath)
	}
}

func TestReconcileHealth_countsFailing(t *testing.T) {
	body, err := json.Marshal(HealthStatus{State: HealthFailing, Failures: 3, Message: "connection refused"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	fm, vmID, _ := healthFleet(t, &HealthcheckSpec{Exec: []string{"/ready"}}, string(body), 0)

	summary := ReconcileSummary{}
	fm.reconcileHealth(context.Background(), &summary)

	if summary.HealthFailing != 1 {
		t.Errorf("failing = %d, want 1", summary.HealthFailing)
	}
	// a failing probe reports and nothing else: the environment stays running
	// and is not torn down for it.
	fm.mu.RLock()
	state := fm.vms[vmID].state
	fm.mu.RUnlock()
	if state != VMStateRunning {
		t.Errorf("state = %q, want %q; a failing probe must not tear anything down", state, VMStateRunning)
	}
}

// a VM created without a healthcheck is never polled, so a fleet that declares
// none pays nothing for this pass.
func TestReconcileHealth_skipsVMsWithoutAProbe(t *testing.T) {
	fm, vmID, env := healthFleet(t, nil, `{"state":"passing"}`, 0)

	summary := ReconcileSummary{}
	fm.reconcileHealth(context.Background(), &summary)

	env.mu.Lock()
	calls := len(env.execArgv)
	env.mu.Unlock()
	if calls != 0 {
		t.Errorf("exec calls = %d, want 0", calls)
	}
	if summary.HealthChecked != 0 {
		t.Errorf("checked = %d, want 0", summary.HealthChecked)
	}
	if vmHealth(fm, vmID) != nil {
		t.Error("verdict recorded for a vm with no probe")
	}
}

// a guest that has not written the file yet, or a read that failed, is not a
// failing probe: the last verdict is left alone rather than being replaced by
// a guess.
func TestReconcileHealth_keepsLastVerdictOnUnreadableGuest(t *testing.T) {
	fm, vmID, _ := healthFleet(t, &HealthcheckSpec{Exec: []string{"/ready"}}, "", 1)
	fm.mu.Lock()
	fm.vms[vmID].health = &HealthStatus{State: HealthPassing}
	fm.mu.Unlock()

	summary := ReconcileSummary{}
	fm.reconcileHealth(context.Background(), &summary)

	got := vmHealth(fm, vmID)
	if got == nil || got.State != HealthPassing {
		t.Errorf("verdict = %+v, want the previous passing verdict", got)
	}
	if summary.HealthChecked != 0 {
		t.Errorf("checked = %d, want 0", summary.HealthChecked)
	}
}

// garbage from the guest must not be recorded as a verdict with an empty
// state, which every consumer would then have to defend against.
func TestReconcileHealth_ignoresUnparseableVerdict(t *testing.T) {
	fm, vmID, _ := healthFleet(t, &HealthcheckSpec{Exec: []string{"/ready"}}, "not json", 0)

	summary := ReconcileSummary{}
	fm.reconcileHealth(context.Background(), &summary)

	if got := vmHealth(fm, vmID); got != nil {
		t.Errorf("verdict = %+v, want nil", got)
	}
}

// the verdict must reach VMInfo, which is what every wire read of an
// environment is built from.
func TestVMInfoCarriesHealth(t *testing.T) {
	fm, vmID, _ := healthFleet(t, &HealthcheckSpec{Exec: []string{"/ready"}}, `{"state":"passing"}`, 0)
	fm.reconcileHealth(context.Background(), &ReconcileSummary{})

	info, ok := fm.GetVM(vmID)
	if !ok {
		t.Fatal("vm not found")
	}
	if info.Health == nil || info.Health.State != HealthPassing {
		t.Fatalf("info.Health = %+v, want passing", info.Health)
	}

	// the snapshot must be a copy: a caller holding it must not observe the
	// next tick rewriting the verdict underneath them.
	info.Health.State = HealthFailing
	if got := vmHealth(fm, vmID); got.State != HealthPassing {
		t.Errorf("mutating the snapshot changed the vm record: %+v", got)
	}
}
