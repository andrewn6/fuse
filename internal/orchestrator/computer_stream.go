package orchestrator

// The computer stream: the live half of the computer surface. Where
// ComputerAction relays one JSON call, ComputerStream opens a raw duplex
// byte stream to the guest agent's /v1/computer/stream route and hands it
// back for the API layer to relay. The bytes are RFB (VNC); the fleet does
// not interpret them.

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// VNCProto is the value of the Upgrade header that opens a computer stream.
// It is spoken on both hops — client to orchestrator, and orchestrator to
// guest agent — so the orchestrator can relay bytes without reframing them.
const VNCProto = "fuse-vnc/1"

// streamHandshakeTimeout bounds the upgrade exchange itself, not the stream
// that follows, which lives as long as the viewer does.
const streamHandshakeTimeout = 15 * time.Second

// ComputerStream opens a live desktop stream to the guest agent of a running
// VM. Auth follows the ComputerAction model: the caller authenticated
// upstream, the fleet attaches the per-VM guest token downstream, and the
// token never leaves the control plane.
//
// The caller owns the returned stream and must Close it. Like attach, an
// open stream counts as activity for as long as it is open: a human watching
// a quiet desktop produces zero bytes and is still using the environment.
func (fm *FleetManager) ComputerStream(ctx context.Context, vmID string) (io.ReadWriteCloser, error) {
	env, err := fm.guestEnvironment(ctx, vmID)
	if err != nil {
		return nil, err
	}

	authority := env.URL()
	if authority == "" || strings.Contains(authority, "://") {
		return nil, fmt.Errorf("%w: vm %s has no dialable guest agent address", ErrComputerUnsupported, vmID)
	}

	conn, err := dialGuestStream(ctx, authority, env.Token())
	if err != nil {
		return nil, fmt.Errorf("computer stream to vm %s: %w", vmID, err)
	}

	fm.openAttachSession(vmID)
	return &activityStream{ReadWriteCloser: conn, release: func() { fm.closeAttachSession(vmID) }}, nil
}

// dialGuestStream performs the HTTP/1.1 upgrade against the guest agent by
// hand and returns the socket once the guest has answered 101.
//
// This is a sibling of hostwire.Dial, which this package cannot import
// without a cycle, pointed at the guest instead of the host agent. The same
// two structural reasons apply for bypassing http.Client: only servers get
// Hijack, and TLS via http.Client may negotiate HTTP/2, which has no
// connection upgrade. TLS is used iff a guest token exists — the same
// decision guestComputerClient and fused itself make from the same facts —
// and verification is skipped for the reason documented on
// guestComputerClient: the guest cert is self-signed with loopback-only
// SANs, and the bearer tokens are the real authentication on this path.
func dialGuestStream(ctx context.Context, authority, token string) (net.Conn, error) {
	netDialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}

	var c net.Conn
	var err error
	scheme := "http"
	if token != "" {
		scheme = "https"
		d := &tls.Dialer{
			NetDialer: netDialer,
			Config:    &tls.Config{InsecureSkipVerify: true}, // #nosec G402 -- see comment above
		}
		c, err = d.DialContext(ctx, "tcp", authority)
	} else {
		c, err = netDialer.DialContext(ctx, "tcp", authority)
	}
	if err != nil {
		return nil, fmt.Errorf("dial guest agent: %w", err)
	}

	// bound the handshake, then hand a deadline-free socket to the caller:
	// an idle desktop produces zero RFB traffic and must not be cut off.
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(deadline)
	} else {
		_ = c.SetDeadline(time.Now().Add(streamHandshakeTimeout))
	}

	req := &http.Request{
		Method: http.MethodGet,
		URL:    &url.URL{Scheme: scheme, Host: authority, Path: "/v1/computer/stream"},
		Host:   authority,
		Header: http.Header{
			"Connection": {"Upgrade"},
			"Upgrade":    {VNCProto},
		},
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if err := req.Write(c); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("write upgrade request: %w", err)
	}

	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("read upgrade response: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = c.Close()
		msg := guestStreamError(body)
		// the guest's 503 is "running, but nothing to stream" — no display,
		// or the vnc unit is down — which callers should see as unavailable,
		// not as a control-plane fault.
		if resp.StatusCode == http.StatusServiceUnavailable {
			return nil, fmt.Errorf("%w: %s", ErrGuestUnavailable, msg)
		}
		return nil, fmt.Errorf("guest agent refused upgrade: http %d: %s", resp.StatusCode, msg)
	}

	_ = c.SetDeadline(time.Time{})

	// http.ReadResponse may have pulled stream bytes into br's buffer along
	// with the response head; reads must go through br or they are dropped.
	return &guestStreamConn{Conn: c, r: br}, nil
}

// guestStreamError extracts the {"error": "..."} the guest agent answers
// errors with, falling back to the raw body.
func guestStreamError(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err == nil && e.Error != "" {
		return e.Error
	}
	return strings.TrimSpace(string(body))
}

// guestStreamConn reads through the bufio.Reader that consumed the 101
// response while writing straight to the socket.
type guestStreamConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *guestStreamConn) Read(p []byte) (int, error) { return c.r.Read(p) }
