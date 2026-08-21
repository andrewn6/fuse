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

	// TenantID is the scope a layer artifact is filed under, and it is only
	// consulted when LayerKey is set.
	//
	// It exists because the two halves of the cache have to agree on a scope,
	// and the default one cannot. An ordinary snapshot is filed under
	// snapshotTenantID, which is the task id, and `fuse build` derives a task
	// id that is unique per build on purpose. A layer filed under that is
	// filed under a name no later build can ever ask for, so the cache could
	// never hit.
	//
	// This is a security boundary. The API layer derives it from how the
	// caller authenticated and never from a query param or a request body,
	// because a caller that could name its own scope could read another
	// tenant's cache. See callerTenantID.
	//
	// It is deliberately ignored for non-layer snapshots, so nothing about
	// existing snapshot tenancy or quota accounting moves.
	TenantID string

	// LayerKey is set by `fuse build` when this snapshot is a cacheable setup
	// layer, and is empty for every ordinary snapshot. It is what makes an
	// artifact findable by recipe rather than by id: without it a build
	// artifact can only be named, and a name is not something a later build
	// can derive from the Fusefile it is holding.
	LayerKey string

	// Live asks for guest memory to be captured alongside the rootfs, so the
	// restore resumes the guest instead of cold-booting it.
	//
	// It stays opt-in rather than becoming the default, because the cost is
	// real and paid on every create: the full memory image is written to disk
	// with no sparseness to hide behind, and the guest is paused for as long
	// as that write takes. A caller that does not ask for it, or a provider
	// that cannot do it, keeps getting exactly the disk snapshot it always
	// got.
	//
	// The kind is not carried into the restore path. The host agent records
	// it per snapshot and branches on it there, so RestoreSnapshot needs to
	// know nothing about this field; see LiveSnapshotCapable.
	Live bool
}

// SnapshotFilter narrows ListSnapshotsFiltered results on exact-match
// fields, plus pagination controls.
//
// Limit caps the page size (<=0 defaults to DefaultPageLimit, >MaxPageLimit
// is clamped down to it). Cursor, when non-empty, resumes after the last
// item of a previous page (see ListSnapshotsFiltered's NextCursor return).
type SnapshotFilter struct {
	VMID     string
	TaskID   string
	TenantID string
	State    SnapshotState

	// Mode narrows to one creation mode, e.g. SnapshotModeBuild to list only
	// build artifacts.
	Mode SnapshotMode

	// LayerKey narrows to artifacts materializing one setup step. Unlike Name
	// below, this is a real indexed column rather than a key read out of the
	// metadata blob: it is an equality lookup performed on every build, so it
	// cannot afford to be a JSON probe over every row. Note that listing is
	// not the hot path for it either; see StateStore.FindSnapshotByLayerKey.
	LayerKey string

	// Arch narrows to artifacts built on one architecture. It is a separate
	// field from LayerKey rather than folded into it because the key is
	// derived client-side before any host is scheduled, so arch is only known
	// after the build lands somewhere. It constrains what may be served, not
	// what the recipe is.
	Arch string

	// Name matches the "name" key inside the snapshot's metadata blob. It is
	// how a build artifact is found by something other than its random id.
	// Metadata is free-form JSON, so an absent key or a malformed blob simply
	// does not match rather than erroring.
	Name string

	Limit  int
	Cursor string
}

// snapshotCursor is the opaque cursor payload for snapshot listing. It
// mirrors sortSnapshotRecords' sort key (CreatedAt desc, SnapshotID desc as
// tiebreak) so pagination and the existing display order stay consistent.
type snapshotCursor struct {
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
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
	// The arch stamped on an artifact is the arch of the host that actually
	// built it, read here from the fleet's own host registry. It is never
	// taken from the caller: a client derives its layer keys before any host
	// is scheduled, so a client-supplied arch is a guess, and a wrong guess
	// labels an artifact with an arch it was not built on, which later hands
	// someone a rootfs they cannot boot. Normalized exactly as the scheduler
	// normalizes it, so a stored value and a lookup value cannot disagree
	// over "x86_64" versus "amd64". Single-provider mode has no host records
	// at all, so arch stays empty there rather than being invented.
	arch := ""
	if h, ok := fm.hosts[hostID]; ok {
		arch = hostArch(h)
	}
	fm.mu.RUnlock()

	// OPEN QUESTION, deliberately not answered here: a paused vm is arguably
	// the ideal snapshot source, especially for a live snapshot, since the
	// guest is already stopped and the agent would not have to pause it. This
	// guard rejects it anyway, and the reason is that there is no paused state
	// to allow: VMState has no paused member, because the only pause the fleet
	// ever performs is the one inside a live snapshot, which the agent opens
	// and closes within a single call and which the orchestrator never
	// observes. Answering "should a paused vm be snapshottable" therefore
	// means first deciding whether pause is a fleet-visible state at all.
	// Behaviour is unchanged here; the question is recorded, not settled.
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

	// a layer is filed under the caller's authenticated scope; everything else
	// keeps the task-derived scope it has always had. gating on LayerKey rather
	// than on TenantID being non-empty is deliberate: a master-token caller
	// authenticates as the empty scope, and treating empty as "unset" would
	// send exactly that caller, the default one, back to a per-build task id
	// that no later build can name.
	tenantID := snapshotTenantID(taskID, vmID)
	if opts.LayerKey != "" {
		tenantID = opts.TenantID
	}
	// Quota is enforced on the honest size, live snapshots included. A live
	// snapshot's SizeBytes carries the whole memory image, so one of them
	// consumes roughly mem_size_mib MORE per create than a disk snapshot of
	// the same vm: a 4 GiB guest turns a ~1 GiB artifact into a ~5 GiB one.
	// That is not special-cased out, because the bytes are really on the host
	// and a quota that under-reports them is a quota that lets a tenant fill a
	// disk it was accounted as not filling.
	//
	// The consequence is for operators, not for this code: every existing
	// per-tenant byte quota was calibrated against rootfs-only snapshots, so a
	// tenant that starts taking live snapshots will hit an unchanged limit far
	// sooner than before. Those limits need re-tuning alongside enabling this.
	if err := fm.enforceSnapshotQuota(ctx, tenantID); err != nil {
		return SnapshotRecord{}, err
	}

	vmSnapshots, err := fm.loadSnapshotsByVM(ctx, vmID)
	if err != nil {
		return SnapshotRecord{}, err
	}
	parentSnapshotID := latestReadySnapshotID(vmSnapshots, vmID)
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
	// The live path is a second, narrower capability rather than a flag on
	// Checkpoint, and it is asserted AFTER the SnapshotCapable block above so
	// a gpu env still fails with the gpu explanation: a gpu env implements
	// neither interface, and "your gpu cannot be checkpointed at all" is the
	// useful answer to `--live` on one, not "live snapshots are unsupported".
	var created Checkpoint
	if opts.Live {
		lc, ok := env.(LiveSnapshotCapable)
		if !ok {
			return SnapshotRecord{}, fmt.Errorf("%w: provider does not support live snapshots for vm %s: retry without live to take a disk snapshot", ErrSnapshotUnsupported, vmID)
		}
		created, err = lc.CheckpointLive(ctx, opts.Comment)
		if err != nil {
			return SnapshotRecord{}, fmt.Errorf("live checkpoint %s: %w", vmID, err)
		}
	} else {
		id, digest, cErr := checkpointWithDigest(ctx, sc, opts.Comment)
		if cErr != nil {
			return SnapshotRecord{}, fmt.Errorf("checkpoint %s: %w", vmID, cErr)
		}
		created = Checkpoint{ID: id, Digest: digest, Kind: SnapshotKindDisk}
	}
	snapshotID, digest := created.ID, created.Digest
	// The kind is whatever the provider says it wrote, not what was asked
	// for: an agent too old to know about live snapshots answers a live
	// request with a disk snapshot, and recording the request instead of the
	// result would file a cold-booting artifact as a resumable one.
	kind := created.Kind
	if kind == "" {
		kind = SnapshotKindDisk
	}
	checkpoint, _ := lookupCheckpoint(ctx, env, snapshotID)
	sizeBytes := checkpoint.SizeBytes
	if sizeBytes == 0 {
		// The listing stays the primary source so nothing about existing disk
		// snapshot accounting moves. The create response only fills the gap it
		// leaves: an agent that lists snapshots without sizes would otherwise
		// record a live artifact, memory image and all, as zero bytes and hand
		// the quota a number it knows to be wrong.
		sizeBytes = created.SizeBytes
	}

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
		LayerKey:         opts.LayerKey,
		Arch:             arch,
		Digest:           digest,
		Kind:             kind,
		State:            SnapshotStateCreating,
		SizeBytes:        sizeBytes,
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
		"size_bytes":         sizeBytes,
		"kind":               string(kind),
	})

	return record, nil
}

// ListSnapshots returns all snapshots known to the state store for the
// given VM ID, newest first. Returns an empty slice when the VM exists
// but has no snapshots. Scoped to one VM's snapshots, so it is not the
// full-table scan ListSnapshotsFiltered can be; it does not paginate.
func (fm *FleetManager) ListSnapshots(ctx context.Context, vmID string) ([]SnapshotRecord, error) {
	fm.mu.RLock()
	_, ok := fm.vms[vmID]
	fm.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrVMNotFound, vmID)
	}

	out, err := fm.loadSnapshotsByVM(ctx, vmID)
	if err != nil {
		return nil, err
	}
	sortSnapshotRecords(out)
	return out, nil
}

// ListSnapshotsFiltered returns one page of snapshots across the fleet
// filtered by optional exact-match fields, newest first (see
// sortSnapshotRecords). Missing resources yield an empty list. It returns
// the page, a next-page cursor (empty when there are no more results), and
// an error only for a malformed cursor or a store failure.
//
// When filter.VMID is set, the underlying lookup is scoped to that VM at
// the store layer instead of loading every snapshot in the fleet.
func (fm *FleetManager) ListSnapshotsFiltered(ctx context.Context, filter SnapshotFilter) ([]SnapshotRecord, string, error) {
	var (
		all []SnapshotRecord
		err error
	)
	if filter.VMID != "" {
		all, err = fm.loadSnapshotsByVM(ctx, filter.VMID)
	} else {
		all, err = fm.loadSnapshots(ctx)
	}
	if err != nil {
		return nil, "", err
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
		// empty means "do not filter", never "match snapshots whose layer key
		// or arch is empty": every non-layer snapshot in the fleet has both
		// unset, so treating empty as a value would match all of them.
		if filter.LayerKey != "" && s.LayerKey != filter.LayerKey {
			continue
		}
		if filter.Arch != "" && s.Arch != filter.Arch {
			continue
		}
		if filter.Name != "" && snapshotMetadataString(s.Metadata, "name") != filter.Name {
			continue
		}
		out = append(out, s)
	}
	sortSnapshotRecords(out)

	limit := clampLimit(filter.Limit)
	start := 0
	if filter.Cursor != "" {
		var c snapshotCursor
		if err := decodeCursor(filter.Cursor, &c); err != nil {
			return nil, "", err
		}
		cursorRec := SnapshotRecord{CreatedAt: c.CreatedAt, SnapshotID: c.ID}
		start = sort.Search(len(out), func(i int) bool { return snapshotRecordLess(cursorRec, out[i]) })
	}

	end := len(out)
	var next string
	if start+limit < end {
		end = start + limit
		last := out[end-1]
		next, _ = encodeCursor(snapshotCursor{CreatedAt: last.CreatedAt, ID: last.SnapshotID})
	}
	if start > end {
		start = end
	}
	return out[start:end], next, nil
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

	// Same open question as CreateSnapshot's guard, from the other side:
	// restoring a live snapshot ends with a resumed guest whatever the vm was
	// doing beforehand, so "must be running" here is really "must be a live vm
	// the agent can act on", not a statement about the guest being unpaused.
	// Left as is deliberately, for the same reason: there is no fleet-visible
	// paused state to admit.
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

// enforceSnapshotQuota checks a tenant's in-flight/ready snapshot usage
// against the configured count/byte ceilings. It asks the state store for a
// scoped count+bytes aggregate (SnapshotQuotaUsage) instead of loading
// every snapshot in the fleet — this runs on every CreateSnapshot call, so
// a full-table scan here would cost every create, quota check, and
// reconcile tick.
func (fm *FleetManager) enforceSnapshotQuota(ctx context.Context, tenantID string) error {
	if fm.snapshotQuotaMaxCount <= 0 && fm.snapshotQuotaMaxBytes <= 0 {
		return nil
	}
	if fm.store == nil {
		return nil
	}

	usage, err := fm.store.SnapshotQuotaUsage(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("snapshot quota usage for tenant %s: %w", tenantID, err)
	}

	if fm.snapshotQuotaMaxCount > 0 && usage.Count >= fm.snapshotQuotaMaxCount {
		return fmt.Errorf("%w: tenant %s exceeds max snapshot count (%d)", ErrSnapshotQuotaExceeded, tenantID, fm.snapshotQuotaMaxCount)
	}
	if fm.snapshotQuotaMaxBytes > 0 && usage.Bytes >= fm.snapshotQuotaMaxBytes {
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

// loadSnapshotsByVM is loadSnapshots scoped to one VM. It is what
// CreateSnapshot uses to find the parent lineage snapshot, so that call
// does not pull the whole fleet's snapshots on every create.
func (fm *FleetManager) loadSnapshotsByVM(ctx context.Context, vmID string) ([]SnapshotRecord, error) {
	if fm.store == nil {
		return []SnapshotRecord{}, nil
	}
	all, err := fm.store.ListSnapshotsByVM(ctx, vmID)
	if err != nil {
		return nil, fmt.Errorf("list snapshots for vm %s: %w", vmID, err)
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

// checkpointWithDigest takes the checkpoint and returns the artifact digest
// alongside its id. The digest is best-effort by construction: it comes from
// the host agent's snapshot-create response, and an agent build that predates
// artifact hashing simply does not send one. That must not fail the snapshot:
// an operator running an older agent still gets a working artifact, it just
// carries no integrity value for a later transfer to check itself against.
// Backends that cannot hash at all (the in-memory stub, qemu) do not implement
// SnapshotDigester and land on the same empty digest.
func checkpointWithDigest(ctx context.Context, sc SnapshotCapable, comment string) (string, string, error) {
	if d, ok := sc.(SnapshotDigester); ok {
		cp, err := d.CheckpointWithDigest(ctx, comment)
		if err != nil {
			return "", "", err
		}
		return cp.ID, cp.Digest, nil
	}
	id, err := sc.Checkpoint(ctx, comment)
	if err != nil {
		return "", "", err
	}
	return id, "", nil
}

// ResolveLayer returns the newest ready artifact materializing layerKey for
// (tenantID, arch), or ok=false when the cache is cold for that key.
//
// A miss is an ordinary outcome, not a failure: the first build of any recipe
// misses every key in its chain, and a caller walks its chain deepest-first
// probing one key at a time, so misses are the common case rather than the
// exceptional one.
//
// tenantID is a security boundary. An artifact carries whatever the build
// baked into it, so it is only ever served back to the tenant that built it;
// the store enforces that and a caller cannot widen it.
//
// This does not go through ListSnapshotsFiltered because that loads every
// snapshot in the fleet and filters in Go, and this runs once per chain key on
// every build.
func (fm *FleetManager) ResolveLayer(ctx context.Context, tenantID, layerKey, arch string) (SnapshotRecord, bool, error) {
	if fm.store == nil {
		return SnapshotRecord{}, false, nil
	}
	record, ok, err := fm.store.FindSnapshotByLayerKey(ctx, tenantID, layerKey, arch)
	if err != nil {
		return SnapshotRecord{}, false, fmt.Errorf("resolve layer %s: %w", layerKey, err)
	}
	if ok {
		// A hit is a use, and this is the only place the system ever learns of
		// one. A caller resolves here and creates in a separate request, so
		// between the two the artifact has no referencing environment at all;
		// without this the collector would see the hottest layer in the cache as
		// the most obviously unused thing in it. See artifactUseGrace.
		fm.touchArtifact(record.SnapshotID)
	}
	return record, ok, nil
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

// snapshotRecordLess reports whether a sorts strictly before b in the
// order sortSnapshotRecords produces (newest first, snapshot id descending
// as a tiebreak for equal timestamps). Shared with ListSnapshotsFiltered's
// cursor search so pagination boundaries line up exactly with display
// order.
func snapshotRecordLess(a, b SnapshotRecord) bool {
	if a.CreatedAt.Equal(b.CreatedAt) {
		return a.SnapshotID > b.SnapshotID
	}
	return a.CreatedAt.After(b.CreatedAt)
}

func sortSnapshotRecords(records []SnapshotRecord) {
	sort.Slice(records, func(i, j int) bool { return snapshotRecordLess(records[i], records[j]) })
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
