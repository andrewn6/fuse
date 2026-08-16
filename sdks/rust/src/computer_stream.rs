//! The live desktop stream: the live half of the computer surface.
//!
//! Where [`Environments::computer`] relays one JSON action, this opens a raw
//! RFB (VNC) byte stream to the environment's desktop — the display and the
//! input — so it is also how a human takes over a session an agent is
//! driving. The bytes are verbatim from the vnc server inside the guest;
//! hand the stream to any RFB client (the `fuse desktop` CLI command wraps
//! it in a browser viewer). There is no frame protocol on top.

use std::collections::HashMap;
use std::io;
use std::pin::Pin;
use std::task::{Context, Poll};

use reqwest::header::{CONNECTION, UPGRADE};
use reqwest::{Method, StatusCode};
use tokio::io::{AsyncRead, AsyncWrite, ReadBuf};

use crate::environments::Environments;
use crate::error::{ApiError, Error};
use crate::transport::{require, Transport};

/// The `Upgrade` token that opens a live desktop stream.
pub const VNC_PROTO: &str = "fuse-vnc/1";

/// A live duplex connection to an environment's desktop, from
/// [`Environments::computer_stream`]. Implements [`AsyncRead`] and
/// [`AsyncWrite`]; dropping it closes the stream.
pub struct ComputerStream {
    inner: reqwest::Upgraded,
}

impl std::fmt::Debug for ComputerStream {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ComputerStream").finish_non_exhaustive()
    }
}

impl AsyncRead for ComputerStream {
    fn poll_read(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &mut ReadBuf<'_>,
    ) -> Poll<io::Result<()>> {
        Pin::new(&mut self.inner).poll_read(cx, buf)
    }
}

impl AsyncWrite for ComputerStream {
    fn poll_write(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &[u8],
    ) -> Poll<io::Result<usize>> {
        Pin::new(&mut self.inner).poll_write(cx, buf)
    }

    fn poll_flush(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        Pin::new(&mut self.inner).poll_flush(cx)
    }

    fn poll_shutdown(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        Pin::new(&mut self.inner).poll_shutdown(cx)
    }
}

impl Environments {
    /// Opens the live view of a running environment's desktop.
    ///
    /// It requires an environment booted from a desktop image; on any other
    /// image the server answers 503 with a reason. Unlike exec this accepts
    /// API keys: the stream views and drives the same display an API key
    /// can already drive click by click through
    /// [`Environments::computer`].
    ///
    /// The connection upgrade rides the SDK's http/1.1-only client, so it
    /// works against both plain and TLS orchestrators. The stream has no
    /// timeout — an idle desktop produces zero RFB traffic — and lives
    /// until dropped.
    pub async fn computer_stream(&self, vm_id: &str) -> Result<ComputerStream, Error> {
        require(vm_id, "vm id")?;

        let response = self
            .transport()
            .upgrade_request(
                Method::GET,
                &["v1", "environments", vm_id, "computer", "stream"],
            )
            .header(CONNECTION, "Upgrade")
            .header(UPGRADE, VNC_PROTO)
            .send()
            .await?;

        if response.status() != StatusCode::SWITCHING_PROTOCOLS {
            // a non-2xx surfaces through the normal error envelope path; a
            // 2xx that is not a 101 is a server that did not upgrade, which
            // has no envelope to parse.
            let response = Transport::check(response).await?;
            return Err(Error::from(ApiError {
                status: response.status().as_u16(),
                code: None,
                message: format!(
                    "expected 101 switching protocols, got {}",
                    response.status()
                ),
                details: HashMap::new(),
                request_id: None,
                body: Vec::new(),
            }));
        }

        let inner = response.upgrade().await?;
        Ok(ComputerStream { inner })
    }
}
