use std::sync::Arc;

use reqwest::Method;
use serde::{Deserialize, Serialize};

use crate::error::Error;
use crate::transport::{require, Transport};
use crate::types::{ApiKey, CreatedApiKey};

/// The API keys service, from [`Client::api_keys`](crate::Client::api_keys).
///
/// Every method requires the master token; the server enforces this.
#[derive(Clone)]
pub struct ApiKeys {
    t: Arc<Transport>,
}

impl ApiKeys {
    pub(crate) fn new(t: Arc<Transport>) -> Self {
        Self { t }
    }

    /// Issues a new API key. The raw secret is returned once in
    /// [`CreatedApiKey::key`] and is unrecoverable afterward.
    pub async fn create(&self, label: impl Into<String>) -> Result<CreatedApiKey, Error> {
        #[derive(Serialize)]
        struct Body {
            #[serde(skip_serializing_if = "String::is_empty")]
            label: String,
        }
        let request = self
            .t
            .request(Method::POST, &["v1", "api-keys"])
            .json(&Body {
                label: label.into(),
            });
        self.t.send_json(request).await
    }

    /// Returns the metadata for all API keys.
    pub async fn list(&self) -> Result<Vec<ApiKey>, Error> {
        #[derive(Deserialize)]
        struct Wire {
            #[serde(default)]
            api_keys: Vec<ApiKey>,
        }
        let request = self.t.request(Method::GET, &["v1", "api-keys"]);
        let wire: Wire = self.t.send_json(request).await?;
        Ok(wire.api_keys)
    }

    /// Deletes the API key with the given id.
    pub async fn revoke(&self, id: &str) -> Result<(), Error> {
        require(id, "id")?;
        let request = self.t.request(Method::DELETE, &["v1", "api-keys", id]);
        self.t.send_unit(request).await
    }
}
