package api

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/folsomintel/fuse/internal/fusefile"
)

func TestDecodeCreateFilesRejectsUnsafePaths(t *testing.T) {
	cases := []struct {
		name        string
		files       map[string]string
		wantContain string
	}{
		{
			name:        "relative path",
			files:       map[string]string{"src/app.go": ""},
			wantContain: "must be an absolute guest path",
		},
		{
			name:        "traversal",
			files:       map[string]string{"/workspace/../../etc/passwd": ""},
			wantContain: "is not a clean path",
		},
		{
			name:        "guest agent directory",
			files:       map[string]string{"/fuse/secrets.json": ""},
			wantContain: "reserved for the guest agent",
		},
		{
			name:        "the guest agent directory itself",
			files:       map[string]string{"/fuse": ""},
			wantContain: "reserved for the guest agent",
		},
		{
			name:        "newline in path",
			files:       map[string]string{"/workspace/a\nb": ""},
			wantContain: "must not contain newlines",
		},
		{
			name:        "bad base64",
			files:       map[string]string{"/workspace/app.go": "!!!"},
			wantContain: "not valid base64",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeCreateFiles(tc.files)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantContain) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantContain)
			}
		})
	}
}

func TestDecodeCreateFilesDecodesContent(t *testing.T) {
	files, err := decodeCreateFiles(map[string]string{
		"/workspace/app.go": base64.StdEncoding.EncodeToString([]byte("package main\n")),
		"/etc/app/conf":     base64.StdEncoding.EncodeToString([]byte("k=v\n")),
	})
	if err != nil {
		t.Fatalf("decodeCreateFiles: %v", err)
	}
	if got := string(files["/workspace/app.go"]); got != "package main\n" {
		t.Errorf("/workspace/app.go = %q", got)
	}
	if got := string(files["/etc/app/conf"]); got != "k=v\n" {
		t.Errorf("/etc/app/conf = %q", got)
	}
}

// The client caps the copy block while it walks, but a caller that skipped the
// CLI must hit the same wall, and the answer has to say "too big" rather than
// "malformed".
func TestCreateEnvironmentRejectsOversizedFiles(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	oversized := base64.StdEncoding.EncodeToString(make([]byte, fusefile.MaxCopyBytes+1))
	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
		Files:          map[string]string{"/workspace/big.bin": oversized},
	})
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413. body: %s", rr.Code, rr.Body.String())
	}
	if e := decodeError(t, rr.Body); !strings.Contains(e.Error.Message, "byte limit") {
		t.Errorf("message = %q, want it to name the limit", e.Error.Message)
	}
}

func TestCreateEnvironmentRejectsReservedFilePath(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
		Files:          map[string]string{"/fuse/secrets.json": base64.StdEncoding.EncodeToString([]byte("{}"))},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body: %s", rr.Code, rr.Body.String())
	}
}
