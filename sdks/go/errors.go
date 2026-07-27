package fuse

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const requestIDHeader = "X-Request-ID"
const maxErrorBodyBytes = 1 << 20

// error codes returned by the server in the error envelope.
const (
	CodeNotFound        = "not_found"
	CodeConflict        = "conflict"
	CodeInvalidArgument = "invalid_argument"
	CodeUnavailable     = "unavailable"
	CodeInternal        = "internal"
	CodeUnauthorized    = "unauthorized"
	CodeUnimplemented   = "unimplemented"

	// CodePayloadTooLarge means the request body ran past the server's limit
	// for that endpoint (HTTP 413). Distinct from CodeInvalidArgument: the
	// body may be valid and simply too big, so the fix is to send less rather
	// than to send something different.
	CodePayloadTooLarge = "payload_too_large"

	// CodeRouteNotFound means the URL doesn't match any route the server
	// exposes — a wrong host/port, or a server that isn't the fuse
	// orchestrator at all — as opposed to CodeNotFound's "route exists,
	// resource doesn't".
	CodeRouteNotFound = "route_not_found"
)

type apiErrorEnvelope struct {
	Error apiErrorBody `json:"error"`
}

type apiErrorBody struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Details map[string]string `json:"details,omitempty"`
}

// APIError is a non-2xx response from the server.
type APIError struct {
	Status    int
	Code      string
	Message   string
	Details   map[string]string
	RequestID string
	Body      []byte
}

func (e *APIError) Error() string {
	if e == nil {
		return ""
	}
	parts := []string{fmt.Sprintf("status=%d", e.Status)}
	if e.Code != "" {
		parts = append(parts, "code="+e.Code)
	}
	if e.Message != "" {
		parts = append(parts, e.Message)
	} else if text := http.StatusText(e.Status); text != "" {
		parts = append(parts, strings.ToLower(text))
	}
	if e.RequestID != "" {
		parts = append(parts, "request_id="+e.RequestID)
	}
	return "fuse api error: " + strings.Join(parts, ", ")
}

// AsAPIError extracts an *APIError from err, if present.
func AsAPIError(err error) (*APIError, bool) {
	if err == nil {
		return nil, false
	}
	var target *APIError
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

func isAPIErrorCode(err error, code string) bool {
	apiErr, ok := AsAPIError(err)
	if !ok {
		return false
	}
	return apiErr.Code == code
}

// IsNotFound reports whether err is a not_found api error.
func IsNotFound(err error) bool { return isAPIErrorCode(err, CodeNotFound) }

// IsConflict reports whether err is a conflict api error.
func IsConflict(err error) bool { return isAPIErrorCode(err, CodeConflict) }

// IsUnauthorized reports whether err is an unauthorized api error.
func IsUnauthorized(err error) bool { return isAPIErrorCode(err, CodeUnauthorized) }

// IsInvalidArgument reports whether err is an invalid_argument api error.
func IsInvalidArgument(err error) bool { return isAPIErrorCode(err, CodeInvalidArgument) }

// IsUnavailable reports whether err is an unavailable api error.
func IsUnavailable(err error) bool { return isAPIErrorCode(err, CodeUnavailable) }

// CheckResponse returns nil for 2xx responses and an *APIError otherwise.
func CheckResponse(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	if readErr != nil {
		return &APIError{
			Status:    resp.StatusCode,
			Message:   fmt.Sprintf("read error body: %v", readErr),
			RequestID: resp.Header.Get(requestIDHeader),
		}
	}
	return parseAPIError(resp.StatusCode, resp.Header, body)
}

func parseAPIError(status int, header http.Header, body []byte) error {
	var env apiErrorEnvelope
	if len(body) > 0 && json.Unmarshal(body, &env) == nil {
		if env.Error.Code != "" || env.Error.Message != "" {
			return &APIError{
				Status:    status,
				Code:      env.Error.Code,
				Message:   env.Error.Message,
				Details:   env.Error.Details,
				RequestID: header.Get(requestIDHeader),
				Body:      append([]byte(nil), body...),
			}
		}
	}
	return &APIError{
		Status:    status,
		Message:   strings.ToLower(http.StatusText(status)),
		RequestID: header.Get(requestIDHeader),
		Body:      append([]byte(nil), body...),
	}
}
