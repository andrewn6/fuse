package fuse

import "time"

// Spec is the hardware/runtime spec for a microVM.
type Spec struct {
	CPUs      int32  `json:"cpus,omitempty"`
	RamMB     int32  `json:"ram_mb,omitempty"`
	StorageGB int32  `json:"storage_gb,omitempty"`
	GPUs      int32  `json:"gpus,omitempty"`
	GPUKind   string `json:"gpu_kind,omitempty"`
	// GPUProfile requests fractional GPU allocation: a MIG profile in
	// mig-parted vocabulary (e.g. "1g.10gb"). When set, GPUs counts MIG
	// instances of this profile rather than whole devices.
	GPUProfile string `json:"gpu_profile,omitempty"`
	Region     string `json:"region,omitempty"`
	// Arch restricts scheduling to hosts of this CPU architecture ("amd64",
	// "arm64"). Empty means any host.
	Arch              string `json:"arch,omitempty"`
	MaxRuntimeSeconds int64  `json:"max_runtime_seconds,omitempty"`
	// IdleTimeoutSeconds destroys the environment after this many seconds
	// with no exec and no attach session. Zero means no idle expiry. Unlike
	// MaxRuntimeSeconds (a ceiling measured from create), this is measured
	// from the last exec or attach.
	IdleTimeoutSeconds int64  `json:"idle_timeout_seconds,omitempty"`
	Image              string `json:"image,omitempty"`
	// HostID pins the environment to an exact host id (the Fusefile's
	// placement.host). The pinned host still has to be active, run the right
	// backend, and fit the request.
	HostID string `json:"host_id,omitempty"`
	// Labels are placement label selectors (the Fusefile's placement.labels):
	// every pair must match the target host's declared labels.
	Labels map[string]string `json:"labels,omitempty"`
}

// ExposeSpec requests that a guest port be published as a reachable endpoint.
type ExposeSpec struct {
	Port int    `json:"port"`
	As   string `json:"as,omitempty"`
}

// Endpoint is a published network endpoint for an environment.
type Endpoint struct {
	As   string `json:"as,omitempty"`
	URL  string `json:"url"`
	Port int    `json:"port"`
}

// CreateRequest is the body for client.Create.
type CreateRequest struct {
	TaskID         string            `json:"task_id"`
	Spec           Spec              `json:"spec"`
	ManifestInline string            `json:"manifest_inline,omitempty"`
	Secrets        map[string]string `json:"secrets,omitempty"`
	StartupScript  string            `json:"startup_script,omitempty"`
	GatewayURL     string            `json:"gateway_url,omitempty"`
	GatewayToken   string            `json:"gateway_token,omitempty"`
	Expose         []ExposeSpec      `json:"expose,omitempty"`

	// StartupScriptTimeoutSeconds bounds StartupScript. Zero uses the
	// orchestrator's default. A value above the orchestrator's configured
	// maximum is rejected rather than clamped.
	StartupScriptTimeoutSeconds int64 `json:"startup_script_timeout_seconds,omitempty"`

	// SeedSnapshotID boots the environment from an existing snapshot artifact
	// instead of Spec.Image. The two are mutually exclusive. The artifact is
	// host-local, so the environment lands on the snapshot's host or the call
	// fails.
	SeedSnapshotID string `json:"seed_snapshot_id,omitempty"`
}

// EnvironmentInfo is the server's view of a single microVM.
type EnvironmentInfo struct {
	ID        string     `json:"id"`
	State     string     `json:"state"`
	TaskID    string     `json:"task_id"`
	HostID    string     `json:"host_id,omitempty"`
	URL       string     `json:"url"`
	Spec      Spec       `json:"spec"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	Error     string     `json:"error,omitempty"`
	Endpoints []Endpoint `json:"endpoints,omitempty"`
}

type environmentList struct {
	Environments []EnvironmentInfo `json:"environments"`
	NextCursor   *string           `json:"next_cursor,omitempty"`
}

// Lifecycle states for EnvironmentInfo.State and Event.State.
const (
	StateProvisioning = "provisioning"
	StateRunning      = "running"
	StateDraining     = "draining"
	StateDestroying   = "destroying"
	StateDestroyed    = "destroyed"
	StateFailed       = "failed"
)

// IsTerminalState reports whether state is a terminal lifecycle state.
func IsTerminalState(state string) bool {
	return state == StateDestroyed || state == StateFailed
}

// IsSettledState reports whether state is one an environment can settle in at
// the end of provisioning: running, or either terminal state. Callers that
// wait for an environment to come up should use this, since a healthy
// environment stops at running and never becomes terminal.
func IsSettledState(state string) bool {
	return state == StateRunning || IsTerminalState(state)
}

// Event is one item from env.Events. It matches the server's wire
// payload. Err is set only on a stream-level failure, as the final
// event before the channel closes.
type Event struct {
	ID        string    `json:"id"`
	Kind      string    `json:"event"`
	VMID      string    `json:"vm_id"`
	State     string    `json:"state"`
	URL       string    `json:"url,omitempty"`
	Error     string    `json:"error,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	Err       error     `json:"-"`

	// fields below are set only on EventKindStep events, which report one
	// setup step's boundary, timing, and cache verdict. they are zero on
	// state events, and on step events from a server that does not report
	// caching yet.
	Index      int      `json:"index,omitempty"`
	Total      int      `json:"total,omitempty"`
	Key        string   `json:"key,omitempty"`
	Cached     bool     `json:"cached,omitempty"`
	MissReason string   `json:"miss_reason,omitempty"`
	MissDetail []string `json:"miss_detail,omitempty"`
	DurationMS int64    `json:"duration_ms,omitempty"`
	ExitCode   int      `json:"exit_code,omitempty"`
}

// Event kinds carried in Event.Kind. An empty kind means "state": older
// servers omit the field.
const (
	EventKindState = "state"
	EventKindStep  = "step"
)

// IsStateEvent reports whether kind is a lifecycle state event, which is the
// only kind that advances an environment's state. An empty kind counts as a
// state event for compatibility with servers that omit it.
func IsStateEvent(kind string) bool {
	return kind == "" || kind == EventKindState
}

// Cache miss reasons carried in Event.MissReason. The set is closed so
// clients can render them, but unknown values must pass through rather than
// be treated as an error.
const (
	MissReasonNoEntry      = "no-entry"
	MissReasonStepChanged  = "step-changed"
	MissReasonInputsChange = "inputs-changed"
	MissReasonParentChange = "parent-changed"
	MissReasonBaseChanged  = "base-changed"
	MissReasonUncacheable  = "uncacheable"
	MissReasonDisabled     = "disabled"
	MissReasonNotOnHost    = "not-on-host"
	MissReasonUnsupported  = "unsupported"
)

// ForkOptions is the optional body for env.Fork.
type ForkOptions struct {
	ReuseSnapshotID string `json:"reuse_snapshot_id,omitempty"`
	Comment         string `json:"comment,omitempty"`
}

// SnapshotRequest is the optional body for env.Snapshot.
type SnapshotRequest struct {
	Comment          string            `json:"comment,omitempty"`
	Mode             string            `json:"mode,omitempty"`
	RetentionSeconds int64             `json:"retention_seconds,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	ExportRef        string            `json:"export_ref,omitempty"`
	ExportStatus     string            `json:"export_status,omitempty"`
}

// SnapshotExport is an optional exported snapshot artifact.
type SnapshotExport struct {
	Destination string    `json:"destination"`
	Status      string    `json:"status,omitempty"`
	RequestedAt time.Time `json:"requested_at,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
}

// Snapshot is a persisted snapshot record.
type Snapshot struct {
	ID               string `json:"id"`
	VMID             string `json:"vm_id"`
	TaskID           string `json:"task_id,omitempty"`
	TenantID         string `json:"tenant_id,omitempty"`
	ParentSnapshotID string `json:"parent_snapshot_id,omitempty"`
	Mode             string `json:"mode,omitempty"`
	State            string `json:"state,omitempty"`
	Comment          string `json:"comment,omitempty"`
	// Name is the caller-chosen lookup key from the snapshot's metadata, set
	// by `fuse build` so an artifact can be found without its random id.
	Name string `json:"name,omitempty"`

	// LayerKey is the content-addressed cache key of the setup step this
	// artifact was taken after. It is empty on every snapshot that is not a
	// build layer, and an empty key never matches a lookup.
	LayerKey string `json:"layer_key,omitempty"`

	// Arch is the CPU architecture, in GOARCH vocabulary ("amd64", "arm64"),
	// of the host that actually built the artifact, not of whoever asked for
	// it. A rootfs is not portable across architectures, so this is a real
	// constraint on whether the artifact can be booted; it is deliberately not
	// folded into LayerKey, so a lookup has to filter on it separately.
	Arch string `json:"arch,omitempty"`

	// Digest is the hex sha256 of the artifact rootfs. It verifies that a
	// given copy of those bytes is intact, and nothing more: it is not a
	// cross-build identity, because two builds of the same recipe produce
	// different rootfs bytes (timestamps, inode ordering, package caches).
	// It can never be used as a cache key.
	Digest string `json:"digest,omitempty"`

	SizeBytes      int64            `json:"size_bytes,omitempty"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at,omitempty"`
	RetentionUntil *time.Time       `json:"retention_until,omitempty"`
	LastError      string           `json:"last_error,omitempty"`
	ExportRef      string           `json:"export_ref,omitempty"`
	Exports        []SnapshotExport `json:"exports,omitempty"`
}

type snapshotList struct {
	Snapshots  []Snapshot `json:"snapshots"`
	NextCursor *string    `json:"next_cursor,omitempty"`
}

// HostCapacity is a host's resource envelope.
type HostCapacity struct {
	CPUs      int `json:"cpus"`
	RamMB     int `json:"ram_mb"`
	StorageGB int `json:"storage_gb"`
	VMCount   int `json:"vm_count"`
	// Arch is the host's CPU architecture in GOARCH vocabulary ("amd64",
	// "arm64"). Empty means amd64 (hosts registered before arch existed).
	Arch    string `json:"arch,omitempty"`
	GPUs    int    `json:"gpus,omitempty"`
	GPUKind string `json:"gpu_kind,omitempty"`

	// MIGProfiles advertises fractional GPU capacity: MIG instance count
	// by profile name (e.g. {"1g.10gb": 4}). Requires backend "qemu". When the
	// host reports per-instance MIG inventory (MIGInstances), this map is a
	// derived summary; otherwise it is the scheduling unit.
	MIGProfiles map[string]int `json:"mig_profiles,omitempty"`

	// MIGInstances is the per-instance MIG inventory probed from the host
	// agent (one entry per carved MIG GPU instance). When non-empty, the
	// orchestrator binds specific instance uuids to VMs instead of
	// decrementing a count. Strictly additive: a host that reports no
	// instances falls back to MIGProfiles. Only populated on capacity for
	// qemu hosts.
	MIGInstances []MIGInstance `json:"mig_instances,omitempty"`

	// MIGInstanceUUIDs is the set of MIG instance uuids currently bound to
	// VMs. Populated only on Allocated, and only for hosts that report
	// per-instance MIG inventory.
	MIGInstanceUUIDs []string `json:"mig_instance_uuids,omitempty"`

	// GPUDevices is the per-device GPU detail probed from the host agent,
	// carried alongside the scalar GPUs/GPUKind counters. Only populated on
	// capacity for qemu hosts.
	GPUDevices []GPUDevice `json:"gpu_devices,omitempty"`
}

// GPUDevice is the per-device detail probed for a single GPU. Every field is
// best-effort and omitted when the host agent could not determine it.
type GPUDevice struct {
	UUID          string `json:"uuid,omitempty"`
	Model         string `json:"model,omitempty"`
	PCIBusID      string `json:"pci_bus_id,omitempty"`
	MemoryMB      int    `json:"memory_mb,omitempty"`
	DriverVersion string `json:"driver_version,omitempty"`
	CUDAVersion   string `json:"cuda_version,omitempty"`
	ComputeCap    string `json:"compute_cap,omitempty"`
	MIGCapable    bool   `json:"mig_capable,omitempty"`
	MIGMode       string `json:"mig_mode,omitempty"`
	IOMMUGroup    string `json:"iommu_group,omitempty"`
}

// MIGInstance is one carved MIG GPU instance probed from the host agent. The
// orchestrator binds a specific instance uuid to a VM so it knows which
// instance went to which VM.
type MIGInstance struct {
	UUID          string `json:"uuid,omitempty"`
	Profile       string `json:"profile,omitempty"`
	Kind          string `json:"kind,omitempty"`
	ParentGPUUUID string `json:"parent_gpu_uuid,omitempty"`
}

// RegisterHostRequest is the body for client.RegisterHost.
type RegisterHostRequest struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	Token   string `json:"token,omitempty"`
	Region  string `json:"region,omitempty"`
	Backend string `json:"backend,omitempty"`
	// Labels are operator-declared key/value pairs matched against a spec's
	// placement label selectors. They are never probed from the host agent.
	Labels   map[string]string `json:"labels,omitempty"`
	Capacity HostCapacity      `json:"capacity"`
}

// Host is the server's view of a registered host.
type Host struct {
	ID        string            `json:"id"`
	URL       string            `json:"url"`
	Region    string            `json:"region,omitempty"`
	State     string            `json:"state"`
	Backend   string            `json:"backend,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	Capacity  HostCapacity      `json:"capacity"`
	Allocated HostCapacity      `json:"allocated"`
	LastSeen  time.Time         `json:"last_seen"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`

	// Warnings carries non-fatal notices from registration (e.g. a
	// declared capacity value that exceeds what was probed from the host
	// agent). Only ever populated on the response to Hosts.Register.
	Warnings []string `json:"warnings,omitempty"`
}

type hostList struct {
	Hosts      []Host  `json:"hosts"`
	NextCursor *string `json:"next_cursor,omitempty"`
}

// APIKey is a key's metadata. The raw secret appears only in
// CreatedAPIKey.Key, returned once at creation.
type APIKey struct {
	ID         string     `json:"id"`
	Label      string     `json:"label,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// CreatedAPIKey is returned by client.CreateAPIKey. Key is unrecoverable
// after this response.
type CreatedAPIKey struct {
	APIKey
	Key string `json:"key"`
}

type apiKeyList struct {
	APIKeys []APIKey `json:"api_keys"`
}
