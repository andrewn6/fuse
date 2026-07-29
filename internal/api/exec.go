package api

import (
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/folsomintel/fuse/internal/hostwire"
	"github.com/folsomintel/fuse/internal/orchestrator"
)

// execEnvironment runs a command inside a running VM's guest and returns its
// exit code with stdout and stderr kept apart.
//
// A guest command that exits non-zero is a 200. The command ran; it failed;
// that is the answer the caller asked for. HTTP errors are reserved for the
// cases where the command could not be run at all: unknown VM (404), VM not
// running (409), provider without a guest (501), host unreachable (500).
//
// Exec is master-token only. It is root in the guest, and API keys carry no
// scopes today, so anything weaker would silently make every issued key a
// root shell on every VM in the fleet.
func (h *Handler) execEnvironment(w http.ResponseWriter, r *http.Request) {
	if !h.requireMasterToken(w, r, "exec requires the master token") {
		return
	}

	vmID := chi.URLParam(r, "vmId")

	var req ExecEnvironmentRequest
	if !decodeJSON(w, r, MaxEnvironmentBodyBytes, &req) {
		return
	}

	cmd, err := execArgv(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, err.Error(), nil)
		return
	}

	// The guest command can run far longer than the server's WriteTimeout,
	// which Go arms at header-read time. Without clearing it, a command past
	// ~WriteTimeout makes the final response flush fail with an i/o timeout and
	// the client gets a reset connection instead of the result, so the
	// advertised long --timeout is unreachable. The SSE and attach handlers
	// clear the deadline the same way. The request context still bounds the
	// call, so this does not make exec unbounded. A writer that cannot carry a
	// deadline (some test recorders) returns an error, which is harmless: the
	// deadline it lacks is the one we wanted gone.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})

	res, err := h.Fleet.Exec(r.Context(), vmID, cmd, orchestrator.ExecOptions{
		Timeout: time.Duration(req.TimeoutMS) * time.Millisecond,
	})
	if err != nil {
		writeFleetError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, ExecEnvironmentResponse{
		ExitCode: res.ExitCode,
		Stdout:   string(res.Stdout),
		Stderr:   string(res.Stderr),
	})
}

// execArgv resolves the request's cmd/shell pair into the single argv the
// guest will run.
//
// Argv is the default because it needs no quoting rules and cannot be turned
// into an injection by an SDK caller interpolating a value. Shell is the
// explicit opt-in for the cases argv genuinely cannot express: pipelines,
// redirects, and globs.
func execArgv(req ExecEnvironmentRequest) ([]string, error) {
	switch {
	case len(req.Cmd) > 0 && req.Shell != "":
		return nil, errors.New("cmd and shell are mutually exclusive")
	case req.Shell != "":
		return []string{"sh", "-lc", req.Shell}, nil
	case len(req.Cmd) > 0:
		return req.Cmd, nil
	default:
		return nil, errors.New("one of cmd or shell is required")
	}
}

// attachEnvironment upgrades the connection and relays a fuse-attach/1 stream
// between the client and a process inside the guest.
//
// The orchestrator is a byte pump here and nothing more. It does not parse
// frames, because it has no reason to: the encoder is the host agent and the
// decoder is the client. Keeping the same protocol on both hops is what lets
// this stay a plain io.Copy in each direction.
func (h *Handler) attachEnvironment(w http.ResponseWriter, r *http.Request) {
	if !h.requireMasterToken(w, r, "attach requires the master token") {
		return
	}

	if !strings.EqualFold(r.Header.Get("Upgrade"), hostwire.AttachProto) {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument,
			"attach requires Upgrade: "+hostwire.AttachProto, nil)
		return
	}

	vmID := chi.URLParam(r, "vmId")
	spec := hostwire.ParseAttachQuery(r.URL.Query())

	// The host agent also refuses a non-tty attach, but its 400 would reach us
	// as an opaque upgrade failure and surface to the client as a 500. Rejecting
	// here is what turns "you forgot tty=1" back into a 400 that says so.
	if !spec.TTY {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument,
			"attach requires tty=1; use ?action=exec for non-interactive commands", nil)
		return
	}

	// Open the guest stream before hijacking. Once the connection is hijacked
	// there is no ResponseWriter left to report a failure through, so every
	// error that can be expressed as HTTP must be raised while we still can.
	guest, err := h.Fleet.Attach(r.Context(), vmID, spec)
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
			"attach requires a hijackable HTTP/1.1 connection: "+err.Error(), nil)
		return
	}
	defer func() { _ = client.Close() }()

	// The server's WriteTimeout is still armed on this socket. An interactive
	// shell idles for as long as a human stares at a prompt, so clearing the
	// deadline is what keeps the session from being cut out from under them.
	_ = client.SetDeadline(time.Time{})

	if _, err := buf.WriteString(
		"HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: " + hostwire.AttachProto + "\r\n" +
			"Connection: Upgrade\r\n\r\n",
	); err != nil {
		return
	}
	if err := buf.Flush(); err != nil {
		return
	}

	relay(client, buf.Reader, guest)
}

// relay pumps bytes in both directions until either end goes away, then tears
// the other one down.
//
// Reads come from the bufio.Reader rather than the socket: Hijack can hand
// back a reader that already holds bytes the client sent immediately after its
// request, and reading past it would drop them. Writes go straight to the
// socket, because a buffered writer would sit on a keystroke until something
// else happened to flush it.
func relay(client net.Conn, fromClient io.Reader, guest io.ReadWriteCloser) {
	done := make(chan struct{}, 2)

	go func() {
		_, _ = io.Copy(guest, fromClient)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, guest)
		done <- struct{}{}
	}()

	// Whichever direction ends first ends the session. The deferred Closes in
	// the caller unblock the surviving goroutine's pending read.
	<-done
}
