package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

// TaskRunStatus captures the lifecycle of a task assignment.
type TaskRunStatus string

const (
	TaskRunAssigned  TaskRunStatus = "assigned"
	TaskRunRunning   TaskRunStatus = "running"
	TaskRunCompleted TaskRunStatus = "completed"
	TaskRunFailed    TaskRunStatus = "failed"
)

// SnapshotMode captures how a snapshot was created.
type SnapshotMode string

const (
	SnapshotModeManual SnapshotMode = "manual"
	SnapshotModeAuto   SnapshotMode = "auto"

	// SnapshotModeBuild marks a build artifact: a deliberately created,
	// durable rootfs meant to seed future environments (see `fuse build`).
	// Unlike manual and auto snapshots it is not an ephemeral checkpoint of
	// one vm's state, so retention gc and the per-tenant snapshot quota both
	// skip it. That means build artifacts accumulate until deleted
	// explicitly; there is no ceiling on them today.
	SnapshotModeBuild SnapshotMode = "build"
)

// SnapshotState captures the lifecycle of a persisted snapshot resource.
type SnapshotState string

const (
	SnapshotStateCreating  SnapshotState = "creating"
	SnapshotStateReady     SnapshotState = "ready"
	SnapshotStateRestoring SnapshotState = "restoring"
	SnapshotStateDeleting  SnapshotState = "deleting"
	SnapshotStateError     SnapshotState = "error"
)

// SnapshotExportStatus tracks the state of an optional export record.
type SnapshotExportStatus string

const (
	SnapshotExportPending SnapshotExportStatus = "pending"
	SnapshotExportReady   SnapshotExportStatus = "ready"
	SnapshotExportError   SnapshotExportStatus = "error"
)

// SnapshotExportRecord captures metadata for an optional exported artifact.
type SnapshotExportRecord struct {
	Destination string
	Status      SnapshotExportStatus
	RequestedAt time.Time
	UpdatedAt   time.Time
	LastError   string
}

// VMRecord is the durable representation of a fleet VM.
//
// HostID is the loose reference to the placement host (orchestrator_hosts.host_id).
// NetworkHost is the externally-reachable host:port Fuse clients dial; it is
// derived from the provider-returned URL and stored verbatim so reconcile
// can rebuild routing without re-parsing.
//
// SecretsEncrypted holds the per-VM secret map sealed with AES-GCM under
// the orchestrator's TOKEN_ENCRYPTION_KEY. It exists so that an
// orchestrator restart can re-upload the same secrets to the guest agent
// without the caller resubmitting them (the secrets path lives in the
// agent profile); without it, a crash mid-deploy would leave the VM
// running with stale or missing secrets and no way to recover them.
type VMRecord struct {
	ID                 string
	HostID             string
	NetworkHost        string
	State              VMState
	URL                string
	TaskID             string
	TenantID           string
	Spec               Spec
	LastError          string
	AuthTokenEncrypted []byte     // AES-GCM encrypted per-VM auth token (nil for legacy VMs)
	SecretsEncrypted   []byte     // AES-GCM encrypted JSON of the secret map (nil when no secrets supplied)
	Endpoints          []Endpoint // published endpoints (e.g. ingress), if any
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// TaskRecord tracks durable task assignment/run metadata.
type TaskRecord struct {
	TaskID     string
	VMID       string
	RunStatus  TaskRunStatus
	RetryCount int
	LastError  string
	AssignedAt time.Time
	UpdatedAt  time.Time
}

// SnapshotRecord tracks checkpoint lineage and retention metadata.
type SnapshotRecord struct {
	SnapshotID       string
	VMID             string
	TaskID           string
	HostID           string
	TenantID         string
	ParentSnapshotID string
	Mode             SnapshotMode

	// LayerKey is the derived cache key of the setup step this artifact
	// materializes (see internal/fusefile.LayerKeys). It is what separates a
	// layer from the rest of SnapshotModeBuild: layers reuse that mode rather
	// than adding a new one, so a non-empty LayerKey is the only thing that
	// marks a build artifact as cache-addressable. Empty on every snapshot
	// that is not a layer, and empty never matches a lookup.
	LayerKey string

	// Arch is the goarch of the host that actually built this artifact, not
	// of whoever asked for it. It is part of every layer lookup even though it
	// is deliberately absent from LayerKey: an ext4 rootfs is not portable
	// across architectures, but the key is derived client-side before a host
	// is scheduled, so arch can only be observed after the fact and applied as
	// a filter.
	Arch string

	// Digest is the hex sha256 of the artifact rootfs, recorded so a transfer
	// of these bytes can be verified against them. It is an integrity check
	// only and is NOT a cross-build identity: two builds of the same recipe
	// produce different rootfs bytes (timestamps, inode ordering, package
	// caches), so it can never serve as a cache key and nothing dedups on it.
	Digest string

	// Kind records whether this snapshot carries guest memory as well as the
	// rootfs. It is written from what the provider reports it actually wrote,
	// never from what the caller asked for, so a live request served by an
	// agent that could only take a disk snapshot is filed as disk.
	//
	// Empty means disk. Every row predating live snapshots reads back that way
	// (the column defaults to 'disk'), and so does any provider that has no
	// second kind to distinguish, which is all of them but firecracker.
	//
	// Nothing consults this on restore: the host agent keeps its own copy in
	// the snapshot's metadata and branches on that, so this is here for
	// reporting, quota reasoning, and telling an operator why one artifact is
	// five times the size of its neighbour.
	Kind SnapshotKind

	State          SnapshotState
	SizeBytes      int64
	RetentionUntil *time.Time
	Metadata       json.RawMessage
	Exports        []SnapshotExportRecord
	LastError      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// EventRecord stores an audit event for critical lifecycle transitions.
type EventRecord struct {
	ID         int64
	EntityType string
	EntityID   string
	EventType  string
	Payload    json.RawMessage
	CreatedAt  time.Time
}

// DeadLetterKind identifies the kind of failure a dead-letter entry represents.
type DeadLetterKind string

const (
	// DeadLetterOrphanDestroy records repeated failures to destroy an
	// orphan VM observed by reconcile but not tracked in the fleet.
	DeadLetterOrphanDestroy DeadLetterKind = "orphan_destroy"

	// DeadLetterStuckTask records a task that exceeded its runtime ceiling
	// and was torn down by the reconcile loop.
	DeadLetterStuckTask DeadLetterKind = "stuck_task"

	// DeadLetterIdleTimeout records an environment torn down because it went
	// longer than its idle timeout with no exec and no attach session.
	DeadLetterIdleTimeout DeadLetterKind = "idle_timeout"
)

// DeadLetterRecord is a failure the reconciler has given up on retrying.
// Entries are keyed uniquely by (Kind, EntityID); repeated failures update
// the RetryCount and LastSeenAt fields rather than inserting new rows.
type DeadLetterRecord struct {
	ID          int64
	Kind        DeadLetterKind
	EntityID    string
	TaskID      string
	Reason      string
	RetryCount  int
	Payload     json.RawMessage
	FirstSeenAt time.Time
	LastSeenAt  time.Time
}

// HostRecord is the durable representation of a compute host in the
// scheduler's registry. It maps 1:1 to a row in orchestrator_hosts.
//
// TokenEncrypted is the agent bearer token sealed with AES-GCM using the
// orchestrator's TOKEN_ENCRYPTION_KEY (same key the per-VM tokens use).
// It is decrypted only into the in-memory Host.Token used by the
// provider client; the plaintext never enters the database.
type HostRecord struct {
	ID             string
	URL            string
	TokenEncrypted []byte
	Region         string
	State          HostState
	TenantID       string
	Backend        HostBackend
	Labels         map[string]string
	Capacity       HostCapacity
	Allocated      HostCapacity
	LastSeen       time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// SnapshotQuotaUsage summarizes a tenant's in-flight/ready snapshot usage
// (count and aggregate size) for quota enforcement, without requiring the
// caller to load every snapshot record to compute it.
type SnapshotQuotaUsage struct {
	Count int
	Bytes int64
}

// StateStore persists orchestrator control-plane state.
type StateStore interface {
	UpsertVM(ctx context.Context, vm VMRecord) error
	DeleteVM(ctx context.Context, vmID string) error
	ListVMs(ctx context.Context) ([]VMRecord, error)

	UpsertTask(ctx context.Context, task TaskRecord) error
	ListTasks(ctx context.Context) ([]TaskRecord, error)

	UpsertSnapshot(ctx context.Context, snapshot SnapshotRecord) error
	GetSnapshot(ctx context.Context, snapshotID string) (SnapshotRecord, error)
	ListSnapshots(ctx context.Context) ([]SnapshotRecord, error)
	DeleteSnapshot(ctx context.Context, snapshotID string) error

	// ListSnapshotsByVM returns snapshots for a single VM, scoped at the
	// store layer (a WHERE clause for Postgres) rather than requiring the
	// caller to load and filter every snapshot in the fleet.
	ListSnapshotsByVM(ctx context.Context, vmID string) ([]SnapshotRecord, error)

	// ListSnapshotsByDigest returns every ready snapshot for tenantID whose
	// artifact hashes to digest. It is the artifact index: the orchestrator
	// answers "which hosts already hold these bytes" by reading the host_id of
	// each row it returns.
	//
	// The index is DERIVED, never a second table. A snapshot row is already the
	// statement "host H holds artifact A", so a separate index could only ever
	// disagree with it, and the disagreement would be invisible until a pull was
	// aimed at a host that no longer had the bytes. Deriving also makes recovery
	// free: there is nothing to rehydrate at startup because the query is the
	// index, and a row whose digest was never recorded is simply not findable
	// rather than wrongly findable.
	//
	// tenantID is a security boundary for the same reason it is one in
	// FindSnapshotByLayerKey: an artifact carries whatever its build baked in.
	ListSnapshotsByDigest(ctx context.Context, tenantID, digest string) ([]SnapshotRecord, error)

	// FindSnapshotByLayerKey returns the newest ready layer artifact matching
	// (tenantID, layerKey, arch), or ok=false when there is no hit.
	//
	// This is its own store method rather than a SnapshotFilter because it
	// runs once per chain key on every build, and ListSnapshotsFiltered loads
	// every snapshot in the fleet and filters in Go. Postgres serves this off
	// a single indexed query.
	//
	// tenantID scoping is a security boundary, not an optimization: serving
	// one tenant's artifact to another leaks whatever that build baked in, so
	// a cross-tenant match must be a miss, never a hit.
	FindSnapshotByLayerKey(ctx context.Context, tenantID, layerKey, arch string) (SnapshotRecord, bool, error)

	// SnapshotQuotaUsage returns the count and aggregate size of snapshots
	// for tenantID that count toward the per-tenant quota: states
	// Creating/Ready/Restoring, excluding SnapshotModeBuild artifacts (see
	// FleetManager.enforceSnapshotQuota). Computed at the store layer (a
	// COUNT/SUM query for Postgres) so quota checks don't require a
	// full-table scan on every CreateSnapshot call.
	SnapshotQuotaUsage(ctx context.Context, tenantID string) (SnapshotQuotaUsage, error)

	AppendEvent(ctx context.Context, event EventRecord) error

	// UpsertDeadLetter inserts or updates a dead-letter entry keyed by
	// (Kind, EntityID). On update, RetryCount and LastSeenAt are advanced.
	UpsertDeadLetter(ctx context.Context, entry DeadLetterRecord) error

	// ListDeadLetters returns all dead-letter entries. Implementations
	// may order arbitrarily.
	ListDeadLetters(ctx context.Context) ([]DeadLetterRecord, error)

	// UpsertHost inserts or updates a host registration.
	UpsertHost(ctx context.Context, host HostRecord) error

	// DeleteHost removes a host from the registry. No-op if absent.
	DeleteHost(ctx context.Context, hostID string) error

	// ListHosts returns all registered hosts.
	ListHosts(ctx context.Context) ([]HostRecord, error)

	// GetHost returns a single host by ID, or an error if not found.
	GetHost(ctx context.Context, hostID string) (HostRecord, error)
}

// MemoryStateStore is a process-local store useful for tests/default behavior.
type MemoryStateStore struct {
	mu          sync.RWMutex
	vms         map[string]VMRecord
	tasks       map[string]TaskRecord
	snapshots   map[string]SnapshotRecord
	events      []EventRecord
	deadLetters map[string]DeadLetterRecord // keyed by "kind|entity_id"
	hosts       map[string]HostRecord
	nextID      int64
	nextDLQID   int64
}

// NewMemoryStateStore returns an in-memory StateStore implementation.
func NewMemoryStateStore() *MemoryStateStore {
	return &MemoryStateStore{
		vms:         make(map[string]VMRecord),
		tasks:       make(map[string]TaskRecord),
		snapshots:   make(map[string]SnapshotRecord),
		deadLetters: make(map[string]DeadLetterRecord),
		hosts:       make(map[string]HostRecord),
	}
}

// cloneVMRecord deep-copies the slice fields on a VMRecord so a caller that
// mutates them (before Upsert or on a returned record) cannot corrupt the
// stored view. Spec.GPUUUIDs, Spec.MIGInstanceUUIDs, and Endpoints share a
// backing array under a shallow value copy.
func cloneVMRecord(v VMRecord) VMRecord {
	if v.Spec.GPUUUIDs != nil {
		v.Spec.GPUUUIDs = append([]string(nil), v.Spec.GPUUUIDs...)
	}
	if v.Spec.MIGInstanceUUIDs != nil {
		v.Spec.MIGInstanceUUIDs = append([]string(nil), v.Spec.MIGInstanceUUIDs...)
	}
	if v.Endpoints != nil {
		v.Endpoints = append([]Endpoint(nil), v.Endpoints...)
	}
	return v
}

func (s *MemoryStateStore) UpsertVM(_ context.Context, vm VMRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vms[vm.ID] = cloneVMRecord(vm)
	return nil
}

func (s *MemoryStateStore) DeleteVM(_ context.Context, vmID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.vms, vmID)
	return nil
}

func (s *MemoryStateStore) ListVMs(_ context.Context) ([]VMRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]VMRecord, 0, len(s.vms))
	for _, v := range s.vms {
		out = append(out, cloneVMRecord(v))
	}
	return out, nil
}

func (s *MemoryStateStore) UpsertTask(_ context.Context, task TaskRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[task.TaskID] = task
	return nil
}

func (s *MemoryStateStore) ListTasks(_ context.Context) ([]TaskRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]TaskRecord, 0, len(s.tasks))
	for _, t := range s.tasks {
		out = append(out, t)
	}
	return out, nil
}

func (s *MemoryStateStore) UpsertSnapshot(_ context.Context, snapshot SnapshotRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshots[snapshot.SnapshotID] = snapshot
	return nil
}

func (s *MemoryStateStore) GetSnapshot(_ context.Context, snapshotID string) (SnapshotRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	snapshot, ok := s.snapshots[snapshotID]
	if !ok {
		return SnapshotRecord{}, fmt.Errorf("snapshot %s not found", snapshotID)
	}
	return snapshot, nil
}

func (s *MemoryStateStore) ListSnapshots(_ context.Context) ([]SnapshotRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]SnapshotRecord, 0, len(s.snapshots))
	for _, snapshot := range s.snapshots {
		out = append(out, snapshot)
	}
	return out, nil
}

func (s *MemoryStateStore) DeleteSnapshot(_ context.Context, snapshotID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.snapshots, snapshotID)
	return nil
}

func (s *MemoryStateStore) ListSnapshotsByVM(_ context.Context, vmID string) ([]SnapshotRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]SnapshotRecord, 0)
	for _, snapshot := range s.snapshots {
		if snapshot.VMID == vmID {
			out = append(out, snapshot)
		}
	}
	return out, nil
}

// ListSnapshotsByDigest scans for every ready snapshot of this tenant holding
// the given artifact bytes. Results are sorted by snapshot id so the caller
// picks the same source peer on every call, which keeps a retried pull hitting
// the host that already answered rather than fanning out across the fleet.
//
// An empty digest matches nothing, deliberately: every snapshot taken by an
// agent build that predates artifact hashing has one, so treating it as a value
// would report the whole fleet as holding a single phantom artifact.
func (s *MemoryStateStore) ListSnapshotsByDigest(_ context.Context, tenantID, digest string) ([]SnapshotRecord, error) {
	if digest == "" {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]SnapshotRecord, 0)
	for _, snapshot := range s.snapshots {
		if snapshot.Digest != digest || snapshot.TenantID != tenantID {
			continue
		}
		if snapshot.State != SnapshotStateReady {
			continue
		}
		out = append(out, snapshot)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SnapshotID < out[j].SnapshotID })
	return out, nil
}

// FindSnapshotByLayerKey scans for the newest ready layer artifact matching
// the triple. The winner is picked with snapshotRecordLess, the same ordering
// sortSnapshotRecords uses (CreatedAt desc, SnapshotID desc as tiebreak), so
// this store and Postgres cannot disagree about which of two concurrently
// built artifacts wins.
//
// The tenant check is a security boundary: a hit from another tenant would
// serve that tenant's baked-in build output, so it has to be a miss.
func (s *MemoryStateStore) FindSnapshotByLayerKey(_ context.Context, tenantID, layerKey, arch string) (SnapshotRecord, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var (
		best  SnapshotRecord
		found bool
	)
	for _, snapshot := range s.snapshots {
		// an empty layer key is not a key: it is every non-layer snapshot in
		// the fleet, so it must never match.
		if layerKey == "" || snapshot.LayerKey != layerKey {
			continue
		}
		if snapshot.TenantID != tenantID {
			continue
		}
		if snapshot.Arch != arch {
			continue
		}
		if snapshot.State != SnapshotStateReady {
			continue
		}
		if !found || snapshotRecordLess(snapshot, best) {
			best = snapshot
			found = true
		}
	}
	return best, found, nil
}

func (s *MemoryStateStore) SnapshotQuotaUsage(_ context.Context, tenantID string) (SnapshotQuotaUsage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var usage SnapshotQuotaUsage
	for _, snapshot := range s.snapshots {
		if snapshot.TenantID != tenantID {
			continue
		}
		// mirrors FleetManager.enforceSnapshotQuota's scope: build
		// artifacts are exempt, only in-flight/ready checkpoints count.
		if snapshot.Mode == SnapshotModeBuild {
			continue
		}
		switch snapshot.State {
		case SnapshotStateCreating, SnapshotStateReady, SnapshotStateRestoring:
			usage.Count++
			usage.Bytes += snapshot.SizeBytes
		}
	}
	return usage, nil
}

func (s *MemoryStateStore) AppendEvent(_ context.Context, event EventRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextID++
	event.ID = s.nextID
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now()
	}
	s.events = append(s.events, event)
	return nil
}

func deadLetterKey(kind DeadLetterKind, entityID string) string {
	return string(kind) + "|" + entityID
}

func (s *MemoryStateStore) UpsertDeadLetter(_ context.Context, entry DeadLetterRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := deadLetterKey(entry.Kind, entry.EntityID)
	now := time.Now()
	if entry.LastSeenAt.IsZero() {
		entry.LastSeenAt = now
	}
	if existing, ok := s.deadLetters[key]; ok {
		entry.ID = existing.ID
		entry.FirstSeenAt = existing.FirstSeenAt
		if entry.RetryCount < existing.RetryCount {
			entry.RetryCount = existing.RetryCount
		}
	} else {
		s.nextDLQID++
		entry.ID = s.nextDLQID
		if entry.FirstSeenAt.IsZero() {
			entry.FirstSeenAt = now
		}
	}
	s.deadLetters[key] = entry
	return nil
}

func (s *MemoryStateStore) ListDeadLetters(_ context.Context) ([]DeadLetterRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]DeadLetterRecord, 0, len(s.deadLetters))
	for _, entry := range s.deadLetters {
		out = append(out, entry)
	}
	return out, nil
}

// cloneHostRecord deep-copies the host labels and the per-device GPU and
// per-instance MIG inventory so a caller that mutates them (before Upsert or
// on a returned record) cannot corrupt the stored view. HostRecord is
// otherwise a flat value.
func cloneHostRecord(h HostRecord) HostRecord {
	if h.Labels != nil {
		labels := make(map[string]string, len(h.Labels))
		for k, v := range h.Labels {
			labels[k] = v
		}
		h.Labels = labels
	}
	if h.Capacity.GPUDevices != nil {
		devices := make([]GPUDevice, len(h.Capacity.GPUDevices))
		copy(devices, h.Capacity.GPUDevices)
		h.Capacity.GPUDevices = devices
	}
	if h.Capacity.MIGInstances != nil {
		instances := make([]MIGInstance, len(h.Capacity.MIGInstances))
		copy(instances, h.Capacity.MIGInstances)
		h.Capacity.MIGInstances = instances
	}
	return h
}

func (s *MemoryStateStore) UpsertHost(_ context.Context, host HostRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hosts[host.ID] = cloneHostRecord(host)
	return nil
}

func (s *MemoryStateStore) DeleteHost(_ context.Context, hostID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.hosts, hostID)
	return nil
}

func (s *MemoryStateStore) ListHosts(_ context.Context) ([]HostRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]HostRecord, 0, len(s.hosts))
	for _, h := range s.hosts {
		out = append(out, cloneHostRecord(h))
	}
	return out, nil
}

func (s *MemoryStateStore) GetHost(_ context.Context, hostID string) (HostRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	h, ok := s.hosts[hostID]
	if !ok {
		return HostRecord{}, fmt.Errorf("host %s not found", hostID)
	}
	return cloneHostRecord(h), nil
}
