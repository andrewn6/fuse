use std::sync::Arc;
use std::time::Duration;

use reqwest::{Method, RequestBuilder, Response};
use serde::de::DeserializeOwned;
use url::Url;

use crate::error::{parse_api_error, Error, REQUEST_ID_HEADER};

pub(crate) const DEFAULT_TIMEOUT: Duration = Duration::from_secs(60);

// mirrors the orchestrator's max page size. list helpers that auto-paginate
// (walking every page) request this size internally to minimize round trips.
pub(crate) const MAX_PAGE_LIMIT: u32 = 200;

pub(crate) type RequestIdFn = dyn Fn() -> String + Send + Sync;

pub(crate) struct Transport {
    base_url: Url,
    http: reqwest::Client,
    // stream_http has no total timeout, for calls whose duration is bounded
    // by something else (a long-running exec, an sse stream). do not use it
    // for normal requests.
    stream_http: reqwest::Client,
    bearer: Option<String>,
    user_agent: String,
    request_id: Option<Arc<RequestIdFn>>,
}

impl Transport {
    pub(crate) fn new(
        base_url: Url,
        http: reqwest::Client,
        stream_http: reqwest::Client,
        bearer: Option<String>,
        user_agent: String,
        request_id: Option<Arc<RequestIdFn>>,
    ) -> Self {
        Self {
            base_url,
            http,
            stream_http,
            bearer,
            user_agent,
            request_id,
        }
    }

    pub(crate) fn base_url(&self) -> &Url {
        &self.base_url
    }

    // url builds an absolute url from path segments, replacing the base
    // url's path (matching the go sdk's resolve semantics). segments are
    // percent-encoded, so a caller-supplied id cannot smuggle in extra path
    // structure.
    fn url(&self, segments: &[&str]) -> Url {
        let mut url = self.base_url.clone();
        {
            // the builder rejects cannot-be-a-base urls, so this cannot fail.
            let mut parts = url.path_segments_mut().expect("base url is a base");
            parts.clear();
            for segment in segments {
                parts.push(segment);
            }
        }
        url.set_query(None);
        url
    }

    fn build(&self, client: &reqwest::Client, method: Method, segments: &[&str]) -> RequestBuilder {
        let mut builder = client
            .request(method, self.url(segments))
            .header(reqwest::header::USER_AGENT, &self.user_agent);
        if let Some(bearer) = &self.bearer {
            builder = builder.bearer_auth(bearer);
        }
        if let Some(generate) = &self.request_id {
            let id = generate();
            if !id.is_empty() {
                builder = builder.header(REQUEST_ID_HEADER, id);
            }
        }
        builder
    }

    /// Starts a request on the normal (timeout-bounded) client.
    pub(crate) fn request(&self, method: Method, segments: &[&str]) -> RequestBuilder {
        self.build(&self.http, method, segments)
    }

    /// Starts a request on the no-timeout stream client, for calls whose
    /// duration is bounded by a per-request deadline or by the stream's own
    /// lifecycle.
    pub(crate) fn stream_request(&self, method: Method, segments: &[&str]) -> RequestBuilder {
        self.build(&self.stream_http, method, segments)
    }

    /// Returns the response for 2xx statuses and an [`Error::Api`]
    /// otherwise.
    pub(crate) async fn check(response: Response) -> Result<Response, Error> {
        let status = response.status();
        if status.is_success() {
            return Ok(response);
        }
        let request_id = response
            .headers()
            .get(REQUEST_ID_HEADER)
            .and_then(|value| value.to_str().ok())
            .filter(|value| !value.is_empty())
            .map(str::to_owned);
        let body = response.bytes().await.unwrap_or_default();
        Err(parse_api_error(
            status.as_u16(),
            status.canonical_reason(),
            request_id,
            &body,
        )
        .into())
    }

    /// Sends the request and decodes a JSON response body.
    pub(crate) async fn send_json<T: DeserializeOwned>(
        &self,
        builder: RequestBuilder,
    ) -> Result<T, Error> {
        let response = Self::check(builder.send().await?).await?;
        let body = response.bytes().await?;
        Ok(serde_json::from_slice(&body)?)
    }

    /// Sends the request and discards any response body.
    pub(crate) async fn send_unit(&self, builder: RequestBuilder) -> Result<(), Error> {
        Self::check(builder.send().await?).await.map(drop)
    }
}

// require rejects empty required string arguments before a request is built,
// mirroring the go sdk's client-side checks.
pub(crate) fn require(value: &str, what: &str) -> Result<(), Error> {
    if value.is_empty() {
        return Err(Error::InvalidRequest(format!("{what} is required")));
    }
    Ok(())
}

// pagination adds the shared limit/cursor query params list endpoints
// accept.
pub(crate) fn pagination(
    query: &mut Vec<(&'static str, String)>,
    limit: Option<u32>,
    cursor: &Option<String>,
) {
    if let Some(limit) = limit {
        if limit > 0 {
            query.push(("limit", limit.to_string()));
        }
    }
    if let Some(cursor) = cursor {
        if !cursor.is_empty() {
            query.push(("cursor", cursor.clone()));
        }
    }
}
