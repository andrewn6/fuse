use std::sync::Arc;

use reqwest::Method;
use serde::Deserialize;

use crate::error::Error;
use crate::transport::{pagination, require, Transport, MAX_PAGE_LIMIT};
use crate::types::{Arch, Snapshot, SnapshotMode, SnapshotRequest, SnapshotState};

/// Filters and pagination for [`Snapshots::list`] and
/// [`Snapshots::list_page`].
#[derive(Debug, Clone, Default)]
#[non_exhaustive]
pub struct ListSnapshotsOptions {
    pub vm_id: Option<String>,
    pub task_id: Option<String>,
    pub tenant_id: Option<String>,
    pub state: Option<SnapshotState>,
    /// Narrows to one creation mode, e.g. [`SnapshotMode::Build`] for build
    /// artifacts.
    pub mode: Option<SnapshotMode>,
    /// Matches the snapshot's metadata name, the lookup key `fuse build`
    /// sets on an artifact.
    pub name: Option<String>,
    /// Narrows to the build layers taken after one setup step. `None` means
    /// "do not filter", never "match artifacts with no layer key".
    pub layer_key: Option<String>,
    /// Narrows to artifacts built on one architecture. It is a separate
    /// filter from `layer_key` rather than part of it because a rootfs is
    /// not portable across architectures, so a layer lookup that does not
    /// constrain arch can be served bytes it cannot boot.
    pub arch: Option<Arch>,
    /// Caps the page size (server default 50, max 200). Only consulted by
    /// [`Snapshots::list_page`] — [`Snapshots::list`] always requests the
    /// server's max page size internally since it walks every page.
    pub limit: Option<u32>,
    /// Resumes after a previous page's [`SnapshotPage::next_cursor`]. It is
    /// an opaque token; treat it as such rather than parsing it.
    pub cursor: Option<String>,
}

impl ListSnapshotsOptions {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn vm_id(mut self, vm_id: impl Into<String>) -> Self {
        self.vm_id = Some(vm_id.into());
        self
    }

    pub fn task_id(mut self, task_id: impl Into<String>) -> Self {
        self.task_id = Some(task_id.into());
        self
    }

    pub fn tenant_id(mut self, tenant_id: impl Into<String>) -> Self {
        self.tenant_id = Some(tenant_id.into());
        self
    }

    pub fn state(mut self, state: SnapshotState) -> Self {
        self.state = Some(state);
        self
    }

    pub fn mode(mut self, mode: SnapshotMode) -> Self {
        self.mode = Some(mode);
        self
    }

    pub fn name(mut self, name: impl Into<String>) -> Self {
        self.name = Some(name.into());
        self
    }

    pub fn layer_key(mut self, layer_key: impl Into<String>) -> Self {
        self.layer_key = Some(layer_key.into());
        self
    }

    pub fn arch(mut self, arch: Arch) -> Self {
        self.arch = Some(arch);
        self
    }

    pub fn limit(mut self, limit: u32) -> Self {
        self.limit = Some(limit);
        self
    }

    pub fn cursor(mut self, cursor: impl Into<String>) -> Self {
        self.cursor = Some(cursor.into());
        self
    }
}

/// One page of a list call, plus the cursor to fetch the next one.
#[derive(Debug, Clone, Default, Deserialize)]
pub struct SnapshotPage {
    #[serde(default)]
    pub snapshots: Vec<Snapshot>,
    /// `None` once there are no more results.
    #[serde(default)]
    pub next_cursor: Option<String>,
}

/// The snapshots service, from
/// [`Client::snapshots`](crate::Client::snapshots).
#[derive(Clone)]
pub struct Snapshots {
    t: Arc<Transport>,
}

impl Snapshots {
    pub(crate) fn new(t: Arc<Transport>) -> Self {
        Self { t }
    }

    /// Takes a snapshot of a running environment.
    pub async fn create(&self, vm_id: &str, request: SnapshotRequest) -> Result<Snapshot, Error> {
        require(vm_id, "vm id")?;
        let request = self
            .t
            .request(Method::POST, &["v1", "environments", vm_id, "snapshots"])
            .json(&request);
        self.t.send_json(request).await
    }

    /// Returns every snapshot matching `options`, transparently walking
    /// every result page. For explicit single-page control (e.g. a CLI
    /// `--cursor` flag), use [`Snapshots::list_page`].
    pub async fn list(&self, options: ListSnapshotsOptions) -> Result<Vec<Snapshot>, Error> {
        let mut out = Vec::new();
        let mut page_options = options;
        page_options.limit = Some(MAX_PAGE_LIMIT);
        loop {
            let page = self.list_page(page_options.clone()).await?;
            out.extend(page.snapshots);
            match page.next_cursor {
                Some(cursor) if !cursor.is_empty() => page_options.cursor = Some(cursor),
                _ => return Ok(out),
            }
        }
    }

    /// Returns one page of snapshots matching `options`.
    pub async fn list_page(&self, options: ListSnapshotsOptions) -> Result<SnapshotPage, Error> {
        let mut query: Vec<(&str, String)> = Vec::new();
        if let Some(vm_id) = &options.vm_id {
            query.push(("vm_id", vm_id.clone()));
        }
        if let Some(task_id) = &options.task_id {
            query.push(("task_id", task_id.clone()));
        }
        if let Some(tenant_id) = &options.tenant_id {
            query.push(("tenant_id", tenant_id.clone()));
        }
        if let Some(state) = &options.state {
            query.push(("state", state.to_string()));
        }
        if let Some(mode) = &options.mode {
            query.push(("mode", mode.to_string()));
        }
        if let Some(name) = &options.name {
            query.push(("name", name.clone()));
        }
        if let Some(layer_key) = &options.layer_key {
            query.push(("layer_key", layer_key.clone()));
        }
        if let Some(arch) = &options.arch {
            query.push(("arch", arch.to_string()));
        }
        pagination(&mut query, options.limit, &options.cursor);
        let request = self
            .t
            .request(Method::GET, &["v1", "snapshots"])
            .query(&query);
        self.t.send_json(request).await
    }

    /// Looks up the newest ready build artifact for one layer cache key and
    /// architecture, reporting `None` when there is none.
    ///
    /// A miss is not an error. A cold cache is the normal state of a first
    /// build, and modelling it as a failure would make every caller wrap
    /// this in error handling to discover nothing was wrong.
    ///
    /// `arch` is required rather than defaulted. An ext4 rootfs is not
    /// portable across architectures, so resolving without one could hand
    /// back an artifact the caller cannot boot, at the exact moment it
    /// believes it got a cache hit.
    ///
    /// The scope searched comes from how the client authenticated. There is
    /// deliberately no tenant parameter: a caller that could name its own
    /// scope could read another tenant's cache.
    pub async fn resolve(&self, layer_key: &str, arch: Arch) -> Result<Option<Snapshot>, Error> {
        require(layer_key, "layer key")?;

        // the found flag stays private so callers get Option instead, which
        // cannot be confused with a decode that happened to produce a
        // zero-value snapshot.
        #[derive(Deserialize)]
        struct Wire {
            #[serde(default)]
            found: bool,
            #[serde(default)]
            snapshot: Option<Snapshot>,
        }

        let request = self
            .t
            .request(Method::GET, &["v1", "snapshots", "resolve"])
            .query(&[("layer_key", layer_key), ("arch", arch.as_str())]);
        let wire: Wire = self.t.send_json(request).await?;
        Ok(if wire.found { wire.snapshot } else { None })
    }

    /// Returns one snapshot by id.
    pub async fn get(&self, snapshot_id: &str) -> Result<Snapshot, Error> {
        require(snapshot_id, "snapshot id")?;
        let request = self
            .t
            .request(Method::GET, &["v1", "snapshots", snapshot_id]);
        self.t.send_json(request).await
    }

    /// Deletes a snapshot.
    pub async fn delete(&self, snapshot_id: &str) -> Result<(), Error> {
        require(snapshot_id, "snapshot id")?;
        let request = self
            .t
            .request(Method::DELETE, &["v1", "snapshots", snapshot_id]);
        self.t.send_unit(request).await
    }

    /// Restores a snapshot into its environment.
    pub async fn restore(&self, snapshot_id: &str) -> Result<(), Error> {
        require(snapshot_id, "snapshot id")?;
        let request = self
            .t
            .request(Method::POST, &["v1", "snapshots", snapshot_id])
            .query(&[("action", "restore")]);
        self.t.send_unit(request).await
    }
}
