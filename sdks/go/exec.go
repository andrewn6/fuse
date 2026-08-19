package fuse

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultExecServerTimeout mirrors the orchestrator's own default guest-command
// bound. It is what the server applies when TimeoutMS is zero, so the client
// waits at least that long before giving up.
const defaultExecServerTimeout = 60 * time.Second

// execHTTPOverhead is headroom on top of the guest timeout for the hops the
// guest timeout does not cover: the ssh connect on the host and the network
// round trip. Without it a command that runs for exactly its guest timeout
// would race the client's deadline.
const execHTTPOverhead = 30 * time.Second

// ExecRequest is the body of an exec call. Exactly one of Cmd or Shell must be
// set.
type ExecRequest struct {
	// Cmd is the argv to run in the guest, e.g. []string{"ls", "-l"}. Argv
	// needs no quoting rules and cannot be turned into an injection by
	// interpolating a value, so prefer it.
	Cmd []string `json:"cmd,omitempty"`

	// Shell runs the string under `sh -lc`. Use it only for what argv cannot
	// express: pipelines, redirects, and globs.
	Shell string `json:"shell,omitempty"`

	// TimeoutMS bounds the command inside the guest. Zero takes the server
	// default; the server clamps anything above its ceiling.
	TimeoutMS int `json:"timeout_ms,omitempty"`
}

// ExecResult is the outcome of a guest command.
//
// A non-zero ExitCode is a successful call: the command ran and failed. Only a
// non-nil error means the command could not be run at all.
type ExecResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// Exec runs a command inside a running environment's guest and returns its exit
// code with stdout and stderr kept separate.
//
// Exec requires the master token.
func (s *EnvironmentsService) Exec(ctx context.Context, vmID string, in ExecRequest) (*ExecResult, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("environments service is not configured")
	}
	if vmID == "" {
		return nil, errors.New("vm id is required")
	}
	if len(in.Cmd) == 0 && in.Shell == "" {
		return nil, errors.New("one of Cmd or Shell is required")
	}
	if len(in.Cmd) > 0 && in.Shell != "" {
		return nil, errors.New("Cmd and Shell are mutually exclusive")
	}

	path := "/v1/environments/" + url.PathEscape(vmID)
	values := url.Values{}
	values.Set("action", "exec")

	// A guest command can run far longer than the default client timeout, so
	// this uses the no-timeout stream client and bounds the call by a context
	// deadline derived from the requested guest timeout instead. Without this
	// the 60s client default would cut a longer command short awaiting headers,
	// making the requested TimeoutMS unreachable. An existing shorter deadline
	// on ctx still wins.
	guest := time.Duration(in.TimeoutMS) * time.Millisecond
	if guest <= 0 {
		guest = defaultExecServerTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, guest+execHTTPOverhead)
	defer cancel()

	req, err := s.t.newRequest(ctx, http.MethodPost, path, values, in)
	if err != nil {
		return nil, err
	}
	resp, err := s.t.doStream(req)
	if err != nil {
		return nil, err
	}
	if err := CheckResponse(resp); err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var res ExecResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, fmt.Errorf("decode exec result: %w", err)
	}
	return &res, nil
}

// ── Attach ────────────────────────────────────────────────────────

// AttachProto is the Upgrade token that opens an attach stream.
const AttachProto = "fuse-attach/1"

// Frame types on an attach stream. Stdin and Resize travel client to server;
// Stdout, Stderr, and Exit travel server to client.
const (
	FrameStdin  byte = 0
	FrameStdout byte = 1
	FrameStderr byte = 2
	FrameResize byte = 3
	FrameExit   byte = 4
)

// frameHeaderLen is type(1) + reserved(3) + length(4, big-endian).
const frameHeaderLen = 8

// maxFramePayload caps a single frame so a bogus length on the wire cannot make
// us allocate unbounded memory.
const maxFramePayload = 1 << 20

// AttachOptions describes the process to attach to.
type AttachOptions struct {
	// Cmd is the argv to run. Empty means the guest's login shell.
	Cmd []string

	// Rows and Cols seed the pty's window size. Send Resize frames afterwards
	// as the terminal changes.
	Rows uint16
	Cols uint16
}

// AttachStream is a live duplex connection to a process inside a guest.
//
// Writes are serialized internally. They have to be: a terminal-resize handler
// fires on a signal and would otherwise interleave a resize frame into the
// middle of a stdin frame being written by another goroutine.
//
// Reads are not serialized; a single reader is the expected shape.
type AttachStream struct {
	conn net.Conn
	r    *bufio.Reader

	wmu sync.Mutex
}

// Frame is one decoded message from an attach stream.
type Frame struct {
	Type    byte
	Payload []byte
}

// ExitPayload is the body of a FrameExit frame.
type ExitPayload struct {
	ExitCode int `json:"exit_code"`
}

// Attach opens an interactive session to a process inside a running
// environment's guest and returns the raw framed stream.
//
// The server requires a tty, so this is for interactive use. For a
// non-interactive command, use Exec.
//
// Attach requires the master token. The caller owns the stream and must Close
// it.
func (s *EnvironmentsService) Attach(ctx context.Context, vmID string, opts AttachOptions) (*AttachStream, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("environments service is not configured")
	}
	if vmID == "" {
		return nil, errors.New("vm id is required")
	}

	q := url.Values{}
	q.Set("tty", "1")
	if opts.Rows > 0 {
		q.Set("rows", strconv.Itoa(int(opts.Rows)))
	}
	if opts.Cols > 0 {
		q.Set("cols", strconv.Itoa(int(opts.Cols)))
	}
	// Repeated cmd params preserve argv boundaries, so an argument containing
	// spaces survives the round trip without a quoting convention.
	for _, arg := range opts.Cmd {
		q.Add("cmd", arg)
	}

	path := "/v1/environments/" + url.PathEscape(vmID) + "/attach"
	conn, br, err := s.t.dialUpgrade(ctx, path, q, AttachProto, "attach")
	if err != nil {
		return nil, err
	}
	return &AttachStream{conn: conn, r: br}, nil
}

// dialUpgrade performs an HTTP/1.1 upgrade by hand and hands back the socket,
// with reads going through the bufio.Reader that consumed the response head.
//
// It cannot go through http.Client for two structural reasons: net/http gives a
// client no way to reclaim the connection after a response (only servers get
// Hijack), and an http.Client over TLS may negotiate HTTP/2, which has no
// connection upgrade at all. Writing the request onto a raw conn pins us to
// HTTP/1.1, where the upgrade is well defined.
func (t *transport) dialUpgrade(ctx context.Context, path string, query url.Values, proto, label string) (net.Conn, *bufio.Reader, error) {
	if t == nil || t.baseURL == nil {
		return nil, nil, errors.New("transport is nil")
	}

	u := *t.baseURL
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawQuery = query.Encode()

	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "https" {
			host = net.JoinHostPort(host, "443")
		} else {
			host = net.JoinHostPort(host, "80")
		}
	}

	var (
		conn net.Conn
		err  error
	)
	if u.Scheme == "https" {
		d := &tls.Dialer{Config: &tls.Config{ServerName: u.Hostname()}}
		conn, err = d.DialContext(ctx, "tcp", host)
	} else {
		d := &net.Dialer{}
		conn, err = d.DialContext(ctx, "tcp", host)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%s: dial: %w", label, err)
	}

	// Bound the handshake, then hand back a deadline-free socket: an
	// interactive session idles for as long as a human stares at a prompt.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", proto)
	if t.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+t.bearer)
	}
	if t.userAgent != "" {
		req.Header.Set("User-Agent", t.userAgent)
	}
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("%s: write upgrade request: %w", label, err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("%s: read upgrade response: %w", label, err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		// The server answered with a normal HTTP error, so surface it in the
		// same shape every other call in this SDK does.
		err := CheckResponse(resp)
		_ = conn.Close()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("%s: unexpected status %d", label, resp.StatusCode)
	}

	_ = conn.SetDeadline(time.Time{})

	// Reads go through br: ReadResponse may have pulled stream bytes into its
	// buffer along with the response head, and reading the socket directly
	// would drop them.
	return conn, br, nil
}

// ReadFrame returns the next frame from the stream. It returns io.EOF when the
// session ends.
func (s *AttachStream) ReadFrame() (Frame, error) {
	var head [frameHeaderLen]byte
	if _, err := io.ReadFull(s.r, head[:]); err != nil {
		return Frame{}, err
	}

	length := binary.BigEndian.Uint32(head[4:8])
	if length > maxFramePayload {
		return Frame{}, fmt.Errorf("attach: frame payload too large: %d", length)
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(s.r, payload); err != nil {
		return Frame{}, err
	}
	return Frame{Type: head[0], Payload: payload}, nil
}

// WriteFrame writes one frame to the stream.
func (s *AttachStream) WriteFrame(t byte, payload []byte) error {
	if len(payload) > maxFramePayload {
		return fmt.Errorf("attach: frame payload too large: %d", len(payload))
	}

	buf := make([]byte, frameHeaderLen+len(payload))
	buf[0] = t
	binary.BigEndian.PutUint32(buf[4:8], uint32(len(payload)))
	copy(buf[frameHeaderLen:], payload)

	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.conn.Write(buf)
	return err
}

// Write sends bytes as a stdin frame, so an AttachStream can be used as the
// destination of an io.Copy from the local terminal.
func (s *AttachStream) Write(p []byte) (int, error) {
	// Payloads larger than one frame are split rather than rejected: a caller
	// piping a file has no reason to know our frame size.
	written := 0
	for len(p) > 0 {
		n := len(p)
		if n > maxFramePayload {
			n = maxFramePayload
		}
		if err := s.WriteFrame(FrameStdin, p[:n]); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

// Resize tells the guest its terminal changed size.
func (s *AttachStream) Resize(rows, cols uint16) error {
	payload, err := json.Marshal(map[string]uint16{"rows": rows, "cols": cols})
	if err != nil {
		return err
	}
	return s.WriteFrame(FrameResize, payload)
}

// Close tears down the session.
func (s *AttachStream) Close() error { return s.conn.Close() }
