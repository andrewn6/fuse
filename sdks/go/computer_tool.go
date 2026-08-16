package fuse

// The computer-use adapter: the glue between a Claude agent loop and an
// environment's desktop.
//
// Computer use is a client-side tool. Claude never connects to the sandbox:
// it emits a tool_use block, the caller's loop executes it against an
// environment it controls, and returns a tool_result with a screenshot. This
// file is that translation, done once, so nobody has to rediscover that the
// tool_use input is already the wire shape the computer endpoint takes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// ToolResultImageSource is a Messages-API base64 image source.
type ToolResultImageSource struct {
	Type      string `json:"type"`       // always "base64"
	MediaType string `json:"media_type"` // always "image/png"
	Data      string `json:"data"`
}

// ToolResultBlock is one content block of a Messages API tool_result. The
// json tags match the Messages API, so a caller can marshal Content straight
// into the tool_result they send back.
type ToolResultBlock struct {
	Type   string                 `json:"type"` // "text" or "image"
	Text   string                 `json:"text,omitempty"`
	Source *ToolResultImageSource `json:"source,omitempty"`
}

// ComputerToolResult is what goes back to Claude for one computer tool_use:
// the content blocks and whether they describe an error. Map it onto the
// tool_result block for the tool_use's id and the loop is closed.
type ComputerToolResult struct {
	Content []ToolResultBlock `json:"content"`
	IsError bool              `json:"is_error,omitempty"`
}

// ComputerToolResult executes one computer tool_use against an environment
// and shapes the answer for the Messages API.
//
// input is the tool_use block's input field, verbatim: it is already the
// shape the computer endpoint takes, and it is forwarded as raw JSON rather
// than re-encoded, so action fields this SDK has not learned yet still reach
// the guest.
//
// An action the environment rejects (a malformed coordinate, a display that
// is not up) comes back as IsError content rather than a Go error: the agent
// loop should feed it to the model, which is the party that can correct it.
// A Go error is reserved for the failures the model cannot fix — transport,
// auth, an unknown environment.
func (s *EnvironmentsService) ComputerToolResult(ctx context.Context, vmID string, input json.RawMessage) (*ComputerToolResult, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("environments service is not configured")
	}
	if vmID == "" {
		return nil, errors.New("vm id is required")
	}
	var shallow struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal(input, &shallow); err != nil {
		return nil, fmt.Errorf("tool input is not a json object: %w", err)
	}
	if shallow.Action == "" {
		return toolError("tool input names no action"), nil
	}

	path := "/v1/environments/" + url.PathEscape(vmID) + "/computer"
	req, err := s.t.newRequest(ctx, http.MethodPost, path, nil, input)
	if err != nil {
		return nil, err
	}
	resp, err := s.t.doStream(req)
	if err != nil {
		return nil, err
	}
	if err := CheckResponse(resp); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			switch apiErr.Status {
			case http.StatusBadRequest, http.StatusConflict, http.StatusServiceUnavailable:
				// the model asked for something the environment refused;
				// telling the model is the fix, not failing the loop.
				return toolError(apiErr.Message), nil
			}
		}
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var res ComputerActionResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, fmt.Errorf("decode computer response: %w", err)
	}

	var blocks []ToolResultBlock
	if res.Output != "" {
		blocks = append(blocks, ToolResultBlock{Type: "text", Text: res.Output})
	}
	if res.Screenshot != "" {
		blocks = append(blocks, ToolResultBlock{Type: "image", Source: &ToolResultImageSource{
			Type:      "base64",
			MediaType: "image/png",
			Data:      res.Screenshot,
		}})
	}
	if len(blocks) == 0 {
		// an action with nothing to show still needs a tool_result.
		blocks = append(blocks, ToolResultBlock{Type: "text", Text: "done"})
	}
	return &ComputerToolResult{Content: blocks}, nil
}

func toolError(msg string) *ComputerToolResult {
	if msg == "" {
		msg = "the computer action failed"
	}
	return &ComputerToolResult{
		IsError: true,
		Content: []ToolResultBlock{{Type: "text", Text: msg}},
	}
}
