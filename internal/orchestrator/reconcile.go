package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// reconcileOrphans destroys provider VMs that are not tracked in the fleet.
// Repeated destroy failures are counted per-VM in fm.orphanRetries; once
// the count exceeds orphanDestroyMaxRetries the entry is dead-lettered and
// skipped on subsequent cycles until it disappears from the provider.
func (fm *FleetManager) reconcileOrphans(ctx context.Context, envs []Environment, providers map[string]Provider, tracked map[string]bool, summary *ReconcileSummary) {
	// Collect the current orphan set so we can clear stale retry counters.
	currentOrphans := make(map[string]bool, len(envs))

	for _, e := range envs {
		name := e.Name()
		if tracked[name] {
			continue
		}
		currentOrphans[name] = true

		fm.mu.Lock()
		retries := fm.orphanRetries[name]
		fm.mu.Unlock()

		if retries >= fm.orphanDestroyMaxRetries {
			// Already dead-lettered — log at debug volume and skip.
			fm.logger.Debug("orphan dead-lettered, skipping", "vm", name, "retries", retries)
			continue
		}

		fm.logger.Warn("orphan vm detected, destroying", "vm", name, "attempt", retries+1)
		provider, ok := providers[name]
		if !ok {
			fm.logger.Error("orphan provider not found", "vm", name)
			summary.OrphansFailed++
			continue
		}
		if err := provider.Destroy(ctx, name); err != nil {
			fm.logger.Error("orphan destroy failed", "vm", name, "err", err, "attempt", retries+1)
			summary.OrphansFailed++

			fm.mu.Lock()
			fm.orphanRetries[name] = retries + 1
			attempt := fm.orphanRetries[name]
			fm.mu.Unlock()

			if attempt >= fm.orphanDestroyMaxRetries {
				fm.recordDeadLetter(ctx, DeadLetterRecord{
					Kind:       DeadLetterOrphanDestroy,
					EntityID:   name,
					Reason:     fmt.Sprintf("orphan destroy failed after %d attempts: %v", attempt, err),
					RetryCount: attempt,
					Payload:    dlPayload("last_error", err.Error()),
				})
				summary.OrphansDeadLettered++
				fm.logger.Error("orphan dead-lettered after max retries",
					"vm", name,
					"retries", attempt,
					"err", err,
				)
				fm.appendEventBackground("vm", name, "vm.orphan_dead_lettered", map[string]any{
					"retries": attempt,
					"error":   err.Error(),
				})
			}
			continue
		}

		// Success — clear retry counter.
		fm.mu.Lock()
		delete(fm.orphanRetries, name)
		fm.mu.Unlock()
		summary.OrphansDestroyed++
	}

	// Clear retry counters for orphans that no longer exist in the provider
	// (either because they were destroyed or disappeared on their own).
	fm.mu.Lock()
	for name := range fm.orphanRetries {
		if !currentOrphans[name] {
			delete(fm.orphanRetries, name)
		}
	}
	fm.mu.Unlock()
}

// reconcileStuckTasks flags and tears down VMs in the Running state whose
// age exceeds the configured runtime ceiling. This is a leak detector, not
// a liveness check — see the docstring on FleetConfig.TaskStuckTimeout.
//
// Detection is two-strike: a VM must exceed its timeout on two consecutive
// reconcile cycles before it is failed. Between strikes it is annotated
// with a task.stuck_suspected event.
func (fm *FleetManager) reconcileStuckTasks(ctx context.Context, summary *ReconcileSummary) {
	now := time.Now()

	type stuckCandidate struct {
		vmID     string
		taskID   string
		age      time.Duration
		ceiling  time.Duration
		strikes  int
		needFail bool
	}

	fm.mu.Lock()
	candidates := make([]stuckCandidate, 0)
	live := make(map[string]bool, len(fm.vms))
	for id, v := range fm.vms {
		live[id] = true

		if v.state != VMStateRunning || v.taskID == "" {
			// Reset any strike history — the VM either transitioned or no
			// longer carries a task, so past staleness is no longer meaningful.
			delete(fm.stuckStrikes, id)
			continue
		}

		ceiling := v.spec.MaxRuntime
		if ceiling <= 0 {
			ceiling = fm.taskStuckTimeout
		}
		age := now.Sub(v.createdAt)
		if age <= ceiling {
			delete(fm.stuckStrikes, id)
			continue
		}

		fm.stuckStrikes[id]++
		strikes := fm.stuckStrikes[id]
		candidates = append(candidates, stuckCandidate{
			vmID:     id,
			taskID:   v.taskID,
			age:      age,
			ceiling:  ceiling,
			strikes:  strikes,
			needFail: strikes >= 2,
		})
	}
	// Drop strike entries for VMs that no longer exist.
	for id := range fm.stuckStrikes {
		if !live[id] {
			delete(fm.stuckStrikes, id)
		}
	}
	fm.mu.Unlock()

	for _, c := range candidates {
		if !c.needFail {
			summary.StuckTasksSuspected++
			fm.logger.Warn("vm exceeded runtime ceiling, suspected stuck",
				"vm", c.vmID,
				"task", c.taskID,
				"age", c.age,
				"ceiling", c.ceiling,
				"strike", c.strikes,
			)
			fm.appendEventBackground("task", c.taskID, "task.stuck_suspected", map[string]any{
				"vm_id":   c.vmID,
				"age_s":   int(c.age.Seconds()),
				"ceiling": c.ceiling.String(),
				"strike":  c.strikes,
			})
			continue
		}

		// Second strike — fail the task and mark the VM destroying.
		fm.failStuckTask(ctx, c.vmID, c.taskID, c.age, c.ceiling)
		summary.StuckTasksFailed++
	}
}

// reconcileIdleVMs tears down Running VMs that have gone longer than their
// idle timeout with no exec and no attach session. This is the liveness check
// reconcileStuckTasks is not: it reads vm.lastActivityAt, which only the
// guest-touching entry points in exec.go bump.
//
// "Idle" means no control-plane activity. In-guest CPU use and traffic on
// exposed ports are deliberately not observed — the orchestrator has no
// channel for either — so a VM busy computing with nobody attached will be
// destroyed once its window elapses. Authors opt in per environment via
// resources.idle_timeout; a zero timeout (the default) disables the check.
//
// Detection is two-strike like reconcileStuckTasks, so resolution is bounded
// by roughly two reconcile ticks.
func (fm *FleetManager) reconcileIdleVMs(ctx context.Context, summary *ReconcileSummary) {
	now := time.Now()

	type idleCandidate struct {
		vmID     string
		taskID   string
		idle     time.Duration
		timeout  time.Duration
		strikes  int
		needFail bool
	}

	fm.mu.Lock()
	candidates := make([]idleCandidate, 0)
	live := make(map[string]bool, len(fm.vms))
	for id, v := range fm.vms {
		live[id] = true

		if v.state != VMStateRunning || v.attachSessions > 0 {
			// Not eligible: either the VM is not running, or somebody is
			// attached right now. Reset any strike history so it starts from a
			// clean slate once it becomes eligible again.
			delete(fm.idleStrikes, id)
			continue
		}

		timeout := v.spec.IdleTimeout
		if timeout <= 0 {
			timeout = fm.defaultIdleTimeout
		}
		if timeout <= 0 {
			delete(fm.idleStrikes, id)
			continue
		}

		// A VM recorded before the activity clock existed (or one that
		// somehow missed initialisation) falls back to its create time.
		since := v.lastActivityAt
		if since.IsZero() {
			since = v.createdAt
		}
		idle := now.Sub(since)
		if idle <= timeout {
			delete(fm.idleStrikes, id)
			continue
		}

		fm.idleStrikes[id]++
		strikes := fm.idleStrikes[id]
		candidates = append(candidates, idleCandidate{
			vmID:     id,
			taskID:   v.taskID,
			idle:     idle,
			timeout:  timeout,
			strikes:  strikes,
			needFail: strikes >= 2,
		})
	}
	// Drop strike entries for VMs that no longer exist.
	for id := range fm.idleStrikes {
		if !live[id] {
			delete(fm.idleStrikes, id)
		}
	}
	fm.mu.Unlock()

	for _, c := range candidates {
		if !c.needFail {
			summary.IdleVMsSuspected++
			fm.logger.Warn("vm exceeded idle timeout, suspected idle",
				"vm", c.vmID,
				"task", c.taskID,
				"idle", c.idle,
				"timeout", c.timeout,
				"strike", c.strikes,
			)
			fm.appendEventBackground("vm", c.vmID, "vm.idle_suspected", map[string]any{
				"task_id": c.taskID,
				"idle_s":  int(c.idle.Seconds()),
				"timeout": c.timeout.String(),
				"strike":  c.strikes,
			})
			continue
		}

		// Second strike — tear the environment down.
		fm.failIdleVM(ctx, c.vmID, c.taskID, c.idle, c.timeout)
		summary.IdleVMsFailed++
	}
}

// expiry describes why a VM is being torn down by a reconcile detector. The
// stuck-task and idle detectors differ only in these strings and counters, so
// they share failExpiredVM below.
type expiry struct {
	reasonCode string         // short code carried on events and dead letters
	kind       DeadLetterKind // dead-letter classification
	reason     string         // human-readable, stored on the vm and the task
	age        time.Duration  // observed age or idle duration
	ceiling    time.Duration  // the window that was exceeded
}

// failStuckTask marks a Running VM as destroying and fails its task with
// a "stuck" reason. Shares structure with the "vm missing from provider"
// path in reconcile() but is triggered by the stuck-task detector.
func (fm *FleetManager) failStuckTask(ctx context.Context, vmID, taskID string, age, ceiling time.Duration) {
	fm.failExpiredVM(ctx, vmID, taskID, expiry{
		reasonCode: "stuck_task",
		kind:       DeadLetterStuckTask,
		reason:     fmt.Sprintf("task stuck: runtime %s exceeded ceiling %s with no state transitions", age.Round(time.Second), ceiling),
		age:        age,
		ceiling:    ceiling,
	})
}

// failIdleVM marks a Running VM as destroying because it has gone longer than
// its idle timeout with no exec and no attach session. Takes the same teardown
// path as failStuckTask, with its own reason code.
func (fm *FleetManager) failIdleVM(ctx context.Context, vmID, taskID string, idle, timeout time.Duration) {
	fm.failExpiredVM(ctx, vmID, taskID, expiry{
		reasonCode: "idle_timeout",
		kind:       DeadLetterIdleTimeout,
		reason:     fmt.Sprintf("environment idle: no exec or attach for %s, exceeding idle timeout %s", idle.Round(time.Second), timeout),
		age:        idle,
		ceiling:    timeout,
	})
}

// failExpiredVM marks a Running VM as destroying, fails its task if it carries
// one, dead-letters the reason, and destroys the VM in the background.
func (fm *FleetManager) failExpiredVM(ctx context.Context, vmID, taskID string, e expiry) {
	fm.mu.Lock()
	v, ok := fm.vms[vmID]
	if !ok || v.state != VMStateRunning || v.taskID != taskID {
		fm.mu.Unlock()
		return
	}
	v.state = VMStateDestroying
	v.taskID = ""
	v.err = e.reason
	v.updatedAt = time.Now()
	assignedAt := v.createdAt
	delete(fm.stuckStrikes, vmID)
	delete(fm.idleStrikes, vmID)
	fm.mu.Unlock()

	fm.logger.Error("tearing down expired vm",
		"vm", vmID,
		"task", taskID,
		"reason", e.reasonCode,
		"age", e.age,
		"ceiling", e.ceiling,
	)

	fm.persistVMBackground(vmID)
	fm.appendEventBackground("vm", vmID, "vm.destroying", map[string]any{"reason": e.reasonCode})
	fm.publishStateChange(vmID, "")
	if taskID != "" {
		fm.upsertTaskBackground(TaskRecord{
			TaskID:     taskID,
			VMID:       vmID,
			RunStatus:  TaskRunFailed,
			RetryCount: 0,
			LastError:  e.reason,
			AssignedAt: assignedAt,
			UpdatedAt:  time.Now(),
		})
		fm.appendEventBackground("task", taskID, "task.failed", map[string]any{
			"vm_id":  vmID,
			"error":  e.reason,
			"reason": e.reasonCode,
		})
	}
	fm.recordDeadLetter(ctx, DeadLetterRecord{
		Kind:     e.kind,
		EntityID: vmID,
		TaskID:   taskID,
		Reason:   e.reason,
		Payload: dlPayload(
			"age_s", fmt.Sprintf("%d", int(e.age.Seconds())),
			"ceiling_s", fmt.Sprintf("%d", int(e.ceiling.Seconds())),
		),
	})
	go fm.destroyAndRemove(vmID)
}

// recordDeadLetter upserts a dead-letter entry through the state store.
// Failures are logged but not propagated — the DLQ is best-effort and a
// missing row should not block reconcile progress.
func (fm *FleetManager) recordDeadLetter(ctx context.Context, entry DeadLetterRecord) {
	if fm.store == nil {
		return
	}
	if entry.FirstSeenAt.IsZero() {
		entry.FirstSeenAt = time.Now()
	}
	if entry.LastSeenAt.IsZero() {
		entry.LastSeenAt = entry.FirstSeenAt
	}
	if err := fm.store.UpsertDeadLetter(ctx, entry); err != nil {
		fm.logger.Warn("upsert dead letter failed",
			"kind", entry.Kind,
			"entity", entry.EntityID,
			"err", err,
		)
	}
}

// dlPayload is a small helper for building the key/value JSON blobs
// attached to dead-letter entries. Pairs are passed as alternating key and
// value strings. Malformed input produces an empty object.
func dlPayload(kv ...string) json.RawMessage {
	if len(kv)%2 != 0 {
		return json.RawMessage(`{}`)
	}
	m := make(map[string]string, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	b, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}
