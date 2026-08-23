from __future__ import annotations

from datetime import datetime
from typing import Any, Optional

from pydantic import BaseModel, ConfigDict, Field

# lifecycle states for EnvironmentInfo.state and Event.state.
STATE_PROVISIONING = "provisioning"
STATE_RUNNING = "running"
STATE_DRAINING = "draining"
STATE_DESTROYING = "destroying"
STATE_DESTROYED = "destroyed"
STATE_FAILED = "failed"


# verdicts carried in Health.state. separate from the lifecycle states above on
# purpose: a failing probe reports, it does not move the environment anywhere
# new.
HEALTH_STARTING = "starting"
HEALTH_PASSING = "passing"
HEALTH_FAILING = "failing"


def is_terminal_state(state: str) -> bool:
    # reports whether state is a terminal lifecycle state.
    return state in (STATE_DESTROYED, STATE_FAILED)


class _Model(BaseModel):
    # populate_by_name lets us build models with python field names while still
    # accepting wire aliases. extra="ignore" tolerates unknown server fields.
    model_config = ConfigDict(populate_by_name=True, extra="ignore")


class Spec(_Model):
    # hardware/runtime spec for a microvm. all fields are optional (omitempty).
    cpus: Optional[int] = None
    ram_mb: Optional[int] = None
    storage_gb: Optional[int] = None
    gpus: Optional[int] = None
    gpu_kind: Optional[str] = None
    # mig profile (e.g. "1g.10gb"); when set, gpus counts mig instances.
    gpu_profile: Optional[str] = None
    region: Optional[str] = None
    # restricts scheduling to hosts of this cpu architecture ("amd64",
    # "arm64"). none means any host.
    arch: Optional[str] = None
    max_runtime_seconds: Optional[int] = None
    # destroys the environment after this many seconds with no exec and no
    # attach session. none or 0 means no idle expiry.
    idle_timeout_seconds: Optional[int] = None
    image: Optional[str] = None
    # pins the environment to an exact host id (the fusefile's placement.host).
    host_id: Optional[str] = None
    # placement label selectors (the fusefile's placement.labels); every pair
    # must match the target host's declared labels.
    labels: Optional[dict[str, str]] = None


class ExposeSpec(_Model):
    # a port to publish from the microvm. as_ maps to the wire key "as"
    # because as is a reserved python keyword.
    port: int
    as_: Optional[str] = Field(default=None, alias="as")


class Endpoint(_Model):
    # a published endpoint reported by the server. as_ maps to the wire
    # key "as" because as is a reserved python keyword.
    as_: Optional[str] = Field(default=None, alias="as")
    url: str = ""
    port: int = 0


class HealthcheckHttp(_Model):
    # an http get against a port inside the guest. the probe runs in-guest, so
    # the port needs no matching expose entry.
    port: int
    # request path, which must start with a slash. none means "/".
    path: Optional[str] = None


class HealthcheckSpec(_Model):
    # the environment-level readiness probe (the fusefile's `healthcheck:`
    # block). exactly one of http and exec must be set; the server rejects a
    # request that sets both or neither.
    #
    # this is not the per-service compose healthcheck that can appear inside a
    # manifest: that one governs a single container and never reaches the
    # control plane. this one is evaluated by the guest agent over the whole
    # environment and its verdict comes back on EnvironmentInfo.health.
    #
    # every duration is in seconds, and none or 0 means "omitted, use the guest
    # agent's default". the server substitutes no defaults of its own, because
    # the probe executes in the guest.
    http: Optional[HealthcheckHttp] = None
    # an argv run inside the guest. exit status 0 passes.
    exec: Optional[list[str]] = None
    # seconds between attempts. none or 0 for the guest agent default (10s).
    interval_seconds: Optional[int] = None
    # bound on one attempt, in seconds. none or 0 for the guest agent default
    # (2s). attempts run one at a time, so a value above interval_seconds is
    # rejected rather than silently stretching the interval.
    timeout_seconds: Optional[int] = None
    # consecutive failures that flip the verdict to failing. none or 0 for the
    # guest agent default (3).
    retries: Optional[int] = None
    # grace window from the first attempt during which failures are not
    # counted. it ends for good once the probe passes once.
    start_period_seconds: Optional[int] = None


class DesktopSpec(_Model):
    # the geometry of the environment's graphical session (the fusefile's
    # `desktop:` block). requires an image that carries the desktop stack; on
    # any other image the declaration is inert and the computer surface
    # reports the display as absent.
    #
    # both fields are required, 320 to 3840 each: a guessed dimension would
    # silently shift every coordinate a computer-use model emits.
    width: int
    height: int


class ComputerAction(_Model):
    # one computer-use action, in the same shape anthropic's computer tool
    # emits as tool_use input, so translating a tool_use block into a call is
    # mechanical. only action is required; which other fields apply depends on
    # the action, and the guest agent enforces the full schema.
    action: str
    # [x, y] target for pointer actions.
    coordinate: Optional[list[int]] = None
    # [x, y] origin for left_click_drag.
    start_coordinate: Optional[list[int]] = None
    # the payload for type, the keysym combo for key/hold_key, or modifiers
    # held during a click action.
    text: Optional[str] = None
    # [x1, y1, x2, y2] crop for zoom.
    region: Optional[list[int]] = None
    # seconds for hold_key and wait, capped guest-side at 100.
    duration: Optional[float] = None
    scroll_direction: Optional[str] = None
    # wheel clicks for scroll, 1 to 100.
    scroll_amount: Optional[int] = None


class ComputerResult(_Model):
    # the guest's answer to one action. screenshot is a base64 png, present on
    # every action that implies one; for zoom it is the cropped region at full
    # resolution.
    output: str = ""
    screenshot: str = ""


class ComputerDisplay(_Model):
    # the environment's display report: whether it is up and at what geometry,
    # so a caller can populate display_width_px / display_height_px in its
    # computer tool definition without hardcoding them.
    display: str = ""
    up: bool = False
    # live display size in pixels, 0 when the display is down.
    width: int = 0
    height: int = 0
    # why the display is down, when it is.
    error: str = ""


class ComputerToolResult(_Model):
    # what goes back to claude for one computer tool_use: the content blocks
    # and whether they describe an error. the blocks are plain dicts already
    # in the messages api shape, so they can be placed on a tool_result block
    # verbatim:
    #
    #   {"type": "tool_result", "tool_use_id": block.id,
    #    "content": result.content, "is_error": result.is_error}
    content: list[dict[str, Any]] = Field(default_factory=list)
    is_error: bool = False


class Health(_Model):
    # the last verdict of an environment's healthcheck.
    #
    # deliberately not folded into EnvironmentInfo.state: that vocabulary is a
    # closed set clients reason about, and an unhealthy environment is still a
    # running one. nothing tears an environment down for a failing probe.
    state: str = ""
    # when the probe entered this state.
    since: Optional[datetime] = None
    # consecutive failed attempts. zero while passing.
    failures: int = 0
    # the last attempt's failure detail. empty while passing.
    message: str = ""


class CreateRequest(_Model):
    # body for client.environments.create.
    task_id: str
    spec: Spec = Field(default_factory=Spec)
    manifest_inline: Optional[str] = None
    secrets: Optional[dict[str, str]] = None
    startup_script: Optional[str] = None
    # guest files written before the startup script runs, keyed by absolute
    # guest path with base64-encoded content. what a fusefile's `copy` block
    # compiles to. paths under /fuse are rejected, the total decoded size is
    # capped at 512 KiB, and permissions are not carried.
    files: Optional[dict[str, str]] = None
    gateway_url: Optional[str] = None
    gateway_token: Optional[str] = None
    expose: Optional[list[ExposeSpec]] = None
    # environment-level readiness probe. none for an environment with no probe,
    # in which case EnvironmentInfo.health is never populated. it is not
    # evaluated inside create: the call returns as soon as the vm is up, and
    # the verdict arrives on later reads.
    healthcheck: Optional[HealthcheckSpec] = None
    # graphical session geometry. none for an environment with no desktop, in
    # which case a desktop image keeps its baked default geometry.
    desktop: Optional[DesktopSpec] = None


class EnvironmentInfo(_Model):
    # the server's view of a single microvm.
    id: str = ""
    state: str = ""
    task_id: str = ""
    host_id: str = ""
    url: str = ""
    spec: Spec = Field(default_factory=Spec)
    created_at: Optional[datetime] = None
    updated_at: Optional[datetime] = None
    error: str = ""
    endpoints: list[Endpoint] = Field(default_factory=list)
    # last verdict of the environment-level healthcheck. none when the
    # environment declared no healthcheck, and none until the first verdict has
    # been read back from the guest. the server refreshes it on its reconcile
    # tick (30s by default), so it lags the guest by up to a tick.
    health: Optional[Health] = None


class ForkOptions(_Model):
    # optional body for environments.fork.
    reuse_snapshot_id: Optional[str] = None
    comment: Optional[str] = None


class ExecRequest(_Model):
    # body for environments.exec. exactly one of cmd or shell must be set.
    #
    # cmd is argv: it needs no quoting rules and interpolating a value into it
    # cannot turn into an injection, so prefer it. shell runs the string under
    # `sh -lc` and is only for what argv cannot express: pipelines, redirects,
    # and globs. timeout_ms bounds the command inside the guest; unset or 0
    # takes the server default and the server clamps anything above its ceiling.
    cmd: Optional[list[str]] = None
    shell: Optional[str] = None
    timeout_ms: Optional[int] = None


class ExecResult(_Model):
    # the outcome of a guest command.
    #
    # a non-zero exit_code is a successful call: the command ran and failed.
    # only a raised ApiError means the command could not be run at all.
    exit_code: int = 0
    stdout: str = ""
    stderr: str = ""


class Event(BaseModel):
    # one item from EnvironmentsService.events. err is set only on a
    # stream-level failure, as the final event before the iterator ends.
    model_config = ConfigDict(
        populate_by_name=True, extra="ignore", arbitrary_types_allowed=True
    )

    id: str = ""
    kind: str = Field(default="", alias="event")
    vm_id: str = ""
    state: str = ""
    url: str = ""
    error: str = ""
    updated_at: Optional[datetime] = None
    err: Optional[Exception] = Field(default=None, exclude=True)

    # set only on "step" events, which report one setup step's boundary,
    # timing, and cache verdict. zero on state events.
    index: int = 0
    total: int = 0
    key: str = ""
    cached: bool = False
    miss_reason: str = ""
    miss_detail: list[str] = Field(default_factory=list)
    duration_ms: int = 0
    exit_code: int = 0


class SnapshotRequest(_Model):
    # optional body for snapshots.create.
    comment: Optional[str] = None
    mode: Optional[str] = None
    retention_seconds: Optional[int] = None
    metadata: Optional[dict[str, str]] = None
    export_ref: Optional[str] = None
    export_status: Optional[str] = None
    # labels this snapshot as the artifact of one cacheable setup step, which is
    # what makes it findable by recipe rather than by its random id. none for an
    # ordinary snapshot. the scope it is filed under comes from how the caller
    # authenticated and is not settable here.
    layer_key: Optional[str] = None
    # captures the guest's memory and vcpu state alongside the rootfs, so
    # restoring resumes the guest instead of cold-booting it. unset (or false)
    # is a disk-only snapshot, which is the default and the fallback.
    #
    # it costs the environment's full configured memory in bytes on every
    # create, pauses the guest for the length of the capture, and produces an
    # artifact pinned to the host that took it. fork reads only a snapshot's
    # rootfs, so forking a live snapshot yields a cold-booting environment with
    # none of the memory, and nothing raises when it does.
    #
    # against a backend with no live-snapshot support this is a 501
    # unimplemented, not a conflict, so is_conflict reports false for it.
    #
    # it is a request, not a guarantee: read Snapshot.kind to find out what was
    # actually written.
    live: Optional[bool] = None


class SnapshotExport(_Model):
    # an optional exported snapshot artifact.
    destination: str = ""
    status: str = ""
    requested_at: Optional[datetime] = None
    updated_at: Optional[datetime] = None
    last_error: str = ""


class Snapshot(_Model):
    # a persisted snapshot record.
    id: str = ""
    vm_id: str = ""
    task_id: str = ""
    tenant_id: str = ""
    parent_snapshot_id: str = ""
    mode: str = ""
    state: str = ""
    comment: str = ""
    # the content-addressed cache key of the setup step this artifact was
    # taken after. empty on anything that is not a build layer, and an empty
    # key never matches a lookup.
    layer_key: str = ""
    # cpu architecture, in goarch vocabulary ("amd64", "arm64"), of the host
    # that actually built the artifact rather than of whoever asked for it. a
    # rootfs is not portable across architectures, so this is a real
    # constraint on whether the artifact can be booted; it is deliberately not
    # folded into layer_key, so a lookup has to filter on it separately.
    arch: str = ""
    # hex sha256 of the artifact rootfs. it verifies that a given copy of
    # those bytes is intact, and nothing more: it is not a cross-build
    # identity, because two builds of the same recipe produce different rootfs
    # bytes (timestamps, inode ordering, package caches). it can never be used
    # as a cache key.
    digest: str = ""
    # "disk" or "live": whether this artifact is the rootfs alone, or the
    # rootfs plus the guest's memory and vcpu state. restoring a disk snapshot
    # cold-boots the guest; restoring a live one resumes it.
    #
    # it is what the host agent reports it actually wrote, not what was
    # requested. a host running an agent too old to know about live snapshots
    # answers SnapshotRequest(live=True) with a disk snapshot, and this then
    # says "disk". branch on it rather than on your own request.
    #
    # a server that predates live snapshots omits it entirely, which reads back
    # as ""; treat that as "disk".
    kind: str = ""
    size_bytes: int = 0
    created_at: Optional[datetime] = None
    updated_at: Optional[datetime] = None
    retention_until: Optional[datetime] = None
    last_error: str = ""
    export_ref: str = ""
    exports: list[SnapshotExport] = Field(default_factory=list)


class GPUDevice(_Model):
    # per-device detail probed for a single gpu. every field is best-effort
    # and omitted when the host agent could not determine it.
    uuid: str = ""
    model: str = ""
    pci_bus_id: str = ""
    memory_mb: int = 0
    driver_version: str = ""
    cuda_version: str = ""
    compute_cap: str = ""
    mig_capable: bool = False
    mig_mode: str = ""
    iommu_group: str = ""


class MIGInstance(_Model):
    # one carved mig gpu instance probed from the host agent. the orchestrator
    # binds a specific instance uuid to a vm so it knows which instance went
    # to which vm.
    uuid: str = ""
    profile: str = ""
    kind: str = ""
    parent_gpu_uuid: str = ""


class HostCapacity(_Model):
    # a host's resource envelope.
    cpus: int = 0
    ram_mb: int = 0
    storage_gb: int = 0
    vm_count: int = 0
    # the host's cpu architecture in goarch vocabulary ("amd64", "arm64").
    # empty means amd64 (hosts registered before arch existed).
    arch: str = ""
    gpus: int = 0
    gpu_kind: str = ""
    # fractional gpu capacity: mig instance count by profile name. when the
    # host reports per-instance mig inventory (mig_instances), this map is a
    # derived summary; otherwise it is the scheduling unit.
    mig_profiles: Optional[dict[str, int]] = None
    # per-instance mig inventory probed from the host agent (one entry per
    # carved mig gpu instance). when non-empty, the orchestrator binds
    # specific instance uuids to vms. strictly additive: a host that reports
    # no instances falls back to mig_profiles. only populated on capacity
    # (not allocated) for qemu hosts.
    mig_instances: list[MIGInstance] = Field(default_factory=list)
    # the set of mig instance uuids currently bound to vms. populated only on
    # allocated, and only for hosts that report per-instance mig inventory.
    mig_instance_uuids: list[str] = Field(default_factory=list)
    # per-device gpu detail probed from the host agent. only populated on
    # capacity (not allocated) for qemu hosts.
    gpu_devices: list[GPUDevice] = Field(default_factory=list)


class RegisterHostRequest(_Model):
    # body for hosts.register.
    id: str
    url: str
    token: Optional[str] = None
    region: Optional[str] = None
    backend: Optional[str] = None
    # operator-declared key/value pairs matched against a spec's placement
    # label selectors. never probed from the host agent.
    labels: Optional[dict[str, str]] = None
    capacity: HostCapacity = Field(default_factory=HostCapacity)


class Host(_Model):
    # the server's view of a registered host.
    id: str = ""
    url: str = ""
    region: str = ""
    state: str = ""
    backend: str = ""
    labels: dict[str, str] = Field(default_factory=dict)
    capacity: HostCapacity = Field(default_factory=HostCapacity)
    allocated: HostCapacity = Field(default_factory=HostCapacity)
    last_seen: Optional[datetime] = None
    created_at: Optional[datetime] = None
    updated_at: Optional[datetime] = None
    # non-fatal registration notices (e.g. declared capacity exceeding the
    # probed value). only ever populated on the response to hosts.register.
    warnings: list[str] = Field(default_factory=list)


class EnvironmentPage(_Model):
    # one page of environments.list_page, plus the cursor to fetch the next
    # one. next_cursor is an opaque token; treat it as such rather than
    # parsing it. it is none once there are no more results.
    environments: list[EnvironmentInfo] = Field(default_factory=list)
    next_cursor: Optional[str] = None


class SnapshotPage(_Model):
    # one page of snapshots.list_page, plus the cursor to fetch the next
    # one. next_cursor is none once there are no more results.
    snapshots: list[Snapshot] = Field(default_factory=list)
    next_cursor: Optional[str] = None


class HostPage(_Model):
    # one page of hosts.list_page, plus the cursor to fetch the next one.
    # next_cursor is none once there are no more results.
    hosts: list[Host] = Field(default_factory=list)
    next_cursor: Optional[str] = None


class APIKey(_Model):
    # a key's metadata. the raw secret appears only in CreatedAPIKey.key.
    id: str = ""
    label: str = ""
    created_at: Optional[datetime] = None
    last_used_at: Optional[datetime] = None
    revoked_at: Optional[datetime] = None


class CreatedAPIKey(APIKey):
    # returned by api_keys.create. key is unrecoverable afterward.
    key: str = ""
