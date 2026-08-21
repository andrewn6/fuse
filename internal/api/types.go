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
	CPUs      int32  `json:"cpus,omitempty"`
	RamMB     int32  `json:"ram_mb,omitempty"`
	StorageGB int32  `json:"storage_gb,omitempty"`
	Region    string `json:"region,omitempty"`
	// Arch restricts scheduling to hosts of this CPU architecture ("amd64",
	// "arm64"; the uname spellings x86_64/aarch64 are accepted and
	// normalized). Empty means any host.
	Arch              string `json:"arch,omitempty"`
	MaxRuntimeSeconds int64  `json:"max_runtime_seconds,omitempty"`
	// IdleTimeoutSeconds destroys the environment after this many seconds
	// with no exec and no attach session. Zero means no idle expiry. Unlike
	// MaxRuntimeSeconds (a leak-detection ceiling measured from create),
	// this is measured from the last control-plane activity.
	IdleTimeoutSeconds int64  `json:"idle_timeout_seconds,omitempty"`
	Image              string `json:"image,omitempty"`
	GPUs               int32  `json:"gpus,omitempty"`
	GPUKind            string `json:"gpu_kind,omitempty"`
	// GPUProfile requests fractional GPU allocation: a MIG profile in
	// mig-parted vocabulary (e.g. "1g.10gb"). When set, GPUs counts MIG
	// instances of this profile rather than whole devices (decision D5).
	GPUProfile string `json:"gpu_profile,omitempty"`
	// HostID pins the environment to an exact host id (the Fusefile's
	// placement.host). A pin is a hard gate, not an override: the host still
	// has to be active, run the right backend, and fit the request. An
	// unknown host id is rejected as a 404 before any VM row is created.
	HostID string `json:"host_id,omitempty"`
	// Labels are placement label selectors (the Fusefile's placement.labels).
	// Every pair must match the target host's operator-declared labels.
	Labels map[string]string `json:"labels,omitempty"`
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

// HealthcheckSpec is the environment-level readiness probe (the Fusefile's
// `healthcheck:` block). Exactly one of HTTP and Exec must be set.
//
// It is not the per-service compose healthcheck that can appear inside
// manifest_inline: that one governs one container and is evaluated by the
// guest's compose, and nothing about it ever reaches the control plane. This
// one is evaluated by the guest agent over the environment as a whole and its
// verdict comes back on Environment.health.
//
// Every duration is in seconds, and a zero means "omitted, use the guest
// agent's default". The orchestrator does not substitute defaults of its own:
// the probe executes in the guest, so the guest owns what unset means.
type HealthcheckSpec struct {
	HTTP *HealthcheckHTTP `json:"http,omitempty"`
	Exec []string         `json:"exec,omitempty"`

	IntervalSeconds    int64 `json:"interval_seconds,omitempty"`
	TimeoutSeconds     int64 `json:"timeout_seconds,omitempty"`
	Retries            int   `json:"retries,omitempty"`
	StartPeriodSeconds int64 `json:"start_period_seconds,omitempty"`
}

// HealthcheckHTTP is an HTTP GET against a port inside the guest. The probe
// runs in-guest, so the port needs no matching expose entry.
type HealthcheckHTTP struct {
	Port int    `json:"port"`
	Path string `json:"path,omitempty"`
}

// DesktopSpec is the geometry of the environment's graphical session (the
// Fusefile's `desktop:` block). It requires an image that carries the
// desktop stack; on any other image the declaration is inert and the
// computer endpoint reports the display as absent.
//
// Both fields are required. There is no "omitted means default" here: a
// guessed dimension would silently shift every coordinate a computer-use
// model emits, which presents as model failure rather than as the config
// error it is.
type DesktopSpec struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// Health is the last verdict of an environment's healthcheck.
//
// State is one of "starting", "passing", "failing". It is deliberately not
// folded into Environment.state: that field's vocabulary is a closed set every
// SDK reasons about, and an unhealthy environment is still a running one. A
// failing probe reports and nothing tears the environment down for it.
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

	// Healthcheck is the environment-level readiness probe. Omit it for an
	// environment with no probe, in which case Environment.health is always
	// absent. It is not evaluated inside this request: the create call
	// returns as soon as the VM is up, and the verdict arrives on later reads.
	Healthcheck *HealthcheckSpec `json:"healthcheck,omitempty"`

	// Desktop is the graphical session's geometry. Omit it for an
	// environment with no desktop, in which case the image's baked default
	// applies if the image has a desktop at all.
	Desktop *DesktopSpec `json:"desktop,omitempty"`

	// Files are caller-supplied guest files written before StartupScript
	// runs, keyed by absolute guest path with base64-encoded content (the
	// same encoding as ManifestInline). It is what a Fusefile's `copy` block
	// compiles to, and it rides this body rather than a post-boot route
	// precisely so the files exist before `setup:` and `run:` do.
	//
	// Paths are validated here, not just client-side: a path must be
	// absolute, must not traverse, and must not be under /fuse, which holds
	// the guest agent's own manifest, secrets, and credentials. The total
	// decoded size is capped at fusefile.MaxCopyBytes. Permissions are not
	// carried: the upload wire is a path and a body, so an executable
	// arrives without its bit and the caller chmods it in the script.
	Files map[string]string `json:"files,omitempty"`

	// StartupScriptTimeoutSeconds bounds StartupScript. Zero uses the
	// orchestrator's default. A value above the orchestrator's configured
	// maximum is rejected with 400 rather than clamped, so a caller is never
	// left thinking it got a bound it did not get.
	StartupScriptTimeoutSeconds int64 `json:"startup_script_timeout_seconds,omitempty"`

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
	// Health is the environment-level healthcheck's last verdict. Absent when
	// the environment declared no healthcheck, and absent until the first
	// verdict has been read back from the guest.
	Health *Health `json:"health,omitempty"`
}

// EnvironmentList is the response body for GET /v1/environments.
//
// NextCursor is an opaque token: pass it back as the "cursor" query param
// to fetch the next page. It is absent once there are no more results.
type EnvironmentList struct {
	Environments []Environment `json:"environments"`
	NextCursor   *string       `json:"next_cursor,omitempty"`
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

	// LayerKey labels this snapshot as the artifact of one cacheable setup
	// step. `fuse build` sets it after each such step; every other caller
	// leaves it empty. It is what a later build looks the artifact up by, so
	// an artifact taken without it can only ever be found by its random id.
	// A layer is still an ordinary build-mode snapshot; the key is the only
	// thing that distinguishes it.
	LayerKey string `json:"layer_key,omitempty"`

	// Live asks for the guest's memory and vCPU state to be captured
	// alongside the rootfs, so restoring resumes the guest instead of
	// cold-booting it. False (the default, and the value of an omitted body)
	// is a disk-only snapshot.
	//
	// It is a request, not a guarantee. A backend with no live-snapshot
	// support rejects it with ErrSnapshotUnsupported (501), and an agent too
	// old to know about the flag answers with a disk snapshot; the Kind on
	// the response says which happened.
	Live bool `json:"live,omitempty"`
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
	Name string `json:"name,omitempty"`
	// LayerKey is the derived cache key of the setup step this artifact
	// materializes, empty on any snapshot that is not a build layer.
	LayerKey string `json:"layer_key,omitempty"`
	// Arch is the goarch of the host that built the artifact, not of whoever
	// asked for it. An ext4 rootfs does not travel across architectures, so
	// this is what tells a caller whether it can boot these bytes.
	Arch string `json:"arch,omitempty"`
	// Digest is the hex sha256 of the artifact rootfs, for verifying a
	// transfer of these exact bytes. It is not an identity two artifacts can
	// share: rebuilding the same recipe produces different bytes, so nothing
	// looks a snapshot up by digest. Empty when the host agent does not hash.
	Digest string `json:"digest,omitempty"`
	// Kind is "disk" or "live" and reports what the artifact actually
	// contains, never what the caller asked for: a live request served by an
	// agent that could only take a disk snapshot reads back as "disk". It is
	// always populated, including on records written before live snapshots
	// existed, which read back as "disk".
	Kind           string           `json:"kind"`
	SizeBytes      int64            `json:"size_bytes,omitempty"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at,omitempty"`
	RetentionUntil *time.Time       `json:"retention_until,omitempty"`
	LastError      string           `json:"last_error,omitempty"`
	ExportRef      string           `json:"export_ref,omitempty"`
	Exports        []SnapshotExport `json:"exports,omitempty"`
}

// SnapshotList is the response body for GET /v1/snapshots.
//
// NextCursor is an opaque token: pass it back as the "cursor" query param
// to fetch the next page. It is absent once there are no more results.
type SnapshotList struct {
	Snapshots  []Snapshot `json:"snapshots"`
	NextCursor *string    `json:"next_cursor,omitempty"`
}

// ResolveSnapshotResponse is the body of GET /v1/snapshots/resolve: at most
// one artifact for one layer key.
//
// A miss is reported as found=false with a 200, not as a 404. The first build
// of any recipe misses every key in its chain, and a caller probes its chain
// one key at a time, so a cold cache is the ordinary path through this
// endpoint and dressing it up as an error would make every clean build look
// like a pile of failures in logs and metrics.
type ResolveSnapshotResponse struct {
	Found    bool      `json:"found"`
	Snapshot *Snapshot `json:"snapshot,omitempty"`
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

// ── Computer types ─────────────────────────────────────────────────

// ComputerActionRequest is the JSON body accepted by
// POST /v1/environments/{vmId}/computer. The field names mirror the tool_use
// input Anthropic's computer tool emits, so a caller's translation layer
// stays mechanical.
//
// The orchestrator validates only that an action was named; the full action
// schema is owned and enforced by the guest agent, which is baked into the
// image and versioned separately, so a field this struct has not learned yet
// still reaches a newer guest intact.
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

// ComputerActionResponse is the guest's answer to an action. Screenshot is a
// base64 PNG, present on every action that implies one (which is most of
// them; a computer-use loop wants pixels back after each step).
type ComputerActionResponse struct {
	Output     string `json:"output,omitempty"`
	Screenshot string `json:"screenshot,omitempty"`
}

// ComputerDisplay reports the guest display: whether it is up and at what
// geometry, so a caller can populate display_width_px / display_height_px in
// its tool definition without hardcoding them. Up is false with a reason in
// Error on an image with no desktop.
type ComputerDisplay struct {
	Display string `json:"display,omitempty"`
	Up      bool   `json:"up"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	Error   string `json:"error,omitempty"`
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
	CPUs      int `json:"cpus"`
	RamMB     int `json:"ram_mb"`
	StorageGB int `json:"storage_gb"`
	VMCount   int `json:"vm_count"`
	// Arch is the host's CPU architecture in GOARCH vocabulary ("amd64",
	// "arm64"). Probed from the host agent when it reports one; may be
	// declared at registration. Empty means amd64 (pre-arch hosts).
	Arch    string `json:"arch,omitempty"`
	GPUs    int    `json:"gpus,omitempty"`
	GPUKind string `json:"gpu_kind,omitempty"`

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
//
// Labels are operator-declared key/value pairs matched against a spec's
// placement label selectors. They are never probed from the host agent.
type RegisterHostRequest struct {
	ID       string            `json:"id"`
	URL      string            `json:"url"`
	Token    string            `json:"token,omitempty"`
	Region   string            `json:"region,omitempty"`
	Backend  string            `json:"backend,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
	Capacity HostCapacity      `json:"capacity"`
}

// HostInfo is the JSON shape returned for a single host.
type HostInfo struct {
	ID        string            `json:"id"`
	URL       string            `json:"url"`
	Region    string            `json:"region,omitempty"`
	Backend   string            `json:"backend,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	State     string            `json:"state"`
	Capacity  HostCapacity      `json:"capacity"`
	Allocated HostCapacity      `json:"allocated"`
	LastSeen  time.Time         `json:"last_seen"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`

	// Warnings carries non-fatal notices from registration (e.g. a
	// declared capacity value that exceeds what was probed from the host
	// agent). Only ever populated on the POST /v1/hosts response.
	Warnings []string `json:"warnings,omitempty"`
}

// HostList is the response body for GET /v1/hosts.
//
// NextCursor is an opaque token: pass it back as the "cursor" query param
// to fetch the next page. It is absent once there are no more results.
type HostList struct {
	Hosts      []HostInfo `json:"hosts"`
	NextCursor *string    `json:"next_cursor,omitempty"`
}
