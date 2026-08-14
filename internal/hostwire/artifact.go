package hostwire

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// Peer-to-peer artifact transfer.
//
// Two very different calls live here and must not be conflated:
//
//   - PullArtifact is orchestrator to host: a small authenticated JSON POST
//     telling host B to go fetch a blob. It uses NewClient, because it is an
//     ordinary control-plane call with an ordinary control-plane deadline.
//   - FetchArtifact is the blob transfer itself: many gigabytes, no bearer
//     token, authorized by a grant. It uses NewArtifactClient, whose timeout
//     shape is the opposite of NewClient's.
//
// The Go FetchArtifact is a second implementation of what the Python agent
// does on the pulling side. Keeping it here means the wire contract (header
// name, path shape, verify-then-commit ordering) is exercised by tests in this
// repo rather than only in production.

// Path shapes, in one place so the Go client and any future caller cannot
// drift from each other. Digests are validated as lowercase hex before use, so
// there is nothing here that needs URL escaping.
const (
	artifactPathPrefix = "/v1/artifacts/"
	artifactPullSuffix = "/pull"
)

// Timeouts for a blob transfer.
//
// There is deliberately NO overall request timeout. NewClient's 10 minute
// ceiling is right for control-plane calls, whose duration is bounded by what
// the agent has to do; it is wrong here, because transfer time is a function
// of artifact size and link speed, neither of which this side knows. A total
// deadline would kill a perfectly healthy 40 GiB pull over a slow link, and
// the only way to pick one that never does is to pick one so large it stops
// detecting anything. Any fixed number is either too small for the biggest
// legitimate transfer or too large to be a useful failure detector.
//
// So instead of bounding the whole thing, bound the parts whose duration does
// NOT scale with size (connect, TLS, and time to first byte), and then require
// PROGRESS on the body via a stall timeout. A transfer that is moving is
// allowed to take as long as it takes; a transfer that has stopped moving
// fails quickly, which is the condition anyone actually wanted to detect.
const (
	artifactDialTimeout           = 10 * time.Second
	artifactTLSHandshakeTimeout   = 10 * time.Second
	artifactResponseHeaderTimeout = 2 * time.Minute

	// DefaultArtifactStallTimeout is the maximum time the transfer may make no
	// progress at all before it is abandoned. A healthy link, however slow,
	// still delivers something inside this window; two minutes of complete
	// silence means the peer or the path is gone.
	DefaultArtifactStallTimeout = 2 * time.Minute
)

// DefaultMaxArtifactBytes caps how much a peer may stream before the pull is
// abandoned. Without a cap, a hostile or broken peer can serve an endless body
// and fill the puller's disk, and the digest check is no defence because it
// only fails once the stream ends.
//
// 128 GiB is well above any plausible rootfs artifact (the largest environment
// disks in the fleet are an order of magnitude smaller) while still being a
// number a disk can survive. Callers with tighter knowledge of the artifact,
// for instance from a size recorded at snapshot time, should pass a smaller
// value.
const DefaultMaxArtifactBytes int64 = 128 << 30

// Blob transfer failures. These are separate sentinels because they mean
// genuinely different things to a caller: a digest mismatch is corruption or
// an attack and must never be retried against the same peer blindly, a size
// overrun is a policy stop, and a stall is a transport problem worth retrying.
var (
	ErrArtifactTooLarge       = errors.New("artifact exceeds the maximum allowed size")
	ErrArtifactDigestMismatch = errors.New("artifact digest does not match")
	ErrArtifactStalled        = errors.New("artifact transfer stalled")
)

// NewArtifactClient returns the HTTP client used for the blob transfer hop.
//
// It is a separate client from NewClient rather than a tuning of it because
// the two want opposite things: NewClient exists to guarantee no call runs
// long, and this one exists to allow exactly that for a single endpoint while
// still failing fast on a peer that has stopped talking. See the timeout
// constants above for why a total deadline is the wrong tool.
//
// Redirects are refused for the same reason NewClient refuses them, with one
// addition: this request carries a grant that authorizes reading one blob from
// one host, and following a redirect would present that grant to a host the
// orchestrator never chose.
func NewArtifactClient() *http.Client {
	return &http.Client{
		// No Timeout on purpose. See above.
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			return fmt.Errorf("%w: %s", ErrRedirectNotAllowed, req.URL.Redacted())
		},
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   artifactDialTimeout,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   artifactTLSHandshakeTimeout,
			ResponseHeaderTimeout: artifactResponseHeaderTimeout,
			IdleConnTimeout:       agentIdleConnTimeout,
			// A pull is one big stream, not a burst of small calls, so there
			// is nothing to gain from a pool.
			MaxIdleConnsPerHost: 1,
		},
	}
}

// ArtifactFetchOptions describes one blob transfer.
type ArtifactFetchOptions struct {
	// PeerBaseURL is the serving host agent's base URL, in the same form as
	// any other host agent URL (see ValidateAgentURL).
	PeerBaseURL string

	// Digest is the expected hex sha256 of the artifact. It is both the thing
	// being asked for and the thing being checked: content addressing means
	// the request and the integrity check are the same value.
	Digest string

	// Grant is the orchestrator-minted capability, sent in ArtifactGrantHeader.
	Grant string

	// DestPath is where a VERIFIED artifact lands. Nothing is ever written to
	// this path directly; see FetchArtifact.
	DestPath string

	// MaxBytes overrides DefaultMaxArtifactBytes when positive.
	MaxBytes int64

	// StallTimeout overrides DefaultArtifactStallTimeout when positive.
	StallTimeout time.Duration
}

// ArtifactFetchResult describes a transfer that completed AND verified.
type ArtifactFetchResult struct {
	Path      string
	SizeBytes int64
}

// FetchArtifact streams one artifact from a peer host agent, verifying it as
// the bytes arrive, and only then places it at DestPath.
//
// The API takes a destination path rather than an io.Writer on purpose. A
// writer would hand the caller every byte as it arrived, and the caller would
// then be responsible for not using those bytes until this function returned,
// which is precisely the mistake that turns "we verify artifacts" into "we
// verify artifacts, usually". Here, unverified bytes only ever exist in a
// temp file this function owns, and the single moment the data becomes visible
// under DestPath is after the digest matched. A caller cannot use the artifact
// early because until success there is nothing at DestPath to use.
//
// The commit is a fresh temp inode plus rename, matching the pattern the
// firecracker host agent uses on restore. That pattern exists there because
// writing into a path something else may already hold open produced corrupt
// (NUL-filled) images; the same hazard applies to an artifact another pull or
// a running VM may be reading.
//
// On any failure the temp file is removed, so a failed pull leaves nothing
// behind and nothing partial is ever reachable at DestPath.
func FetchArtifact(ctx context.Context, client *http.Client, opts ArtifactFetchOptions) (ArtifactFetchResult, error) {
	if client == nil {
		return ArtifactFetchResult{}, errors.New("artifact fetch: nil client")
	}
	if !validArtifactDigest(opts.Digest) {
		return ArtifactFetchResult{}, fmt.Errorf("artifact fetch: %q is not a sha256 digest", opts.Digest)
	}
	if opts.Grant == "" {
		return ArtifactFetchResult{}, errors.New("artifact fetch: grant is required")
	}
	if opts.DestPath == "" {
		return ArtifactFetchResult{}, errors.New("artifact fetch: destination path is required")
	}
	if err := ValidateAgentURL(opts.PeerBaseURL); err != nil {
		return ArtifactFetchResult{}, fmt.Errorf("artifact fetch: peer %w", err)
	}

	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxArtifactBytes
	}
	stall := opts.StallTimeout
	if stall <= 0 {
		stall = DefaultArtifactStallTimeout
	}

	// The request is built on a context this function owns so the stall
	// watchdog can abort it. Cancelling a context the request was NOT built
	// with does nothing to a read already blocked inside the transport, which
	// is exactly the case the watchdog exists for.
	ctx, abort := context.WithCancel(ctx)
	defer abort()

	endpoint := strings.TrimRight(opts.PeerBaseURL, "/") + artifactPathPrefix + opts.Digest
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ArtifactFetchResult{}, fmt.Errorf("artifact fetch: %w", err)
	}
	// The grant, and only the grant. A peer is not the orchestrator and must
	// never be handed a bearer token for anything.
	req.Header.Set(ArtifactGrantHeader, opts.Grant)

	res, err := client.Do(req)
	if err != nil {
		return ArtifactFetchResult{}, fmt.Errorf("artifact fetch: %w", err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode >= 300 {
		return ArtifactFetchResult{}, fmt.Errorf("artifact fetch: %w",
			&orchestrator.HTTPStatusError{Code: res.StatusCode, Body: ReadErrorBody(res.Body)})
	}
	// A declared length over the cap is knowable before a single body byte is
	// read, so refuse then rather than after writing the cap to disk.
	if res.ContentLength > maxBytes {
		return ArtifactFetchResult{}, fmt.Errorf("artifact fetch: %w: peer declared %d bytes, limit %d",
			ErrArtifactTooLarge, res.ContentLength, maxBytes)
	}

	size, err := streamVerified(ctx, abort, res.Body, opts.DestPath, opts.Digest, maxBytes, stall)
	if err != nil {
		return ArtifactFetchResult{}, err
	}
	return ArtifactFetchResult{Path: opts.DestPath, SizeBytes: size}, nil
}

// streamVerified writes body to a temp file next to dest, hashing as it goes,
// and renames into place only if the hash matches wantDigest.
//
// The temp file is created in dest's directory because rename is only atomic
// (and only works at all) within a filesystem; a temp elsewhere would degrade
// into a copy, which reintroduces exactly the partially-visible-file problem
// the rename is there to avoid.
//
// abort must cancel the context the in-flight request was built with; that is
// the only thing that unblocks a read stuck in the transport.
func streamVerified(ctx context.Context, abort context.CancelFunc, body io.Reader, dest, wantDigest string, maxBytes int64, stall time.Duration) (int64, error) {
	dir := filepath.Dir(dest)
	tmp, err := os.CreateTemp(dir, filepath.Base(dest)+".partial-*")
	if err != nil {
		return 0, fmt.Errorf("artifact fetch: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			// A failed pull must leave nothing behind: no partial file to be
			// mistaken for an artifact, and no disk held by a dead transfer.
			_ = os.Remove(tmpName)
		}
	}()

	// The watchdog aborts the request when the peer goes silent. stalled is
	// what lets the error path tell "we gave up on a dead peer" apart from
	// "our caller cancelled", since both surface as a cancelled read.
	var stalled atomic.Bool
	watchdog := time.AfterFunc(stall, func() {
		stalled.Store(true)
		abort()
	})
	defer watchdog.Stop()

	hash := sha256.New()
	// max+1 so an over-long body is detected by reading one byte past the cap
	// rather than by trusting a length the peer supplied.
	limited := io.LimitReader(&stallReader{r: body, watchdog: watchdog, every: stall, ctx: ctx}, maxBytes+1)

	written, err := io.Copy(io.MultiWriter(tmp, hash), limited)
	if err != nil {
		if stalled.Load() {
			return 0, fmt.Errorf("artifact fetch: %w after %s with no data (%d bytes read)", ErrArtifactStalled, stall, written)
		}
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, context.Canceled) {
			return 0, fmt.Errorf("artifact fetch: %w", ctxErr)
		}
		// A short read lands here (io.ErrUnexpectedEOF when the peer declared
		// a Content-Length it did not deliver). It is not distinguished from
		// any other transport error because the response is the same: nothing
		// is committed.
		return 0, fmt.Errorf("artifact fetch: read body: %w", err)
	}
	if written > maxBytes {
		return 0, fmt.Errorf("artifact fetch: %w: peer sent more than %d bytes", ErrArtifactTooLarge, maxBytes)
	}

	got := hex.EncodeToString(hash.Sum(nil))
	// Not a constant-time comparison, and it does not need to be: both sides
	// are public content addresses. Note also that this digest is an INTEGRITY
	// check on these bytes, never a cross-build identity; two builds of the
	// same recipe produce different rootfs bytes.
	if got != wantDigest {
		return 0, fmt.Errorf("artifact fetch: %w: want %s, got %s", ErrArtifactDigestMismatch, wantDigest, got)
	}

	// Flush before the rename. Without it a crash can leave the name published
	// with unwritten contents, and a truncated file under a verified digest is
	// worse than no file: nothing would ever check it again.
	if err := tmp.Sync(); err != nil {
		return 0, fmt.Errorf("artifact fetch: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return 0, fmt.Errorf("artifact fetch: close: %w", err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return 0, fmt.Errorf("artifact fetch: commit: %w", err)
	}
	committed = true
	return written, nil
}

// stallReader enforces progress rather than a deadline: the watchdog is pushed
// forward on every read that returns data, so a slow transfer never trips it
// while a silent one trips it in bounded time.
type stallReader struct {
	r        io.Reader
	watchdog *time.Timer
	every    time.Duration
	ctx      context.Context
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.watchdog.Reset(s.every)
	}
	if err == nil {
		// Cheap cancellation check so a caller who cancels mid-transfer stops
		// at the next chunk boundary even if the transport has buffered data
		// and would otherwise keep satisfying reads.
		if ctxErr := s.ctx.Err(); ctxErr != nil {
			return n, ctxErr
		}
	}
	return n, err
}

// ArtifactPullRequest is the body of POST /v1/artifacts/{digest}/pull, the
// call the orchestrator makes to tell a host to fetch an artifact from a peer.
//
// Keeping the shape in one struct means renaming a field is one edit here and
// one in the agent, rather than a search across every call site.
type ArtifactPullRequest struct {
	// Digest travels in the URL path, not the body, so the agent can route and
	// authorize on it without parsing a body. It is here so callers pass one
	// value object.
	Digest string `json:"-"`

	// PeerURL is the base URL of the host agent that already has the artifact.
	PeerURL string `json:"peer_url"`

	// Grant authorizes exactly this digest on exactly that peer. It is minted
	// with the PEER's agent token; see MintArtifactGrant.
	Grant string `json:"grant"`

	// SnapshotID is the local id the pulled artifact is stored under. It is
	// optional: the agent derives one from the digest when it is empty. The
	// orchestrator should set it, because the id is what its own snapshot
	// records key on and letting the agent invent one means the control plane
	// has to read it back to know what it now has.
	SnapshotID string `json:"snapshot_id,omitempty"`
}

// ArtifactPullResponse is what the pulling agent answers once the artifact is
// verified and committed. It is the agent's snapshot record, so the field
// names follow the agent's meta.json rather than being invented here.
type ArtifactPullResponse struct {
	SnapshotID string `json:"snapshot_id"`
	Digest     string `json:"digest"`
	SizeBytes  int64  `json:"bytes"`
	CreatedAt  string `json:"created_at,omitempty"`
	// SourcePeer is the peer the bytes came from. Provenance matters here in
	// a way it does not for a locally built snapshot: there is no origin VM on
	// this host to point at.
	SourcePeer string `json:"source_peer,omitempty"`
}

// PullArtifact asks the host agent at baseURL to pull an artifact from a peer.
//
// This is an ordinary control-plane call: small body, the TARGET host's own
// bearer token, and NewClient's timeouts. It is not the transfer, and must not
// be given the artifact client: the request returns when the agent has done
// the work it is going to do, and if that work is a long blocking transfer
// then the ceiling belongs on the agent side, not on an orchestrator call that
// would otherwise leak a goroutine per pull with no bound at all.
func PullArtifact(ctx context.Context, client *http.Client, baseURL, token string, req ArtifactPullRequest) (ArtifactPullResponse, error) {
	if client == nil {
		return ArtifactPullResponse{}, errors.New("artifact pull: nil client")
	}
	if !validArtifactDigest(req.Digest) {
		return ArtifactPullResponse{}, fmt.Errorf("artifact pull: %q is not a sha256 digest", req.Digest)
	}
	if req.PeerURL == "" || req.Grant == "" {
		return ArtifactPullResponse{}, errors.New("artifact pull: peer url and grant are required")
	}

	body, err := json.Marshal(req)
	if err != nil {
		return ArtifactPullResponse{}, fmt.Errorf("artifact pull: %w", err)
	}

	endpoint := strings.TrimRight(baseURL, "/") + artifactPathPrefix + req.Digest + artifactPullSuffix
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return ArtifactPullResponse{}, fmt.Errorf("artifact pull: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}

	res, err := client.Do(httpReq)
	if err != nil {
		return ArtifactPullResponse{}, fmt.Errorf("artifact pull: %w", err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode >= 300 {
		return ArtifactPullResponse{}, fmt.Errorf("artifact pull: %w",
			&orchestrator.HTTPStatusError{Code: res.StatusCode, Body: ReadErrorBody(res.Body)})
	}

	var out ArtifactPullResponse
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return ArtifactPullResponse{}, fmt.Errorf("artifact pull: decode response: %w", err)
	}
	return out, nil
}

// validArtifactDigest reports whether s is a lowercase hex sha256.
//
// The shape is enforced everywhere a digest enters this package because it is
// concatenated into a URL path and into the dot-separated grant format; a
// digest containing a dot or a slash would silently change the meaning of
// both. Rejecting anything that is not 64 lowercase hex characters removes
// that class of problem rather than escaping around it.
func validArtifactDigest(s string) bool {
	return len(s) == hex.EncodedLen(sha256.Size) && isLowerHex(s)
}
