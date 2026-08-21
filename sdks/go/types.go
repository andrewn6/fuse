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

// HealthcheckSpec is the environment-level readiness probe (the Fusefile's
// `healthcheck:` block). Exactly one of HTTP and Exec must be set; the server
// rejects a request that sets both or neither.
//
// It is not the per-service compose healthcheck that can appear inside a
// manifest: that one governs a single container and never reaches the control
// plane. This one is evaluated by the guest agent over the environment as a
// whole and its verdict comes back on EnvironmentInfo.Health.
//
// Every duration is in seconds, and zero means "omitted, use the guest agent's
// default". The server substitutes no defaults of its own, because the probe
// executes in the guest.
type HealthcheckSpec struct {
	HTTP *HealthcheckHTTP `json:"http,omitempty"`
	Exec []string         `json:"exec,omitempty"`

	IntervalSeconds    int64 `json:"interval_seconds,omitempty"`
	TimeoutSeconds     int64 `json:"timeout_seconds,omitempty"`
	Retries            int   `json:"retries,omitempty"`
	StartPeriodSeconds int64 `json:"start_period_seconds,omitempty"`
}

// HealthcheckHTTP is an HTTP GET against a port inside the guest. The probe
// runs in-guest, so the port needs no matching ExposeSpec.
type HealthcheckHTTP struct {
	Port int    `json:"port"`
	Path string `json:"path,omitempty"`
}

// DesktopSpec is the geometry of the environment's graphical session (the
// Fusefile's `desktop:` block). It requires an image that carries the desktop
// stack; on any other image the declaration is inert and the computer surface
// reports the display as absent.
//
// Both fields are required, 320 to 3840 each: a guessed dimension would
// silently shift every coordinate a computer-use model emits.
type DesktopSpec struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// ComputerActionRequest is one computer-use action, in the same shape
// Anthropic's computer tool emits as tool_use input, so translating a
// tool_use block into a call is mechanical. Only Action is required; which
// other fields apply depends on the action, and the guest agent enforces the
// full schema.
type ComputerActionRequest struct {
	Action          string  `json:"action"`
	Coordinate      []int   `json:"coordinate,omitempty"`
	StartCoordinate []int   `json:"start_coordinate,omitempty"`
	Text            string  `json:"text,omitempty"`
	Region          []int   `json:"region,omitempty"`
	Duration        float64 `json:"duration,omitempty"`
	ScrollDirection string  `json:"scroll_direction,omitempty"`
	ScrollAmount    int     `json:"scroll_amount,omitempty"`
}

// ComputerActionResponse is the guest's answer to one action. Screenshot is
// a base64 PNG, present on every action that implies one; for zoom it is the
// cropped region at full resolution.
type ComputerActionResponse struct {
	Output     string `json:"output,omitempty"`
	Screenshot string `json:"screenshot,omitempty"`
}

// ComputerDisplay reports the environment's display: whether it is up and at
// what geometry, so a caller can populate display_width_px /
// display_height_px in its computer tool definition without hardcoding them.
type ComputerDisplay struct {
	Display string `json:"display,omitempty"`
	Up      bool   `json:"up"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	Error   string `json:"error,omitempty"`
}

// Health is the last verdict of an environment's healthcheck. State is one of
// the Health* constants below.
//
// It is deliberately not folded into EnvironmentInfo.State: that vocabulary is
// a closed set IsSettledState reasons about, and an unhealthy environment is
// still a running one. Nothing tears an environment down for a failing probe.
type Health struct {
	State string `json:"state"`
	// Since is when the probe entered State.
	Since time.Time `json:"since,omitempty"`
	// Failures is the count of consecutive failed attempts, zero while
	// passing.
	Failures int `json:"failures,omitempty"`
	// Message is the last attempt's failure detail, empty while passing.
	Message string `json:"message,omitempty"`
}

// Verdicts carried in Health.State.
const (
	// HealthStarting means the probe has not passed yet and is still inside
	// its start period, so failures are not being counted.
	HealthStarting = "starting"
	// HealthPassing means the most recent attempt succeeded.
	HealthPassing = "passing"
	// HealthFailing means the probe failed its configured retries in a row
	// after the start period ended.
	HealthFailing = "failing"
)

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

	// Healthcheck is the environment-level readiness probe. Omit it for an
	// environment with no probe, in which case EnvironmentInfo.Health is
	// never populated. It is not evaluated inside Create: the call returns as
	// soon as the VM is up, and the verdict arrives on later reads.
	Healthcheck *HealthcheckSpec `json:"healthcheck,omitempty"`

	// Desktop is the graphical session's geometry. Omit it for an
	// environment with no desktop, in which case a desktop image keeps its
	// baked default geometry.
	Desktop *DesktopSpec `json:"desktop,omitempty"`

	// Files are guest files written before StartupScript runs, keyed by
	// absolute guest path with base64-encoded content. It is what a
	// Fusefile's `copy` block compiles to. Paths under /fuse are rejected
	// (that is the guest agent's own directory), and the total decoded size
	// is capped at 512 KiB, since these travel inside the create body.
	// Permissions are not carried: chmod in the startup script.
	Files map[string]string `json:"files,omitempty"`

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
	// Health is the environment-level healthcheck's last verdict. Nil when
	// the environment declared no healthcheck, and nil until the first
	// verdict has been read back from the guest. The server refreshes it on
	// its reconcile tick (30s by default), so it lags the guest by up to a
	// tick.
	Health *Health `json:"health,omitempty"`
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

	// LayerKey labels this snapshot as the artifact of one cacheable setup
	// step, which is what makes it findable by recipe rather than by its
	// random id. Empty for an ordinary snapshot.
	//
	// The scope it is filed under comes from how the caller authenticated and
	// is not settable here.
	LayerKey string `json:"layer_key,omitempty"`

	// Live captures the guest's memory and vCPU state alongside the rootfs,
	// so restoring resumes the guest instead of cold-booting it. The zero
	// value is a disk-only snapshot, which is the default and the fallback.
	//
	// It costs the environment's full configured memory in bytes on every
	// create, pauses the guest for the length of the capture, and produces an
	// artifact pinned to the host that took it. Fork reads only a snapshot's
	// rootfs, so forking a live snapshot yields a cold-booting environment
	// with none of the memory, and nothing errors when it does.
	//
	// Against a backend with no live-snapshot support this is a 501
	// unimplemented, not a conflict, so IsConflict does not match it.
	//
	// It is a request, not a guarantee: read Snapshot.Kind to find out what
	// was actually written.
	Live bool `json:"live,omitempty"`
}

// resolveSnapshotResponse is the wire envelope for a layer lookup. It stays
// unexported so the found flag never reaches the public surface: callers get
// the ok idiom instead, which cannot be confused with a decode that happened to
// produce a zero-value snapshot.
type resolveSnapshotResponse struct {
	Found    bool      `json:"found"`
	Snapshot *Snapshot `json:"snapshot,omitempty"`
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

	// Kind is "disk" or "live": whether this artifact is the rootfs alone, or
	// the rootfs plus the guest's memory and vCPU state. Restoring a disk
	// snapshot cold-boots the guest; restoring a live one resumes it.
	//
	// It is what the host agent reports it actually wrote, not what was
	// requested. A host running an agent too old to know about live snapshots
	// answers SnapshotRequest.Live with a disk snapshot, and this then says
	// "disk". Branch on it rather than on your own request.
	//
	// A server that predates live snapshots omits it entirely, which reads
	// back as the empty string; treat that as "disk".
	Kind string `json:"kind,omitempty"`

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
