package orchestrator

import (
	"context"
	"testing"
	"time"
)

// idleFleet builds a fleet with a single Running VM whose last activity is
// back-dated by idleFor, so idle detection can be exercised without waiting.
func idleFleet(t *testing.T, cfg FleetConfig, taskID string, idleFor time.Duration) (*FleetManager, string) {
	t.Helper()
	// A long stuck ceiling keeps the stuck detector out of the way; these
	// tests are only about the idle predicate.
	if cfg.TaskStuckTimeout == 0 {
		cfg.TaskStuckTimeout = 24 * time.Hour
	}
	fm, vmID := fleetWithRunningVM(t, cfg, taskID, idleFor)
	fm.mu.Lock()
	fm.vms[vmID].lastActivityAt = time.Now().Add(-idleFor)
	fm.mu.Unlock()
	return fm, vmID
}

func TestIdleVM_noTimeoutConfiguredIsIgnored(t *testing.T) {
	metrics := &captureMetrics{}
	fm, _ := idleFleet(t, FleetConfig{Metrics: metrics}, "task-1", 10*time.Hour)

	fm.reconcile(context.Background())

	got := metrics.last()
	if got.IdleVMsSuspected != 0 || got.IdleVMsFailed != 0 {
		t.Errorf("idle detection fired with no timeout configured: %+v", got)
	}
}

func TestIdleVM_underTimeoutIsIgnored(t *testing.T) {
	metrics := &captureMetrics{}
	fm, _ := idleFleet(t, FleetConfig{
		DefaultIdleTimeout: 30 * time.Minute,
		Metrics:            metrics,
	}, "task-1", 5*time.Minute)

	fm.reconcile(context.Background())

	got := metrics.last()
	if got.IdleVMsSuspected != 0 || got.IdleVMsFailed != 0 {
		t.Errorf("active VM flagged idle: %+v", got)
	}
}

func TestIdleVM_firstCycleIsSuspectedOnly(t *testing.T) {
	metrics := &captureMetrics{}
	fm, vmID := idleFleet(t, FleetConfig{
		DefaultIdleTimeout: 5 * time.Minute,
		Metrics:            metrics,
	}, "task-1", 30*time.Minute)

	fm.reconcile(context.Background())

	got := metrics.last()
	if got.IdleVMsSuspected != 1 {
		t.Errorf("IdleVMsSuspected = %d, want 1", got.IdleVMsSuspected)
	}
	if got.IdleVMsFailed != 0 {
		t.Errorf("IdleVMsFailed = %d, want 0 on first cycle", got.IdleVMsFailed)
	}

	info, ok := fm.GetVM(vmID)
	if !ok {
		t.Fatal("vm removed prematurely")
	}
	if info.State != VMStateRunning {
		t.Errorf("VM state = %q, want running", info.State)
	}

	fm.mu.RLock()
	strikes := fm.idleStrikes[vmID]
	fm.mu.RUnlock()
	if strikes != 1 {
		t.Errorf("strike count = %d, want 1", strikes)
	}
}

func TestIdleVM_secondCycleTearsDownAndDeadLetters(t *testing.T) {
	store := NewMemoryStateStore()
	metrics := &captureMetrics{}
	fm, vmID := idleFleet(t, FleetConfig{
		DefaultIdleTimeout: 5 * time.Minute,
		StateStore:         store,
		Metrics:            metrics,
	}, "task-1", 30*time.Minute)

	fm.reconcile(context.Background()) // strike 1
	fm.reconcile(context.Background()) // strike 2 -> tear down

	var failed int
	for _, s := range metrics.summaries {
		failed += s.IdleVMsFailed
	}
	if failed != 1 {
		t.Errorf("IdleVMsFailed total = %d, want 1", failed)
	}

	// The teardown path writes the dead-letter entry synchronously but the
	// task record in the background; poll briefly for the task.
	var foundFailed bool
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !foundFailed {
		tasks, err := store.ListTasks(context.Background())
		if err != nil {
			t.Fatalf("list tasks: %v", err)
		}
		for _, task := range tasks {
			if task.TaskID == "task-1" && task.RunStatus == TaskRunFailed {
				foundFailed = true
			}
		}
		if !foundFailed {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !foundFailed {
		t.Error("task-1 not marked failed in state store within deadline")
	}

	entries, err := store.ListDeadLetters(context.Background())
	if err != nil {
		t.Fatalf("list dead letters: %v", err)
	}
	var foundDLQ bool
	for _, e := range entries {
		if e.Kind == DeadLetterIdleTimeout && e.EntityID == vmID {
			foundDLQ = true
			if e.TaskID != "task-1" {
				t.Errorf("DLQ TaskID = %q", e.TaskID)
			}
		}
	}
	if !foundDLQ {
		t.Errorf("idle VM not dead-lettered under the idle kind: %+v", entries)
	}
}

func TestIdleVM_specTimeoutOverridesFleetDefault(t *testing.T) {
	metrics := &captureMetrics{}
	fm, vmID := idleFleet(t, FleetConfig{
		DefaultIdleTimeout: 5 * time.Minute, // default would say "idle"
		Metrics:            metrics,
	}, "task-1", 30*time.Minute)

	fm.mu.Lock()
	fm.vms[vmID].spec.IdleTimeout = 2 * time.Hour
	fm.mu.Unlock()

	fm.reconcile(context.Background())

	got := metrics.last()
	if got.IdleVMsSuspected != 0 || got.IdleVMsFailed != 0 {
		t.Errorf("per-spec idle timeout not honored: %+v", got)
	}
}

func TestIdleVM_specTimeoutAppliesWithoutFleetDefault(t *testing.T) {
	metrics := &captureMetrics{}
	fm, vmID := idleFleet(t, FleetConfig{Metrics: metrics}, "task-1", 30*time.Minute)

	fm.mu.Lock()
	fm.vms[vmID].spec.IdleTimeout = 5 * time.Minute
	fm.mu.Unlock()

	fm.reconcile(context.Background())

	got := metrics.last()
	if got.IdleVMsSuspected != 1 {
		t.Errorf("IdleVMsSuspected = %d, want 1", got.IdleVMsSuspected)
	}
}

func TestIdleVM_activityClearsStrike(t *testing.T) {
	fm, vmID := idleFleet(t, FleetConfig{
		DefaultIdleTimeout: 5 * time.Minute,
	}, "task-1", 30*time.Minute)

	fm.reconcile(context.Background())

	fm.mu.RLock()
	if fm.idleStrikes[vmID] != 1 {
		fm.mu.RUnlock()
		t.Fatal("strike 1 not recorded")
	}
	fm.mu.RUnlock()

	// A caller comes back: the window restarts and the strike is dropped.
	fm.touchActivity(vmID)
	fm.reconcile(context.Background())

	fm.mu.RLock()
	_, present := fm.idleStrikes[vmID]
	fm.mu.RUnlock()
	if present {
		t.Error("strike counter should be cleared once activity is recorded")
	}
	info, ok := fm.GetVM(vmID)
	if !ok || info.State != VMStateRunning {
		t.Errorf("VM should still be running, got %+v (tracked=%v)", info, ok)
	}
}

func TestIdleVM_openAttachSessionIsNeverIdle(t *testing.T) {
	metrics := &captureMetrics{}
	fm, vmID := idleFleet(t, FleetConfig{
		DefaultIdleTimeout: 5 * time.Minute,
		Metrics:            metrics,
	}, "task-1", 30*time.Minute)

	// An attach session is open, and quiet — quiet is not idle.
	fm.openAttachSession(vmID)
	fm.mu.Lock()
	fm.vms[vmID].lastActivityAt = time.Now().Add(-30 * time.Minute)
	fm.mu.Unlock()

	fm.reconcile(context.Background())
	fm.reconcile(context.Background())

	for _, s := range metrics.summaries {
		if s.IdleVMsSuspected != 0 || s.IdleVMsFailed != 0 {
			t.Fatalf("VM with an open attach session flagged idle: %+v", s)
		}
	}

	// Closing the session restarts the window rather than expiring instantly.
	fm.closeAttachSession(vmID)
	fm.reconcile(context.Background())
	got := metrics.last()
	if got.IdleVMsSuspected != 0 {
		t.Errorf("idle window not restarted on session close: %+v", got)
	}

	fm.mu.RLock()
	sessions := fm.vms[vmID].attachSessions
	fm.mu.RUnlock()
	if sessions != 0 {
		t.Errorf("attachSessions = %d, want 0", sessions)
	}
}

func TestIdleVM_nonRunningIsIgnored(t *testing.T) {
	metrics := &captureMetrics{}
	fm, vmID := idleFleet(t, FleetConfig{
		DefaultIdleTimeout: 5 * time.Minute,
		Metrics:            metrics,
	}, "task-1", 30*time.Minute)

	fm.mu.Lock()
	fm.vms[vmID].state = VMStateDraining
	fm.mu.Unlock()

	fm.reconcile(context.Background())

	got := metrics.last()
	if got.IdleVMsSuspected != 0 || got.IdleVMsFailed != 0 {
		t.Errorf("draining VM flagged idle: %+v", got)
	}
	fm.mu.RLock()
	_, present := fm.idleStrikes[vmID]
	fm.mu.RUnlock()
	if present {
		t.Error("strike recorded for a non-running VM")
	}
}
