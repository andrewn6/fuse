from __future__ import annotations

from datetime import datetime
from typing import Optional

from pydantic import BaseModel, ConfigDict, Field

# lifecycle states for EnvironmentInfo.state and Event.state.
STATE_PROVISIONING = "provisioning"
STATE_RUNNING = "running"
STATE_DRAINING = "draining"
STATE_DESTROYING = "destroying"
STATE_DESTROYED = "destroyed"
STATE_FAILED = "failed"


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


class CreateRequest(_Model):
    # body for client.environments.create.
    task_id: str
    spec: Spec = Field(default_factory=Spec)
    manifest_inline: Optional[str] = None
    secrets: Optional[dict[str, str]] = None
    startup_script: Optional[str] = None
    gateway_url: Optional[str] = None
    gateway_token: Optional[str] = None
    expose: Optional[list[ExposeSpec]] = None


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
