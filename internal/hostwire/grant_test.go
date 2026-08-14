package hostwire

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	testDigest      = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	testOtherDigest = "0000000000000000000000000000000000000000000000000000000000000001"
	testToken       = "fc-agent-token-for-host-a"
)

func TestArtifactGrant_RoundTrip(t *testing.T) {
	grant, err := MintArtifactGrant(testToken, testDigest, 0)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	if err := VerifyArtifactGrant(testToken, testDigest, grant); err != nil {
		t.Fatalf("verify: %v", err)
	}

	parsed, err := ParseArtifactGrant(grant)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Digest != testDigest {
		t.Errorf("digest: got %q, want %q", parsed.Digest, testDigest)
	}
	if got := time.Until(parsed.Expiry); got <= 0 || got > DefaultArtifactGrantTTL+time.Second {
		t.Errorf("default ttl: expiry is %s away, want ~%s", got, DefaultArtifactGrantTTL)
	}
	if len(parsed.Nonce) != 32 {
		t.Errorf("nonce: got %d hex chars, want 32", len(parsed.Nonce))
	}
	if len(parsed.MAC) != 64 {
		t.Errorf("mac: got %d hex chars, want 64", len(parsed.MAC))
	}
}

// TestArtifactGrant_NonceIsFresh guards against a nonce that is constant or
// derived from the other fields, which would make grants a stable identifier
// for a (host, digest) pair.
func TestArtifactGrant_NonceIsFresh(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		grant, err := MintArtifactGrant(testToken, testDigest, time.Minute)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		parsed, err := ParseArtifactGrant(grant)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if seen[parsed.Nonce] {
			t.Fatalf("nonce %q repeated", parsed.Nonce)
		}
		seen[parsed.Nonce] = true
	}
}

func TestArtifactGrant_CustomTTL(t *testing.T) {
	grant, err := MintArtifactGrant(testToken, testDigest, 30*time.Second)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	parsed, err := ParseArtifactGrant(grant)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := time.Until(parsed.Expiry); got <= 0 || got > 31*time.Second {
		t.Fatalf("expiry is %s away, want ~30s", got)
	}
}

func TestArtifactGrant_Rejects(t *testing.T) {
	valid, err := MintArtifactGrant(testToken, testDigest, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	fields := strings.Split(valid, ".")

	// flip one hex character of the mac, the minimal possible tamper
	tamperedMAC := strings.Join(append(append([]string{}, fields[:4]...), flipHex(fields[4])), ".")
	// re-point a valid grant at another digest without re-signing
	swappedDigest := strings.Join([]string{fields[0], testOtherDigest, fields[2], fields[3], fields[4]}, ".")
	// push the expiry out, which changes the preimage and so invalidates the mac
	extendedExpiry := strings.Join([]string{
		fields[0], fields[1],
		strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10),
		fields[3], fields[4],
	}, ".")

	expired, err := mintArtifactGrantAt(testToken, testDigest, time.Now().Add(-time.Second))
	if err != nil {
		t.Fatalf("mint expired: %v", err)
	}
	otherKey, err := MintArtifactGrant("some-other-hosts-token", testDigest, time.Minute)
	if err != nil {
		t.Fatalf("mint other key: %v", err)
	}
	forOtherDigest, err := MintArtifactGrant(testToken, testOtherDigest, time.Minute)
	if err != nil {
		t.Fatalf("mint other digest: %v", err)
	}

	cases := []struct {
		name       string
		token      string
		wantDigest string
		grant      string
		wantErr    error
	}{
		{"tampered mac", testToken, testDigest, tamperedMAC, ErrArtifactGrantSignature},
		{"digest swapped in the grant", testToken, testDigest, swappedDigest, ErrArtifactGrantDigestMismatch},
		{"expiry extended without re-signing", testToken, testDigest, extendedExpiry, ErrArtifactGrantSignature},
		{"expired", testToken, testDigest, expired, ErrArtifactGrantExpired},
		{"signed with another host's token", testToken, testDigest, otherKey, ErrArtifactGrantSignature},
		{"grant is for a different digest", testToken, testDigest, forOtherDigest, ErrArtifactGrantDigestMismatch},
		{"path asks for a digest the grant does not cover", testToken, testOtherDigest, valid, ErrArtifactGrantDigestMismatch},
		{"no serving token", "", testDigest, valid, ErrArtifactGrantInvalid},
		{"empty", testToken, testDigest, "", ErrArtifactGrantMalformed},
		{"wrong version", testToken, testDigest, "v2." + strings.Join(fields[1:], "."), ErrArtifactGrantMalformed},
		{"too few fields", testToken, testDigest, strings.Join(fields[:4], "."), ErrArtifactGrantMalformed},
		{"too many fields", testToken, testDigest, valid + ".extra", ErrArtifactGrantMalformed},
		{"digest not hex", testToken, testDigest, "v1." + strings.Repeat("z", 64) + "." + strings.Join(fields[2:], "."), ErrArtifactGrantMalformed},
		{"digest wrong length", testToken, testDigest, "v1.abcd." + strings.Join(fields[2:], "."), ErrArtifactGrantMalformed},
		{"expiry not a number", testToken, testDigest, strings.Join([]string{fields[0], fields[1], "soon", fields[3], fields[4]}, "."), ErrArtifactGrantMalformed},
		{"nonce wrong length", testToken, testDigest, strings.Join([]string{fields[0], fields[1], fields[2], "abcd", fields[4]}, "."), ErrArtifactGrantMalformed},
		{"mac wrong length", testToken, testDigest, strings.Join([]string{fields[0], fields[1], fields[2], fields[3], "abcd"}, "."), ErrArtifactGrantMalformed},
		{"mac uppercase hex", testToken, testDigest, strings.Join([]string{fields[0], fields[1], fields[2], fields[3], strings.ToUpper(fields[4])}, "."), ErrArtifactGrantMalformed},
		{"not a grant at all", testToken, testDigest, "Bearer some-token", ErrArtifactGrantMalformed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifyArtifactGrant(tc.token, tc.wantDigest, tc.grant)
			if err == nil {
				t.Fatal("grant was accepted, want rejection")
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error: got %v, want %v", err, tc.wantErr)
			}
			// every rejection reason must also satisfy the single umbrella
			// check the HTTP layer uses to answer a bare 403
			if !errors.Is(err, ErrArtifactGrantInvalid) {
				t.Fatalf("error %v does not wrap ErrArtifactGrantInvalid", err)
			}
		})
	}
}

func TestMintArtifactGrant_RejectsBadInput(t *testing.T) {
	if _, err := MintArtifactGrant("", testDigest, time.Minute); err == nil {
		t.Error("minting with an empty key was allowed")
	}
	for _, digest := range []string{"", "not-a-digest", strings.ToUpper(testDigest), testDigest + "0"} {
		if _, err := MintArtifactGrant(testToken, digest, time.Minute); err == nil {
			t.Errorf("minting for digest %q was allowed", digest)
		}
	}
}

// TestArtifactGrant_MACIsStable pins the exact preimage. If someone reorders
// the fields or drops the domain separator, the mac changes and this fails,
// which is the point: the Python agent computes the same bytes and the two
// implementations must not drift apart silently.
func TestArtifactGrant_MACIsStable(t *testing.T) {
	// Independently computed with python's hmac module, the same library the
	// host agent verifies with:
	//
	//   msg = "fuse-artifact-grant/v1\n" + digest + "\n1700000000\n" + "0"*32
	//   hmac.new(b"fc-agent-token-for-host-a", msg.encode(), hashlib.sha256).hexdigest()
	const want = "4b61b93a667b4c76df6de2bf0afd487950f4fe0c2343183bc340f13ab6953785"
	got := artifactGrantMAC(testToken, testDigest, "1700000000", strings.Repeat("0", 32))
	if len(got) != 64 {
		t.Fatalf("mac length: got %d, want 64", len(got))
	}
	if got != want {
		t.Fatalf("preimage changed: got %s, want %s\n"+
			"if this is an intentional format change, bump artifactGrantVersion and "+
			"artifactGrantDomain together and update the Python agent", got, want)
	}
}

// TestArtifactGrant_UsesConstantTimeCompare asserts the property by code shape
// rather than by timing. A timing assertion would be flaky on a shared CI box
// and would not actually prove the comparison is constant time; what we can
// prove cheaply is that the mac comparison goes through hmac.Equal and that
// nobody has quietly replaced it with ==.
func TestArtifactGrant_UsesConstantTimeCompare(t *testing.T) {
	src, err := os.ReadFile("grant.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	if !strings.Contains(body, "hmac.Equal([]byte(want), []byte(grant.MAC))") {
		t.Error("the mac comparison no longer goes through hmac.Equal")
	}
	for _, banned := range []string{"want == grant.MAC", "grant.MAC == want", "want != grant.MAC", "grant.MAC != want"} {
		if strings.Contains(body, banned) {
			t.Errorf("mac compared with %q; use hmac.Equal, a byte-at-a-time compare leaks the mac prefix", banned)
		}
	}
}

// flipHex changes exactly one character of a hex string.
func flipHex(s string) string {
	if s == "" {
		return "0"
	}
	last := s[len(s)-1]
	repl := byte('a')
	if last == 'a' {
		repl = 'b'
	}
	return s[:len(s)-1] + string(repl)
}
