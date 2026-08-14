package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/folsomintel/fuse/internal/fusefile"
)

// writeCopyTree lays out a small source tree and returns its root.
func writeCopyTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "start.sh"), "#!/bin/sh\necho hi\n")
	mustWrite(t, filepath.Join(root, "src", "main.go"), "package main\n")
	mustWrite(t, filepath.Join(root, "src", "internal", "util.go"), "package internal\n")
	return root
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCollectCopyFilesFile(t *testing.T) {
	root := writeCopyTree(t)
	files, err := collectCopyFiles([]fusefile.CopySpec{
		{From: "./start.sh", To: "/workspace/start.sh"},
	}, root, nil)
	if err != nil {
		t.Fatalf("collectCopyFiles: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("got %d files, want 1: %v", len(files), keys(files))
	}
	if got := string(files["/workspace/start.sh"]); got != "#!/bin/sh\necho hi\n" {
		t.Errorf("/workspace/start.sh = %q", got)
	}
}

func TestCollectCopyFilesDirectory(t *testing.T) {
	root := writeCopyTree(t)
	files, err := collectCopyFiles([]fusefile.CopySpec{
		{From: "./src", To: "/workspace/src"},
	}, root, nil)
	if err != nil {
		t.Fatalf("collectCopyFiles: %v", err)
	}
	want := map[string]string{
		"/workspace/src/main.go":          "package main\n",
		"/workspace/src/internal/util.go": "package internal\n",
	}
	if len(files) != len(want) {
		t.Fatalf("got %v, want %v", keys(files), keys(want))
	}
	for path, content := range want {
		if got := string(files[path]); got != content {
			t.Errorf("%s = %q, want %q", path, got, content)
		}
	}
}

// `from` is relative to the Fusefile's directory, not the process working
// directory, so `fuse up ../other/Fusefile` copies that directory's files.
func TestCollectCopyFilesResolvesAgainstTheFusefile(t *testing.T) {
	root := writeCopyTree(t)
	t.Chdir(t.TempDir())

	files, err := collectCopyFiles([]fusefile.CopySpec{{From: "start.sh", To: "/workspace/start.sh"}}, root, nil)
	if err != nil {
		t.Fatalf("collectCopyFiles: %v", err)
	}
	if _, ok := files["/workspace/start.sh"]; !ok {
		t.Fatalf("source was resolved against the working directory, not the Fusefile: %v", keys(files))
	}
}

func TestCollectCopyFilesMissingSource(t *testing.T) {
	_, err := collectCopyFiles([]fusefile.CopySpec{{From: "./nope", To: "/workspace/nope"}}, t.TempDir(), nil)
	if err == nil {
		t.Fatal("expected an error for a source that does not exist")
	}
	if !strings.Contains(err.Error(), "copy[0].from") {
		t.Errorf("error %q does not name the entry", err)
	}
}

func TestCollectCopyFilesRejectsSymlinks(t *testing.T) {
	root := writeCopyTree(t)
	if err := os.Symlink(filepath.Join(root, "start.sh"), filepath.Join(root, "link.sh")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "start.sh"), filepath.Join(root, "src", "link.sh")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	cases := []fusefile.CopySpec{
		{From: "./link.sh", To: "/workspace/link.sh"}, // named directly
		{From: "./src", To: "/workspace/src"},         // found while walking
	}
	for _, entry := range cases {
		_, err := collectCopyFiles([]fusefile.CopySpec{entry}, root, nil)
		if err == nil {
			t.Fatalf("copy of %s: expected an error, got nil", entry.From)
		}
		if !strings.Contains(err.Error(), "symlink") {
			t.Errorf("error %q does not say it was a symlink", err)
		}
	}
}

func TestCollectCopyFilesEnforcesTheSizeCap(t *testing.T) {
	root := t.TempDir()
	// two files that fit individually and do not fit together, so the cap is
	// enforced across the block rather than per file.
	half := strings.Repeat("x", fusefile.MaxCopyBytes/2+1)
	mustWrite(t, filepath.Join(root, "big", "a.bin"), half)
	mustWrite(t, filepath.Join(root, "big", "b.bin"), half)

	_, err := collectCopyFiles([]fusefile.CopySpec{{From: "./big", To: "/workspace/big"}}, root, nil)
	if err == nil {
		t.Fatal("expected an error for a copy block over the size cap")
	}
	for _, want := range []string{"byte limit", "setup"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestCollectCopyFilesRejectsCollidingEntries(t *testing.T) {
	root := writeCopyTree(t)
	mustWrite(t, filepath.Join(root, "other", "main.go"), "package other\n")

	_, err := collectCopyFiles([]fusefile.CopySpec{
		{From: "./src", To: "/workspace/app"},
		{From: "./other", To: "/workspace/app"},
	}, root, nil)
	if err == nil {
		t.Fatal("expected an error for two entries writing the same guest path")
	}
	if !strings.Contains(err.Error(), "/workspace/app/main.go") {
		t.Errorf("error %q does not name the colliding path", err)
	}
}

// The walk takes a filter so an ignore-file matcher can be added without
// touching it: a false for a directory prunes the subtree, a false for a file
// drops that file.
func TestCollectCopyFilesHonoursAFilter(t *testing.T) {
	root := writeCopyTree(t)
	files, err := collectCopyFiles([]fusefile.CopySpec{{From: "./src", To: "/workspace/src"}}, root,
		func(rel string, isDir bool) bool { return rel != "internal" })
	if err != nil {
		t.Fatalf("collectCopyFiles: %v", err)
	}
	if _, ok := files["/workspace/src/internal/util.go"]; ok {
		t.Errorf("a pruned directory still contributed a file: %v", keys(files))
	}
	if _, ok := files["/workspace/src/main.go"]; !ok {
		t.Errorf("filter dropped a file it accepted: %v", keys(files))
	}
}

// End to end through `fuse up`: a copy block lands in the create body as
// base64 under `files`, with guest paths resolved against the workspace.
func TestUpSendsCopiedFilesInTheCreateBody(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		fmt.Fprint(w, `{"id":"vm1","state":"pending","task_id":"t","url":"","spec":{}}`)
	}))
	defer srv.Close()

	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "src", "main.go"), "package main\n")
	mustWrite(t, filepath.Join(dir, "Fusefile"), `version: 1
copy:
  - from: ./src
    to: src
run: ./start.sh
`)
	cfg := writeConfig(t, srv.URL)

	_, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{
			"--config", cfg, "-o", "json",
			"up", "-f", filepath.Join(dir, "Fusefile"), "--task-id", "t", "--no-wait",
		})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	files, ok := gotBody["files"].(map[string]any)
	if !ok {
		t.Fatalf("files missing from the create body: %v", gotBody)
	}
	encoded, ok := files["/workspace/src/main.go"].(string)
	if !ok {
		t.Fatalf("files does not carry /workspace/src/main.go: %v", keys(files))
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("file content is not valid base64: %v", err)
	}
	if string(data) != "package main\n" {
		t.Errorf("content = %q, want %q", data, "package main\n")
	}
}

func TestEncodeCopyFiles(t *testing.T) {
	if got := encodeCopyFiles(nil); got != nil {
		t.Errorf("no copy files should send no field, got %v", got)
	}
	got := encodeCopyFiles(map[string][]byte{"/workspace/a": []byte("hi\n")})
	if got["/workspace/a"] != "aGkK" {
		t.Errorf("/workspace/a = %q, want base64 of \"hi\\n\"", got["/workspace/a"])
	}
}

// keys renders a map's keys for a failure message.
func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
