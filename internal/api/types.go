// Package api is the HTTP (REST) surface of the orchestrator. It wraps
// FleetManager and Environment operations behind a chi-based router,
// serves the harness, and is consumed by operations tooling. The shape
// of every request and response mirrors apps/orchestrator/api/openapi.yaml,
// which is the human-readable contract for this API.
//
// Types in this file are hand-maintained and intentionally decoupled
// from the internal FleetManager/orchestrator types so that the wire
// contract can evolve independently of the in-process API. The
// conversion helpers in convert.go enforce that separation.
package api

import "time"

// ResourceSpec is the JSON shape of the hardware spec attached to an
// environment create request or response body.
//
// Image names a base rootfs for the provider to boot the VM from (a name
// the firecracker host agent resolves against its own named-rootfs
// directory). Empty means the provider's default base.
type ResourceSpec struct {
	CPUs              int32  `json:"cpus,omitempty"`
	RamMB             int32  `json:"ram_mb,omitempty"`
	StorageGB         int32  `json:"storage_gb,omitempty"`
	Region            string `json:"region,omitempty"`
	MaxRuntimeSeconds int64  `json:"max_runtime_seconds,omitempty"`
	Image             string `json:"image,omitempty"`
	GPUs              int32  `json:"gpus,omitempty"`
	GPUKind           string `json:"gpu_kind,omitempty"`
	// GPUProfile requests fractional GPU allocation: a MIG profile in
	// mig-parted vocabulary (e.g. "1g.10gb"). When set, GPUs counts MIG
	// instances of this profile rather than whole devices (decision D5).
	GPUProfile string `json:"gpu_profile,omitempty"`
}

// ExposeSpec requests that a guest port be published as a reachable
// endpoint.
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

// CreateEnvironmentRequest is the JSON body accepted by
// POST /v1/environments.
//
// ManifestInline is optional raw manifest JSON, base64-encoded.
// When omitted, the orchestrator uses a minimal internal manifest.
type CreateEnvironmentRequest struct {
	TaskID         string            `json:"task_id"`
	Spec           ResourceSpec      `json:"spec"`
	ManifestInline string            `json:"manifest_inline"`
	Secrets        map[string]string `json:"secrets,omitempty"`
	StartupScript  string            `json:"startup_script,omitempty"`
	GatewayURL     string            `json:"gateway_url,omitempty"`
	GatewayToken   string            `json:"gateway_token,omitempty"`
	Expose         []ExposeSpec      `json:"expose,omitempty"`

	// SeedSnapshotID boots the environment from an existing snapshot artifact
	// (see `fuse build`) rather than from spec.image, which it is mutually
	// exclusive with. The artifact is host-local and there is no object
	// storage, so the environment is pinned to the snapshot's host and the
	// request fails if that host cannot take it, rather than silently booting
	// a base image somewhere else.
	SeedSnapshotID string `json:"seed_snapshot_id,omitempty"`
}

// Environment is the JSON shape returned for a single VM.
type Environment struct {
	ID        string       `json:"id"`
	State     string       `json:"state"`
	TaskID    string       `json:"task_id"`
	HostID    string       `json:"host_id,omitempty"`
	URL       string       `json:"url"`
	Spec      ResourceSpec `json:"spec"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
	Error     string       `json:"error,omitempty"`
	Endpoints []Endpoint   `json:"endpoints,omitempty"`
}

// EnvironmentList is the response body for GET /v1/environments.
type EnvironmentList struct {
	Environments []Environment `json:"environments"`
}

// CreateSnapshotRequest is the optional body for POST
// /v1/environments/{vm}/snapshots.
type CreateSnapshotRequest struct {
	Comment          string            `json:"comment,omitempty"`
	Mode             string            `json:"mode,omitempty"`
	RetentionSeconds int64             `json:"retention_seconds,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	ExportRef        string            `json:"export_ref,omitempty"`
	ExportStatus     string            `json:"export_status,omitempty"`
}

// ForkEnvironmentRequest is the optional body for
// POST /v1/environments/{vmId}?action=fork.
type ForkEnvironmentRequest struct {
	ReuseSnapshotID string `json:"reuse_snapshot_id,omitempty"`
	Comment         string `json:"comment,omitempty"`
}

// SnapshotExport is the JSON shape of an optional exported snapshot artifact.
type SnapshotExport struct {
	Destination string    `json:"destination"`
	Status      string    `json:"status,omitempty"`
	RequestedAt time.Time `json:"requested_at,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
}

// Snapshot is the JSON shape of a persisted snapshot record.
type Snapshot struct {
	ID               string `json:"id"`
	VMID             string `json:"vm_id"`
	TaskID           string `json:"task_id,omitempty"`
	TenantID         string `json:"tenant_id,omitempty"`
	ParentSnapshotID string `json:"parent_snapshot_id,omitempty"`
	Mode             string `json:"mode,omitempty"`
	State            string `json:"state,omitempty"`
	Comment          string `json:"comment,omitempty"`
	// Name is the caller-chosen lookup key from the snapshot's metadata,
	// surfaced so a build artifact can be found without its random id.
	Name           string           `json:"name,omitempty"`
	SizeBytes      int64            `json:"size_bytes,omitempty"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at,omitempty"`
	RetentionUntil *time.Time       `json:"retention_until,omitempty"`
	LastError      string           `json:"last_error,omitempty"`
	ExportRef      string           `json:"export_ref,omitempty"`
	Exports        []SnapshotExport `json:"exports,omitempty"`
}

// SnapshotList is the response body for GET /v1/snapshots.
type SnapshotList struct {
	Snapshots []Snapshot `json:"snapshots"`
}

// Error is the JSON envelope returned for every non-2xx response. It
// intentionally wraps a single inner Error object so clients have one
// place to look for machine-readable failure metadata.
type Error struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody is the inner payload of an Error envelope.
type ErrorBody struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Details map[string]string `json:"details,omitempty"`
}

// Standard error codes. The REST boundary uses these to give callers a
// stable vocabulary for programmatic handling.
const (
	CodeNotFound        = "not_found"
	CodeConflict        = "conflict"
	CodeInvalidArgument = "invalid_argument"
	CodeUnavailable     = "unavailable"
	CodeInternal        = "internal"
	CodeUnauthorized    = "unauthorized"
	CodeUnimplemented   = "unimplemented"

	// CodePayloadTooLarge marks a request body that ran past the endpoint's
	// size limit. Distinct from CodeInvalidArgument because the body may be
	// perfectly valid JSON and simply too big, which a client can fix by
	// sending less rather than by sending something different.
	CodePayloadTooLarge = "payload_too_large"

	// CodeRouteNotFound distinguishes "this URL isn't a route this server
	// exposes" (wrong host, wrong port, not the orchestrator at all) from
	// CodeNotFound's "route exists, resource doesn't" (e.g. an unknown vm
	// or host id).
	CodeRouteNotFound = "route_not_found"
)

// ── Exec types ─────────────────────────────────────────────────────

// ExecEnvironmentRequest is the JSON body accepted by
// POST /v1/environments/{vmId}?action=exec.
//
// Exactly one of cmd or shell must be set.
type ExecEnvironmentRequest struct {
	// Cmd is the argv to run in the guest, e.g. ["ls", "-l", "/var/log"].
	// Argv needs no quoting rules and cannot be turned into an injection by
	// interpolating a value, so it is the default.
	Cmd []string `json:"cmd,omitempty"`

	// Shell runs the string under `sh -lc`. It is the explicit opt-in for
	// what argv cannot express: pipelines, redirects, and globs.
	Shell string `json:"shell,omitempty"`

	// TimeoutMS bounds the command inside the guest. Zero takes the server
	// default; anything above the server ceiling is clamped to it.
	TimeoutMS int `json:"timeout_ms,omitempty"`
}

// ExecEnvironmentResponse is the outcome of a guest command.
//
// A non-zero exit_code arrives with HTTP 200: the command ran and failed,
// which is a successful answer to the question that was asked. Stdout and
// stderr are separate, and are plain strings rather than base64 because exec
// output is overwhelmingly text and every caller would otherwise pay a
// decoding tax for the rare binary case.
type ExecEnvironmentResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// ── API key types ──────────────────────────────────────────────────

// CreateAPIKeyRequest is the JSON body accepted by POST /v1/api-keys.
// label is an optional human memory aid ("ci", "partner-acme").
type CreateAPIKeyRequest struct {
	Label string `json:"label,omitempty"`
}

// APIKey is the JSON shape of a key's metadata. The raw secret is never
// included here — it appears only once, in CreateAPIKeyResponse.
type APIKey struct {
	ID         string     `json:"id"`
	Label      string     `json:"label,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// CreateAPIKeyResponse is returned by POST /v1/api-keys. Key is the raw
// secret and is shown exactly once — it cannot be recovered afterward.
type CreateAPIKeyResponse struct {
	APIKey
	Key string `json:"key"`
}

// APIKeyList is the response body for GET /v1/api-keys.
type APIKeyList struct {
	APIKeys []APIKey `json:"api_keys"`
}

// ── Host management types ──────────────────────────────────────────

// HostCapacity is the wire shape of a host's resource envelope.
type HostCapacity struct {
	CPUs      int    `json:"cpus"`
	RamMB     int    `json:"ram_mb"`
	StorageGB int    `json:"storage_gb"`
	VMCount   int    `json:"vm_count"`
	GPUs      int    `json:"gpus,omitempty"`
	GPUKind   string `json:"gpu_kind,omitempty"`

	// MIGProfiles advertises fractional GPU capacity: MIG instance count
	// by profile name (e.g. {"1g.10gb": 4}). Like GPUs, it requires
	// backend "qemu" and is declared by the operator, never probed.
	//
	// When the host reports per-instance MIG inventory (MIGInstances), this
	// map is a derived summary (count by profile) and the scheduler binds
	// specific instance uuids. A host that only declares counts (the
	// --mig-profile registration override) keeps this map as the scheduling
	// unit.
	MIGProfiles map[string]int `json:"mig_profiles,omitempty"`

	// MIGInstances is the per-instance MIG inventory probed from the host
	// agent (one entry per carved MIG GPU instance). When non-empty, the
	// scheduler switches from count-based to instance-based allocation and
	// binds concrete uuids to VMs. Strictly additive: a host that reports no
	// instances falls back to MIGProfiles. Only populated on capacity (not
	// allocated) for qemu hosts.
	MIGInstances []MIGInstance `json:"mig_instances,omitempty"`

	// MIGInstanceUUIDs is the set of MIG instance uuids currently bound to
	// VMs. Populated only on Allocated (never on Capacity), and only for
	// hosts that report per-instance MIG inventory. The durable source of
	// truth is the per-VM mig_instance_uuids binding.
	MIGInstanceUUIDs []string `json:"mig_instance_uuids,omitempty"`

	// GPUDevices is the per-device GPU detail probed from the host agent,
	// carried alongside the scalar GPUs/GPUKind counters. Only populated on
	// capacity (not allocated) for qemu hosts.
	GPUDevices []GPUDevice `json:"gpu_devices,omitempty"`
}

// GPUDevice is the wire shape of a single probed GPU. It mirrors
// orchestrator.GPUDevice; every field is best-effort and omitted when the
// host agent could not determine it.
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

// MIGInstance is the wire shape of a single carved MIG GPU instance. It
// mirrors orchestrator.MIGInstance. The orchestrator binds a specific
// instance uuid to a VM so it knows which instance went to which VM.
type MIGInstance struct {
	UUID          string `json:"uuid,omitempty"`
	Profile       string `json:"profile,omitempty"`
	Kind          string `json:"kind,omitempty"`
	ParentGPUUUID string `json:"parent_gpu_uuid,omitempty"`
}

// RegisterHostRequest is the JSON body accepted by POST /v1/hosts.
//
// Backend selects the host's virtualization backend ("firecracker" or
// "qemu"). Omitted or empty defaults to "firecracker". Only "qemu" hosts
// may register with Capacity.GPUs > 0.
type RegisterHostRequest struct {
	ID       string       `json:"id"`
	URL      string       `json:"url"`
	Token    string       `json:"token,omitempty"`
	Region   string       `json:"region,omitempty"`
	Backend  string       `json:"backend,omitempty"`
	Capacity HostCapacity `json:"capacity"`
}

// HostInfo is the JSON shape returned for a single host.
type HostInfo struct {
	ID        string       `json:"id"`
	URL       string       `json:"url"`
	Region    string       `json:"region,omitempty"`
	Backend   string       `json:"backend,omitempty"`
	State     string       `json:"state"`
	Capacity  HostCapacity `json:"capacity"`
	Allocated HostCapacity `json:"allocated"`
	LastSeen  time.Time    `json:"last_seen"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`

	// Warnings carries non-fatal notices from registration (e.g. a
	// declared capacity value that exceeds what was probed from the host
	// agent). Only ever populated on the POST /v1/hosts response.
	Warnings []string `json:"warnings,omitempty"`
}

// HostList is the response body for GET /v1/hosts.
type HostList struct {
	Hosts []HostInfo `json:"hosts"`
}
