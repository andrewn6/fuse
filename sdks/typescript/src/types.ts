// Wire types for the Fuse API. Field names match the JSON sent on the wire
// (snake_case), so request/response bodies pass through JSON.stringify /
// JSON.parse untouched — a faithful 1:1 mirror of the Go SDK's struct tags
// (see sdks/go/types.go). List *options* (see each service) use camelCase
// keys and are translated to snake_case query params by the service.

/** Spec is the hardware/runtime spec for a microVM. */
export interface Spec {
  cpus?: number;
  ram_mb?: number;
  storage_gb?: number;
  gpus?: number;
  gpu_kind?: string;
  /** Requests fractional GPU allocation: a MIG profile in mig-parted
   * vocabulary (e.g. "1g.10gb"). When set, gpus counts MIG instances of this
   * profile rather than whole devices. */
  gpu_profile?: string;
  region?: string;
  /** Restricts scheduling to hosts of this CPU architecture ("amd64",
   * "arm64"). Omitted means any host. */
  arch?: string;
  max_runtime_seconds?: number;
  /** Destroys the environment after this many seconds with no exec and no
   * attach session. Omitted or 0 means no idle expiry. Unlike
   * max_runtime_seconds (a ceiling measured from create), this is measured
   * from the last exec or attach. */
  idle_timeout_seconds?: number;
  image?: string;
  /** Pins the environment to an exact host id (the Fusefile's
   * placement.host). The pinned host still has to be active, run the right
   * backend, and fit the request. */
  host_id?: string;
  /** Placement label selectors (the Fusefile's placement.labels): every pair
   * must match the target host's declared labels. */
  labels?: Record<string, string>;
}

/** ExposeSpec requests that a guest port be published at boot. */
export interface ExposeSpec {
  port: number;
  as?: string;
}

/** Endpoint is a published port with its externally reachable URL. */
export interface Endpoint {
  as?: string;
  url: string;
  port: number;
}

/** HealthcheckHttp is an HTTP GET against a port inside the guest. The probe
 * runs in-guest, so the port needs no matching ExposeSpec. */
export interface HealthcheckHttp {
  port: number;
  /** Request path, which must start with a slash. Defaults to "/". */
  path?: string;
}

/**
 * HealthcheckSpec is the environment-level readiness probe (the Fusefile's
 * `healthcheck:` block). Exactly one of http and exec must be set; the server
 * rejects a request that sets both or neither.
 *
 * It is not the per-service compose healthcheck that can appear inside a
 * manifest: that one governs a single container and never reaches the control
 * plane. This one is evaluated by the guest agent over the environment as a
 * whole and its verdict comes back on EnvironmentInfo.health.
 *
 * Every duration is in seconds, and 0 means "omitted, use the guest agent's
 * default". The server substitutes no defaults of its own, because the probe
 * executes in the guest.
 */
export interface HealthcheckSpec {
  http?: HealthcheckHttp;
  /** An argv run inside the guest. Exit status 0 passes. */
  exec?: string[];
  /** Seconds between attempts. 0 for the guest agent default (10s). */
  interval_seconds?: number;
  /** Bound on one attempt, in seconds. 0 for the guest agent default (2s).
   * Attempts run one at a time, so a value above interval_seconds is
   * rejected rather than silently stretching the interval. */
  timeout_seconds?: number;
  /** Consecutive failures that flip the verdict to failing. 0 for the guest
   * agent default (3). */
  retries?: number;
  /** Grace window from the first attempt during which failures are not
   * counted. It ends for good once the probe passes once. */
  start_period_seconds?: number;
}

/**
 * DesktopSpec is the geometry of the environment's graphical session (the
 * Fusefile's `desktop:` block). It requires an image that carries the desktop
 * stack; on any other image the declaration is inert and the computer surface
 * reports the display as absent.
 *
 * Both fields are required, 320 to 3840 each: a guessed dimension would
 * silently shift every coordinate a computer-use model emits.
 */
export interface DesktopSpec {
  /** Display width in pixels. */
  width: number;
  /** Display height in pixels. */
  height: number;
}

/**
 * ComputerAction is one computer-use action, in the same shape Anthropic's
 * computer tool emits as tool_use input, so translating a tool_use block into
 * a call is mechanical. Only action is required; which other fields apply
 * depends on the action, and the guest agent enforces the full schema.
 */
export interface ComputerAction {
  /** The action to perform, e.g. "screenshot", "left_click", "type", "key",
   * "scroll", "zoom". */
  action: string;
  /** [x, y] target for pointer actions. */
  coordinate?: number[];
  /** [x, y] origin for left_click_drag. */
  start_coordinate?: number[];
  /** The payload for type, the keysym combo for key/hold_key, or modifiers
   * held during a click action. */
  text?: string;
  /** [x1, y1, x2, y2] crop for zoom. */
  region?: number[];
  /** Seconds for hold_key and wait, capped guest-side at 100. */
  duration?: number;
  scroll_direction?: "up" | "down" | "left" | "right";
  /** Wheel clicks for scroll, 1 to 100. */
  scroll_amount?: number;
}

/**
 * ComputerResult is the guest's answer to one action. screenshot is a base64
 * PNG, present on every action that implies one; for zoom it is the cropped
 * region at full resolution.
 */
export interface ComputerResult {
  output?: string;
  screenshot?: string;
}

/**
 * ComputerDisplay reports the environment's display: whether it is up and at
 * what geometry, so a caller can populate display_width_px /
 * display_height_px in its computer tool definition without hardcoding them.
 */
export interface ComputerDisplay {
  display?: string;
  up: boolean;
  /** Live display width in pixels, 0 when the display is down. */
  width: number;
  /** Live display height in pixels, 0 when the display is down. */
  height: number;
  /** Why the display is down, when it is. */
  error?: string;
}

/** ToolResultImageSource is a Messages-API base64 image source. */
export interface ToolResultImageSource {
  type: "base64";
  media_type: "image/png";
  data: string;
}

/**
 * ToolResultBlock is one content block of a Messages API tool_result. The
 * field names match the Messages API, so the blocks can be sent back verbatim
 * as the tool_result content.
 */
export interface ToolResultBlock {
  type: "text" | "image";
  text?: string;
  source?: ToolResultImageSource;
}

/**
 * ComputerToolResult is what goes back to Claude for one computer tool_use:
 * the content blocks and whether they describe an error. Map it onto the
 * tool_result block for the tool_use's id and the loop is closed.
 */
export interface ComputerToolResult {
  content: ToolResultBlock[];
  is_error?: boolean;
}

/**
 * Health is the last verdict of an environment's healthcheck.
 *
 * It is deliberately not folded into EnvironmentInfo.state: that vocabulary is
 * a closed set clients reason about, and an unhealthy environment is still a
 * running one. Nothing tears an environment down for a failing probe.
 */
export interface Health {
  /** "starting", "passing" or "failing". */
  state: string;
  /** When the probe entered this state. */
  since?: string;
  /** Consecutive failed attempts. Zero while passing. */
  failures?: number;
  /** The last attempt's failure detail. Empty while passing. */
  message?: string;
}

/** CreateRequest is the body for environments.create. */
export interface CreateRequest {
  task_id: string;
  spec?: Spec;
  manifest_inline?: string;
  secrets?: Record<string, string>;
  startup_script?: string;
  /**
   * Guest files written before the startup script runs, keyed by absolute
   * guest path with base64-encoded content. What a Fusefile's `copy` block
   * compiles to. Paths under /fuse are rejected, the total decoded size is
   * capped at 512 KiB, and permissions are not carried.
   */
  files?: Record<string, string>;
  gateway_url?: string;
  gateway_token?: string;
  expose?: ExposeSpec[];
  /** Environment-level readiness probe. Omit it for an environment with no
   * probe, in which case EnvironmentInfo.health is never populated. It is not
   * evaluated inside create: the call returns as soon as the VM is up, and
   * the verdict arrives on later reads. */
  healthcheck?: HealthcheckSpec;
  /** Graphical session geometry. Omit it for an environment with no desktop,
   * in which case a desktop image keeps its baked default geometry. */
  desktop?: DesktopSpec;
}

/** EnvironmentInfo is the server's view of a single microVM. */
export interface EnvironmentInfo {
  id: string;
  state: string;
  task_id: string;
  host_id?: string;
  url: string;
  spec: Spec;
  created_at: string;
  updated_at: string;
  error?: string;
  endpoints?: Endpoint[];
  /** Last verdict of the environment-level healthcheck. Absent when the
   * environment declared no healthcheck, and absent until the first verdict
   * has been read back from the guest. The server refreshes it on its
   * reconcile tick (30s by default), so it lags the guest by up to a tick. */
  health?: Health;
}

/** ForkOptions is the optional body for environments.fork. */
export interface ForkOptions {
  reuse_snapshot_id?: string;
  comment?: string;
}

/** ExecRequest is the body for environments.exec. Exactly one of cmd or shell must be set. */
export interface ExecRequest {
  /**
   * The argv to run in the guest, e.g. ["ls", "-l"]. Argv needs no quoting
   * rules and cannot be turned into an injection by interpolating a value, so
   * prefer it.
   */
  cmd?: string[];

  /**
   * Runs the string under `sh -lc`. Use it only for what argv cannot express:
   * pipelines, redirects, and globs.
   */
  shell?: string;

  /**
   * Bounds the command inside the guest. Zero or unset takes the server
   * default; the server clamps anything above its ceiling.
   */
  timeout_ms?: number;
}

/**
 * ExecResult is the outcome of a guest command.
 *
 * A non-zero exit_code is a successful call: the command ran and failed. Only a
 * thrown error means the command could not be run at all.
 */
export interface ExecResult {
  exit_code: number;
  stdout: string;
  stderr: string;
}

/**
 * Event is one item from environments.events. It matches the server's SSE
 * wire payload. Stream-level failures are thrown from the async iterator
 * rather than delivered as an in-band event.
 */
export interface Event {
  id: string;
  /** Event kind: "state" (an omitted kind means "state") or "step". */
  event: string;
  vm_id: string;
  state: string;
  url?: string;
  error?: string;
  updated_at: string;
  /** index of the setup step, 1-based. step events only. */
  index?: number;
  /** total number of setup steps. step events only. */
  total?: number;
  /** layer cache key for the step. step events only. */
  key?: string;
  /** whether the step was served from the layer cache. step events only. */
  cached?: boolean;
  /** why the step missed the cache. step events only. */
  miss_reason?: string;
  /** what changed, e.g. the input paths. step events only. */
  miss_detail?: string[];
  /** wall-clock duration of the step in milliseconds. step events only. */
  duration_ms?: number;
  /** exit code of the step. step events only. */
  exit_code?: number;
}

/** SnapshotRequest is the optional body for snapshots.create. */
export interface SnapshotRequest {
  comment?: string;
  mode?: string;
  retention_seconds?: number;
  metadata?: Record<string, string>;
  export_ref?: string;
  export_status?: string;
  /** Labels this snapshot as the artifact of one cacheable setup step, which is
   * what makes it findable by recipe rather than by its random id. Empty for an
   * ordinary snapshot. The scope it is filed under comes from how the caller
   * authenticated and is not settable here. */
  layer_key?: string;
  /** Capture the guest's memory and vCPU state alongside the rootfs, so
   * restoring resumes the guest instead of cold-booting it. Omitted or false
   * takes a disk-only snapshot, which is the default and the fallback.
   *
   * It costs the environment's full configured memory in bytes on every
   * create, pauses the guest for the length of the capture, and produces an
   * artifact pinned to the host that took it. fork reads only a snapshot's
   * rootfs, so forking a live snapshot yields a cold-booting environment with
   * none of the memory, and nothing throws when it does.
   *
   * Against a backend with no live-snapshot support this is a 501
   * `unimplemented`, not a conflict.
   *
   * It is a request, not a guarantee: read `Snapshot.kind` to find out what
   * was actually written. */
  live?: boolean;
}

/** ResolveSnapshotResponse is the wire envelope for a layer lookup. Internal:
 * resolve() returns Snapshot | null so the found flag never reaches callers. */
export interface ResolveSnapshotResponse {
  found: boolean;
  snapshot?: Snapshot;
}

/** SnapshotExport is an optional exported snapshot artifact. */
export interface SnapshotExport {
  destination: string;
  status?: string;
  requested_at?: string;
  updated_at?: string;
  last_error?: string;
}

/** Snapshot is a persisted snapshot record. */
export interface Snapshot {
  id: string;
  vm_id: string;
  task_id?: string;
  tenant_id?: string;
  parent_snapshot_id?: string;
  mode?: string;
  state?: string;
  comment?: string;
  /** The content-addressed cache key of the setup step this artifact was
   * taken after. Absent on anything that is not a build layer, and an empty
   * key never matches a lookup. */
  layer_key?: string;
  /** CPU architecture, in GOARCH vocabulary ("amd64", "arm64"), of the host
   * that actually built the artifact rather than of whoever asked for it. A
   * rootfs is not portable across architectures, so this is a real constraint
   * on whether the artifact can be booted; it is deliberately not folded into
   * layer_key, so a lookup has to filter on it separately. */
  arch?: string;
  /** Hex sha256 of the artifact rootfs. It verifies that a given copy of
   * those bytes is intact, and nothing more: it is not a cross-build
   * identity, because two builds of the same recipe produce different rootfs
   * bytes (timestamps, inode ordering, package caches). It can never be used
   * as a cache key. */
  digest?: string;
  /** Whether this artifact is the rootfs alone (`"disk"`), or the rootfs plus
   * the guest's memory and vCPU state (`"live"`). Restoring a disk snapshot
   * cold-boots the guest; restoring a live one resumes it.
   *
   * It is what the host agent reports it actually wrote, not what was
   * requested. A host running an agent too old to know about live snapshots
   * answers `live: true` with a disk snapshot, and this then says `"disk"`.
   * Branch on it rather than on your own request.
   *
   * Typed loosely rather than as a `"disk" | "live"` union so a server that
   * grows a third kind does not turn into a compile error here. A server that
   * predates live snapshots omits it entirely; treat undefined as `"disk"`. */
  kind?: string;
  size_bytes?: number;
  created_at: string;
  updated_at?: string;
  retention_until?: string;
  last_error?: string;
  export_ref?: string;
  exports?: SnapshotExport[];
}

/** HostCapacity is a host's resource envelope. */
export interface HostCapacity {
  cpus: number;
  ram_mb: number;
  storage_gb: number;
  vm_count: number;
  /** The host's CPU architecture in GOARCH vocabulary ("amd64", "arm64").
   * Omitted means amd64 (hosts registered before arch existed). */
  arch?: string;
  gpus?: number;
  gpu_kind?: string;
  /** Fractional GPU capacity: MIG instance count by profile name (e.g.
   * {"1g.10gb": 4}). Like gpus, it requires backend "qemu". When the host
   * reports per-instance MIG inventory (mig_instances), this map is a derived
   * summary; otherwise it is the scheduling unit. */
  mig_profiles?: Record<string, number>;
  /** Per-instance MIG inventory probed from the host agent (one entry per
   * carved MIG GPU instance). When non-empty, the orchestrator binds specific
   * instance uuids to VMs instead of decrementing a count. Strictly additive:
   * a host that reports no instances falls back to mig_profiles. Only
   * populated on capacity (not allocated) for qemu hosts. */
  mig_instances?: MIGInstance[];
  /** The set of MIG instance uuids currently bound to VMs. Populated only on
   * allocated, and only for hosts that report per-instance MIG inventory. */
  mig_instance_uuids?: string[];
  /** Per-device GPU detail probed from the host agent, carried alongside the
   * scalar gpus/gpu_kind counters. Only populated on capacity (not
   * allocated) for qemu hosts. */
  gpu_devices?: GPUDevice[];
}

/** GPUDevice is the per-device detail probed for a single GPU. Every field is
 * best-effort and omitted when the host agent could not determine it. */
export interface GPUDevice {
  uuid?: string;
  model?: string;
  pci_bus_id?: string;
  memory_mb?: number;
  driver_version?: string;
  cuda_version?: string;
  compute_cap?: string;
  mig_capable?: boolean;
  mig_mode?: string;
  iommu_group?: string;
}

/** MIGInstance is one carved MIG GPU instance probed from the host agent. The
 * orchestrator binds a specific instance uuid to a VM so it knows which
 * instance went to which VM. */
export interface MIGInstance {
  uuid?: string;
  profile?: string;
  kind?: string;
  parent_gpu_uuid?: string;
}

/** RegisterHostRequest is the body for hosts.register. */
export interface RegisterHostRequest {
  id: string;
  url: string;
  token?: string;
  region?: string;
  backend?: string;
  /** Operator-declared key/value pairs matched against a spec's placement
   * label selectors. Never probed from the host agent. */
  labels?: Record<string, string>;
  capacity?: HostCapacity;
}

/** Host is the server's view of a registered host. */
export interface Host {
  id: string;
  url: string;
  region?: string;
  state: string;
  backend?: string;
  labels?: Record<string, string>;
  capacity: HostCapacity;
  allocated: HostCapacity;
  last_seen: string;
  created_at: string;
  updated_at: string;
  /** Non-fatal registration notices (e.g. declared capacity exceeding the
   * probed value). Only ever populated on the response to hosts.register. */
  warnings?: string[];
}

/**
 * APIKey is a key's metadata. The raw secret appears only in
 * CreatedAPIKey.key, returned once at creation.
 */
export interface APIKey {
  id: string;
  label?: string;
  created_at: string;
  last_used_at?: string;
  revoked_at?: string;
}

/**
 * CreatedAPIKey is returned by apiKeys.create. `key` is the raw secret and is
 * unrecoverable after this response.
 */
export interface CreatedAPIKey extends APIKey {
  key: string;
}

/** Lifecycle states for EnvironmentInfo.state and Event.state. */
export const State = {
  Provisioning: "provisioning",
  Running: "running",
  Draining: "draining",
  Destroying: "destroying",
  Destroyed: "destroyed",
  Failed: "failed",
} as const;

export type State = (typeof State)[keyof typeof State];

/** isTerminalState reports whether state is a terminal lifecycle state. */
export function isTerminalState(state: string): boolean {
  return state === State.Destroyed || state === State.Failed;
}

/**
 * Verdicts carried in Health.state. Separate from State on purpose: a failing
 * probe reports, it does not move the environment anywhere new.
 */
export const HealthState = {
  /** The probe has not passed yet and is still inside its start period, so
   * failures are not being counted. */
  Starting: "starting",
  /** The most recent attempt succeeded. */
  Passing: "passing",
  /** The probe failed its configured retries in a row after the start period
   * ended. */
  Failing: "failing",
} as const;

export type HealthState = (typeof HealthState)[keyof typeof HealthState];
