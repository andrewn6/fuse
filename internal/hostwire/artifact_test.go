package hostwire

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// artifactDir returns a temp dir and a destination path inside it, plus a
// helper that asserts nothing but the expected files are left behind. A failed
// pull leaving a *.partial-* file around is a real bug (it fills the disk and
// can be mistaken for an artifact), so every failure test checks for it.
func artifactDir(t *testing.T) (dest string, assertOnly func(names ...string)) {
	t.Helper()
	dir := t.TempDir()
	return filepath.Join(dir, "artifact.ext4"), func(names ...string) {
		t.Helper()
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]string, 0, len(entries))
		for _, e := range entries {
			got = append(got, e.Name())
		}
		if len(got) != len(names) {
			t.Fatalf("directory contents: got %v, want %v", got, names)
		}
		for i, name := range names {
			if got[i] != name {
				t.Fatalf("directory contents: got %v, want %v", got, names)
			}
		}
	}
}

func TestFetchArtifact_VerifiesAndCommits(t *testing.T) {
	payload := make([]byte, 512<<10)
	for i := range payload {
		payload[i] = byte(i)
	}
	digest := digestOf(payload)

	var gotGrant, gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotGrant = r.Header.Get(ArtifactGrantHeader)
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	dest, assertOnly := artifactDir(t)
	res, err := FetchArtifact(context.Background(), NewArtifactClient(), ArtifactFetchOptions{
		PeerBaseURL: srv.URL,
		Digest:      digest,
		Grant:       "v1.grant-goes-here",
		DestPath:    dest,
	})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	if res.SizeBytes != int64(len(payload)) {
		t.Errorf("size: got %d, want %d", res.SizeBytes, len(payload))
	}
	if res.Path != dest {
		t.Errorf("path: got %q, want %q", res.Path, dest)
	}
	if gotGrant != "v1.grant-goes-here" {
		t.Errorf("grant header: got %q", gotGrant)
	}
	// The whole point of the grant scheme: a peer never sees a bearer token.
	if gotAuth != "" {
		t.Errorf("peer received an Authorization header: %q", gotAuth)
	}
	if want := "/v1/artifacts/" + digest; gotPath != want {
		t.Errorf("path: got %q, want %q", gotPath, want)
	}

	onDisk, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if digestOf(onDisk) != digest {
		t.Error("committed file does not match the requested digest")
	}
	assertOnly("artifact.ext4")
}

func TestFetchArtifact_RejectsWrongDigest(t *testing.T) {
	payload := []byte("these are not the bytes you asked for")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	dest, assertOnly := artifactDir(t)
	_, err := FetchArtifact(context.Background(), NewArtifactClient(), ArtifactFetchOptions{
		PeerBaseURL: srv.URL,
		Digest:      testDigest, // the digest of the empty string, not of payload
		Grant:       "v1.grant",
		DestPath:    dest,
	})
	if !errors.Is(err, ErrArtifactDigestMismatch) {
		t.Fatalf("error: got %v, want ErrArtifactDigestMismatch", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatal("unverified bytes were committed to the destination path")
	}
	assertOnly()
}

func TestFetchArtifact_EnforcesMaxBytes(t *testing.T) {
	// Chunked, so the cap has to be enforced on the stream itself rather than
	// on a Content-Length the peer could simply lie about.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := make([]byte, 4096)
		for i := 0; i < 8; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()

	dest, assertOnly := artifactDir(t)
	_, err := FetchArtifact(context.Background(), NewArtifactClient(), ArtifactFetchOptions{
		PeerBaseURL: srv.URL,
		Digest:      testDigest,
		Grant:       "v1.grant",
		DestPath:    dest,
		MaxBytes:    4096,
	})
	if !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("error: got %v, want ErrArtifactTooLarge", err)
	}
	assertOnly()
}

func TestFetchArtifact_RejectsOversizedContentLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(1<<20))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dest, assertOnly := artifactDir(t)
	_, err := FetchArtifact(context.Background(), NewArtifactClient(), ArtifactFetchOptions{
		PeerBaseURL: srv.URL,
		Digest:      testDigest,
		Grant:       "v1.grant",
		DestPath:    dest,
		MaxBytes:    1024,
	})
	if !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("error: got %v, want ErrArtifactTooLarge", err)
	}
	// Nothing was created at all: the declared length was refused before the
	// first body byte, so there is not even a temp file.
	assertOnly()
}

func TestFetchArtifact_TruncatedStream(t *testing.T) {
	payload := make([]byte, 64<<10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Promise more than we deliver, then hang up.
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload[:1024])
	}))
	defer srv.Close()

	dest, assertOnly := artifactDir(t)
	_, err := FetchArtifact(context.Background(), NewArtifactClient(), ArtifactFetchOptions{
		PeerBaseURL: srv.URL,
		Digest:      digestOf(payload),
		Grant:       "v1.grant",
		DestPath:    dest,
	})
	// It must fail as a transport error, not as a digest mismatch: a peer that
	// hangs up mid-stream is a retryable transport problem, while a mismatch
	// means the bytes were wrong and the peer should not be trusted to repeat
	// them.
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("error: got %v, want io.ErrUnexpectedEOF", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatal("a truncated artifact was committed")
	}
	assertOnly()
}

// TestFetchArtifact_StalledPeer is deliberately timing-based but not
// timing-sensitive: the peer never sends another byte, so the only way the
// test can fail is if the stall detector does not work at all. The 200ms
// budget is three orders of magnitude below the server's own hold time, so
// scheduler jitter under -race cannot flip the result.
func TestFetchArtifact_StalledPeer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("first bytes arrive, then silence"))
		w.(http.Flusher).Flush()
		// Hold until the client gives up, with a backstop so a broken client
		// cannot wedge the test binary.
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	}))
	defer srv.Close()

	dest, assertOnly := artifactDir(t)
	start := time.Now()
	_, err := FetchArtifact(context.Background(), NewArtifactClient(), ArtifactFetchOptions{
		PeerBaseURL:  srv.URL,
		Digest:       testDigest,
		Grant:        "v1.grant",
		DestPath:     dest,
		StallTimeout: 200 * time.Millisecond,
	})
	if !errors.Is(err, ErrArtifactStalled) {
		t.Fatalf("error: got %v, want ErrArtifactStalled", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("stall detected after %s, far past the 200ms budget", elapsed)
	}
	assertOnly()
}

func TestFetchArtifact_HonoursContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello"))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	dest, assertOnly := artifactDir(t)
	_, err := FetchArtifact(ctx, NewArtifactClient(), ArtifactFetchOptions{
		PeerBaseURL: srv.URL,
		Digest:      testDigest,
		Grant:       "v1.grant",
		DestPath:    dest,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error: got %v, want context.Canceled", err)
	}
	assertOnly()
}

func TestFetchArtifact_PeerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()

	dest, assertOnly := artifactDir(t)
	_, err := FetchArtifact(context.Background(), NewArtifactClient(), ArtifactFetchOptions{
		PeerBaseURL: srv.URL,
		Digest:      testDigest,
		Grant:       "v1.expired-grant",
		DestPath:    dest,
	})

	var statusErr *orchestrator.HTTPStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("error: got %v, want *orchestrator.HTTPStatusError", err)
	}
	if statusErr.Code != http.StatusForbidden {
		t.Errorf("status: got %d, want 403", statusErr.Code)
	}
	assertOnly()
}

func TestFetchArtifact_RejectsBadOptions(t *testing.T) {
	dest, _ := artifactDir(t)
	base := ArtifactFetchOptions{
		PeerBaseURL: "http://10.0.0.1:8090",
		Digest:      testDigest,
		Grant:       "v1.grant",
		DestPath:    dest,
	}

	cases := map[string]func(o *ArtifactFetchOptions){
		"no digest":      func(o *ArtifactFetchOptions) { o.Digest = "" },
		"bad digest":     func(o *ArtifactFetchOptions) { o.Digest = "../../etc/passwd" },
		"no grant":       func(o *ArtifactFetchOptions) { o.Grant = "" },
		"no destination": func(o *ArtifactFetchOptions) { o.DestPath = "" },
		"no peer url":    func(o *ArtifactFetchOptions) { o.PeerBaseURL = "" },
		"bad peer url":   func(o *ArtifactFetchOptions) { o.PeerBaseURL = "file:///etc/passwd" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			opts := base
			mutate(&opts)
			if _, err := FetchArtifact(context.Background(), NewArtifactClient(), opts); err == nil {
				t.Fatal("options were accepted, want an error")
			}
		})
	}
}

// TestNewArtifactClient_TimeoutShape pins the deliberate asymmetry with
// NewClient: no total deadline, but every phase that does not scale with
// artifact size is bounded.
func TestNewArtifactClient_TimeoutShape(t *testing.T) {
	c := NewArtifactClient()
	if c.Timeout != 0 {
		t.Errorf("artifact client has an overall timeout of %s; a large transfer over a slow link would be killed", c.Timeout)
	}

	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport: got %T, want *http.Transport", c.Transport)
	}
	if tr.TLSHandshakeTimeout <= 0 {
		t.Error("transport has no TLS handshake timeout")
	}
	if tr.ResponseHeaderTimeout <= 0 {
		t.Error("transport has no response header timeout")
	}
	if tr.IdleConnTimeout <= 0 {
		t.Error("transport has no idle connection timeout")
	}
}

func TestNewArtifactClient_RefusesRedirects(t *testing.T) {
	var elsewhereHits int
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhereHits++
		if r.Header.Get(ArtifactGrantHeader) != "" {
			t.Error("redirect target received the artifact grant")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer elsewhere.Close()

	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusFound)
	}))
	defer peer.Close()

	dest, _ := artifactDir(t)
	_, err := FetchArtifact(context.Background(), NewArtifactClient(), ArtifactFetchOptions{
		PeerBaseURL: peer.URL,
		Digest:      testDigest,
		Grant:       "v1.grant",
		DestPath:    dest,
	})
	if !errors.Is(err, ErrRedirectNotAllowed) {
		t.Fatalf("error: got %v, want ErrRedirectNotAllowed", err)
	}
	if elsewhereHits != 0 {
		t.Fatalf("redirect target was contacted %d times", elsewhereHits)
	}
}

func TestPullArtifact(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotAuth   string
		gotBody   ArtifactPullRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)

		w.Header().Set("Content-Type", "application/json")
		// Written as the agent writes it, not via the struct, so a rename of a
		// json tag on this side fails the test instead of moving both halves.
		_, _ = w.Write([]byte(`{"snapshot_id":"art-abc","comment":"pulled from peer",` +
			`"created_at":"2026-08-13T00:00:00Z","origin_vm_id":"","digest":"` + testDigest + `",` +
			`"bytes":4096,"source_peer":"http://10.0.0.7:8090"}`))
	}))
	defer srv.Close()

	res, err := PullArtifact(context.Background(), NewClient(), srv.URL, "host-b-token", ArtifactPullRequest{
		Digest:     testDigest,
		PeerURL:    "http://10.0.0.7:8090",
		Grant:      "v1.some-grant",
		SnapshotID: "art-abc",
	})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method: got %s, want POST", gotMethod)
	}
	if want := "/v1/artifacts/" + testDigest + "/pull"; gotPath != want {
		t.Errorf("path: got %q, want %q", gotPath, want)
	}
	// The pull instruction is a normal control-plane call, so it does carry
	// the target host's own token.
	if gotAuth != "Bearer host-b-token" {
		t.Errorf("authorization: got %q", gotAuth)
	}
	if gotBody.PeerURL != "http://10.0.0.7:8090" || gotBody.Grant != "v1.some-grant" || gotBody.SnapshotID != "art-abc" {
		t.Errorf("body: got %+v", gotBody)
	}
	// The digest rides in the path; repeating it in the body would be a second
	// source of truth the agent would have to reconcile.
	if gotBody.Digest != "" {
		t.Errorf("digest was serialized into the body: %q", gotBody.Digest)
	}
	if res.Digest != testDigest || res.SizeBytes != 4096 || res.SnapshotID != "art-abc" {
		t.Errorf("response: got %+v", res)
	}
	if res.SourcePeer != "http://10.0.0.7:8090" {
		t.Errorf("source peer: got %q", res.SourcePeer)
	}
}

func TestPullArtifact_Errors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "peer unreachable", http.StatusBadGateway)
	}))
	defer srv.Close()

	_, err := PullArtifact(context.Background(), NewClient(), srv.URL, "host-b-token", ArtifactPullRequest{
		Digest:  testDigest,
		PeerURL: "http://10.0.0.7:8090",
		Grant:   "v1.some-grant",
	})
	var statusErr *orchestrator.HTTPStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("error: got %v, want *orchestrator.HTTPStatusError", err)
	}
	if statusErr.Code != http.StatusBadGateway {
		t.Errorf("status: got %d, want 502", statusErr.Code)
	}

	if _, err := PullArtifact(context.Background(), NewClient(), srv.URL, "t", ArtifactPullRequest{
		Digest: "nope", PeerURL: "http://10.0.0.7:8090", Grant: "g",
	}); err == nil {
		t.Error("a malformed digest was accepted")
	}
	if _, err := PullArtifact(context.Background(), NewClient(), srv.URL, "t", ArtifactPullRequest{
		Digest: testDigest, Grant: "g",
	}); err == nil {
		t.Error("a missing peer url was accepted")
	}
}

// TestFetchArtifact_GrantRoundTripsToAVerifyingPeer wires the two halves
// together: a peer that verifies the grant exactly as the host agent will
// (path digest, expiry, constant-time mac) and only then serves the blob.
// This is the closest this repo can get to the real trust edge without a VM.
func TestFetchArtifact_GrantRoundTripsToAVerifyingPeer(t *testing.T) {
	payload := []byte("rootfs bytes")
	digest := digestOf(payload)
	const peerToken = "host-a-agent-token"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pathDigest := filepath.Base(r.URL.Path)
		if err := VerifyArtifactGrant(peerToken, pathDigest, r.Header.Get(ArtifactGrantHeader)); err != nil {
			// Exactly what the agent does: one opaque 403, no hint about
			// which check failed.
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	grant, err := MintArtifactGrant(peerToken, digest, 0)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	dest, _ := artifactDir(t)
	if _, err := FetchArtifact(context.Background(), NewArtifactClient(), ArtifactFetchOptions{
		PeerBaseURL: srv.URL,
		Digest:      digest,
		Grant:       grant,
		DestPath:    dest,
	}); err != nil {
		t.Fatalf("fetch with a valid grant: %v", err)
	}

	// A grant minted for a different digest must not open this blob, even
	// though it is a perfectly well formed grant for the same host.
	otherGrant, err := MintArtifactGrant(peerToken, testOtherDigest, 0)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	dest2, _ := artifactDir(t)
	_, err = FetchArtifact(context.Background(), NewArtifactClient(), ArtifactFetchOptions{
		PeerBaseURL: srv.URL,
		Digest:      digest,
		Grant:       otherGrant,
		DestPath:    dest2,
	})
	var statusErr *orchestrator.HTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.Code != http.StatusForbidden {
		t.Fatalf("grant for another digest: got %v, want 403", err)
	}
}
