package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Request body size limits, in bytes. Every JSON handler decodes through one
// of these so a single request can never make the orchestrator allocate an
// unbounded amount of memory.
//
// The limits are generous relative to real payloads — they exist to bound the
// worst case, not to police field sizes, which the handlers validate
// separately. They apply to chunked bodies as well as declared
// Content-Length ones, because [http.MaxBytesReader] counts bytes actually
// read rather than trusting the header.
const (
	// MaxLoginBodyBytes bounds POST /login. It is the only unauthenticated
	// body-carrying route, so it gets the tightest limit: the body is one
	// token string and nothing else.
	MaxLoginBodyBytes = 4 << 10 // 4 KiB

	// MaxJSONBodyBytes bounds ordinary authenticated JSON requests —
	// snapshot options, fork options, host registration, API key labels.
	// These are all a handful of short fields.
	MaxJSONBodyBytes = 64 << 10 // 64 KiB

	// MaxEnvironmentBodyBytes bounds POST /v1/environments, which carries an
	// inline manifest, a startup script, and a secrets map, and POST exec,
	// whose command can be a long shell line. This is the largest body the
	// API accepts.
	MaxEnvironmentBodyBytes = 1 << 20 // 1 MiB
)

// bodyReadTimeout bounds how long a client may take to deliver a JSON body.
// A size limit alone does not stop a client that sends one byte a minute and
// holds a connection open forever.
//
// This is applied per request inside the decode helpers rather than as a
// server-wide http.Server.ReadTimeout. A server-wide read deadline stays armed
// for the whole request, and Go's background client-disconnect read treats its
// expiry as a dead connection — which would cut off the two handlers that hold
// a connection open on purpose, SSE event streams and attach sessions. Arming
// it only around the decode, and clearing it after, leaves those untouched.
const bodyReadTimeout = 15 * time.Second

// decodeJSON decodes a required JSON request body into dst, reading at most
// limit bytes. It writes the error response itself and reports whether the
// caller should continue.
//
// An oversized body gets 413 rather than 400: the request may be perfectly
// well-formed and simply too big, and a client that retries after trimming it
// needs to be able to tell the two apart.
func decodeJSON(w http.ResponseWriter, r *http.Request, limit int64, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	defer boundBodyRead(w)()

	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeDecodeError(w, err, limit)
		return false
	}
	return true
}

// boundBodyRead arms a read deadline for the body decode and returns the
// function that clears it. Clearing matters: exec and attach legitimately run
// far past bodyReadTimeout once their (short) body has been read.
//
// A transport that cannot carry a read deadline — some test recorders — is
// fine to skip: the deadline it lacks is one it also cannot violate.
func boundBodyRead(w http.ResponseWriter) func() {
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Now().Add(bodyReadTimeout)); err != nil {
		return func() {}
	}
	return func() { _ = rc.SetReadDeadline(time.Time{}) }
}

// decodeOptionalJSON is decodeJSON for routes whose body may be omitted
// entirely. An absent or empty body leaves dst at its zero value and succeeds;
// anything present is decoded and bounded exactly as decodeJSON does.
//
// Emptiness is decided by reading, not by Content-Length, so a chunked body is
// bounded too. Content-Length is -1 for chunked requests, and the older
// `if r.ContentLength > 0` guard therefore skipped those bodies rather than
// reading them.
func decodeOptionalJSON(w http.ResponseWriter, r *http.Request, limit int64, dst any) bool {
	if r.Body == nil || r.ContentLength == 0 {
		return true
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	defer boundBodyRead(w)()

	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		// An empty body decodes to io.EOF. That is the "omitted body" case,
		// which these routes accept.
		if errors.Is(err, io.EOF) {
			return true
		}
		writeDecodeError(w, err, limit)
		return false
	}
	return true
}

// writeDecodeError maps a decode failure to a response: 413 when the body ran
// past the limit, 400 otherwise.
func writeDecodeError(w http.ResponseWriter, err error, limit int64) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, CodePayloadTooLarge,
			fmt.Sprintf("request body exceeds the %d byte limit for this endpoint", limit), nil)
		return
	}
	writeError(w, http.StatusBadRequest, CodeInvalidArgument,
		"invalid JSON body: "+err.Error(), nil)
}
