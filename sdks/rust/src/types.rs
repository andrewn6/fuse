use std::collections::HashMap;

use base64::Engine as _;
use chrono::{DateTime, Utc};
use serde::{Deserialize, Deserializer, Serialize};

use crate::strenum::string_enum;

fn is_false(value: &bool) -> bool {
    !*value
}

fn is_zero_u32(value: &u32) -> bool {
    *value == 0
}

// deserializes an optional string field where the empty string means absent,
// into any of this module's string-backed enums.
fn empty_as_none<'de, D, T>(deserializer: D) -> Result<Option<T>, D::Error>
where
    D: Deserializer<'de>,
    T: From<String>,
{
    let value = Option::<String>::deserialize(deserializer)?;
    Ok(value.filter(|s| !s.is_empty()).map(T::from))
}

string_enum! {
    /// Lifecycle states for [`EnvironmentInfo::state`] and [`Event::state`].
    pub enum EnvironmentState {
        Provisioning => "provisioning",
        Running => "running",
        Draining => "draining",
        Destroying => "destroying",
        Destroyed => "destroyed",
        Failed => "failed",
    }
}

impl EnvironmentState {
    /// Reports whether this is a terminal lifecycle state.
    pub fn is_terminal(&self) -> bool {
        matches!(self, Self::Destroyed | Self::Failed)
    }

    /// Reports whether this is a state an environment can settle in at the
    /// end of provisioning: running, or either terminal state. Callers that
    /// wait for an environment to come up should use this, since a healthy
    /// environment stops at running and never becomes terminal.
    pub fn is_settled(&self) -> bool {
        matches!(self, Self::Running) || self.is_terminal()
    }
}

string_enum! {
    /// Verdicts carried in [`Health::state`].
    pub enum HealthState {
        /// The probe has not passed yet and is still inside its start
        /// period, so failures are not being counted.
        Starting => "starting",
        /// The most recent attempt succeeded.
        Passing => "passing",
        /// The probe failed its configured retries in a row after the start
        /// period ended.
        Failing => "failing",
    }
}

string_enum! {
    /// CPU architectures, in Go's GOARCH vocabulary.
    pub enum Arch {
        Amd64 => "amd64",
        Arm64 => "arm64",
    }
}

string_enum! {
    /// Scheduling eligibility of a registered host, in [`Host::state`].
    pub enum HostState {
        Active => "active",
        Cordoned => "cordoned",
        Draining => "draining",
    }
}

string_enum! {
    /// Virtualization backends a host can run.
    pub enum HostBackend {
        Firecracker => "firecracker",
        Qemu => "qemu",
    }
}

string_enum! {
    /// Lifecycle states for [`Snapshot::state`].
    pub enum SnapshotState {
        Creating => "creating",
        Ready => "ready",
        Restoring => "restoring",
        Deleting => "deleting",
        Error => "error",
    }
}

string_enum! {
    /// How a snapshot came to exist, in [`Snapshot::mode`].
    pub enum SnapshotMode {
        Manual => "manual",
        Auto => "auto",
        Build => "build",
    }
}

string_enum! {
    /// What a snapshot actually captured, in [`Snapshot::kind`].
    ///
    /// This is a different axis from [`SnapshotMode`]: mode is why the
    /// snapshot was taken, kind is what is inside it. A `Disk` snapshot holds
    /// the rootfs only and cold-boots on restore; a `Live` one also holds
    /// guest memory and vCPU state, and resumes.
    ///
    /// It reports what the host actually wrote, which is not always what was
    /// asked for: a host agent too old to know about live snapshots answers
    /// [`SnapshotRequest::live`] with a disk snapshot. Check this rather than
    /// assuming the request was honoured.
    pub enum SnapshotKind {
        Disk => "disk",
        Live => "live",
    }
}

/// The hardware/runtime spec for a microVM.
///
/// Every field is optional; the orchestrator substitutes defaults. Build one
/// with the chained setters:
///
/// ```
/// use fuse::{Arch, Spec};
///
/// let spec = Spec::new()
///     .cpus(2)
///     .ram_mb(2048)
///     .image("docker.io/library/python:3.12-slim")
///     .arch(Arch::Amd64);
/// ```
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct Spec {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub cpus: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub ram_mb: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub storage_gb: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub gpus: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub gpu_kind: Option<String>,
    /// Requests fractional GPU allocation: a MIG profile in mig-parted
    /// vocabulary (e.g. `1g.10gb`). When set, `gpus` counts MIG instances of
    /// this profile rather than whole devices.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub gpu_profile: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub region: Option<String>,
    /// Restricts scheduling to hosts of this CPU architecture. `None` means
    /// any host.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub arch: Option<Arch>,
    /// Ceiling on the environment's lifetime, measured from create.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub max_runtime_seconds: Option<u64>,
    /// Destroys the environment after this many seconds with no exec and no
    /// attach session. `None` means no idle expiry. Unlike
    /// `max_runtime_seconds` (a ceiling measured from create), this is
    /// measured from the last exec or attach.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub idle_timeout_seconds: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub image: Option<String>,
    /// Pins the environment to an exact host id (the Fusefile's
    /// `placement.host`). The pinned host still has to be active, run the
    /// right backend, and fit the request.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub host_id: Option<String>,
    /// Placement label selectors (the Fusefile's `placement.labels`): every
    /// pair must match the target host's declared labels.
    #[serde(skip_serializing_if = "HashMap::is_empty")]
    pub labels: HashMap<String, String>,
}

impl Spec {
    /// Returns an empty spec; chain setters to fill it in.
    pub fn new() -> Self {
        Self::default()
    }

    pub fn cpus(mut self, cpus: u32) -> Self {
        self.cpus = Some(cpus);
        self
    }

    pub fn ram_mb(mut self, ram_mb: u32) -> Self {
        self.ram_mb = Some(ram_mb);
        self
    }

    pub fn storage_gb(mut self, storage_gb: u32) -> Self {
        self.storage_gb = Some(storage_gb);
        self
    }

    pub fn gpus(mut self, gpus: u32) -> Self {
        self.gpus = Some(gpus);
        self
    }

    pub fn gpu_kind(mut self, gpu_kind: impl Into<String>) -> Self {
        self.gpu_kind = Some(gpu_kind.into());
        self
    }

    pub fn gpu_profile(mut self, gpu_profile: impl Into<String>) -> Self {
        self.gpu_profile = Some(gpu_profile.into());
        self
    }

    pub fn region(mut self, region: impl Into<String>) -> Self {
        self.region = Some(region.into());
        self
    }

    pub fn arch(mut self, arch: Arch) -> Self {
        self.arch = Some(arch);
        self
    }

    pub fn max_runtime_seconds(mut self, seconds: u64) -> Self {
        self.max_runtime_seconds = Some(seconds);
        self
    }

    pub fn idle_timeout_seconds(mut self, seconds: u64) -> Self {
        self.idle_timeout_seconds = Some(seconds);
        self
    }

    pub fn image(mut self, image: impl Into<String>) -> Self {
        self.image = Some(image.into());
        self
    }

    pub fn host_id(mut self, host_id: impl Into<String>) -> Self {
        self.host_id = Some(host_id.into());
        self
    }

    /// Adds one placement label selector.
    pub fn label(mut self, key: impl Into<String>, value: impl Into<String>) -> Self {
        self.labels.insert(key.into(), value.into());
        self
    }
}

/// Requests that a guest port be published as a reachable endpoint.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ExposeSpec {
    pub port: u16,
    /// Optional name the endpoint is published under.
    #[serde(rename = "as", default, skip_serializing_if = "Option::is_none")]
    pub alias: Option<String>,
}

impl ExposeSpec {
    pub fn new(port: u16) -> Self {
        Self { port, alias: None }
    }

    pub fn named(port: u16, alias: impl Into<String>) -> Self {
        Self {
            port,
            alias: Some(alias.into()),
        }
    }
}

/// A published network endpoint for an environment.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct Endpoint {
    #[serde(rename = "as", skip_serializing_if = "Option::is_none")]
    pub alias: Option<String>,
    pub url: String,
    pub port: u16,
}

/// An HTTP GET probe against a port inside the guest. The probe runs
/// in-guest, so the port needs no matching [`ExposeSpec`].
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct HealthcheckHttp {
    pub port: u16,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub path: Option<String>,
}

impl HealthcheckHttp {
    pub fn new(port: u16) -> Self {
        Self { port, path: None }
    }

    pub fn path(mut self, path: impl Into<String>) -> Self {
        self.path = Some(path.into());
        self
    }
}

/// The probe half of a [`HealthcheckSpec`]. The server requires exactly one
/// of HTTP and exec, which this enum makes unrepresentable to get wrong.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub enum HealthcheckProbe {
    #[serde(rename = "http")]
    Http(HealthcheckHttp),
    #[serde(rename = "exec")]
    Exec(Vec<String>),
}

/// The environment-level readiness probe (the Fusefile's `healthcheck:`
/// block).
///
/// It is not the per-service compose healthcheck that can appear inside a
/// manifest: that one governs a single container and never reaches the
/// control plane. This one is evaluated by the guest agent over the
/// environment as a whole and its verdict comes back on
/// [`EnvironmentInfo::health`].
///
/// Every duration is in seconds, and `None` means "omitted, use the guest
/// agent's default". The server substitutes no defaults of its own, because
/// the probe executes in the guest.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct HealthcheckSpec {
    #[serde(flatten)]
    pub probe: HealthcheckProbe,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub interval_seconds: Option<u64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub timeout_seconds: Option<u64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub retries: Option<u32>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub start_period_seconds: Option<u64>,
}

impl HealthcheckSpec {
    /// Returns an HTTP probe healthcheck.
    pub fn http(probe: HealthcheckHttp) -> Self {
        Self::probe(HealthcheckProbe::Http(probe))
    }

    /// Returns an exec probe healthcheck running the given argv in the
    /// guest.
    pub fn exec<I, S>(argv: I) -> Self
    where
        I: IntoIterator<Item = S>,
        S: Into<String>,
    {
        Self::probe(HealthcheckProbe::Exec(
            argv.into_iter().map(Into::into).collect(),
        ))
    }

    fn probe(probe: HealthcheckProbe) -> Self {
        Self {
            probe,
            interval_seconds: None,
            timeout_seconds: None,
            retries: None,
            start_period_seconds: None,
        }
    }

    pub fn interval_seconds(mut self, seconds: u64) -> Self {
        self.interval_seconds = Some(seconds);
        self
    }

    pub fn timeout_seconds(mut self, seconds: u64) -> Self {
        self.timeout_seconds = Some(seconds);
        self
    }

    pub fn retries(mut self, retries: u32) -> Self {
        self.retries = Some(retries);
        self
    }

    pub fn start_period_seconds(mut self, seconds: u64) -> Self {
        self.start_period_seconds = Some(seconds);
        self
    }
}

/// The geometry of the environment's graphical session (the Fusefile's
/// `desktop:` block). It requires an image that carries the desktop stack;
/// on any other image the declaration is inert and the computer surface
/// reports the display as absent.
///
/// Both fields are required, 320 to 3840 each: a guessed dimension would
/// silently shift every coordinate a computer-use model emits.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub struct DesktopSpec {
    pub width: u32,
    pub height: u32,
}

impl DesktopSpec {
    pub fn new(width: u32, height: u32) -> Self {
        Self { width, height }
    }
}

/// The last verdict of an environment's healthcheck.
///
/// It is deliberately not folded into [`EnvironmentInfo::state`]: that
/// vocabulary is a closed set [`EnvironmentState::is_settled`] reasons
/// about, and an unhealthy environment is still a running one. Nothing tears
/// an environment down for a failing probe.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct Health {
    pub state: HealthState,
    /// When the probe entered `state`.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub since: Option<DateTime<Utc>>,
    /// Count of consecutive failed attempts, zero while passing.
    #[serde(skip_serializing_if = "is_zero_u32")]
    pub failures: u32,
    /// The last attempt's failure detail, empty while passing.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub message: Option<String>,
}

impl Default for Health {
    fn default() -> Self {
        Self {
            state: HealthState::Starting,
            since: None,
            failures: 0,
            message: None,
        }
    }
}

/// The body of [`Environments::create`](crate::Environments::create).
///
/// `task_id` is the only required field; build the rest with the chained
/// setters:
///
/// ```
/// use fuse::{CreateRequest, Spec};
///
/// let request = CreateRequest::new("task-1")
///     .spec(Spec::new().cpus(2).ram_mb(2048).image("docker.io/library/python:3.12-slim"))
///     .secret("API_KEY", "hunter2")
///     .startup_script("pip install requests");
/// ```
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct CreateRequest {
    pub task_id: String,
    pub spec: Spec,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub manifest_inline: Option<String>,
    #[serde(skip_serializing_if = "HashMap::is_empty")]
    pub secrets: HashMap<String, String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub startup_script: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub gateway_url: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub gateway_token: Option<String>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub expose: Vec<ExposeSpec>,
    /// The environment-level readiness probe. Omit it for an environment
    /// with no probe, in which case [`EnvironmentInfo::health`] is never
    /// populated. It is not evaluated inside create: the call returns as
    /// soon as the VM is up, and the verdict arrives on later reads.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub healthcheck: Option<HealthcheckSpec>,
    /// The graphical session's geometry. Omit it for an environment with no
    /// desktop, in which case a desktop image keeps its baked default
    /// geometry.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub desktop: Option<DesktopSpec>,
    /// Guest files written before `startup_script` runs, keyed by absolute
    /// guest path with base64-encoded content. It is what a Fusefile's
    /// `copy` block compiles to. Paths under `/fuse` are rejected (that is
    /// the guest agent's own directory), and the total decoded size is
    /// capped at 512 KiB, since these travel inside the create body.
    /// Permissions are not carried: chmod in the startup script.
    #[serde(skip_serializing_if = "HashMap::is_empty")]
    pub files: HashMap<String, String>,
    /// Bounds `startup_script`. `None` uses the orchestrator's default. A
    /// value above the orchestrator's configured maximum is rejected rather
    /// than clamped.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub startup_script_timeout_seconds: Option<u64>,
    /// Boots the environment from an existing snapshot artifact instead of
    /// the spec's image. The two are mutually exclusive. The artifact is
    /// host-local, so the environment lands on the snapshot's host or the
    /// call fails.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub seed_snapshot_id: Option<String>,
}

impl CreateRequest {
    pub fn new(task_id: impl Into<String>) -> Self {
        Self {
            task_id: task_id.into(),
            ..Self::default()
        }
    }

    pub fn spec(mut self, spec: Spec) -> Self {
        self.spec = spec;
        self
    }

    pub fn manifest_inline(mut self, manifest: impl Into<String>) -> Self {
        self.manifest_inline = Some(manifest.into());
        self
    }

    /// Adds one secret exposed to the guest.
    pub fn secret(mut self, key: impl Into<String>, value: impl Into<String>) -> Self {
        self.secrets.insert(key.into(), value.into());
        self
    }

    pub fn startup_script(mut self, script: impl Into<String>) -> Self {
        self.startup_script = Some(script.into());
        self
    }

    pub fn gateway_url(mut self, url: impl Into<String>) -> Self {
        self.gateway_url = Some(url.into());
        self
    }

    pub fn gateway_token(mut self, token: impl Into<String>) -> Self {
        self.gateway_token = Some(token.into());
        self
    }

    /// Adds one published port.
    pub fn expose(mut self, expose: ExposeSpec) -> Self {
        self.expose.push(expose);
        self
    }

    pub fn healthcheck(mut self, healthcheck: HealthcheckSpec) -> Self {
        self.healthcheck = Some(healthcheck);
        self
    }

    pub fn desktop(mut self, width: u32, height: u32) -> Self {
        self.desktop = Some(DesktopSpec::new(width, height));
        self
    }

    /// Adds one guest file, base64-encoding `contents`. `path` must be an
    /// absolute guest path.
    pub fn file(mut self, path: impl Into<String>, contents: impl AsRef<[u8]>) -> Self {
        let encoded = base64::engine::general_purpose::STANDARD.encode(contents.as_ref());
        self.files.insert(path.into(), encoded);
        self
    }

    pub fn startup_script_timeout_seconds(mut self, seconds: u64) -> Self {
        self.startup_script_timeout_seconds = Some(seconds);
        self
    }

    pub fn seed_snapshot_id(mut self, snapshot_id: impl Into<String>) -> Self {
        self.seed_snapshot_id = Some(snapshot_id.into());
        self
    }
}

/// The server's view of a single microVM.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct EnvironmentInfo {
    pub id: String,
    pub state: EnvironmentState,
    #[serde(default)]
    pub task_id: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub host_id: Option<String>,
    #[serde(default)]
    pub url: String,
    #[serde(default)]
    pub spec: Spec,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub endpoints: Vec<Endpoint>,
    /// The environment-level healthcheck's last verdict. `None` when the
    /// environment declared no healthcheck, and `None` until the first
    /// verdict has been read back from the guest. The server refreshes it on
    /// its reconcile tick (30s by default), so it lags the guest by up to a
    /// tick.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub health: Option<Health>,
}

string_enum! {
    /// Event kinds carried in [`Event::kind`].
    pub enum EventKind {
        /// A lifecycle state event, the only kind that advances an
        /// environment's state.
        State => "state",
        /// One setup step's boundary, timing, and cache verdict.
        Step => "step",
    }
}

string_enum! {
    /// Cache miss reasons carried in [`Event::miss_reason`]. The set is
    /// closed so clients can render them, but unknown values pass through as
    /// [`MissReason::Other`] rather than failing to decode.
    pub enum MissReason {
        NoEntry => "no-entry",
        StepChanged => "step-changed",
        InputsChanged => "inputs-changed",
        ParentChanged => "parent-changed",
        BaseChanged => "base-changed",
        Uncacheable => "uncacheable",
        Disabled => "disabled",
        NotOnHost => "not-on-host",
        Unsupported => "unsupported",
    }
}

/// One item from [`EventStream`](crate::EventStream). It matches the
/// server's wire payload.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct Event {
    pub id: String,
    #[serde(rename = "event")]
    pub kind: EventKind,
    pub vm_id: String,
    /// The environment's lifecycle state. `None` on step events.
    #[serde(
        deserialize_with = "empty_as_none",
        skip_serializing_if = "Option::is_none"
    )]
    pub state: Option<EnvironmentState>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub url: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub updated_at: Option<DateTime<Utc>>,

    // fields below are set only on step events, which report one setup
    // step's boundary, timing, and cache verdict. they are zero on state
    // events, and on step events from a server that does not report caching
    // yet.
    #[serde(skip_serializing_if = "is_zero_u32")]
    pub index: u32,
    #[serde(skip_serializing_if = "is_zero_u32")]
    pub total: u32,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub key: Option<String>,
    #[serde(skip_serializing_if = "is_false")]
    pub cached: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub miss_reason: Option<MissReason>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub miss_detail: Vec<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub duration_ms: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub exit_code: Option<i32>,
}

impl Default for Event {
    fn default() -> Self {
        Self {
            id: String::new(),
            kind: EventKind::State,
            vm_id: String::new(),
            state: None,
            url: None,
            error: None,
            updated_at: None,
            index: 0,
            total: 0,
            key: None,
            cached: false,
            miss_reason: None,
            miss_detail: Vec::new(),
            duration_ms: None,
            exit_code: None,
        }
    }
}

impl Event {
    /// Reports whether this is a lifecycle state event, the only kind that
    /// advances an environment's state.
    pub fn is_state(&self) -> bool {
        self.kind == EventKind::State
    }

    /// Reports whether this event carries a terminal lifecycle state, after
    /// which the stream closes.
    pub fn is_terminal(&self) -> bool {
        matches!(&self.state, Some(state) if state.is_terminal())
    }
}

/// The optional body of [`Environments::fork`](crate::Environments::fork).
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct ForkOptions {
    /// Reuses the named snapshot instead of snapshotting the source first.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub reuse_snapshot_id: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub comment: Option<String>,
}

impl ForkOptions {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn reuse_snapshot_id(mut self, snapshot_id: impl Into<String>) -> Self {
        self.reuse_snapshot_id = Some(snapshot_id.into());
        self
    }

    pub fn comment(mut self, comment: impl Into<String>) -> Self {
        self.comment = Some(comment.into());
        self
    }
}

/// The optional body of [`Snapshots::create`](crate::Snapshots::create).
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct SnapshotRequest {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub comment: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub mode: Option<SnapshotMode>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub retention_seconds: Option<u64>,
    #[serde(skip_serializing_if = "HashMap::is_empty")]
    pub metadata: HashMap<String, String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub export_ref: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub export_status: Option<String>,
    /// Asks for guest memory and vCPU state to be captured alongside the
    /// rootfs, so restoring resumes the guest instead of cold-booting it.
    ///
    /// Opt-in, and it costs the VM's full RAM in extra bytes per snapshot
    /// while briefly pausing the guest. Not every backend can do it: a GPU
    /// environment never can, and a provider without live support answers
    /// 501. Read [`Snapshot::kind`] back to see what was actually written.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub live: Option<bool>,
    /// Labels this snapshot as the artifact of one cacheable setup step,
    /// which is what makes it findable by recipe rather than by its random
    /// id. `None` for an ordinary snapshot.
    ///
    /// The scope it is filed under comes from how the caller authenticated
    /// and is not settable here.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub layer_key: Option<String>,
}

impl SnapshotRequest {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn comment(mut self, comment: impl Into<String>) -> Self {
        self.comment = Some(comment.into());
        self
    }

    pub fn mode(mut self, mode: SnapshotMode) -> Self {
        self.mode = Some(mode);
        self
    }

    pub fn retention_seconds(mut self, seconds: u64) -> Self {
        self.retention_seconds = Some(seconds);
        self
    }

    /// Adds one metadata key/value pair.
    pub fn metadata(mut self, key: impl Into<String>, value: impl Into<String>) -> Self {
        self.metadata.insert(key.into(), value.into());
        self
    }

    pub fn export_ref(mut self, export_ref: impl Into<String>) -> Self {
        self.export_ref = Some(export_ref.into());
        self
    }

    pub fn export_status(mut self, export_status: impl Into<String>) -> Self {
        self.export_status = Some(export_status.into());
        self
    }

    /// Captures guest memory alongside the rootfs. See [`SnapshotRequest::live`].
    pub fn live(mut self, live: bool) -> Self {
        self.live = Some(live);
        self
    }

    pub fn layer_key(mut self, layer_key: impl Into<String>) -> Self {
        self.layer_key = Some(layer_key.into());
        self
    }
}

/// An optional exported snapshot artifact.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct SnapshotExport {
    pub destination: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub status: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub requested_at: Option<DateTime<Utc>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub updated_at: Option<DateTime<Utc>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub last_error: Option<String>,
}

/// A persisted snapshot record.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Snapshot {
    pub id: String,
    #[serde(default)]
    pub vm_id: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub task_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub tenant_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub parent_snapshot_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub mode: Option<SnapshotMode>,
    /// What the snapshot captured, disk-only or disk plus guest memory. See
    /// [`SnapshotKind`]; absent from a server that predates live snapshots,
    /// which means disk.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub kind: Option<SnapshotKind>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub state: Option<SnapshotState>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub comment: Option<String>,
    /// The caller-chosen lookup key from the snapshot's metadata, set by
    /// `fuse build` so an artifact can be found without its random id.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub name: Option<String>,
    /// The content-addressed cache key of the setup step this artifact was
    /// taken after. `None` on every snapshot that is not a build layer, and
    /// an empty key never matches a lookup.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub layer_key: Option<String>,
    /// The CPU architecture of the host that actually built the artifact,
    /// not of whoever asked for it. A rootfs is not portable across
    /// architectures, so this is a real constraint on whether the artifact
    /// can be booted; it is deliberately not folded into `layer_key`, so a
    /// lookup has to filter on it separately.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub arch: Option<Arch>,
    /// The hex sha256 of the artifact rootfs. It verifies that a given copy
    /// of those bytes is intact, and nothing more: it is not a cross-build
    /// identity, because two builds of the same recipe produce different
    /// rootfs bytes (timestamps, inode ordering, package caches). It can
    /// never be used as a cache key.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub digest: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub size_bytes: Option<u64>,
    pub created_at: DateTime<Utc>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub updated_at: Option<DateTime<Utc>>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub retention_until: Option<DateTime<Utc>>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub last_error: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub export_ref: Option<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub exports: Vec<SnapshotExport>,
}

/// A host's resource envelope.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct HostCapacity {
    pub cpus: u32,
    pub ram_mb: u64,
    pub storage_gb: u64,
    pub vm_count: u32,
    /// The host's CPU architecture. `None` means amd64 (hosts registered
    /// before arch existed).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub arch: Option<Arch>,
    #[serde(skip_serializing_if = "is_zero_u32")]
    pub gpus: u32,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub gpu_kind: Option<String>,
    /// Advertises fractional GPU capacity: MIG instance count by profile
    /// name (e.g. `{"1g.10gb": 4}`). Requires backend qemu. When the host
    /// reports per-instance MIG inventory (`mig_instances`), this map is a
    /// derived summary; otherwise it is the scheduling unit.
    #[serde(skip_serializing_if = "HashMap::is_empty")]
    pub mig_profiles: HashMap<String, u32>,
    /// The per-instance MIG inventory probed from the host agent (one entry
    /// per carved MIG GPU instance). When non-empty, the orchestrator binds
    /// specific instance uuids to VMs instead of decrementing a count.
    /// Strictly additive: a host that reports no instances falls back to
    /// `mig_profiles`. Only populated on capacity for qemu hosts.
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub mig_instances: Vec<MigInstance>,
    /// The set of MIG instance uuids currently bound to VMs. Populated only
    /// on [`Host::allocated`], and only for hosts that report per-instance
    /// MIG inventory.
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub mig_instance_uuids: Vec<String>,
    /// The per-device GPU detail probed from the host agent, carried
    /// alongside the scalar `gpus`/`gpu_kind` counters. Only populated on
    /// capacity for qemu hosts.
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub gpu_devices: Vec<GpuDevice>,
}

impl HostCapacity {
    /// Returns a capacity declaring the three probe-able hardware facts.
    /// Use `HostCapacity::default()` with just [`vm_count`](Self::vm_count)
    /// to have the orchestrator probe them from the host agent instead.
    pub fn new(cpus: u32, ram_mb: u64, storage_gb: u64) -> Self {
        Self {
            cpus,
            ram_mb,
            storage_gb,
            ..Self::default()
        }
    }

    /// Sets the scheduling VM cap. Never probed, always required on
    /// registration.
    pub fn vm_count(mut self, vm_count: u32) -> Self {
        self.vm_count = vm_count;
        self
    }

    pub fn arch(mut self, arch: Arch) -> Self {
        self.arch = Some(arch);
        self
    }

    pub fn gpus(mut self, gpus: u32, gpu_kind: impl Into<String>) -> Self {
        self.gpus = gpus;
        self.gpu_kind = Some(gpu_kind.into());
        self
    }
}

/// Per-device detail probed for a single GPU. Every field is best-effort
/// and `None` when the host agent could not determine it.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct GpuDevice {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub uuid: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub model: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub pci_bus_id: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub memory_mb: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub driver_version: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub cuda_version: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub compute_cap: Option<String>,
    #[serde(skip_serializing_if = "is_false")]
    pub mig_capable: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub mig_mode: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub iommu_group: Option<String>,
}

/// One carved MIG GPU instance probed from the host agent. The orchestrator
/// binds a specific instance uuid to a VM so it knows which instance went
/// to which VM.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct MigInstance {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub uuid: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub profile: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub kind: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub parent_gpu_uuid: Option<String>,
}

/// The body of [`Hosts::register`](crate::Hosts::register).
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct RegisterHostRequest {
    pub id: String,
    pub url: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub token: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub region: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub backend: Option<HostBackend>,
    /// Operator-declared key/value pairs matched against a spec's placement
    /// label selectors. They are never probed from the host agent.
    #[serde(skip_serializing_if = "HashMap::is_empty")]
    pub labels: HashMap<String, String>,
    pub capacity: HostCapacity,
}

impl RegisterHostRequest {
    pub fn new(id: impl Into<String>, url: impl Into<String>, capacity: HostCapacity) -> Self {
        Self {
            id: id.into(),
            url: url.into(),
            capacity,
            ..Self::default()
        }
    }

    pub fn token(mut self, token: impl Into<String>) -> Self {
        self.token = Some(token.into());
        self
    }

    pub fn region(mut self, region: impl Into<String>) -> Self {
        self.region = Some(region.into());
        self
    }

    pub fn backend(mut self, backend: HostBackend) -> Self {
        self.backend = Some(backend);
        self
    }

    /// Adds one placement label.
    pub fn label(mut self, key: impl Into<String>, value: impl Into<String>) -> Self {
        self.labels.insert(key.into(), value.into());
        self
    }
}

/// The server's view of a registered host.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Host {
    pub id: String,
    #[serde(default)]
    pub url: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub region: Option<String>,
    pub state: HostState,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub backend: Option<HostBackend>,
    #[serde(default, skip_serializing_if = "HashMap::is_empty")]
    pub labels: HashMap<String, String>,
    #[serde(default)]
    pub capacity: HostCapacity,
    #[serde(default)]
    pub allocated: HostCapacity,
    pub last_seen: DateTime<Utc>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
    /// Non-fatal notices from registration (e.g. a declared capacity value
    /// that exceeds what was probed from the host agent). Only ever
    /// populated on the response to [`Hosts::register`](crate::Hosts::register).
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub warnings: Vec<String>,
}

/// A key's metadata. The raw secret appears only in [`CreatedApiKey::key`],
/// returned once at creation.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ApiKey {
    pub id: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub label: Option<String>,
    pub created_at: DateTime<Utc>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub last_used_at: Option<DateTime<Utc>>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub revoked_at: Option<DateTime<Utc>>,
}

/// Returned by [`ApiKeys::create`](crate::ApiKeys::create). `key` is
/// unrecoverable after this response.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct CreatedApiKey {
    #[serde(flatten)]
    pub api_key: ApiKey,
    pub key: String,
}

/// The orchestrator self-identification returned by `GET /v1/version`. A
/// fuse orchestrator answers with service `fuse-orchestrator`; anything
/// else (a host agent, an unrelated service, a proxy) will not, which is
/// how a client tells them apart before it has a confirmed token.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
#[non_exhaustive]
pub struct VersionInfo {
    pub service: String,
    pub version: String,
    /// The raw `Server` response header (e.g. `fuse-orchestrator/0.4.0` or
    /// `fc-agent/0.1`). Populated from any HTTP response, even a non-2xx
    /// one, so a caller can name the wrong service it actually reached.
    pub server_header: String,
    /// The HTTP status of the `/v1/version` response.
    pub status: u16,
}

impl VersionInfo {
    /// Reports whether the probed endpoint identified itself as a fuse
    /// orchestrator.
    pub fn is_orchestrator(&self) -> bool {
        self.service == "fuse-orchestrator"
    }
}
