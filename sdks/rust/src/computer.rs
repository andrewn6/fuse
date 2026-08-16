//! The computer-use adapter: the glue between a Claude agent loop and an
//! environment's desktop.
//!
//! Computer use is a client-side tool. Claude never connects to the sandbox:
//! it emits a `tool_use` block, the caller's loop executes it against an
//! environment it controls, and returns a `tool_result` with a screenshot.
//! This module is that translation, done once, so nobody has to rediscover
//! that the `tool_use` input is already the wire shape the computer endpoint
//! takes.

use reqwest::Method;
use serde::{Deserialize, Serialize};

use crate::environments::Environments;
use crate::error::Error;
use crate::strenum::string_enum;
use crate::transport::require;

string_enum! {
    /// Computer-use actions the guest agent understands, in the same
    /// vocabulary Anthropic's computer tool emits.
    pub enum ComputerActionKind {
        Screenshot => "screenshot",
        CursorPosition => "cursor_position",
        MouseMove => "mouse_move",
        LeftClick => "left_click",
        RightClick => "right_click",
        MiddleClick => "middle_click",
        DoubleClick => "double_click",
        TripleClick => "triple_click",
        LeftClickDrag => "left_click_drag",
        LeftMouseDown => "left_mouse_down",
        LeftMouseUp => "left_mouse_up",
        /// Types a string of text (the tool's `type` action).
        Type => "type",
        Key => "key",
        HoldKey => "hold_key",
        Scroll => "scroll",
        Wait => "wait",
        Zoom => "zoom",
    }
}

string_enum! {
    /// Directions for [`ComputerActionKind::Scroll`].
    pub enum ScrollDirection {
        Up => "up",
        Down => "down",
        Left => "left",
        Right => "right",
    }
}

/// An `[x, y]` pixel position on the display.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub struct Coordinate(pub u32, pub u32);

/// An `[x, y, width, height]` crop of the display, for
/// [`ComputerActionKind::Zoom`].
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub struct Region(pub u32, pub u32, pub u32, pub u32);

/// One computer-use action, in the same shape Anthropic's computer tool
/// emits as `tool_use` input, so translating a `tool_use` block into a call
/// is mechanical. Only `action` is required; which other fields apply
/// depends on the action, and the guest agent enforces the full schema.
///
/// ```
/// use fuse::{ComputerAction, ScrollDirection};
///
/// let shot = ComputerAction::screenshot();
/// let click = ComputerAction::left_click(640, 400);
/// let scroll = ComputerAction::scroll(640, 400, ScrollDirection::Down, 3);
/// ```
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ComputerAction {
    pub action: ComputerActionKind,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub coordinate: Option<Coordinate>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub start_coordinate: Option<Coordinate>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub text: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub region: Option<Region>,
    /// Seconds, for [`ComputerActionKind::Wait`] and
    /// [`ComputerActionKind::HoldKey`].
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub duration: Option<f64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub scroll_direction: Option<ScrollDirection>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub scroll_amount: Option<u32>,
}

impl ComputerAction {
    /// Returns a bare action of the given kind; chain setters for its
    /// parameters.
    pub fn new(action: ComputerActionKind) -> Self {
        Self {
            action,
            coordinate: None,
            start_coordinate: None,
            text: None,
            region: None,
            duration: None,
            scroll_direction: None,
            scroll_amount: None,
        }
    }

    /// Captures the display.
    pub fn screenshot() -> Self {
        Self::new(ComputerActionKind::Screenshot)
    }

    /// Left-clicks at `(x, y)`.
    pub fn left_click(x: u32, y: u32) -> Self {
        Self::new(ComputerActionKind::LeftClick).coordinate(x, y)
    }

    /// Types a string of text.
    pub fn type_text(text: impl Into<String>) -> Self {
        Self::new(ComputerActionKind::Type).text(text)
    }

    /// Presses a key or key combination (e.g. `Return`, `ctrl+s`).
    pub fn key(text: impl Into<String>) -> Self {
        Self::new(ComputerActionKind::Key).text(text)
    }

    /// Scrolls at `(x, y)` by `amount` clicks in `direction`.
    pub fn scroll(x: u32, y: u32, direction: ScrollDirection, amount: u32) -> Self {
        let mut action = Self::new(ComputerActionKind::Scroll).coordinate(x, y);
        action.scroll_direction = Some(direction);
        action.scroll_amount = Some(amount);
        action
    }

    pub fn coordinate(mut self, x: u32, y: u32) -> Self {
        self.coordinate = Some(Coordinate(x, y));
        self
    }

    pub fn start_coordinate(mut self, x: u32, y: u32) -> Self {
        self.start_coordinate = Some(Coordinate(x, y));
        self
    }

    pub fn text(mut self, text: impl Into<String>) -> Self {
        self.text = Some(text.into());
        self
    }

    pub fn region(mut self, x: u32, y: u32, width: u32, height: u32) -> Self {
        self.region = Some(Region(x, y, width, height));
        self
    }

    pub fn duration(mut self, seconds: f64) -> Self {
        self.duration = Some(seconds);
        self
    }
}

/// The guest's answer to one action. `screenshot` is a base64 PNG, present
/// on every action that implies one; for zoom it is the cropped region at
/// full resolution.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct ComputerResult {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub output: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub screenshot: Option<String>,
}

/// Reports the environment's display: whether it is up and at what
/// geometry, so a caller can populate `display_width_px` /
/// `display_height_px` in its computer tool definition without hardcoding
/// them.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct ComputerDisplay {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub display: Option<String>,
    pub up: bool,
    pub width: u32,
    pub height: u32,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
}

/// A Messages-API base64 image source.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ImageSource {
    /// Always `base64`.
    #[serde(rename = "type")]
    pub kind: String,
    /// Always `image/png`.
    pub media_type: String,
    pub data: String,
}

impl ImageSource {
    /// Returns a base64 PNG source.
    pub fn png(data: impl Into<String>) -> Self {
        Self {
            kind: "base64".to_owned(),
            media_type: "image/png".to_owned(),
            data: data.into(),
        }
    }
}

/// One content block of a Messages API `tool_result`. The serialized shape
/// matches the Messages API, so a caller can serialize the content straight
/// into the `tool_result` they send back.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum ToolResultBlock {
    Text { text: String },
    Image { source: ImageSource },
}

/// What goes back to Claude for one computer `tool_use`: the content blocks
/// and whether they describe an error. Map it onto the `tool_result` block
/// for the `tool_use`'s id and the loop is closed.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct ComputerToolResult {
    pub content: Vec<ToolResultBlock>,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub is_error: bool,
}

fn tool_error(message: &str) -> ComputerToolResult {
    let message = if message.is_empty() {
        "the computer action failed"
    } else {
        message
    };
    ComputerToolResult {
        is_error: true,
        content: vec![ToolResultBlock::Text {
            text: message.to_owned(),
        }],
    }
}

impl Environments {
    /// Relays one computer-use action to the environment's desktop and
    /// returns the result, usually carrying a screenshot. It requires an
    /// environment booted from a desktop image; on any other image the
    /// server answers 503 with a reason. Screenshots ride back
    /// base64-encoded, so the no-timeout stream client carries the call.
    pub async fn computer(
        &self,
        vm_id: &str,
        action: ComputerAction,
    ) -> Result<ComputerResult, Error> {
        require(vm_id, "vm id")?;
        let request = self
            .transport()
            .stream_request(Method::POST, &["v1", "environments", vm_id, "computer"])
            .json(&action);
        self.transport().send_json(request).await
    }

    /// Reports whether the environment has a live display and at what
    /// geometry. `up` is false with a reason on an image with no desktop.
    pub async fn computer_display(&self, vm_id: &str) -> Result<ComputerDisplay, Error> {
        require(vm_id, "vm id")?;
        let request = self
            .transport()
            .request(Method::GET, &["v1", "environments", vm_id, "computer"]);
        self.transport().send_json(request).await
    }

    /// Executes one computer `tool_use` against an environment and shapes
    /// the answer for the Messages API.
    ///
    /// `input` is the `tool_use` block's input field, verbatim: it is
    /// already the shape the computer endpoint takes, and it is forwarded as
    /// raw JSON rather than re-encoded, so action fields this SDK has not
    /// learned yet still reach the guest.
    ///
    /// An action the environment rejects (a malformed coordinate, a display
    /// that is not up) comes back as `is_error` content rather than an
    /// `Err`: the agent loop should feed it to the model, which is the party
    /// that can correct it. An `Err` is reserved for the failures the model
    /// cannot fix — transport, auth, an unknown environment.
    pub async fn computer_tool_result(
        &self,
        vm_id: &str,
        input: &serde_json::Value,
    ) -> Result<ComputerToolResult, Error> {
        require(vm_id, "vm id")?;
        let action = match input {
            serde_json::Value::Object(fields) => fields
                .get("action")
                .and_then(serde_json::Value::as_str)
                .unwrap_or_default(),
            _ => {
                return Err(Error::InvalidRequest(
                    "tool input is not a json object".to_owned(),
                ));
            }
        };
        if action.is_empty() {
            return Ok(tool_error("tool input names no action"));
        }

        let request = self
            .transport()
            .stream_request(Method::POST, &["v1", "environments", vm_id, "computer"])
            .json(input);
        let result: ComputerResult = match self.transport().send_json(request).await {
            Ok(result) => result,
            Err(Error::Api(api_err)) if matches!(api_err.status, 400 | 409 | 503) => {
                // the model asked for something the environment refused;
                // telling the model is the fix, not failing the loop.
                return Ok(tool_error(&api_err.message));
            }
            Err(err) => return Err(err),
        };

        let mut content = Vec::new();
        if let Some(output) = result.output.filter(|text| !text.is_empty()) {
            content.push(ToolResultBlock::Text { text: output });
        }
        if let Some(screenshot) = result.screenshot.filter(|data| !data.is_empty()) {
            content.push(ToolResultBlock::Image {
                source: ImageSource::png(screenshot),
            });
        }
        if content.is_empty() {
            // an action with nothing to show still needs a tool_result.
            content.push(ToolResultBlock::Text {
                text: "done".to_owned(),
            });
        }
        Ok(ComputerToolResult {
            content,
            is_error: false,
        })
    }
}
