use reqwest::Response;

use crate::error::Error;
use crate::types::Event;

/// The live SSE stream from
/// [`Environments::events`](crate::Environments::events).
///
/// Call [`next`](EventStream::next) until it returns `None`. A terminal
/// state event (destroyed/failed) is delivered, then the stream ends. Drop
/// the stream to stop early; there is no other cancellation handle.
///
/// ```no_run
/// # async fn run(client: fuse::Client) -> Result<(), fuse::Error> {
/// let mut events = client.environments().events("vm-1").await?;
/// while let Some(event) = events.next().await {
///     let event = event?;
///     println!("{:?}", event.state);
/// }
/// # Ok(())
/// # }
/// ```
pub struct EventStream {
    response: Response,
    buffer: Vec<u8>,
    data: String,
    has_data: bool,
    done: bool,
}

impl EventStream {
    pub(crate) fn new(response: Response) -> Self {
        Self {
            response,
            buffer: Vec::new(),
            data: String::new(),
            has_data: false,
            done: false,
        }
    }

    /// Returns the next event, or `None` when the stream has ended: after a
    /// terminal state event, on clean EOF, or after an error has been
    /// yielded.
    pub async fn next(&mut self) -> Option<Result<Event, Error>> {
        if self.done {
            return None;
        }
        loop {
            while let Some(position) = self.buffer.iter().position(|&byte| byte == b'\n') {
                let raw: Vec<u8> = self.buffer.drain(..=position).collect();
                let line = String::from_utf8_lossy(&raw);
                let line = line.trim_end_matches(['\n', '\r']);
                if line.is_empty() {
                    // blank line terminates the current event.
                    if !self.has_data {
                        continue;
                    }
                    let payload = std::mem::take(&mut self.data);
                    self.has_data = false;
                    match serde_json::from_str::<Event>(&payload) {
                        Ok(event) => {
                            if event.is_terminal() {
                                self.done = true;
                            }
                            return Some(Ok(event));
                        }
                        Err(err) => {
                            self.done = true;
                            return Some(Err(err.into()));
                        }
                    }
                } else if line.starts_with(':') {
                    // comment / keepalive line, skip.
                } else if let Some(payload) = line.strip_prefix("data:") {
                    // strip the prefix and a single optional leading space.
                    self.data
                        .push_str(payload.strip_prefix(' ').unwrap_or(payload));
                    self.has_data = true;
                }
                // other fields (id:, event:, ...) are ignored.
            }
            match self.response.chunk().await {
                Ok(Some(bytes)) => self.buffer.extend_from_slice(&bytes),
                Ok(None) => {
                    // clean eof with no terminal state: just end.
                    self.done = true;
                    return None;
                }
                Err(err) => {
                    self.done = true;
                    return Some(Err(err.into()));
                }
            }
        }
    }
}
