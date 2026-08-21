//! Rust client for the Fuse microVM control plane.
//!
//! The crate is published as `folsom-fuse`; the library target is `fuse`,
//! mirroring the Python SDK's `import fuse`.
//!
//! [`Client`] is the entry point. It groups four resource services behind
//! one shared transport: [`Environments`], [`Snapshots`], [`Hosts`], and
//! [`ApiKeys`].
//!
//! ```no_run
//! use fuse::{Client, CreateRequest, ExecRequest, Spec};
//!
//! # async fn run() -> Result<(), fuse::Error> {
//! let client = Client::builder("https://orchestrator.example.com")
//!     .token("secret")
//!     .build()?;
//!
//! let env = client
//!     .environments()
//!     .create(
//!         CreateRequest::new("task-1")
//!             .spec(Spec::new().cpus(2).ram_mb(2048).image("docker.io/library/python:3.12-slim")),
//!     )
//!     .await?;
//!
//! // watch the environment settle.
//! let mut events = client.environments().events(&env.id).await?;
//! while let Some(event) = events.next().await {
//!     let event = event?;
//!     if event.state.as_ref().is_some_and(|s| s.is_settled()) {
//!         break;
//!     }
//! }
//!
//! let result = client
//!     .environments()
//!     .exec(&env.id, ExecRequest::cmd(["python3", "--version"]))
//!     .await?;
//! println!("{}", result.stdout);
//!
//! client.environments().destroy(&env.id).await?;
//! # Ok(())
//! # }
//! ```
//!
//! Every fallible call returns [`Result<T, Error>`](Error). A non-2xx
//! response surfaces as [`Error::Api`] carrying the server's error
//! envelope; use predicates like [`Error::is_not_found`] to branch on the
//! code.

mod apikeys;
mod client;
mod computer;
mod environments;
mod error;
mod events;
mod hosts;
mod snapshots;
mod strenum;
mod transport;
mod types;

pub use apikeys::ApiKeys;
pub use client::{Client, ClientBuilder};
pub use computer::{
    ComputerAction, ComputerActionKind, ComputerDisplay, ComputerResult, ComputerToolResult,
    Coordinate, ImageSource, Region, ScrollDirection, ToolResultBlock,
};
pub use environments::{
    EnvironmentPage, Environments, ExecCommand, ExecRequest, ExecResult, ListEnvironmentsOptions,
};
pub use error::{ApiError, Error, ErrorCode};
pub use events::EventStream;
pub use hosts::{HostPage, Hosts, ListHostsOptions};
pub use snapshots::{ListSnapshotsOptions, SnapshotPage, Snapshots};
pub use types::{
    ApiKey, Arch, CreateRequest, CreatedApiKey, DesktopSpec, Endpoint, EnvironmentInfo,
    EnvironmentState, Event, EventKind, ExposeSpec, ForkOptions, GpuDevice, Health, HealthState,
    HealthcheckHttp, HealthcheckProbe, HealthcheckSpec, Host, HostBackend, HostCapacity, HostState,
    MigInstance, MissReason, RegisterHostRequest, Snapshot, SnapshotExport, SnapshotKind,
    SnapshotMode, SnapshotRequest, SnapshotState, Spec, VersionInfo,
};
