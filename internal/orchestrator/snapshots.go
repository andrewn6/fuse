package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// SnapshotOptions tunes a CreateSnapshot call. All fields are optional.
type SnapshotOptions struct {
	// Comment is a free-form note attached to the snapshot. Passed
	// through to the underlying Environment.Checkpoint implementation
	// and persisted in the snapshot record.
	Comment string

	// Mode records whether this snapshot was taken manually (via the
	// REST API) or by an automated process. Defaults to
	// SnapshotModeManual when empty.
	Mode SnapshotMode

	// RetentionUntil, if non-nil, records when the snapshot is
	// eligible for garbage collection.
	RetentionUntil *time.Time

	// Metadata augments the persisted metadata blob. Reserved fields
	// such as "comment" are set by the orchestrator.
	Metadata map[string]string

	// Exports records optional object-storage export intents/status.
	Exports []SnapshotExportRecord
}

// SnapshotFilter narrows ListSnapshotsFiltered results on exact-match fields.
type SnapshotFilter struct {
	VMID     string
	TaskID   string
	TenantID string
	State    SnapshotState

	// Mode narrows to one creation mode, e.g. SnapshotModeBuild to list only
	// build artifacts.
	Mode SnapshotMode

	// Name matches the "name" key inside the snapshot's metadata blob. It is
	// how a build artifact is found by something other than its random id.
	// Metadata is free-form JSON, so an absent key or a malformed blob simply
	// does not match rather than erroring.
	Name string
}

// CreateSnapshot quiesces the given VM, invokes Environment.Checkpoint,
// persists a first-class SnapshotRecord, and marks it ready once the
// provider confirms the new checkpoint exists.
func (fm *FleetManager) CreateSnapshot(ctx context.Context, vmID string, opts SnapshotOptions) (SnapshotRecord, error) {
	fm.mu.RLock()
	v, ok := fm.vms[vmID]
	if !ok {
		fm.mu.RUnlock()
		return SnapshotRecord{}, fmt.Errorf("%w: %s", ErrVMNotFound, vmID)
	}
	env := v.env
	state := v.state
	taskID := v.taskID
	hostID := v.hostID
	gpus := v.spec.GPUs
	fm.mu.RUnlock()

	if state != VMStateRunning {
		return SnapshotRecord{}, fmt.Errorf("%w: vm %s in state %s: snapshots require running", ErrVMNotRunning, vmID, state)
	}
	if env == nil {
		return SnapshotRecord{}, fmt.Errorf("vm %s has no active environment handle", vmID)
	}

	now := time.Now()
	mode := opts.Mode
	if mode == "" {
		mode = SnapshotModeManual
	}
	retentionUntil := opts.RetentionUntil
	if retentionUntil == nil && fm.defaultSnapshotRetention > 0 {
		t := now.Add(fm.defaultSnapshotRetention)
		retentionUntil = &t
	}

	tenantID := snapshotTenantID(taskID, vmID)
	allSnapshots, err := fm.loadSnapshots(ctx)
	if err != nil {
		return SnapshotRecord{}, err
	}
	if err := fm.enforceSnapshotQuota(allSnapshots, tenantID); err != nil {
		return SnapshotRecord{}, err
	}

	parentSnapshotID := latestReadySnapshotID(allSnapshots, vmID)
	metadataJSON, err := marshalSnapshotMetadata(opts.Comment, opts.Metadata)
	if err != nil {
		return SnapshotRecord{}, fmt.Errorf("marshal snapshot metadata: %w", err)
	}

	sc, ok := env.(SnapshotCapable)
	if !ok {
		// gpu environments run under a backend that passes the device through
		// via vfio, which cannot be checkpointed (d4). the env deliberately
		// omits SnapshotCapable; surface that reason rather than a generic one.
		if gpus > 0 {
			return SnapshotRecord{}, fmt.Errorf("%w: vm %s has a gpu passthrough device: snapshots are not supported for gpu environments", ErrGPUUnsupported, vmID)
		}
		return SnapshotRecord{}, fmt.Errorf("%w: provider does not support snapshots for vm %s", ErrSnapshotUnsupported, vmID)
	}
	snapshotID, err := sc.Checkpoint(ctx, opts.Comment)
	if err != nil {
		return SnapshotRecord{}, fmt.Errorf("checkpoint %s: %w", vmID, err)
	}
	checkpoint, _ := lookupCheckpoint(ctx, env, snapshotID)

	createdAt := now
	if !checkpoint.CreatedAt.IsZero() {
		createdAt = checkpoint.CreatedAt
	}
	record := SnapshotRecord{
		SnapshotID:       snapshotID,
		VMID:             vmID,
		TaskID:           taskID,
		HostID:           hostID,
		TenantID:         tenantID,
		ParentSnapshotID: parentSnapshotID,
		Mode:             mode,
		State:            SnapshotStateCreating,
		SizeBytes:        checkpoint.SizeBytes,
		RetentionUntil:   retentionUntil,
		Metadata:         metadataJSON,
		Exports:          cloneSnapshotExports(opts.Exports),
		CreatedAt:        createdAt,
		UpdatedAt:        now,
	}

	if err := fm.upsertSnapshotRecord(ctx, record); err != nil {
		return SnapshotRecord{}, fmt.Errorf("persist snapshot %s: %w", snapshotID, err)
	}

	record.State = SnapshotStateReady
	record.LastError = ""
	record.UpdatedAt = time.Now()
	if err := fm.upsertSnapshotRecord(ctx, record); err != nil {
		return SnapshotRecord{}, fmt.Errorf("mark snapshot %s ready: %w", snapshotID, err)
	}

	fm.appendEvent(ctx, "vm", vmID, "snapshot.created", map[string]any{
		"snapshot_id":        snapshotID,
		"mode":               string(mode),
		"parent_snapshot_id": parentSnapshotID,
		"size_bytes":         checkpoint.SizeBytes,
	})

	return record, nil
}

// ListSnapshots returns all snapshots known to the state store for the
// given VM ID, newest first. Returns an empty slice when the VM exists
// but has no snapshots.
func (fm *FleetManager) ListSnapshots(ctx context.Context, vmID string) ([]SnapshotRecord, error) {
	fm.mu.RLock()
	_, ok := fm.vms[vmID]
	fm.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrVMNotFound, vmID)
	}

	all, err := fm.loadSnapshots(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]SnapshotRecord, 0, len(all))
	for _, s := range all {
		if s.VMID == vmID {
			out = append(out, s)
		}
	}
	sortSnapshotRecords(out)
	return out, nil
}

// ListSnapshotsFiltered returns snapshots across the fleet filtered by
// optional exact-match fields. Missing resources yield an empty list.
func (fm *FleetManager) ListSnapshotsFiltered(ctx context.Context, filter SnapshotFilter) ([]SnapshotRecord, error) {
	all, err := fm.loadSnapshots(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]SnapshotRecord, 0, len(all))
	for _, s := range all {
		if filter.VMID != "" && s.VMID != filter.VMID {
			continue
		}
		if filter.TaskID != "" && s.TaskID != filter.TaskID {
			continue
		}
		if filter.TenantID != "" && s.TenantID != filter.TenantID {
			continue
		}
		if filter.State != "" && s.State != filter.State {
			continue
		}
		if filter.Mode != "" && s.Mode != filter.Mode {
			continue
		}
		if filter.Name != "" && snapshotMetadataString(s.Metadata, "name") != filter.Name {
			continue
		}
		out = append(out, s)
	}
	sortSnapshotRecords(out)
	return out, nil
}

// GetSnapshot returns one persisted snapshot resource scoped to the VM.
func (fm *FleetManager) GetSnapshot(ctx context.Context, vmID, snapshotID string) (SnapshotRecord, error) {
	fm.mu.RLock()
	_, ok := fm.vms[vmID]
	fm.mu.RUnlock()
	if !ok {
		return SnapshotRecord{}, fmt.Errorf("%w: %s", ErrVMNotFound, vmID)
	}

	record, err := fm.getSnapshotRecord(ctx, snapshotID)
	if err != nil {
		return SnapshotRecord{}, err
	}
	if record.VMID != vmID {
		return SnapshotRecord{}, fmt.Errorf("%w: %s", ErrSnapshotNotFound, snapshotID)
	}
	return record, nil
}

// GetSnapshotByID returns one persisted snapshot resource by its global ID.
func (fm *FleetManager) GetSnapshotByID(ctx context.Context, snapshotID string) (SnapshotRecord, error) {
	return fm.getSnapshotRecord(ctx, snapshotID)
}

// DeleteSnapshot removes a leaf snapshot resource after deleting the
// provider-side artifact.
func (fm *FleetManager) DeleteSnapshot(ctx context.Context, vmID, snapshotID string) error {
	record, err := fm.GetSnapshot(ctx, vmID, snapshotID)
	if err != nil {
		return err
	}
	return fm.deleteSnapshotRecord(ctx, record)
}

// DeleteSnapshotByID removes a leaf snapshot resource by global snapshot ID.
func (fm *FleetManager) DeleteSnapshotByID(ctx context.Context, snapshotID string) error {
	record, err := fm.GetSnapshotByID(ctx, snapshotID)
	if err != nil {
		return err
	}
	return fm.deleteSnapshotRecord(ctx, record)
}

func (fm *FleetManager) deleteSnapshotRecord(ctx context.Context, record SnapshotRecord) error {
	vmID := record.VMID
	snapshotID := record.SnapshotID

	all, err := fm.loadSnapshots(ctx)
	if err != nil {
		return err
	}
	for _, candidate := range all {
		if candidate.ParentSnapshotID == snapshotID && candidate.State != SnapshotStateDeleting {
			return fmt.Errorf("%w: snapshot %s has child %s", ErrSnapshotHasChildren, snapshotID, candidate.SnapshotID)
		}
	}

	record.State = SnapshotStateDeleting
	record.UpdatedAt = time.Now()
	record.LastError = ""
	if err := fm.upsertSnapshotRecord(ctx, record); err != nil {
		return fmt.Errorf("mark snapshot %s deleting: %w", snapshotID, err)
	}

	if err := fm.deleteSnapshotArtifact(ctx, record); err != nil {
		// A 404 means the VM is gone, and the host reclaimed its checkpoints
		// along with it, so the metadata row is all that is left to remove.
		// Returning the error here would strand the record permanently: the
		// VM is never coming back, so no retry can succeed, and because a
		// child pins its parent the whole ancestor chain becomes undeletable.
		if !IsNotFound(err) {
			record.State = SnapshotStateError
			record.LastError = err.Error()
			record.UpdatedAt = time.Now()
			_ = fm.upsertSnapshotRecord(ctx, record)
			return err
		}
		fm.logger.Info("snapshot artifact already gone; dropping metadata record",
			"snapshot", snapshotID, "vm", vmID, "err", err)
	}

	if fm.store != nil {
		if err := fm.store.DeleteSnapshot(ctx, snapshotID); err != nil {
			return fmt.Errorf("delete snapshot %s metadata: %w", snapshotID, err)
		}
	}
	fm.appendEvent(ctx, "vm", vmID, "snapshot.deleted", map[string]any{"snapshot_id": snapshotID})
	return nil
}

// RestoreSnapshot rolls a VM back to a prior snapshot via
// Environment.Restore after validating the metadata record and provider
// visibility of the checkpoint.
func (fm *FleetManager) RestoreSnapshot(ctx context.Context, vmID, snapshotID string) error {
	fm.mu.RLock()
	v, ok := fm.vms[vmID]
	if !ok {
		fm.mu.RUnlock()
		return fmt.Errorf("%w: %s", ErrVMNotFound, vmID)
	}
	env := v.env
	state := v.state
	fm.mu.RUnlock()

	if state != VMStateRunning {
		return fmt.Errorf("%w: vm %s in state %s: restore requires running", ErrVMNotRunning, vmID, state)
	}
	if env == nil {
		return fmt.Errorf("vm %s has no active environment handle", vmID)
	}

	record, err := fm.getSnapshotRecord(ctx, snapshotID)
	if err != nil {
		return err
	}
	if record.VMID != vmID {
		return fmt.Errorf("%w: %s", ErrSnapshotNotFound, snapshotID)
	}
	if record.State != SnapshotStateReady {
		return fmt.Errorf("%w: snapshot %s is %s", ErrSnapshotInvalidState, snapshotID, record.State)
	}
	if _, ok := lookupCheckpoint(ctx, env, snapshotID); !ok {
		record.State = SnapshotStateError
		record.LastError = "snapshot missing from provider"
		record.UpdatedAt = time.Now()
		_ = fm.upsertSnapshotRecord(ctx, record)
		return fmt.Errorf("%w: %s", ErrSnapshotNotFound, snapshotID)
	}

	record.State = SnapshotStateRestoring
	record.LastError = ""
	record.UpdatedAt = time.Now()
	if err := fm.upsertSnapshotRecord(ctx, record); err != nil {
		return fmt.Errorf("mark snapshot %s restoring: %w", snapshotID, err)
	}

	sc, ok := env.(SnapshotCapable)
	if !ok {
		record.State = SnapshotStateError
		record.LastError = "provider does not support snapshots"
		record.UpdatedAt = time.Now()
		_ = fm.upsertSnapshotRecord(ctx, record)
		return fmt.Errorf("%w: restore %s on vm %s: provider does not support snapshots", ErrSnapshotUnsupported, snapshotID, vmID)
	}
	if err := sc.Restore(ctx, snapshotID); err != nil {
		record.State = SnapshotStateError
		record.LastError = err.Error()
		record.UpdatedAt = time.Now()
		_ = fm.upsertSnapshotRecord(ctx, record)
		return fmt.Errorf("restore %s on vm %s: %w", snapshotID, vmID, err)
	}

	record.State = SnapshotStateReady
	record.LastError = ""
	record.UpdatedAt = time.Now()
	if err := fm.upsertSnapshotRecord(ctx, record); err != nil {
		return fmt.Errorf("mark snapshot %s ready after restore: %w", snapshotID, err)
	}

	fm.appendEvent(ctx, "vm", vmID, "snapshot.restored", map[string]any{
		"snapshot_id": snapshotID,
	})

	return nil
}

// RestoreSnapshotByID restores a VM from a snapshot identified by its global ID.
func (fm *FleetManager) RestoreSnapshotByID(ctx context.Context, snapshotID string) error {
	record, err := fm.GetSnapshotByID(ctx, snapshotID)
	if err != nil {
		return err
	}
	return fm.RestoreSnapshot(ctx, record.VMID, record.SnapshotID)
}

func (fm *FleetManager) reconcileSnapshots(ctx context.Context) {
	all, err := fm.loadSnapshots(ctx)
	if err != nil {
		fm.logger.Warn("list snapshots for gc failed", "err", err)
		return
	}
	if len(all) == 0 {
		return
	}

	now := time.Now()
	children := make(map[string]int, len(all))
	latestPerVM := make(map[string]SnapshotRecord)
	for _, snapshot := range all {
		if snapshot.ParentSnapshotID != "" && snapshot.State != SnapshotStateDeleting {
			children[snapshot.ParentSnapshotID]++
		}
		if current, ok := latestPerVM[snapshot.VMID]; !ok || snapshot.CreatedAt.After(current.CreatedAt) {
			latestPerVM[snapshot.VMID] = snapshot
		}
	}

	for _, snapshot := range all {
		if snapshot.State != SnapshotStateDeleting {
			// Build artifacts are removed only on explicit request. Sweeping
			// one silently breaks every `fuse up --from-build` that references
			// it, with nothing to tell the operator why.
			if snapshot.Mode == SnapshotModeBuild {
				continue
			}
			if snapshot.RetentionUntil == nil || snapshot.RetentionUntil.After(now) {
				continue
			}
			if children[snapshot.SnapshotID] > 0 {
				continue
			}
			if latest, ok := latestPerVM[snapshot.VMID]; ok && latest.SnapshotID == snapshot.SnapshotID {
				continue
			}
			snapshot.State = SnapshotStateDeleting
			snapshot.UpdatedAt = now
			snapshot.LastError = ""
			if err := fm.upsertSnapshotRecord(ctx, snapshot); err != nil {
				fm.logger.Warn("mark snapshot deleting for gc failed", "snapshot", snapshot.SnapshotID, "err", err)
				continue
			}
		}

		if err := fm.deleteSnapshotArtifact(ctx, snapshot); err != nil {
			snapshot.State = SnapshotStateError
			snapshot.LastError = err.Error()
			snapshot.UpdatedAt = time.Now()
			_ = fm.upsertSnapshotRecord(ctx, snapshot)
			fm.appendEventBackground("vm", snapshot.VMID, "snapshot.gc_failed", map[string]any{
				"snapshot_id": snapshot.SnapshotID,
				"error":       err.Error(),
			})
			continue
		}

		if fm.store != nil {
			if err := fm.store.DeleteSnapshot(ctx, snapshot.SnapshotID); err != nil {
				fm.logger.Warn("delete snapshot metadata during gc failed", "snapshot", snapshot.SnapshotID, "err", err)
				continue
			}
		}
		fm.appendEventBackground("vm", snapshot.VMID, "snapshot.gc_deleted", map[string]any{
			"snapshot_id": snapshot.SnapshotID,
		})
	}
}

func (fm *FleetManager) enforceSnapshotQuota(all []SnapshotRecord, tenantID string) error {
	if fm.snapshotQuotaMaxCount <= 0 && fm.snapshotQuotaMaxBytes <= 0 {
		return nil
	}

	var (
		count int
		bytes int64
	)
	for _, snapshot := range all {
		if snapshot.TenantID != tenantID {
			continue
		}
		// See reconcileSnapshots: a build artifact is not an ephemeral
		// checkpoint, so it is not charged against the checkpoint quota.
		if snapshot.Mode == SnapshotModeBuild {
			continue
		}
		switch snapshot.State {
		case SnapshotStateCreating, SnapshotStateReady, SnapshotStateRestoring:
			count++
			bytes += snapshot.SizeBytes
		}
	}

	if fm.snapshotQuotaMaxCount > 0 && count >= fm.snapshotQuotaMaxCount {
		return fmt.Errorf("%w: tenant %s exceeds max snapshot count (%d)", ErrSnapshotQuotaExceeded, tenantID, fm.snapshotQuotaMaxCount)
	}
	if fm.snapshotQuotaMaxBytes > 0 && bytes >= fm.snapshotQuotaMaxBytes {
		return fmt.Errorf("%w: tenant %s exceeds max snapshot bytes (%d)", ErrSnapshotQuotaExceeded, tenantID, fm.snapshotQuotaMaxBytes)
	}
	return nil
}

func (fm *FleetManager) deleteSnapshotArtifact(ctx context.Context, snapshot SnapshotRecord) error {
	env, err := fm.snapshotEnvironment(ctx, snapshot)
	if err != nil {
		return err
	}
	deleter, ok := env.(SnapshotDeleter)
	if !ok {
		return fmt.Errorf("environment %s does not support snapshot deletion", snapshot.VMID)
	}
	if err := deleter.DeleteCheckpoint(ctx, snapshot.SnapshotID); err != nil {
		return fmt.Errorf("delete snapshot %s on vm %s: %w", snapshot.SnapshotID, snapshot.VMID, err)
	}
	return nil
}

func (fm *FleetManager) snapshotEnvironment(ctx context.Context, snapshot SnapshotRecord) (Environment, error) {
	fm.mu.RLock()
	if v, ok := fm.vms[snapshot.VMID]; ok && v.env != nil {
		env := v.env
		fm.mu.RUnlock()
		return env, nil
	}
	provider := fm.provider
	if snapshot.HostID != "" {
		if hostProvider, ok := fm.providerForHost(snapshot.HostID); ok {
			provider = hostProvider
		}
	}
	fm.mu.RUnlock()

	if provider == nil {
		return nil, fmt.Errorf("no provider available for snapshot %s", snapshot.SnapshotID)
	}
	env, err := provider.Get(ctx, snapshot.VMID)
	if err != nil {
		return nil, fmt.Errorf("get environment %s for snapshot %s: %w", snapshot.VMID, snapshot.SnapshotID, err)
	}
	return env, nil
}

func (fm *FleetManager) getSnapshotRecord(ctx context.Context, snapshotID string) (SnapshotRecord, error) {
	if fm.store == nil {
		return SnapshotRecord{}, fmt.Errorf("%w: %s", ErrSnapshotNotFound, snapshotID)
	}
	record, err := fm.store.GetSnapshot(ctx, snapshotID)
	if err != nil {
		return SnapshotRecord{}, fmt.Errorf("%w: %s", ErrSnapshotNotFound, snapshotID)
	}
	if record.State == "" {
		record.State = SnapshotStateReady
	}
	return record, nil
}

func (fm *FleetManager) loadSnapshots(ctx context.Context) ([]SnapshotRecord, error) {
	if fm.store == nil {
		return []SnapshotRecord{}, nil
	}
	all, err := fm.store.ListSnapshots(ctx)
	if err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	for i := range all {
		if all[i].State == "" {
			all[i].State = SnapshotStateReady
		}
	}
	return all, nil
}

func (fm *FleetManager) upsertSnapshotRecord(ctx context.Context, record SnapshotRecord) error {
	if fm.store == nil {
		return nil
	}
	if record.State == "" {
		record.State = SnapshotStateReady
	}
	if record.Exports == nil {
		record.Exports = []SnapshotExportRecord{}
	}
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = time.Now()
	}
	return fm.store.UpsertSnapshot(ctx, record)
}

func lookupCheckpoint(ctx context.Context, env Environment, snapshotID string) (Checkpoint, bool) {
	sc, ok := env.(SnapshotCapable)
	if !ok {
		return Checkpoint{}, false
	}
	checkpoints, err := sc.ListCheckpoints(ctx)
	if err != nil {
		return Checkpoint{}, false
	}
	for _, checkpoint := range checkpoints {
		if checkpoint.ID == snapshotID {
			return checkpoint, true
		}
	}
	return Checkpoint{}, false
}

func latestReadySnapshotID(all []SnapshotRecord, vmID string) string {
	var (
		found bool
		best  SnapshotRecord
	)
	for _, snapshot := range all {
		if snapshot.VMID != vmID {
			continue
		}
		switch snapshot.State {
		case SnapshotStateReady, SnapshotStateRestoring:
		default:
			continue
		}
		if !found || snapshot.CreatedAt.After(best.CreatedAt) {
			best = snapshot
			found = true
		}
	}
	if !found {
		return ""
	}
	return best.SnapshotID
}

func sortSnapshotRecords(records []SnapshotRecord) {
	sort.Slice(records, func(i, j int) bool {
		if records[i].CreatedAt.Equal(records[j].CreatedAt) {
			return records[i].SnapshotID > records[j].SnapshotID
		}
		return records[i].CreatedAt.After(records[j].CreatedAt)
	})
}

func snapshotTenantID(taskID, vmID string) string {
	if taskID != "" {
		return taskID
	}
	return vmID
}

// snapshotMetadataString reads one key out of a snapshot's metadata blob.
// marshalSnapshotMetadata always writes a flat map[string]string, so that is
// the shape read back. A missing key, a blob written by an older or
// hand-edited path, or unparseable JSON all yield "" rather than an error:
// metadata is best-effort by construction and must not fail a list call.
func snapshotMetadataString(raw json.RawMessage, key string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	return m[key]
}

func marshalSnapshotMetadata(comment string, extra map[string]string) (json.RawMessage, error) {
	metadata := make(map[string]string, len(extra)+1)
	for k, v := range extra {
		metadata[k] = v
	}
	if comment != "" {
		metadata["comment"] = comment
	}
	if len(metadata) == 0 {
		return json.RawMessage(`{}`), nil
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func cloneSnapshotExports(exports []SnapshotExportRecord) []SnapshotExportRecord {
	if len(exports) == 0 {
		return []SnapshotExportRecord{}
	}
	out := make([]SnapshotExportRecord, len(exports))
	copy(out, exports)
	return out
}
