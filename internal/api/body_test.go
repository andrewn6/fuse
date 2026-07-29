package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// oversized returns a JSON body larger than limit for a struct with a single
// string field, so the decoder must run past the limit to finish it.
func oversized(field string, limit int64) string {
	return `{"` + field + `":"` + strings.Repeat("x", int(limit)+1024) + `"}`
}

// TestLogin_OversizedBodyRejected is the case that matters most: /login is
// unauthenticated, so anyone who can reach the orchestrator can post to it.
func TestLogin_OversizedBodyRejected(t *testing.T) {
	h := &Handler{AuthToken: "secret"}
	req := httptest.NewRequest(http.MethodPost, "/login",
		strings.NewReader(oversized("token", MaxLoginBodyBytes)))
	rec := httptest.NewRecorder()

	h.login(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized login: got %d, want 413", rec.Code)
	}
	if findCookie(rec, SessionCookieName) != nil {
		t.Fatal("oversized login set a session cookie")
	}
	assertErrorCode(t, rec, CodePayloadTooLarge)
}

// TestLogin_OversizedChunkedBodyRejected pins the limit to bytes actually
// read. A chunked request declares no Content-Length, so a limit derived from
// that header would not apply to it at all.
func TestLogin_OversizedChunkedBodyRejected(t *testing.T) {
	h := &Handler{AuthToken: "secret"}
	req := httptest.NewRequest(http.MethodPost, "/login",
		strings.NewReader(oversized("token", MaxLoginBodyBytes)))
	req.ContentLength = -1
	req.TransferEncoding = []string{"chunked"}
	rec := httptest.NewRecorder()

	h.login(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized chunked login: got %d, want 413", rec.Code)
	}
}

// TestLogin_NormalBodyStillWorks guards against the limit being set so low
// that a legitimate token is rejected.
func TestLogin_NormalBodyStillWorks(t *testing.T) {
	h := &Handler{AuthToken: "secret"}
	body, _ := json.Marshal(loginRequest{Token: "secret"})
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()

	h.login(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("normal login: got %d, want 204", rec.Code)
	}
}

// TestCreateEnvironment_OversizedBodyRejected covers the largest limit, the
// one an inline manifest and secrets map are measured against.
func TestCreateEnvironment_OversizedBodyRejected(t *testing.T) {
	h := &Handler{}
	req := httptest.NewRequest(http.MethodPost, "/v1/environments",
		strings.NewReader(oversized("manifest_inline", MaxEnvironmentBodyBytes)))
	rec := httptest.NewRecorder()

	h.createEnvironment(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized createEnvironment: got %d, want 413", rec.Code)
	}
	assertErrorCode(t, rec, CodePayloadTooLarge)
}

// TestCreateEnvironment_LargeButAllowedManifestDecodes verifies a manifest
// just under the ceiling still parses, so the limit bounds abuse rather than
// real payloads. It stops at the decode: what happens afterwards needs a
// Fleet, which the other handler tests supply.
func TestCreateEnvironment_LargeButAllowedManifestDecodes(t *testing.T) {
	body, _ := json.Marshal(CreateEnvironmentRequest{
		ManifestInline: strings.Repeat("y", (MaxEnvironmentBodyBytes / 2)),
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/environments",
		strings.NewReader(string(body)))
	rec := httptest.NewRecorder()

	var got CreateEnvironmentRequest
	if !decodeJSON(rec, req, MaxEnvironmentBodyBytes, &got) {
		t.Fatalf("large-but-allowed manifest rejected with %d: %s", rec.Code, rec.Body.String())
	}
	if len(got.ManifestInline) != MaxEnvironmentBodyBytes/2 {
		t.Fatalf("manifest length: got %d, want %d", len(got.ManifestInline), MaxEnvironmentBodyBytes/2)
	}
}

// TestRegisterHost_OversizedBodyRejected covers the ordinary JSON limit.
func TestRegisterHost_OversizedBodyRejected(t *testing.T) {
	h := &Handler{NewProvider: func(string, string, orchestrator.HostBackend) orchestrator.Provider { return nil }}
	req := httptest.NewRequest(http.MethodPost, "/v1/hosts",
		strings.NewReader(oversized("url", MaxJSONBodyBytes)))
	rec := httptest.NewRecorder()

	h.registerHost(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized registerHost: got %d, want 413", rec.Code)
	}
}

// TestDecodeOptionalJSON_EmptyBodyIsAccepted pins the optional-body contract:
// snapshot and fork accept no body at all, and must keep doing so.
func TestDecodeOptionalJSON_EmptyBodyIsAccepted(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/environments/vm-1/snapshots", nil)
	rec := httptest.NewRecorder()

	var got CreateSnapshotRequest
	if !decodeOptionalJSON(rec, req, MaxJSONBodyBytes, &got) {
		t.Fatalf("empty optional body rejected with %d: %s", rec.Code, rec.Body.String())
	}
	if got.Comment != "" {
		t.Fatalf("empty body populated the struct: %+v", got)
	}
}

// TestDecodeOptionalJSON_ChunkedEmptyBodyIsAccepted covers the same contract
// for a client that sends a chunked body with nothing in it, which reaches the
// decoder as io.EOF rather than as a zero Content-Length.
func TestDecodeOptionalJSON_ChunkedEmptyBodyIsAccepted(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/environments/vm-1/snapshots",
		strings.NewReader(""))
	req.ContentLength = -1
	req.TransferEncoding = []string{"chunked"}
	rec := httptest.NewRecorder()

	var got CreateSnapshotRequest
	if !decodeOptionalJSON(rec, req, MaxJSONBodyBytes, &got) {
		t.Fatalf("empty chunked optional body rejected with %d: %s", rec.Code, rec.Body.String())
	}
}

// TestDecodeOptionalJSON_OversizedBodyRejected verifies the optional path is
// bounded too — an optional body is not an unbounded one.
func TestDecodeOptionalJSON_OversizedBodyRejected(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/environments/vm-1/snapshots",
		strings.NewReader(oversized("comment", MaxJSONBodyBytes)))
	rec := httptest.NewRecorder()

	var got CreateSnapshotRequest
	if decodeOptionalJSON(rec, req, MaxJSONBodyBytes, &got) {
		t.Fatal("oversized optional body was accepted")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized optional body: got %d, want 413", rec.Code)
	}
}

// TestDecodeJSON_MalformedBodyIsStill400 verifies the size limit did not
// swallow the ordinary malformed-JSON case.
func TestDecodeJSON_MalformedBodyIsStill400(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("{not json"))
	rec := httptest.NewRecorder()

	var got loginRequest
	if decodeJSON(rec, req, MaxLoginBodyBytes, &got) {
		t.Fatal("malformed body was accepted")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body: got %d, want 400", rec.Code)
	}
	assertErrorCode(t, rec, CodeInvalidArgument)
}

func assertErrorCode(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	var env Error
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v (body %q)", err, rec.Body.String())
	}
	if env.Error.Code != want {
		t.Fatalf("error code: got %q, want %q", env.Error.Code, want)
	}
}
