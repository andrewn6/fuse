use std::sync::Arc;
use std::time::Duration;

use reqwest::Method;
use serde::Deserialize;
use url::Url;

use crate::apikeys::ApiKeys;
use crate::environments::Environments;
use crate::error::Error;
use crate::hosts::Hosts;
use crate::snapshots::Snapshots;
use crate::transport::{RequestIdFn, Transport, DEFAULT_TIMEOUT};
use crate::types::VersionInfo;

/// The entry point for the Fuse API. It groups the resource services that
/// share a single transport, and is cheap to clone.
///
/// ```no_run
/// use fuse::{Client, CreateRequest, Spec};
///
/// # async fn run() -> Result<(), fuse::Error> {
/// let client = Client::builder("https://orchestrator.example.com")
///     .token("secret")
///     .build()?;
///
/// let env = client
///     .environments()
///     .create(CreateRequest::new("task-1").spec(Spec::new().cpus(2).ram_mb(2048)))
///     .await?;
/// println!("{} is {}", env.id, env.state);
/// # Ok(())
/// # }
/// ```
#[derive(Clone)]
pub struct Client {
    transport: Arc<Transport>,
}

impl std::fmt::Debug for Client {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Client")
            .field("base_url", self.transport.base_url())
            .finish_non_exhaustive()
    }
}

impl Client {
    /// Builds a client with default options. `base_url` is the
    /// orchestrator's root (no `/v1` suffix); `token` is sent as a bearer
    /// token and may be empty for an orchestrator running without auth.
    pub fn new(base_url: impl Into<String>, token: impl Into<String>) -> Result<Self, Error> {
        Self::builder(base_url).token(token).build()
    }

    /// Returns a builder for a client with non-default options.
    pub fn builder(base_url: impl Into<String>) -> ClientBuilder {
        ClientBuilder::new(base_url)
    }

    /// The environments service.
    pub fn environments(&self) -> Environments {
        Environments::new(self.transport.clone())
    }

    /// The snapshots service.
    pub fn snapshots(&self) -> Snapshots {
        Snapshots::new(self.transport.clone())
    }

    /// The hosts service.
    pub fn hosts(&self) -> Hosts {
        Hosts::new(self.transport.clone())
    }

    /// The API keys service.
    pub fn api_keys(&self) -> ApiKeys {
        ApiKeys::new(self.transport.clone())
    }

    /// Probes `GET /v1/version` (unauthenticated) and reports what the
    /// endpoint says it is. A transport-level error (connection refused,
    /// DNS failure, timeout) is returned as `Err`; a reachable-but-wrong
    /// service returns a [`VersionInfo`] whose
    /// [`is_orchestrator`](VersionInfo::is_orchestrator) reports false, so
    /// the caller can distinguish "nothing there" from "wrong thing there".
    pub async fn version(&self) -> Result<VersionInfo, Error> {
        let response = self
            .transport
            .request(Method::GET, &["v1", "version"])
            .send()
            .await?;
        let server_header = response
            .headers()
            .get(reqwest::header::SERVER)
            .and_then(|value| value.to_str().ok())
            .unwrap_or_default()
            .to_owned();
        let status = response.status().as_u16();
        let body = response.bytes().await.unwrap_or_default();

        #[derive(Deserialize, Default)]
        struct Wire {
            #[serde(default)]
            service: String,
            #[serde(default)]
            version: String,
        }
        // best-effort decode: a non-fuse service may not answer with our
        // shape (or with json at all), which is itself the signal that this
        // is not an orchestrator.
        let wire: Wire = serde_json::from_slice(&body).unwrap_or_default();
        Ok(VersionInfo {
            service: wire.service,
            version: wire.version,
            server_header,
            status,
        })
    }
}

/// Configures and builds a [`Client`].
pub struct ClientBuilder {
    base_url: String,
    token: Option<String>,
    user_agent: String,
    timeout: Duration,
    request_id: Option<Arc<RequestIdFn>>,
    http: Option<reqwest::Client>,
}

impl ClientBuilder {
    fn new(base_url: impl Into<String>) -> Self {
        Self {
            base_url: base_url.into(),
            token: None,
            user_agent: concat!("fuse-rust/", env!("CARGO_PKG_VERSION")).to_owned(),
            timeout: DEFAULT_TIMEOUT,
            request_id: None,
            http: None,
        }
    }

    /// Sets the bearer token sent on every request. An empty token sends no
    /// `Authorization` header.
    pub fn token(mut self, token: impl Into<String>) -> Self {
        let token = token.into();
        self.token = (!token.is_empty()).then_some(token);
        self
    }

    /// Overrides the `User-Agent` header. Default: `fuse-rust/<version>`.
    pub fn user_agent(mut self, user_agent: impl Into<String>) -> Self {
        self.user_agent = user_agent.into();
        self
    }

    /// Overrides the timeout for normal (non-streaming) requests. Default:
    /// 60 seconds. Streaming calls (events, exec, computer) are bounded per
    /// call instead.
    pub fn timeout(mut self, timeout: Duration) -> Self {
        self.timeout = timeout;
        self
    }

    /// Sets a generator for the `X-Request-ID` header. It is called once
    /// per request; an empty return value omits the header.
    pub fn request_id(mut self, generate: impl Fn() -> String + Send + Sync + 'static) -> Self {
        self.request_id = Some(Arc::new(generate));
        self
    }

    /// Overrides the [`reqwest::Client`] used for normal requests. Streaming
    /// calls keep their own no-timeout client.
    pub fn http_client(mut self, client: reqwest::Client) -> Self {
        self.http = Some(client);
        self
    }

    /// Builds the client. Fails if the base URL is empty or not a valid
    /// absolute HTTP URL.
    pub fn build(self) -> Result<Client, Error> {
        if self.base_url.is_empty() {
            return Err(Error::InvalidRequest("base url is required".to_owned()));
        }
        let base_url = Url::parse(&self.base_url)
            .map_err(|_| Error::InvalidRequest("base url is invalid".to_owned()))?;
        if base_url.cannot_be_a_base() {
            return Err(Error::InvalidRequest("base url is invalid".to_owned()));
        }
        let http = match self.http {
            Some(client) => client,
            None => reqwest::Client::builder()
                .timeout(self.timeout)
                .build()
                .map_err(Error::Transport)?,
        };
        // no total timeout: stream lifetimes are bounded per call.
        let stream_http = reqwest::Client::builder()
            .build()
            .map_err(Error::Transport)?;
        Ok(Client {
            transport: Arc::new(Transport::new(
                base_url,
                http,
                stream_http,
                self.token,
                self.user_agent,
                self.request_id,
            )),
        })
    }
}
