use std::collections::HashMap;
use std::fmt;

use serde::Deserialize;

use crate::strenum::string_enum;

pub(crate) const REQUEST_ID_HEADER: &str = "x-request-id";

string_enum! {
    /// Machine-readable error codes returned by the server in the error
    /// envelope.
    pub enum ErrorCode {
        NotFound => "not_found",
        Conflict => "conflict",
        InvalidArgument => "invalid_argument",
        Unavailable => "unavailable",
        Internal => "internal",
        Unauthorized => "unauthorized",
        Unimplemented => "unimplemented",
        /// The request body ran past the server's limit for that endpoint
        /// (HTTP 413). Distinct from [`ErrorCode::InvalidArgument`]: the body
        /// may be valid and simply too big, so the fix is to send less rather
        /// than to send something different.
        PayloadTooLarge => "payload_too_large",
        /// The URL doesn't match any route the server exposes — a wrong
        /// host/port, or a server that isn't the fuse orchestrator at all —
        /// as opposed to [`ErrorCode::NotFound`]'s "route exists, resource
        /// doesn't".
        RouteNotFound => "route_not_found",
    }
}

/// A non-2xx response from the server.
#[derive(Debug, Clone)]
#[non_exhaustive]
pub struct ApiError {
    /// HTTP status code of the response.
    pub status: u16,
    /// Machine-readable code from the error envelope, when the server sent
    /// one.
    pub code: Option<ErrorCode>,
    /// Human-readable message. Falls back to the HTTP status text when the
    /// body carried no envelope.
    pub message: String,
    /// Extra key/value context from the error envelope.
    pub details: HashMap<String, String>,
    /// The `X-Request-ID` response header, when present.
    pub request_id: Option<String>,
    /// The raw response body, for debugging responses that did not parse.
    pub body: Vec<u8>,
}

impl fmt::Display for ApiError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "fuse api error: status={}", self.status)?;
        if let Some(code) = &self.code {
            write!(f, ", code={code}")?;
        }
        if !self.message.is_empty() {
            write!(f, ", {}", self.message)?;
        }
        if let Some(request_id) = &self.request_id {
            write!(f, ", request_id={request_id}")?;
        }
        Ok(())
    }
}

impl std::error::Error for ApiError {}

/// The error type for every fallible call in this SDK.
#[derive(Debug, thiserror::Error)]
#[non_exhaustive]
pub enum Error {
    /// The server answered with a non-2xx status. Boxed to keep the `Err`
    /// variant small on every `Result` this SDK returns.
    #[error(transparent)]
    Api(#[from] Box<ApiError>),
    /// The request could not be sent or the response body could not be read.
    #[error("transport: {0}")]
    Transport(#[from] reqwest::Error),
    /// A 2xx response body did not decode as the expected shape.
    #[error("decode response: {0}")]
    Decode(#[from] serde_json::Error),
    /// The request was rejected client-side before being sent.
    #[error("invalid request: {0}")]
    InvalidRequest(String),
}

impl From<ApiError> for Error {
    fn from(err: ApiError) -> Self {
        Self::Api(Box::new(err))
    }
}

impl Error {
    /// Returns the underlying [`ApiError`] when the server rejected the
    /// request.
    pub fn api(&self) -> Option<&ApiError> {
        match self {
            Self::Api(err) => Some(err),
            _ => None,
        }
    }

    /// Returns the HTTP status of an [`Error::Api`].
    pub fn status(&self) -> Option<u16> {
        self.api().map(|err| err.status)
    }

    /// Returns the server error code of an [`Error::Api`].
    pub fn code(&self) -> Option<&ErrorCode> {
        self.api().and_then(|err| err.code.as_ref())
    }

    fn is_code(&self, code: ErrorCode) -> bool {
        self.code() == Some(&code)
    }

    /// Reports whether this is a `not_found` api error.
    pub fn is_not_found(&self) -> bool {
        self.is_code(ErrorCode::NotFound)
    }

    /// Reports whether this is a `conflict` api error.
    pub fn is_conflict(&self) -> bool {
        self.is_code(ErrorCode::Conflict)
    }

    /// Reports whether this is an `unauthorized` api error.
    pub fn is_unauthorized(&self) -> bool {
        self.is_code(ErrorCode::Unauthorized)
    }

    /// Reports whether this is an `invalid_argument` api error.
    pub fn is_invalid_argument(&self) -> bool {
        self.is_code(ErrorCode::InvalidArgument)
    }

    /// Reports whether this is an `unavailable` api error.
    pub fn is_unavailable(&self) -> bool {
        self.is_code(ErrorCode::Unavailable)
    }
}

#[derive(Deserialize)]
struct ErrorEnvelope {
    error: ErrorBody,
}

#[derive(Deserialize, Default)]
struct ErrorBody {
    #[serde(default)]
    code: String,
    #[serde(default)]
    message: String,
    #[serde(default)]
    details: HashMap<String, String>,
}

// parse_api_error builds an ApiError from a non-2xx response: the envelope
// when the body carries one, the status text otherwise.
pub(crate) fn parse_api_error(
    status: u16,
    status_text: Option<&str>,
    request_id: Option<String>,
    body: &[u8],
) -> ApiError {
    if !body.is_empty() {
        if let Ok(envelope) = serde_json::from_slice::<ErrorEnvelope>(body) {
            if !envelope.error.code.is_empty() || !envelope.error.message.is_empty() {
                let code = if envelope.error.code.is_empty() {
                    None
                } else {
                    Some(ErrorCode::from(envelope.error.code))
                };
                return ApiError {
                    status,
                    code,
                    message: envelope.error.message,
                    details: envelope.error.details,
                    request_id,
                    body: body.to_vec(),
                };
            }
        }
    }
    ApiError {
        status,
        code: None,
        message: status_text.map(str::to_lowercase).unwrap_or_default(),
        details: HashMap::new(),
        request_id,
        body: body.to_vec(),
    }
}
