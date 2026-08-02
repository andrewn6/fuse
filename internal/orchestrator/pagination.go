package orchestrator

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// DefaultPageLimit is the page size used when a caller passes limit <= 0.
// MaxPageLimit is the ceiling a larger request is clamped to (never
// rejected — a caller that asks for too much just gets less than it asked
// for plus a cursor to keep paging).
const (
	DefaultPageLimit = 50
	MaxPageLimit     = 200
)

// ErrInvalidCursor is returned when a caller-supplied pagination cursor
// cannot be decoded. It is a client error (maps to 400 at the API
// boundary), not a server fault.
var ErrInvalidCursor = errors.New("invalid pagination cursor")

// clampLimit normalizes a caller-supplied page size: <=0 becomes
// DefaultPageLimit, anything above MaxPageLimit is clamped down to it.
func clampLimit(limit int) int {
	if limit <= 0 {
		return DefaultPageLimit
	}
	if limit > MaxPageLimit {
		return MaxPageLimit
	}
	return limit
}

// encodeCursor renders v (a small struct describing the last-seen sort key)
// as an opaque cursor token. The token's internal shape is not a stable
// contract — callers must treat it as opaque and round-trip it verbatim.
func encodeCursor(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("encode cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// decodeCursor parses a token produced by encodeCursor into v. A malformed
// token (tampered, truncated, or from an incompatible server version)
// yields ErrInvalidCursor rather than a raw json/base64 error, since the
// caller only needs to know "this cursor is unusable."
func decodeCursor(token string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return ErrInvalidCursor
	}
	if err := json.Unmarshal(b, v); err != nil {
		return ErrInvalidCursor
	}
	return nil
}
