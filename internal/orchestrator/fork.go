package orchestrator

import (
	"context"
	"fmt"
	"time"

	"github.com/folsomintel/fuse/internal/secrets"
)

// releaseForkCapacity gives back the host capacity a fork reserved, for a fork
// that never became a live vm.
func (fm *FleetManager) releaseForkCapacity(hostID string, spec Spec) {
	if hostID == "" {
		return
	}
	fm.mu.Lock()
	fm.deallocateOnHost(hostID, spec)
	fm.mu.Unlock()
}

// abandonFork tears down a fork that was created on the provider but could not
// be finished, releasing both the real microVM (with its tap and forwards) and
// the host capacity reserved for it. the vm is not in fm.vms and was never
// persisted, so those need no cleanup. failures are logged rather than
// returned: the caller is already reporting the error that got us here.
func (fm *FleetManager) abandonFork(ctx context.Context, provider Provider, hostID, vmID string, spec Spec) {
	if err := provider.Destroy(ctx, vmID); err != nil {
		fm.logger.Warn("destroy partially forked vm failed", "vm", vmID, "err", err)
	}
	fm.releaseForkCapacity(hostID, spec)
}

// ForkOptions tunes a ForkEnvironment call. All fields are optional.
type ForkOptions struct {
	// Comment is attached to the seed snapshot when ForkEnvironment
	// creates one (empty ReuseSnapshotID). ignored when reusing an
	// existing snapshot.
	Comment string

	// ReuseSnapshotID selects an existing ready snapshot of the source
	// vm to seed the fork from. empty means snapshot the source first.
	ReuseSnapshotID string
}

// ForkEnvironment creates a brand-new vm seeded from a checkpoint of an
// existing running vm. it obtains a seed snapshot (creating one when
// ReuseSnapshotID is empty), asks a SnapshotForkable provider to build a
// new environment from that checkpoint, registers the new vm as running,
// and records lineage so the fork references its seed snapshot.
//
// lineage is recorded by persisting a SnapshotRecord for the new vm whose
// ParentSnapshotID is the seed snapshot id (state ready). this reuses the
// same store path CreateSnapshot uses (upsertSnapshotRecord) and is
// directly assertable via ListSnapshots / GetSnapshotByID.
//
// the fork is charged to the source's host (a fork is pinned there: the seed
// snapshot's rootfs is host-local) and is given its OWN guest credentials,
// since it boots a copy of the source's disk and would otherwise answer to the
// source's token.
//
// providers that cannot fork do not implement SnapshotForkable, so this reports
// fork as unsupported for them. the firecracker provider implements it; the
// qemu provider deliberately does not (vfio gpu passthrough cannot be
// checkpointed).
func (fm *FleetManager) ForkEnvironment(ctx context.Context, srcVMID string, opts ForkOptions) (string, error) {
	// validate the source vm exists and is running, and resolve the
	// provider under the lock (providerForHost requires fm.mu held),
	// mirroring snapshotEnvironment's resolution.
	fm.mu.RLock()
	src, ok := fm.vms[srcVMID]
	if !ok {
		fm.mu.RUnlock()
		return "", fmt.Errorf("%w: %s", ErrVMNotFound, srcVMID)
	}
	state := src.state
	srcSpec := src.spec
	srcEnv := src.env
	srcHostID := src.hostID
	provider := fm.provider
	if srcHostID != "" {
		if hostProvider, ok := fm.providerForHost(srcHostID); ok {
			provider = hostProvider
		}
	}
	fm.mu.RUnlock()

	if state != VMStateRunning {
		return "", fmt.Errorf("vm %s in state %s: fork requires running", srcVMID, state)
	}

	// gpu environments cannot be forked: a vfio passthrough device cannot be
	// checkpointed (d4). this is checked up front, before the provider is
	// consulted, because it is a property of the ENVIRONMENT, not of the
	// provider's capabilities. it previously lived inside the not-forkable
	// branch below, which only held while no provider implemented
	// SnapshotForkable; the firecracker provider now does, so keying the
	// guardrail off the type assertion would silently stop protecting gpu vms.
	if srcSpec.GPUs > 0 {
		return "", fmt.Errorf("%w: vm %s has a gpu passthrough device: fork is not supported for gpu environments", ErrGPUUnsupported, srcVMID)
	}

	// obtain the seed snapshot: snapshot the source when no existing
	// snapshot was requested, otherwise validate the requested snapshot
	// belongs to the source and is ready.
	var seed SnapshotRecord
	if opts.ReuseSnapshotID == "" {
		created, err := fm.CreateSnapshot(ctx, srcVMID, SnapshotOptions{Comment: opts.Comment})
		if err != nil {
			return "", err
		}
		seed = created
	} else {
		existing, err := fm.GetSnapshotByID(ctx, opts.ReuseSnapshotID)
		if err != nil {
			return "", err
		}
		if existing.VMID != srcVMID {
			return "", fmt.Errorf("%w: %s", ErrSnapshotNotFound, opts.ReuseSnapshotID)
		}
		if existing.State != SnapshotStateReady {
			return "", fmt.Errorf("%w: snapshot %s is %s", ErrSnapshotInvalidState, opts.ReuseSnapshotID, existing.State)
		}
		// a ready record is not proof of an artifact on the host. fork itself
		// writes lineage records (below) that are ready but back no checkpoint,
		// and a snapshot can also be deleted out from under its record. confirm
		// the provider really holds it, the same guard RestoreSnapshot applies,
		// so a doomed fork fails here instead of 404ing deep in the host agent.
		if srcEnv == nil {
			return "", fmt.Errorf("vm %s has no active environment handle", srcVMID)
		}
		if _, found := lookupCheckpoint(ctx, srcEnv, opts.ReuseSnapshotID); !found {
			return "", fmt.Errorf("%w: %s is not present on the host", ErrSnapshotNotFound, opts.ReuseSnapshotID)
		}
		seed = existing
	}

	// a true fork needs a provider that can seed a new environment from a
	// checkpoint. the firecracker provider implements this; the qemu provider
	// deliberately does not (its gpu envs are already rejected above).
	forkable, ok := provider.(SnapshotForkable)
	if !ok {
		return "", fmt.Errorf("provider does not support fork for vm %s", srcVMID)
	}

	// mint a new vm id. vm ids are always fm.prefix + taskID, so
	// synthesise a unique fork task id.
	forkTaskID := "fork-" + NewEventID()
	newVMID := fm.prefix + forkTaskID
	spec := srcSpec
	spec.Name = newVMID
	// placement is cleared, not inherited: a fork is already pinned to the
	// source's host (the seed snapshot's rootfs is host-local) and never goes
	// through Schedule, so carrying the source's placement.host/labels forward
	// would record selectors nothing ever evaluates -- and they could name a
	// different host than the one the fork actually lands on.
	spec.HostID = ""
	spec.Labels = nil

	// a fork is a real vm consuming real resources on the source's host, so it
	// must be charged to that host. the fork does NOT go through Schedule: the
	// seed snapshot's rootfs is host-local, so the fork can only be born where
	// its source lives.
	//
	// this is load-bearing beyond bookkeeping: DestroyVM deallocates by v.spec
	// unconditionally (fleet.go), so a fork that was never allocated would, on
	// destroy, credit back its (source-sized) cpu/ram to a host that never
	// charged for it. the counters clamp at zero, making the drift permanent
	// and leaving the scheduler to overcommit that host from then on.
	// allocateOnHost binds by *vm; forks are gpu-free (rejected above), so a
	// throwaway wrapper carrying only the spec charges cpu/ram/storage with no
	// per-device gpu binding to record.
	fm.mu.Lock()
	if srcHostID != "" {
		fm.allocateOnHost(srcHostID, &vm{spec: spec})
	}
	fm.mu.Unlock()

	newEnv, err := forkable.CreateFromCheckpoint(ctx, spec, srcVMID, seed.SnapshotID)
	if err != nil {
		fm.releaseForkCapacity(srcHostID, spec)
		return "", fmt.Errorf("fork vm %s from snapshot %s: %w", srcVMID, seed.SnapshotID, err)
	}

	// the fork booted a byte copy of the source's disk, so its guest already has
	// the SOURCE's credentials on it (/fuse/auth-token, /fuse/tls/*) and its
	// fused unit auto-starts from that disk holding the SOURCE's identity.
	// leaving it there would both leave this vm's record with no token at all
	// and let two live vms authenticate with one shared secret, so mint fresh
	// credentials for the fork.
	//
	// uploading the files is NOT enough on its own: fused reads --auth-token-file
	// exactly once at process start (cmd/fused/main.go) and has no reloader, so
	// the already-running process would keep serving the source's token. StartAgent
	// is what makes the host agent rewrite the unit's env with the fork's own
	// vm id and restart fused, which is the step that actually takes effect.
	//
	// only the credential files are uploaded, never a full FusedAgentSpec: the
	// manifest and secrets the fork should run with are the ones already on the
	// copied disk, and a nil manifest would clobber them.
	//
	// like Boot, this is a no-op without a 32-byte encryption key (dev mode):
	// the source then had no credentials either, so the fork inherits none and
	// stays consistent with it.
	var encToken []byte
	drainCommand := DefaultFusedDrainCommand
	if len(fm.tokenEncryptionKey) == 32 {
		creds, credErr := secrets.GenerateVMCredentials(newVMID)
		if credErr != nil {
			fm.abandonFork(ctx, provider, srcHostID, newVMID, spec)
			return "", fmt.Errorf("generate credentials for forked vm %s: %w", newVMID, credErr)
		}
		if upErr := uploadFiles(ctx, newEnv, fusedCredentialFiles(creds)); upErr != nil {
			fm.abandonFork(ctx, provider, srcHostID, newVMID, spec)
			return "", fmt.Errorf("upload credentials to forked vm %s: %w", newVMID, upErr)
		}
		setTokenIfSupported(newEnv, creds)
		encToken, err = secrets.EncryptToken(creds.AuthToken, fm.tokenEncryptionKey)
		if err != nil {
			fm.abandonFork(ctx, provider, srcHostID, newVMID, spec)
			return "", fmt.Errorf("encrypt token for forked vm %s: %w", newVMID, err)
		}
		if agentErr := newEnv.StartAgent(ctx, AgentSpec{
			AuthToken:    creds.AuthToken,
			DrainCommand: drainCommand,
		}); agentErr != nil {
			fm.abandonFork(ctx, provider, srcHostID, newVMID, spec)
			return "", fmt.Errorf("restart guest agent on forked vm %s with its own credentials: %w", newVMID, agentErr)
		}
	}

	// register the new vm as running and persist it, mirroring the
	// running-state bookkeeping in ProvisionAndAssign (fleet.go 582-660):
	// env handle, url, state, spec, host, then persistVMByID, task upsert,
	// and publishStateChange.
	now := time.Now()
	v := &vm{
		id:                 newVMID,
		state:              VMStateRunning,
		taskID:             forkTaskID,
		hostID:             srcHostID,
		env:                newEnv,
		url:                newEnv.URL(),
		spec:               spec,
		authTokenEncrypted: encToken,
		drainCommand:       drainCommand,
		createdAt:          now,
		updatedAt:          now,
	}
	fm.mu.Lock()
	fm.vms[newVMID] = v
	fm.mu.Unlock()

	// persisting the running state is load-bearing: roll the in-memory
	// registration back on failure so the map stays consistent with the
	// store (same guard ProvisionAndAssign applies to persistVMByID). the
	// microVM is real by this point, so it has to be torn down too, or it
	// keeps running on the host with nothing tracking it.
	if err := fm.persistVMByID(ctx, newVMID); err != nil {
		fm.mu.Lock()
		delete(fm.vms, newVMID)
		fm.mu.Unlock()
		fm.abandonFork(ctx, provider, srcHostID, newVMID, spec)
		return "", fmt.Errorf("persist forked vm %s running state: %w", newVMID, err)
	}
	// the task record mirrors the running task upsert in ProvisionAndAssign;
	// the vm is already running, so a persist failure here is best-effort.
	if fm.store != nil {
		if err := fm.store.UpsertTask(ctx, TaskRecord{
			TaskID:     forkTaskID,
			VMID:       newVMID,
			RunStatus:  TaskRunRunning,
			AssignedAt: now,
			UpdatedAt:  now,
		}); err != nil {
			fm.logger.Warn("persist forked task running state failed", "vm", newVMID, "task", forkTaskID, "err", err)
		}
	}
	fm.publishStateChange(newVMID, "")

	// record lineage: a ready snapshot record for the new vm whose parent is
	// the seed snapshot. this is the assertable link back to the source's
	// checkpoint and reuses CreateSnapshot's persist path.
	lineageMeta, _ := marshalSnapshotMetadata("", nil)
	lineage := SnapshotRecord{
		SnapshotID:       "fork-seed-" + NewEventID(),
		VMID:             newVMID,
		TaskID:           forkTaskID,
		HostID:           srcHostID,
		TenantID:         snapshotTenantID(forkTaskID, newVMID),
		ParentSnapshotID: seed.SnapshotID,
		Mode:             SnapshotModeAuto,
		State:            SnapshotStateReady,
		Metadata:         lineageMeta,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := fm.upsertSnapshotRecord(ctx, lineage); err != nil {
		fm.logger.Warn("persist fork lineage record failed", "vm", newVMID, "parent", seed.SnapshotID, "err", err)
	}

	fm.appendEvent(ctx, "vm", newVMID, "vm.forked", map[string]any{
		"source_vm_id":     srcVMID,
		"seed_snapshot_id": seed.SnapshotID,
	})

	return newVMID, nil
}
