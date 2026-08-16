use std::sync::Arc;

use reqwest::Method;
use serde::Deserialize;

use crate::error::Error;
use crate::transport::{pagination, require, Transport, MAX_PAGE_LIMIT};
use crate::types::{Host, RegisterHostRequest};

/// Pagination for [`Hosts::list_page`].
#[derive(Debug, Clone, Default)]
#[non_exhaustive]
pub struct ListHostsOptions {
    /// Caps the page size (server default 50, max 200).
    pub limit: Option<u32>,
    /// Resumes after a previous page's [`HostPage::next_cursor`]. It is an
    /// opaque token; treat it as such rather than parsing it.
    pub cursor: Option<String>,
}

impl ListHostsOptions {
    pub fn new() -> Self {
        Self::default()
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
pub struct HostPage {
    #[serde(default)]
    pub hosts: Vec<Host>,
    /// `None` once there are no more results.
    #[serde(default)]
    pub next_cursor: Option<String>,
}

/// The hosts service, from [`Client::hosts`](crate::Client::hosts).
#[derive(Clone)]
pub struct Hosts {
    t: Arc<Transport>,
}

impl Hosts {
    pub(crate) fn new(t: Arc<Transport>) -> Self {
        Self { t }
    }

    /// Registers a compute host with the orchestrator.
    pub async fn register(&self, request: RegisterHostRequest) -> Result<Host, Error> {
        let request = self
            .t
            .request(Method::POST, &["v1", "hosts"])
            .json(&request);
        self.t.send_json(request).await
    }

    /// Returns every registered host, transparently walking every result
    /// page. For explicit single-page control (e.g. a CLI `--cursor` flag),
    /// use [`Hosts::list_page`].
    pub async fn list(&self) -> Result<Vec<Host>, Error> {
        let mut out = Vec::new();
        let mut options = ListHostsOptions::new().limit(MAX_PAGE_LIMIT);
        loop {
            let page = self.list_page(options.clone()).await?;
            out.extend(page.hosts);
            match page.next_cursor {
                Some(cursor) if !cursor.is_empty() => options.cursor = Some(cursor),
                _ => return Ok(out),
            }
        }
    }

    /// Returns one page of registered hosts.
    pub async fn list_page(&self, options: ListHostsOptions) -> Result<HostPage, Error> {
        let mut query: Vec<(&str, String)> = Vec::new();
        pagination(&mut query, options.limit, &options.cursor);
        let request = self.t.request(Method::GET, &["v1", "hosts"]).query(&query);
        self.t.send_json(request).await
    }

    /// Returns one host by id.
    pub async fn get(&self, host_id: &str) -> Result<Host, Error> {
        require(host_id, "host id")?;
        let request = self.t.request(Method::GET, &["v1", "hosts", host_id]);
        self.t.send_json(request).await
    }

    /// Marks a host ineligible for new placements.
    pub async fn cordon(&self, host_id: &str) -> Result<(), Error> {
        self.action(host_id, "cordon").await
    }

    /// Returns a cordoned host to service.
    pub async fn uncordon(&self, host_id: &str) -> Result<(), Error> {
        self.action(host_id, "uncordon").await
    }

    /// Removes a host from the fleet.
    pub async fn deregister(&self, host_id: &str) -> Result<(), Error> {
        require(host_id, "host id")?;
        let request = self.t.request(Method::DELETE, &["v1", "hosts", host_id]);
        self.t.send_unit(request).await
    }

    async fn action(&self, host_id: &str, action: &str) -> Result<(), Error> {
        require(host_id, "host id")?;
        let request = self
            .t
            .request(Method::POST, &["v1", "hosts", host_id])
            .query(&[("action", action)]);
        self.t.send_unit(request).await
    }
}
