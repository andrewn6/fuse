package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
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
	State            SnapshotState
	SizeBytes        int64
	RetentionUntil   *time.Time
	Metadata         json.RawMessage
	Exports          []SnapshotExportRecord
	LastError        string
	CreatedAt        time.Time
	UpdatedAt        time.Time
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
