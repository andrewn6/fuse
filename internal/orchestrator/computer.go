package orchestrator

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// The computer proxy: the control-plane half of the guest's
// /v1/computer/* surface. A caller authenticates with their Fuse API key and
// the fleet relays the action to the guest agent with the per-VM guest token,
// so the guest token never leaves the control plane.
//
// This is deliberately the first thing in the orchestrator to make outbound
// HTTP to a sandbox. Every prior guest interaction goes orchestrator -> host
// agent -> SSH, which is fine for a file read on a reconcile loop but adds a
// full SSH exec to every mouse click; the computer surface exists to be
// called in a tight loop with a screenshot riding back on each response, so
// it dials the same per-VM DNAT authority (env.URL) callers are already
// handed, with the guest bearer token (env.Token) that until now had no
// in-tree consumer.

// maxGuestComputerBody bounds a guest response body. A full 4k screenshot is
// ~25MB of PNG, ~34MB as base64 in its JSON envelope; 64MB leaves headroom
// without letting a broken guest stream unbounded bytes into fleet memory.
const maxGuestComputerBody = 64 << 20

// guestComputerTimeout bounds one proxied call. The slowest legitimate action
// is hold_key/wait at their 100s guest-side cap plus capture time.
const guestComputerTimeout = 150 * time.Second

// GuestHTTPResponse is a relayed guest answer: the status code and body as
// the guest agent produced them. The API layer owns translating non-2xx
// bodies into its error envelope; the fleet does not reinterpret them.
type GuestHTTPResponse struct {
	Status int
	Body   []byte
}

// guestComputerClient dials guest agents through the per-VM DNAT.
//
// TLS verification is skipped by design, not by accident: the guest cert is
// self-signed with loopback-only SANs (internal/secrets/crypto.go), so
// verification against the DNAT authority proves nothing, and the real
// authentication on this path is the per-VM bearer token in both directions —
// the caller's API key upstream, the guest token downstream. Redirects are
// refused for the same reason the host-agent client refuses them: the request
// carries a bearer token and a guest has no reason to redirect.
var guestComputerClient = &http.Client{
	Timeout: guestComputerTimeout,
	CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		return fmt.Errorf("guest agent redirects are not followed: %s", req.URL.Redacted())
	},
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true}, // #nosec G402 -- see comment above
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: guestComputerTimeout,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   4,
	},
}

// ComputerAction relays one computer action to the guest agent of a running
// VM. The body is passed through verbatim rather than re-modelled: the action
// schema is owned by the guest agent, which is baked into the image and
// versioned separately from the control plane, and a proxy that re-encoded
// the request would silently drop fields it had not learned yet.
func (fm *FleetManager) ComputerAction(ctx context.Context, vmID string, action []byte) (GuestHTTPResponse, error) {
	return fm.guestComputer(ctx, vmID, http.MethodPost, "/v1/computer/action", action)
}

// ComputerDisplay relays the guest's display report: geometry and whether the
// display is up.
func (fm *FleetManager) ComputerDisplay(ctx context.Context, vmID string) (GuestHTTPResponse, error) {
	return fm.guestComputer(ctx, vmID, http.MethodGet, "/v1/computer/display", nil)
}

func (fm *FleetManager) guestComputer(ctx context.Context, vmID, method, path string, body []byte) (GuestHTTPResponse, error) {
	env, err := fm.guestEnvironment(ctx, vmID)
	if err != nil {
		return GuestHTTPResponse{}, err
	}

	// env.URL is a bare host:port authority on real providers; the in-memory
	// stub and a host agent that answered without one report a fc:// (or
	// otherwise schemed) placeholder, which is not dialable and means there
	// is no guest agent behind this environment.
	authority := env.URL()
	if authority == "" || strings.Contains(authority, "://") {
		return GuestHTTPResponse{}, fmt.Errorf("%w: vm %s has no dialable guest agent address", ErrComputerUnsupported, vmID)
	}

	// driving the desktop is activity: a computer-use loop is exactly the
	// kind of caller the idle reaper must not shoot mid-session.
	fm.touchActivity(vmID)

	// TLS on the guest is on iff per-VM credentials were minted, which is
	// also exactly when a guest token exists; fused makes the same decision
	// from the same facts (fused/main.go), so the two sides cannot disagree.
	token := env.Token()
	scheme := "http"
	if token != "" {
		scheme = "https"
	}

	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, scheme+"://"+authority+path, reqBody)
	if err != nil {
		return GuestHTTPResponse{}, fmt.Errorf("computer request for vm %s: %w", vmID, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := guestComputerClient.Do(req)
	if err != nil {
		return GuestHTTPResponse{}, fmt.Errorf("computer call to vm %s: %w", vmID, err)
	}
	defer func() { _ = resp.Body.Close() }()

	b, err := io.ReadAll(io.LimitReader(resp.Body, maxGuestComputerBody))
	if err != nil {
		return GuestHTTPResponse{}, fmt.Errorf("read computer response from vm %s: %w", vmID, err)
	}
	return GuestHTTPResponse{Status: resp.StatusCode, Body: b}, nil
}
