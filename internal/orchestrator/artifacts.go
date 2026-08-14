package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Content-addressed build artifacts, and moving them between hosts.
//
// An artifact is a rootfs file sitting in one host agent's snapshot store. That
// is the whole reason this file exists: an environment seeded from an artifact
// could previously only be born on the one host that happened to hold it, so a
// cache hit could turn a working `fuse up` into a failing one the moment that
// host filled up. The fix is to make the artifact follow the workload instead
// of the workload following the artifact.
//
// Three properties are load-bearing and none of them are negotiable.
//
// The orchestrator is the INDEX and the COORDINATOR, never the data path.
// Artifacts run from a few hundred megabytes to tens of gigabytes, and this
// control plane is a single process with a single replica. Relaying blobs
// through it would make one goroutine's copy loop the bandwidth ceiling for the
// entire fleet, and would put a multi-gigabyte transfer in the same process
// that has to answer health checks. Bytes go host to host; the orchestrator
// only says who should fetch what from whom, and hands over a capability to do
// it (see internal/hostwire.MintArtifactGrant).
//
// The index is DERIVED from snapshot records, not stored alongside them. A
// snapshot row already asserts "host H holds artifact A"; a second copy of that
// fact could only ever disagree with the first, and the disagreement would stay
// invisible until a pull was aimed at a host that no longer had the bytes.
// Deriving also answers the recovery question by making it vacuous: there is
// nothing to rehydrate at startup, because the query IS the index. Losing the
// mapping (a row whose digest was never recorded, from an agent build that
// predates hashing) loses only findability -- the artifact is still on disk and
// still bootable by id, and its digest is recoverable by rehashing the file.
//
// A digest is an INTEGRITY CHECK on one artifact's bytes and never a
// cross-build identity. Two builds of the same recipe produce different rootfs
// bytes (timestamps, inode ordering, package caches), so rows that share a
// digest are copies of a single artifact, which is precisely what a peer pull
// creates. Nothing here dedups, and nothing may start.

// ArtifactEndpoint identifies one side of a host-to-host artifact transfer.
// Token is the host agent's own bearer token, which the orchestrator already
// stores in order to talk to that host at all.
type ArtifactEndpoint struct {
	HostID string
	URL    string
	Token  string
}

// ArtifactMove describes one transfer: fetch Digest from From, and commit it on
// To under SnapshotID.
//
// SnapshotID is the id the RECEIVING host stores it under, and it is chosen by
// the orchestrator rather than the agent. Committing under an id the control
// plane already knows is what makes a pulled artifact indistinguishable from a
// locally created one: a later create with that seed id resolves it out of the
// receiving agent's snapshot store with no special case anywhere.
type ArtifactMove struct {
	Digest     string
	SnapshotID string
	From       ArtifactEndpoint
	To         ArtifactEndpoint
}

// ArtifactMoved is what the receiving host reports once the artifact is
// verified and committed.
type ArtifactMoved struct {
	SnapshotID string
	SizeBytes  int64
}

// ArtifactMover performs one host-to-host artifact transfer.
//
// It is an interface on FleetConfig rather than a direct call into
// internal/hostwire because hostwire imports this package (for
// HTTPStatusError), so depending on it here would be an import cycle. The
// concrete implementation lives at the composition root, alongside the
// host provider factory, and is the only thing in the system that holds both
// hosts' agent tokens at once.
//
// Implementations MUST mint the grant with the SERVING host's token
// (hostwire.MintArtifactGrant) and present only that grant to the serving host.
// Handing the pulling host a bearer token for the serving host would trade one
// blob read for full control of that host.
//
// A nil mover means artifacts cannot move. That is a degraded mode, not an
// error: placement falls back to pinning the workload to the host that already
// holds the artifact, which is exactly the behaviour that predates this file.
type ArtifactMover interface {
	MoveArtifact(ctx context.Context, move ArtifactMove) (ArtifactMoved, error)
}

// Artifact failures at the API boundary.
//
// The two "cannot place this" cases wrap ErrNoCapacity on purpose. They ARE
// capacity outcomes from the caller's point of view (the fleet cannot host this
// environment right now), the handler layer already maps ErrNoCapacity to 503,
// and 503 is the honest answer: retrying later may well succeed. Wrapping is
// also what keeps the mapping in one place rather than requiring every new
// boundary condition to be threaded through the HTTP layer.
var (
	// ErrSeedUnplaceable is returned when no host in the fleet can take an
	// environment seeded from an artifact: not the host holding it, not any
	// other host holding a copy, and not any host the artifact could be moved
	// to. The wrapping context names each rung that was tried, because the
	// operator asked for an environment and needs to know which of the three
	// reasons stopped it.
	ErrSeedUnplaceable = fmt.Errorf("%w: no host can take an environment seeded from this artifact", ErrNoCapacity)

	// ErrArtifactImmovable is returned when an artifact cannot be copied off
	// the host that holds it: no digest was recorded for it (an agent build
	// that predates artifact hashing, so there is nothing to verify a transfer
	// against), or no ArtifactMover is configured, or no host holding it is
	// still registered with a usable agent URL.
	//
	// This is deliberately not a hard failure on its own. Placement treats an
	// immovable artifact the way it always did: the workload is pinned to the
	// artifact rather than the artifact following the workload.
	ErrArtifactImmovable = fmt.Errorf("%w: artifact cannot be copied to another host", ErrNoCapacity)

	// ErrArtifactTransferFailed is returned when a peer pull was attempted and
	// did not complete. It is NOT wrapped into ErrNoCapacity: the fleet had
	// room, the copy failed, and reporting that as a capacity shortfall would
	// send an operator to look at the wrong thing.
	ErrArtifactTransferFailed = errors.New("artifact transfer failed")
)

// artifactUseGrace is how long an artifact is protected from eviction after it
// was last resolved, seeded from, or pulled.
//
// It exists for one specific race and closes it explicitly. A client resolves a
// cache hit and then, in a separate request, creates an environment from it. In
// between, that artifact has no referencing environment at all, so a
// refcount-driven collector looking only at references would consider it free
// exactly when it is about to be used. A cache that evicts the artifact you
// just resolved is worse than no cache: it converts a fast path into a failure
// that only reproduces under load.
//
// Ten minutes is far longer than a resolve-then-create round trip and far
// shorter than any sane retention window, so it costs nothing to hold and never
// masks a real idle artifact. It applies to BOTH eviction paths, the idle
// sweep and the per-tenant cap, because a cap overflow can otherwise evict a
// just-resolved artifact that happens to be the least recently used.
const artifactUseGrace = 10 * time.Minute

// touchArtifact records that these artifacts were just used. "Used" means
// resolved by a cache lookup, seeded from by a create, or moved between hosts.
//
// Cache hits count, and that is the whole point. An artifact that every build
// hits but no environment holds open is the hottest thing in the cache and the
// first thing a reference-only collector would throw away, which is precisely
// the layer a warm cache depends on.
func (fm *FleetManager) touchArtifact(snapshotIDs ...string) {
	now := time.Now()
	fm.mu.Lock()
	defer fm.mu.Unlock()
	for _, id := range snapshotIDs {
		if id == "" {
			continue
		}
		fm.artifactUse[id] = now
	}
}

// artifactLastUsedLocked returns when an artifact was last used, falling back
// to the record's own updated_at for one this process has not seen used.
//
// The fallback is what makes an orchestrator restart safe: use times are
// in-memory only (persisting them would mean a database write on every cache
// hit, on the hot path of every build), so after a restart every artifact looks
// exactly as old as its record says, which is the same information the store
// had all along. Nothing becomes suddenly collectable because the process
// bounced. Caller must hold fm.mu.
func (fm *FleetManager) artifactLastUsedLocked(record SnapshotRecord) time.Time {
	used := fm.artifactUse[record.SnapshotID]
	if record.UpdatedAt.After(used) {
		return record.UpdatedAt
	}
	return used
}

// beginArtifactPull marks both ends of a transfer as in flight, so the
// collector cannot delete the artifact out from under a copy that is already
// running: the source is being read and the destination is being written, and
// removing either mid-flight leaves a half-written rootfs somebody will later
// boot.
func (fm *FleetManager) beginArtifactPull(snapshotIDs ...string) {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	for _, id := range snapshotIDs {
		if id != "" {
			fm.artifactPulls[id]++
		}
	}
}

// endArtifactPull releases the in-flight marks taken by beginArtifactPull. It
// also stamps a use, so an artifact that has just been copied is not eligible
// for the next sweep purely because nothing has referenced it yet.
func (fm *FleetManager) endArtifactPull(snapshotIDs ...string) {
	now := time.Now()
	fm.mu.Lock()
	defer fm.mu.Unlock()
	for _, id := range snapshotIDs {
		if id == "" {
			continue
		}
		if n := fm.artifactPulls[id] - 1; n > 0 {
			fm.artifactPulls[id] = n
		} else {
			delete(fm.artifactPulls, id)
		}
		fm.artifactUse[id] = now
	}
}

// ArtifactHolder is one host that holds a copy of an artifact, and the snapshot
// id that host stores it under. The id differs per host: a pulled copy is a
// distinct row with a distinct id, because "which host has it" is a per-copy
// fact and a single record has only one host_id.
type ArtifactHolder struct {
	HostID     string
	SnapshotID string
}

// HostsHoldingArtifact returns every host that already holds digest for
// tenantID, keyed by host id.
//
// This is the index. It is a query over snapshot records rather than a
// maintained map, so it cannot drift from the records it describes and needs no
// recovery step at startup. Records with no host id are skipped: they describe
// an artifact whose location is unknown, which is not the same as an artifact
// nobody has.
func (fm *FleetManager) HostsHoldingArtifact(ctx context.Context, tenantID, digest string) (map[string]ArtifactHolder, error) {
	if fm.store == nil || digest == "" {
		return nil, nil
	}
	records, err := fm.store.ListSnapshotsByDigest(ctx, tenantID, digest)
	if err != nil {
		return nil, fmt.Errorf("index artifact %s: %w", digest, err)
	}
	out := make(map[string]ArtifactHolder, len(records))
	for _, record := range records {
		if record.HostID == "" {
			continue
		}
		// First writer wins, and ListSnapshotsByDigest orders by snapshot id, so
		// a host holding two rows for the same bytes always resolves to the same
		// one. A retried pull then asks the same peer again instead of spreading
		// half-finished transfers over the fleet.
		if _, seen := out[record.HostID]; seen {
			continue
		}
		out[record.HostID] = ArtifactHolder{HostID: record.HostID, SnapshotID: record.SnapshotID}
	}
	return out, nil
}

// seedPlacement is what placement needs to know about a spec's seed artifact:
// the record itself, which hosts already hold those bytes, and whether the
// artifact can be moved to a host that does not.
type seedPlacement struct {
	record  SnapshotRecord
	holders map[string]ArtifactHolder

	// movable is false when the artifact cannot leave its host, in which case
	// placement keeps the old hard pin rather than scheduling somewhere the
	// artifact can never arrive. immovableReason says why, for the error a
	// failed placement reports.
	movable         bool
	immovableReason string
}

// hostPreference renders the holder set as scheduler hints. A host already
// holding the artifact is preferred because seeding from it is a local
// `cp --reflink=auto` and no network transfer at all, which is a different
// order of magnitude from copying tens of gigabytes between hosts.
//
// Local does not always mean free: --reflink=auto is a copy-on-write clone on
// xfs and btrfs, but silently degrades to a full byte-for-byte copy on a
// filesystem without reflink support, which most ext4 deployments are. Even
// then a local copy beats a network transfer of the same bytes, so the
// preference is still right; it is the size of the win that varies.
func (s seedPlacement) hostPreference() map[string]bool {
	if len(s.holders) == 0 {
		return nil
	}
	out := make(map[string]bool, len(s.holders))
	for hostID := range s.holders {
		out[hostID] = true
	}
	return out
}

// needsMove reports whether placing the workload on hostID requires copying the
// artifact there first.
//
// The holder map is the authority, not the scheduler's decision: when the
// artifact cannot move, no hints were supplied, so the decision would report
// "not local" for a workload that is in fact pinned to the artifact and must
// not trigger a transfer.
func (s seedPlacement) needsMove(hostID string) bool {
	if !s.movable || hostID == "" {
		return false
	}
	_, local := s.holders[hostID]
	return !local
}

// whyImmovable explains, in one clause, why the artifact has to stay where it
// is. It never returns "" so the caller can always say something specific.
func (s seedPlacement) whyImmovable() string {
	if s.immovableReason != "" {
		return s.immovableReason
	}
	return "the artifact cannot be copied to another host"
}

// describe renders the artifact and where its copies are, for the error a
// failed placement reports. Naming the holders is what tells an operator
// whether the fleet is full or whether the only host with these bytes is the
// one that is full.
func (s seedPlacement) describe() string {
	id := s.record.SnapshotID
	if id == "" {
		return "an unresolvable snapshot"
	}
	hosts := make([]string, 0, len(s.holders))
	for hostID := range s.holders {
		hosts = append(hosts, hostID)
	}
	sort.Strings(hosts)
	switch {
	case len(hosts) == 0:
		return fmt.Sprintf("snapshot %s, held by no registered host", id)
	case s.movable:
		return fmt.Sprintf("snapshot %s, held by %v and copyable to any host that fits", id, hosts)
	default:
		return fmt.Sprintf("snapshot %s, held only by %v (%s)", id, hosts, s.whyImmovable())
	}
}

// planSeedPlacement resolves everything placement needs to know about a spec
// that boots from an artifact. It runs before any lock is taken because it
// touches the state store.
//
// A miss on the snapshot record is not fatal here. The API layer already
// resolved and validated the seed before calling in, and a create must not be
// failed by this package for a reason the caller did not ask about; an
// unresolvable record simply degrades to the immovable case, which is the
// behaviour that predates artifact movement.
func (fm *FleetManager) planSeedPlacement(ctx context.Context, spec Spec) seedPlacement {
	plan := seedPlacement{}
	if spec.SeedSnapshotID == "" {
		return plan
	}

	record, err := fm.getSnapshotRecord(ctx, spec.SeedSnapshotID)
	if err != nil {
		plan.immovableReason = fmt.Sprintf("snapshot %s is not in the state store", spec.SeedSnapshotID)
		return plan
	}
	plan.record = record

	holders, err := fm.HostsHoldingArtifact(ctx, record.TenantID, record.Digest)
	if err != nil {
		// The index is an optimisation for placement, not a precondition for it.
		// Failing the create because the index query failed would take the fleet
		// down for a reason the operator did not ask about, so degrade to the
		// pinned behaviour and say so in the log.
		fm.logger.Warn("artifact index lookup failed; falling back to pinning the seed's host",
			"snapshot", record.SnapshotID, "err", err)
		plan.immovableReason = "the artifact index could not be read"
	}
	plan.holders = holders
	if len(plan.holders) == 0 && record.HostID != "" {
		// The record's own host holds it by definition, even when the index
		// cannot see it (no digest recorded, or the query above failed).
		plan.holders = map[string]ArtifactHolder{
			record.HostID: {HostID: record.HostID, SnapshotID: record.SnapshotID},
		}
	}

	switch {
	case plan.immovableReason != "":
	case record.Digest == "":
		// Without a digest there is nothing for the receiving host to verify the
		// bytes against, and an unverified rootfs must never be committed. This
		// is an old-agent condition, not a corruption one.
		plan.immovableReason = fmt.Sprintf("snapshot %s has no recorded digest, so a copy could not be verified", record.SnapshotID)
	case fm.artifactMover == nil:
		plan.immovableReason = "this orchestrator is not configured to move artifacts between hosts"
	default:
		plan.movable = true
	}
	return plan
}

// ensureArtifactOnHost makes an artifact available on hostID and returns the
// snapshot id it is stored under THERE, which is what a create must seed from.
//
// The id is per host by necessity: a snapshot record carries one host_id, so a
// copy on a second host is a second record. It is derived from the digest and
// the host so a retried pull re-commits over its own previous attempt instead
// of accumulating a directory per try.
//
// Failure leaves nothing behind in the fleet's view: no record is written
// unless the receiving agent reported the artifact verified and committed, and
// the agent itself streams to a temp file outside its snapshot store and only
// renames into place on a digest match.
func (fm *FleetManager) ensureArtifactOnHost(ctx context.Context, record SnapshotRecord, hostID string) (string, error) {
	if hostID == "" {
		return "", fmt.Errorf("%w: no target host", ErrArtifactImmovable)
	}
	if record.HostID == hostID {
		return record.SnapshotID, nil
	}
	if record.Digest == "" {
		return "", fmt.Errorf("%w: snapshot %s has no recorded digest", ErrArtifactImmovable, record.SnapshotID)
	}
	if fm.artifactMover == nil {
		return "", fmt.Errorf("%w: no artifact mover is configured", ErrArtifactImmovable)
	}

	holders, err := fm.HostsHoldingArtifact(ctx, record.TenantID, record.Digest)
	if err != nil {
		return "", err
	}
	if holder, ok := holders[hostID]; ok {
		// Already there, under whatever id that host committed it as. Nothing to
		// transfer; this is the common case on the second create against a host
		// that pulled the artifact for the first one.
		fm.touchArtifact(record.SnapshotID, holder.SnapshotID)
		return holder.SnapshotID, nil
	}

	source, target, err := fm.artifactEndpoints(record, holders, hostID)
	if err != nil {
		return "", err
	}

	localID := replicaSnapshotID(record.Digest, hostID)

	// Both ends are held for the duration: the source is being read and the
	// destination is being written, and collecting either mid-flight leaves a
	// torn rootfs that a later create would happily boot.
	fm.beginArtifactPull(record.SnapshotID, source.snapshotID, localID)
	defer fm.endArtifactPull(record.SnapshotID, source.snapshotID, localID)

	pullCtx := ctx
	if fm.artifactPullTimeout > 0 {
		var cancel context.CancelFunc
		pullCtx, cancel = context.WithTimeout(ctx, fm.artifactPullTimeout)
		defer cancel()
	}

	fm.logger.Info("copying artifact between hosts",
		"digest", record.Digest, "from", source.endpoint.HostID, "to", hostID, "snapshot", localID)

	moved, err := fm.artifactMover.MoveArtifact(pullCtx, ArtifactMove{
		Digest:     record.Digest,
		SnapshotID: localID,
		From:       source.endpoint,
		To:         target,
	})
	if err != nil {
		// Cancellation is reported as itself rather than as a transfer failure:
		// the caller gave up, the peer did nothing wrong, and a dead-lettered
		// "transfer failed" would send an operator looking at the network.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("copy artifact %s to host %s: %w", record.Digest, hostID, ctxErr)
		}
		return "", fmt.Errorf("%w: copy artifact %s from host %s to host %s: %w",
			ErrArtifactTransferFailed, record.Digest, source.endpoint.HostID, hostID, err)
	}

	committedID := moved.SnapshotID
	if committedID == "" {
		committedID = localID
	}
	size := moved.SizeBytes
	if size <= 0 {
		size = record.SizeBytes
	}

	now := time.Now()
	metadata, _ := marshalSnapshotMetadata("", map[string]string{
		"source_snapshot_id": record.SnapshotID,
		"source_host_id":     source.endpoint.HostID,
	})
	replica := SnapshotRecord{
		SnapshotID: committedID,
		// No VM: this copy was not checkpointed from anything running here, it
		// arrived over the wire. Leaving VMID empty is what marks it as a
		// free-standing artifact rather than a vm's checkpoint.
		VMID:     "",
		HostID:   hostID,
		TenantID: record.TenantID,
		// ParentSnapshotID is deliberately NOT the source. Lineage means "this
		// snapshot descends from that one", and deleting a snapshot with
		// children is refused; pointing at the source would make every origin
		// artifact undeletable the moment anything pulled a copy of it.
		Mode:      SnapshotModeBuild,
		LayerKey:  record.LayerKey,
		Arch:      record.Arch,
		Digest:    record.Digest,
		State:     SnapshotStateReady,
		SizeBytes: size,
		Metadata:  metadata,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := fm.upsertSnapshotRecord(ctx, replica); err != nil {
		// The bytes are on the host but nothing records them. Failing is the
		// consistent choice: the create that triggered this would fail at its
		// own next persist anyway, and the id is derived from (digest, host) so
		// the next attempt re-commits over the same directory rather than
		// leaving one orphan per try.
		return "", fmt.Errorf("persist pulled artifact %s on host %s: %w", committedID, hostID, err)
	}

	fm.appendEventBackground("host", hostID, "artifact.pulled", map[string]any{
		"digest":             record.Digest,
		"snapshot_id":        committedID,
		"source_host_id":     source.endpoint.HostID,
		"source_snapshot_id": record.SnapshotID,
		"size_bytes":         size,
	})
	return committedID, nil
}

// artifactSource is a chosen peer to pull from, plus the id it holds the
// artifact under (needed only so the collector can hold it for the transfer).
type artifactSource struct {
	endpoint   ArtifactEndpoint
	snapshotID string
}

// artifactEndpoints resolves the peer to pull from and the host to pull to.
//
// A cordoned or draining host is a perfectly good SOURCE. Those states mean
// "send me no new work", not "your disks are unreadable", and refusing to read
// from a draining host is how an operator draining a build host discovers their
// whole layer cache went cold. The TARGET, by contrast, has already cleared
// every scheduling gate before this is called.
func (fm *FleetManager) artifactEndpoints(record SnapshotRecord, holders map[string]ArtifactHolder, targetHostID string) (artifactSource, ArtifactEndpoint, error) {
	fm.mu.RLock()
	defer fm.mu.RUnlock()

	target, ok := fm.hosts[targetHostID]
	if !ok {
		return artifactSource{}, ArtifactEndpoint{}, fmt.Errorf("%w: target host %s is not registered", ErrArtifactImmovable, targetHostID)
	}
	if target.URL == "" || target.Token == "" {
		return artifactSource{}, ArtifactEndpoint{}, fmt.Errorf(
			"%w: host %s has no usable agent url or token", ErrArtifactImmovable, targetHostID)
	}

	// Candidates are sorted so the choice is deterministic across calls: a
	// retried pull then asks the same peer again, which is both easier to reason
	// about in logs and kinder to a peer that has already warmed its page cache.
	candidates := make([]string, 0, len(holders))
	for hostID := range holders {
		if hostID == targetHostID {
			continue
		}
		candidates = append(candidates, hostID)
	}
	sort.Strings(candidates)
	// The record's own host is tried first: it is the copy the caller resolved,
	// so it is the one most likely to actually be there.
	if record.HostID != "" && record.HostID != targetHostID {
		candidates = append([]string{record.HostID}, candidates...)
	}

	seen := make(map[string]bool, len(candidates))
	for _, hostID := range candidates {
		if seen[hostID] {
			continue
		}
		seen[hostID] = true
		h, ok := fm.hosts[hostID]
		if !ok || h.URL == "" || h.Token == "" {
			continue
		}
		snapshotID := record.SnapshotID
		if holder, ok := holders[hostID]; ok {
			snapshotID = holder.SnapshotID
		}
		return artifactSource{
				endpoint:   ArtifactEndpoint{HostID: h.ID, URL: h.URL, Token: h.Token},
				snapshotID: snapshotID,
			}, ArtifactEndpoint{
				HostID: target.ID, URL: target.URL, Token: target.Token,
			}, nil
	}

	return artifactSource{}, ArtifactEndpoint{}, fmt.Errorf(
		"%w: no registered host holding artifact %s can serve it", ErrArtifactImmovable, record.Digest)
}

// replicaSnapshotID names a pulled copy of an artifact on a particular host.
//
// It is derived rather than random so a retried pull re-commits over its own
// previous attempt instead of leaving a directory per try, and so a crash
// between the agent's commit and the orchestrator's record leaves something the
// next attempt reuses rather than an orphan nothing will ever name again.
//
// The shape is constrained by the host agent, which accepts a snapshot id of
// [A-Za-z0-9][A-Za-z0-9._-]{0,127} and turns it into a directory name. Only
// lowercase hex and hyphens are emitted here, so nothing can escape that.
func replicaSnapshotID(digest, hostID string) string {
	short := digest
	if len(short) > 16 {
		short = short[:16]
	}
	sum := sha256.Sum256([]byte(hostID))
	return "art-" + short + "-" + hex.EncodeToString(sum[:4])
}

// reconcileArtifacts collects layer artifacts nothing needs any more, and
// enforces a per-tenant ceiling on how many a tenant may keep.
//
// Why this exists: build artifacts are exempt from retention gc and from the
// snapshot quota, deliberately, because sweeping a named `fuse build` output
// would break every environment that references it with nothing to tell the
// operator why. Per-step layers made that exemption untenable -- one build now
// produces an artifact per cacheable step instead of one for the whole build --
// so layers get a ceiling of their own here while named build artifacts keep
// the explicit-delete-only contract they had.
//
// Scope is therefore narrow and intentional: SnapshotModeBuild artifacts with a
// non-empty LayerKey. Those are machine-derived and reproducible (a miss just
// rebuilds the step), which is exactly what makes them safe to evict and unsafe
// to keep forever.
//
// Both ceilings are off unless configured. Turning eviction on by default would
// mean an orchestrator upgrade silently deleted artifacts an existing fleet was
// relying on, which is not a thing an upgrade may do.
func (fm *FleetManager) reconcileArtifacts(ctx context.Context) {
	if fm.artifactIdleTTL <= 0 && fm.artifactMaxPerTenant <= 0 {
		return
	}
	all, err := fm.loadSnapshots(ctx)
	if err != nil {
		fm.logger.Warn("list snapshots for artifact gc failed", "err", err)
		return
	}
	if len(all) == 0 {
		return
	}

	now := time.Now()
	candidates, protected := fm.classifyArtifacts(all, now)
	fm.pruneArtifactUse(all)

	// Pass one: artifacts nothing references and nothing has used recently.
	collect := make([]SnapshotRecord, 0)
	if fm.artifactIdleTTL > 0 {
		for _, c := range candidates {
			if protected[c.record.SnapshotID] {
				continue
			}
			if now.Sub(c.lastUsed) <= fm.artifactIdleTTL {
				continue
			}
			collect = append(collect, c.record)
			protected[c.record.SnapshotID] = true
		}
	}

	// Pass two: a cheap per-tenant ceiling, evicting least recently used first.
	// It is a backstop for the idle sweep rather than a replacement: a tenant
	// building constantly keeps every layer inside the idle window and would
	// otherwise grow without bound.
	if fm.artifactMaxPerTenant > 0 {
		collect = append(collect, overflowArtifacts(candidates, protected, fm.artifactMaxPerTenant)...)
	}

	for _, record := range collect {
		fm.evictArtifact(ctx, record)
	}
}

// artifactCandidate is one collectable artifact plus the clock the collector
// sorts and thresholds on.
type artifactCandidate struct {
	record   SnapshotRecord
	lastUsed time.Time
}

// classifyArtifacts splits the fleet's snapshots into eviction candidates and
// the set that must never be touched.
//
// The protected set is the safety contract of this whole file, so it is
// computed in one place rather than checked ad hoc at each eviction site:
//
//   - an artifact a tracked VM is seeding from. Destroying the rootfs a running
//     guest was booted from is the one failure nobody recovers from.
//   - an artifact with a child snapshot, matching the lineage rule
//     deleteSnapshotRecord already enforces.
//   - an artifact at either end of an in-flight transfer.
//   - an artifact used inside artifactUseGrace, which is what closes the window
//     between a client resolving a cache hit and creating from it.
func (fm *FleetManager) classifyArtifacts(all []SnapshotRecord, now time.Time) ([]artifactCandidate, map[string]bool) {
	protected := make(map[string]bool)
	for _, snapshot := range all {
		if snapshot.ParentSnapshotID != "" && snapshot.State != SnapshotStateDeleting {
			protected[snapshot.ParentSnapshotID] = true
		}
	}

	fm.mu.RLock()
	defer fm.mu.RUnlock()

	for _, v := range fm.vms {
		if v.spec.SeedSnapshotID != "" {
			protected[v.spec.SeedSnapshotID] = true
		}
	}
	for id, inflight := range fm.artifactPulls {
		if inflight > 0 {
			protected[id] = true
		}
	}

	candidates := make([]artifactCandidate, 0)
	for _, snapshot := range all {
		// Only cache layers. A named build artifact is something an operator
		// created and references by name, so it stays explicit-delete-only.
		if snapshot.Mode != SnapshotModeBuild || snapshot.LayerKey == "" {
			continue
		}
		if snapshot.State != SnapshotStateReady {
			// Creating, restoring, deleting: all mid-transition. Nothing here is
			// urgent enough to race a state machine.
			protected[snapshot.SnapshotID] = true
			continue
		}
		lastUsed := fm.artifactLastUsedLocked(snapshot)
		if now.Sub(lastUsed) < artifactUseGrace {
			protected[snapshot.SnapshotID] = true
			continue
		}
		candidates = append(candidates, artifactCandidate{record: snapshot, lastUsed: lastUsed})
	}
	return candidates, protected
}

// overflowArtifacts returns the least recently used artifacts of every tenant
// that is over the cap.
//
// Protected artifacts are counted against the cap but never evicted: they are
// real disk on a real host, so pretending they do not exist would let a tenant
// hold unlimited artifacts by keeping them warm. When a tenant is over the cap
// and everything above the line is protected, nothing is evicted this cycle.
// Overshooting a soft ceiling for one reconcile tick is strictly better than
// deleting an artifact somebody is about to boot.
func overflowArtifacts(candidates []artifactCandidate, protected map[string]bool, maxPerTenant int) []SnapshotRecord {
	byTenant := make(map[string][]artifactCandidate)
	counts := make(map[string]int)
	for _, c := range candidates {
		counts[c.record.TenantID]++
		if protected[c.record.SnapshotID] {
			continue
		}
		byTenant[c.record.TenantID] = append(byTenant[c.record.TenantID], c)
	}

	out := make([]SnapshotRecord, 0)
	tenants := make([]string, 0, len(byTenant))
	for tenant := range byTenant {
		tenants = append(tenants, tenant)
	}
	// Deterministic tenant order so a test (and an operator reading the log)
	// sees the same evictions for the same fleet state.
	sort.Strings(tenants)
	for _, tenant := range tenants {
		over := counts[tenant] - maxPerTenant
		if over <= 0 {
			continue
		}
		evictable := byTenant[tenant]
		sort.Slice(evictable, func(i, j int) bool {
			if evictable[i].lastUsed.Equal(evictable[j].lastUsed) {
				return evictable[i].record.SnapshotID < evictable[j].record.SnapshotID
			}
			return evictable[i].lastUsed.Before(evictable[j].lastUsed)
		})
		if over > len(evictable) {
			over = len(evictable)
		}
		for _, c := range evictable[:over] {
			out = append(out, c.record)
			protected[c.record.SnapshotID] = true
		}
	}
	return out
}

// pruneArtifactUse drops use timestamps for artifacts that no longer exist, so
// the map cannot grow without bound across the life of the process.
func (fm *FleetManager) pruneArtifactUse(all []SnapshotRecord) {
	live := make(map[string]bool, len(all))
	for _, snapshot := range all {
		live[snapshot.SnapshotID] = true
	}
	fm.mu.Lock()
	defer fm.mu.Unlock()
	for id := range fm.artifactUse {
		if !live[id] && fm.artifactPulls[id] == 0 {
			delete(fm.artifactUse, id)
		}
	}
}

// evictArtifact removes one collected artifact, host-side first and metadata
// second, so a failure never leaves a record pointing at bytes that are gone.
//
// A pulled copy has no origin VM on its host (it arrived over the wire), so
// there is no environment handle to ask for a delete. Those are dropped from
// the store directly. The bytes then rely on the host agent's own artifact
// housekeeping, which is a real gap and is logged as one rather than silently
// treated as reclaimed.
func (fm *FleetManager) evictArtifact(ctx context.Context, record SnapshotRecord) {
	record.State = SnapshotStateDeleting
	record.UpdatedAt = time.Now()
	record.LastError = ""
	if err := fm.upsertSnapshotRecord(ctx, record); err != nil {
		fm.logger.Warn("mark artifact deleting failed", "snapshot", record.SnapshotID, "err", err)
		return
	}

	if record.VMID != "" {
		if err := fm.deleteSnapshotArtifact(ctx, record); err != nil && !IsNotFound(err) {
			record.State = SnapshotStateError
			record.LastError = err.Error()
			record.UpdatedAt = time.Now()
			_ = fm.upsertSnapshotRecord(ctx, record)
			fm.appendEventBackground("vm", record.VMID, "artifact.gc_failed", map[string]any{
				"snapshot_id": record.SnapshotID,
				"error":       err.Error(),
			})
			return
		}
	} else {
		fm.logger.Info("evicting a pulled artifact copy; host-side bytes are reclaimed by the agent",
			"snapshot", record.SnapshotID, "host", record.HostID)
	}

	if fm.store != nil {
		if err := fm.store.DeleteSnapshot(ctx, record.SnapshotID); err != nil {
			fm.logger.Warn("delete artifact metadata during gc failed", "snapshot", record.SnapshotID, "err", err)
			return
		}
	}
	fm.mu.Lock()
	delete(fm.artifactUse, record.SnapshotID)
	fm.mu.Unlock()

	fm.appendEventBackground("host", record.HostID, "artifact.gc_deleted", map[string]any{
		"snapshot_id": record.SnapshotID,
		"layer_key":   record.LayerKey,
		"tenant_id":   record.TenantID,
	})
}
