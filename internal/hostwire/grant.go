package hostwire

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Host-to-host artifact grants.
//
// A host pulling an artifact from another host is a new trust edge: until now
// every agent only ever trusted `Bearer $FC_AGENT_TOKEN` presented by the
// orchestrator. Handing the pulling host the serving host's token would
// over-grant catastrophically, because that token is full control of the
// serving host: create VMs, exec in guests, delete snapshots.
//
// Instead the orchestrator mints a capability that the serving host can verify
// entirely on its own. The key is the serving host's own agent token, which
// needs no new key distribution: the orchestrator already stores every host's
// token in order to talk to it at all.
//
//	grant = "v1." + digest + "." + expiry_unix + "." + nonce + "." + mac
//	mac   = HMAC-SHA256(
//	            key = <FC_AGENT_TOKEN of the SERVING host>,
//	            msg = "fuse-artifact-grant/v1\n" + digest + "\n"
//	                  + expiry_unix + "\n" + nonce)
//
// What this buys, and why each part matters:
//
//   - the pulling host never learns the serving host's token. HMAC is one way,
//     so possession of (message, mac) does not yield the key.
//   - the grant authorizes exactly one digest on exactly one endpoint. The
//     digest is inside the signed preimage and the verifier also checks it
//     against the digest in the request path, so a grant for blob X cannot be
//     replayed against blob Y, and it is not a general credential for the
//     serving agent's API.
//   - the grant is worthless after expiry. Nothing has to revoke it.
//   - the serving host never has to call the orchestrator to verify. Pulls stay
//     possible while the control plane is busy, restarting, or unreachable,
//     and verification costs one HMAC rather than a network round trip.
//
// What it deliberately does NOT defend against, so nobody assumes more:
//
//   - within its TTL the grant is replayable by anyone who holds it. There is
//     no per-grant server-side state, no nonce ledger, no binding to the
//     pulling host's identity or address. That is acceptable because the only
//     thing a replay achieves is re-reading one blob whose contents the holder
//     was already authorized to read. It grants no write, no exec, and no
//     access to any other artifact.
//   - it says nothing about who the puller is. It is a bearer capability, not
//     an authentication of the peer.
//   - it does not protect the blob in transit. Confidentiality on the wire is
//     TLS's job, exactly as it is for every other orchestrator-to-agent call.
//
// The live verifier is the Python host agent, which recomputes the same mac
// and compares it with hmac.compare_digest. The Go verifier below is not on
// that path; it exists so the scheme is testable in this repo and so the wire
// format is pinned by a second independent implementation. If the two ever
// disagree, one of them is a bug and the tests here are what catch it.
const (
	// ArtifactGrantHeader carries the grant on the peer-to-peer fetch. It is a
	// distinct header from Authorization on purpose: a grant is not a bearer
	// token for the agent API, and mixing them invites a verifier that
	// accidentally accepts one where it meant the other.
	ArtifactGrantHeader = "X-Fuse-Artifact-Grant"

	// artifactGrantVersion prefixes the grant, and artifactGrantDomain is the
	// domain separator inside the signed preimage. Both move together if the
	// format ever changes, so a v1 mac can never validate a v2 grant.
	artifactGrantVersion = "v1"
	artifactGrantDomain  = "fuse-artifact-grant/v1"

	// artifactGrantNonceBytes is the length of the random nonce. It does not
	// carry any anti-replay meaning on its own (nothing remembers nonces); it
	// is there so two grants for the same digest and expiry are not byte
	// identical, which keeps grants from being usable as a stable identifier.
	artifactGrantNonceBytes = 16

	// artifactGrantFields is the number of dot-separated fields in a grant.
	artifactGrantFields = 5
)

// DefaultArtifactGrantTTL is how long a minted grant stays valid. Five minutes
// is long enough to cover scheduling the pull and the peer opening the
// connection, and short enough that a leaked grant is stale before anyone can
// do much with it. Note that the TTL bounds when the transfer may START, not
// how long it may run: the serving agent checks the grant once, at request
// time, so a multi-hour 40 GiB transfer is not killed by a five minute grant.
const DefaultArtifactGrantTTL = 5 * time.Minute

// Grant verification failures. Every specific reason wraps
// ErrArtifactGrantInvalid so a caller can reject with one check, which is what
// the HTTP layer wants: the response is a bare 403 that says nothing about
// which check failed, because telling an attacker whether the mac or the
// expiry was wrong is free help. The specific sentinels exist for tests and
// logs on the trusted side.
var (
	ErrArtifactGrantInvalid        = errors.New("artifact grant is not valid")
	ErrArtifactGrantMalformed      = fmt.Errorf("%w: malformed", ErrArtifactGrantInvalid)
	ErrArtifactGrantExpired        = fmt.Errorf("%w: expired", ErrArtifactGrantInvalid)
	ErrArtifactGrantDigestMismatch = fmt.Errorf("%w: digest mismatch", ErrArtifactGrantInvalid)
	ErrArtifactGrantSignature      = fmt.Errorf("%w: signature mismatch", ErrArtifactGrantInvalid)
)

// ArtifactGrant is a parsed grant. The fields are exactly the signed preimage
// plus the mac, so a parsed grant can be re-verified without the original
// string.
type ArtifactGrant struct {
	Digest string
	Expiry time.Time
	Nonce  string // hex
	MAC    string // hex
}

// MintArtifactGrant returns a grant authorizing the holder to fetch exactly
// digest from the host whose agent token is servingToken.
//
// servingToken is the SERVING host's token, never the pulling host's: the
// point of the scheme is that the serving host can verify with a key it
// already has. Passing the wrong host's token produces a grant that simply
// fails verification, which is a confusing failure, so the orchestrator should
// take the token from the same host record it takes the peer URL from.
//
// A ttl of zero or less means DefaultArtifactGrantTTL.
func MintArtifactGrant(servingToken, digest string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = DefaultArtifactGrantTTL
	}
	return mintArtifactGrantAt(servingToken, digest, time.Now().Add(ttl))
}

// mintArtifactGrantAt is the seam MintArtifactGrant and the tests share. Tests
// need to mint an already-expired grant, which a public TTL-based API cannot
// express without inviting callers to mint grants in the past.
func mintArtifactGrantAt(servingToken, digest string, expiry time.Time) (string, error) {
	if servingToken == "" {
		// An empty HMAC key is not a weak key, it is no key: anyone who knows
		// the format could forge grants. Fail loudly rather than mint
		// something that looks fine and is worthless.
		return "", errors.New("mint artifact grant: serving host token is empty")
	}
	if !validArtifactDigest(digest) {
		return "", fmt.Errorf("mint artifact grant: %q is not a sha256 digest", digest)
	}

	nonce := make([]byte, artifactGrantNonceBytes)
	// crypto/rand, never math/rand: a predictable nonce is not fatal to this
	// scheme (the mac is what authorizes) but a PRNG here is the kind of thing
	// that gets copied into a place where it is fatal.
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("mint artifact grant: read random nonce: %w", err)
	}
	nonceHex := hex.EncodeToString(nonce)

	expiryUnix := strconv.FormatInt(expiry.Unix(), 10)
	mac := artifactGrantMAC(servingToken, digest, expiryUnix, nonceHex)

	return strings.Join([]string{artifactGrantVersion, digest, expiryUnix, nonceHex, mac}, "."), nil
}

// ParseArtifactGrant splits a grant into its fields without verifying it.
//
// Parsing is separate from verification because the serving agent has to read
// the digest out of the grant before it can compare it to the one in the URL
// path, and because a malformed grant should be rejected without ever touching
// the token. A parsed grant is NOT a trusted grant; nothing may act on one
// until VerifyArtifactGrant has returned nil.
func ParseArtifactGrant(raw string) (ArtifactGrant, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != artifactGrantFields {
		return ArtifactGrant{}, ErrArtifactGrantMalformed
	}
	if parts[0] != artifactGrantVersion {
		return ArtifactGrant{}, ErrArtifactGrantMalformed
	}

	digest, expiryUnix, nonce, mac := parts[1], parts[2], parts[3], parts[4]
	if !validArtifactDigest(digest) {
		return ArtifactGrant{}, ErrArtifactGrantMalformed
	}
	secs, err := strconv.ParseInt(expiryUnix, 10, 64)
	if err != nil {
		return ArtifactGrant{}, ErrArtifactGrantMalformed
	}
	if len(nonce) != hex.EncodedLen(artifactGrantNonceBytes) || !isLowerHex(nonce) {
		return ArtifactGrant{}, ErrArtifactGrantMalformed
	}
	if len(mac) != hex.EncodedLen(sha256.Size) || !isLowerHex(mac) {
		return ArtifactGrant{}, ErrArtifactGrantMalformed
	}

	return ArtifactGrant{
		Digest: digest,
		Expiry: time.Unix(secs, 0),
		Nonce:  nonce,
		MAC:    mac,
	}, nil
}

// VerifyArtifactGrant checks that raw is a live grant, signed with
// servingToken, for wantDigest.
//
// wantDigest is the digest from the request path, not from the grant: checking
// the grant against itself would prove nothing. The checks run in the order
// the design fixes (shape, digest, expiry, mac) and any failure is a single
// opaque rejection to the peer.
func VerifyArtifactGrant(servingToken, wantDigest, raw string) error {
	return verifyArtifactGrantAt(servingToken, wantDigest, raw, time.Now())
}

func verifyArtifactGrantAt(servingToken, wantDigest, raw string, now time.Time) error {
	if servingToken == "" {
		// Same reasoning as minting: with no key there is nothing to verify
		// against, so accepting anything here would be an open door.
		return fmt.Errorf("%w: no serving host token", ErrArtifactGrantInvalid)
	}

	grant, err := ParseArtifactGrant(raw)
	if err != nil {
		return err
	}
	if grant.Digest != wantDigest {
		// Plain comparison is correct here: both digests are public content
		// addresses, not secrets, so there is nothing for a timing side
		// channel to leak.
		return ErrArtifactGrantDigestMismatch
	}
	if now.After(grant.Expiry) {
		return ErrArtifactGrantExpired
	}

	want := artifactGrantMAC(servingToken, grant.Digest, strconv.FormatInt(grant.Expiry.Unix(), 10), grant.Nonce)
	// hmac.Equal, never ==: a byte-at-a-time comparison of a mac against a
	// value the attacker controls leaks the correct prefix through timing, and
	// an attacker who can retry can walk the mac out one byte at a time.
	if !hmac.Equal([]byte(want), []byte(grant.MAC)) {
		return ErrArtifactGrantSignature
	}
	return nil
}

// artifactGrantMAC computes the hex mac over the domain-separated preimage.
// The domain string and the newline framing are what stop a mac minted for one
// purpose from being replayed as a mac for another: without them, a signer of
// arbitrary strings elsewhere in the system could be tricked into producing a
// valid grant.
func artifactGrantMAC(servingToken, digest, expiryUnix, nonce string) string {
	m := hmac.New(sha256.New, []byte(servingToken))
	// hash.Hash writes never return an error.
	_, _ = m.Write([]byte(artifactGrantDomain + "\n" + digest + "\n" + expiryUnix + "\n" + nonce))
	return hex.EncodeToString(m.Sum(nil))
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
