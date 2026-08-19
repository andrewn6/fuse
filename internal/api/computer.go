package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// The computer endpoint: the control-plane door to the guest's
// /v1/computer/* surface. Callers authenticate with their Fuse API key (or
// the master token) and never see the guest token; the fleet attaches it on
// the far side.
//
// Unlike exec and attach this is deliberately NOT master-token only. Exec is
// root in the guest; a computer action is a mouse click on a display the
// environment's image chose to run, which is the level of capability an API
// key already implies for the environments it can see.

// computerAction relays one computer action to the guest agent.
//
// The body passes through verbatim after a shallow check that it is a JSON
// object naming an action. The action schema is owned by the guest agent,
// which is baked into the image and versioned separately from this server; a
// handler that re-encoded the request through ComputerActionRequest would
// silently drop fields it had not learned yet.
func (h *Handler) computerAction(w http.ResponseWriter, r *http.Request) {
	vmID := chi.URLParam(r, "vmId")

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxEnvironmentBodyBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, CodePayloadTooLarge,
				"computer action body too large", nil)
			return
		}
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "read body: "+err.Error(), nil)
		return
	}
	var shallow struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal(body, &shallow); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "malformed action: "+err.Error(), nil)
		return
	}
	if shallow.Action == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "action is required", nil)
		return
	}

	// A hold_key or wait action can legitimately outlive the server's
	// WriteTimeout, and every response carries a screenshot worth of bytes;
	// clear the write deadline the way exec does so the flush cannot be cut
	// off mid-payload. The request context still bounds the call.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})

	res, err := h.Fleet.ComputerAction(r.Context(), vmID, body)
	if err != nil {
		writeFleetError(w, err)
		return
	}
	relayGuestComputer(w, res.Status, res.Body)
}

// computerDisplay relays the guest's display report.
func (h *Handler) computerDisplay(w http.ResponseWriter, r *http.Request) {
	vmID := chi.URLParam(r, "vmId")
	res, err := h.Fleet.ComputerDisplay(r.Context(), vmID)
	if err != nil {
		writeFleetError(w, err)
		return
	}
	relayGuestComputer(w, res.Status, res.Body)
}

// computerStream upgrades the connection and relays a fuse-vnc/1 stream —
// raw RFB bytes — between the client and the vnc server inside the guest.
//
// Like attach, the orchestrator is a byte pump here and nothing more. Unlike
// attach it is not master-token only, for the reason documented at the top
// of this file: the stream grants a view of, and input to, the same display
// an API key can already drive click by click through computerAction.
func (h *Handler) computerStream(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), orchestrator.VNCProto) {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument,
			"stream requires Upgrade: "+orchestrator.VNCProto, nil)
		return
	}

	vmID := chi.URLParam(r, "vmId")

	// Open the guest stream before hijacking. Once the connection is hijacked
	// there is no ResponseWriter left to report a failure through, so every
	// error that can be expressed as HTTP must be raised while we still can.
	guest, err := h.Fleet.ComputerStream(r.Context(), vmID)
	if err != nil {
		writeFleetError(w, err)
		return
	}
	defer func() { _ = guest.Close() }()

	// ResponseController, not a direct w.(http.Hijacker) assertion: the
	// metrics middleware wraps the ResponseWriter, and only the controller
	// walks the Unwrap chain to find the transport underneath.
	rc := http.NewResponseController(w)
	client, buf, err := rc.Hijack()
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal,
			"stream requires a hijackable HTTP/1.1 connection: "+err.Error(), nil)
		return
	}
	defer func() { _ = client.Close() }()

	// a watched-but-idle desktop produces zero RFB traffic; clearing the
	// deadline is what keeps the session from being cut out from under the
	// viewer.
	_ = client.SetDeadline(time.Time{})

	if _, err := buf.WriteString(
		"HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: " + orchestrator.VNCProto + "\r\n" +
			"Connection: Upgrade\r\n\r\n",
	); err != nil {
		return
	}
	if err := buf.Flush(); err != nil {
		return
	}

	relay(client, buf.Reader, guest)
}

// relayGuestComputer forwards a guest agent answer to the caller. A 2xx body
// is relayed verbatim (it is already the wire shape); a guest error is
// re-wrapped into this API's error envelope so callers see one error grammar
// everywhere.
func relayGuestComputer(w http.ResponseWriter, status int, body []byte) {
	if status >= 200 && status < 300 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
		return
	}

	msg := guestErrorMessage(body)
	switch status {
	case http.StatusBadRequest:
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, msg, nil)
	case http.StatusServiceUnavailable:
		// the desktop image's "no display" answer: the environment exists and
		// is running, there is just nothing to click on.
		writeError(w, http.StatusServiceUnavailable, CodeUnavailable, msg, nil)
	case http.StatusUnauthorized, http.StatusForbidden:
		// the guest refused the control plane's own token; a caller cannot
		// fix that, so it is an internal fault, not their 401.
		writeError(w, http.StatusInternalServerError, CodeInternal,
			"guest agent refused the control plane's credentials: "+msg, nil)
	default:
		writeError(w, http.StatusInternalServerError, CodeInternal, msg, nil)
	}
}

// guestErrorMessage extracts the {"error": "..."} the guest agent answers
// errors with, falling back to the raw body.
func guestErrorMessage(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err == nil && e.Error != "" {
		return e.Error
	}
	if len(body) > 512 {
		body = body[:512]
	}
	return string(body)
}
