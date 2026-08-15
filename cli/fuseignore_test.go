package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/folsomintel/fuse/internal/fusefile"
)

// writeFuseignore drops a .fuseignore in dir and loads it.
func writeFuseignore(t *testing.T, dir, body string) *fuseignore {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, fuseignoreName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	ig, err := loadFuseignore(dir)
	if err != nil {
		t.Fatalf("loadFuseignore: %v", err)
	}
	return ig
}

// A directory with no .fuseignore is not an error: the defaults still apply,
// so nothing has to write a file to avoid shipping .git.
func TestFuseignoreMissingFile(t *testing.T) {
	ig, err := loadFuseignore(t.TempDir())
	if err != nil {
		t.Fatalf("loadFuseignore: %v", err)
	}
	if !ig.Match(".git", true) {
		t.Error(".git is not ignored without a .fuseignore")
	}
	if ig.Match("main.go", false) {
		t.Error("main.go is ignored without a .fuseignore")
	}
}

func TestFuseignoreMatching(t *testing.T) {
	ig := writeFuseignore(t, t.TempDir(), `
# comments and blank lines are skipped

*.log
!keep-this.log
/tmp-scratch
docs/drafts
cache/
`)

	cases := []struct {
		rel   string
		isDir bool
		want  bool
		why   string
	}{
		{rel: "app.log", want: true, why: "unanchored pattern matches at the top"},
		{rel: "logs/app.log", want: true, why: "unanchored pattern matches at any depth"},
		{rel: "keep-this.log", want: false, why: "a later ! re-includes what *.log dropped"},
		{rel: "tmp-scratch", want: true, why: "a leading / anchors to the source root"},
		{rel: "src/tmp-scratch", want: false, why: "an anchored pattern does not match deeper"},
		{rel: "docs/drafts", isDir: true, want: true, why: "an interior / anchors the pattern too"},
		{rel: "other/docs/drafts", isDir: true, want: false, why: "an interior / anchors the pattern too"},
		{rel: "cache", isDir: true, want: true, why: "a trailing / matches a directory"},
		{rel: "cache", isDir: false, want: false, why: "a trailing / does not match a file of the same name"},
		{rel: "src/main.go", want: false, why: "nothing matches it"},
	}
	for _, tc := range cases {
		if got := ig.Match(tc.rel, tc.isDir); got != tc.want {
			t.Errorf("Match(%q, isDir=%v) = %v, want %v: %s", tc.rel, tc.isDir, got, tc.want, tc.why)
		}
	}
}

// Precedence is last-match-wins over the whole sequence, so the order two
// patterns are written in is what decides.
func TestFuseignorePrecedenceIsLastMatchWins(t *testing.T) {
	ig := writeFuseignore(t, t.TempDir(), "!*.log\n*.log\n")
	if !ig.Match("app.log", false) {
		t.Error("the later *.log did not beat the earlier !*.log")
	}

	ig = writeFuseignore(t, t.TempDir(), "*.log\n!*.log\n")
	if ig.Match("app.log", false) {
		t.Error("the later !*.log did not beat the earlier *.log")
	}
}

// The defaults are compiled in before the file's lines, which is the whole
// reason a one-line `!` can override one.
func TestFuseignoreNegationOverridesADefault(t *testing.T) {
	dir := t.TempDir()

	ig, err := loadFuseignore(dir)
	if err != nil {
		t.Fatalf("loadFuseignore: %v", err)
	}
	if !ig.Match(".env", false) {
		t.Fatal(".env is not ignored by default")
	}

	ig = writeFuseignore(t, dir, "!.env\n")
	if ig.Match(".env", false) {
		t.Error("!.env did not override the built-in default")
	}
	// overriding one default leaves the rest alone.
	if !ig.Match("node_modules", true) {
		t.Error("!.env disabled an unrelated default")
	}
}

func TestFuseignoreGlobs(t *testing.T) {
	ig := writeFuseignore(t, t.TempDir(), "build/**/*.o\nvendor/**\n")

	cases := []struct {
		rel  string
		want bool
	}{
		{rel: "build/a.o", want: true},         // ** spans zero segments
		{rel: "build/x/y/a.o", want: true},     // and any number of them
		{rel: "build/a.c", want: false},        // the final segment still has to match
		{rel: "vendor/pkg/mod.go", want: true}, // ** with nothing after it takes the subtree
		{rel: "elsewhere/a.o", want: false},    // an interior / anchors it
	}
	for _, tc := range cases {
		if got := ig.Match(tc.rel, false); got != tc.want {
			t.Errorf("Match(%q) = %v, want %v", tc.rel, got, tc.want)
		}
	}
}

// decide separates the two so --show-copy can report them separately.
func TestFuseignoreDecideNamesTheRuleThatDropped(t *testing.T) {
	ig := writeFuseignore(t, t.TempDir(), "*.log\n")

	if got := ig.decide("node_modules", true); got != copySkipDefault {
		t.Errorf("node_modules decided %v, want copySkipDefault", got)
	}
	if got := ig.decide("app.log", false); got != copySkipPattern {
		t.Errorf("app.log decided %v, want copySkipPattern", got)
	}
	if got := ig.decide("main.go", false); got != copyKeep {
		t.Errorf("main.go decided %v, want copyKeep", got)
	}
}

// The fixture a real `copy: {from: .}` hits: a checkout with the two
// directories nobody means to ship and the file nobody means to leak.
func writeIgnoreFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, ".git", "config"), "[core]\n")
	mustWrite(t, filepath.Join(root, "node_modules", "pkg", "index.js"), "module.exports = {}\n")
	mustWrite(t, filepath.Join(root, ".env"), "PG_PASSWORD=hunter2\n")
	mustWrite(t, filepath.Join(root, "app.log"), "noise\n")
	mustWrite(t, filepath.Join(root, "src", "main.go"), "package main\n")
	return root
}

func TestCopyWalkAppliesFuseignore(t *testing.T) {
	root := writeIgnoreFixture(t)
	ig := writeFuseignore(t, root, "*.log\n")

	files, stats, err := collectCopy([]fusefile.CopySpec{{From: ".", To: "/workspace"}}, root, ig.decide)
	if err != nil {
		t.Fatalf("collectCopy: %v", err)
	}
	for _, gone := range []string{
		"/workspace/.git/config",
		"/workspace/node_modules/pkg/index.js",
		"/workspace/.env",
		"/workspace/app.log",
	} {
		if _, ok := files[gone]; ok {
			t.Errorf("%s reached the file map: %v", gone, keys(files))
		}
	}
	if _, ok := files["/workspace/src/main.go"]; !ok {
		t.Errorf("a file nothing ignores was dropped: %v", keys(files))
	}

	// .git, node_modules and .env are defaults; app.log is the file's own.
	if stats[0].SkippedDefault != 3 {
		t.Errorf("SkippedDefault = %d, want 3", stats[0].SkippedDefault)
	}
	if stats[0].SkippedPattern != 1 {
		t.Errorf("SkippedPattern = %d, want 1", stats[0].SkippedPattern)
	}
}

// An ignored directory is pruned, not walked and then discarded. The proof is
// a symlink inside one: the walk refuses symlinks, so reaching it at all would
// fail the copy.
func TestCopyWalkPrunesIgnoredDirectories(t *testing.T) {
	root := writeIgnoreFixture(t)
	if err := os.Symlink(filepath.Join(root, "src", "main.go"), filepath.Join(root, "node_modules", "link.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	ig, err := loadFuseignore(root)
	if err != nil {
		t.Fatalf("loadFuseignore: %v", err)
	}

	if _, _, err := collectCopy([]fusefile.CopySpec{{From: ".", To: "/workspace"}}, root, ig.decide); err != nil {
		t.Fatalf("the walk descended into an ignored directory: %v", err)
	}
}

// The size cap is checked from stat sizes as the walk goes, so an ignored file
// must be dropped before it is sized. Otherwise .fuseignore could not rescue
// an over-cap tree, which is most of what it is for.
func TestFuseignoreKeepsIgnoredFilesOutOfTheSizeCap(t *testing.T) {
	root := t.TempDir()
	// two files that fit alone and not together, so what counts is decided by
	// whether the ignored one was counted at all.
	half := strings.Repeat("x", fusefile.MaxCopyBytes/2+1)
	mustWrite(t, filepath.Join(root, "src", "keep.bin"), half)
	mustWrite(t, filepath.Join(root, "src", "vendor", "drop.bin"), half)
	entries := []fusefile.CopySpec{{From: "./src", To: "/workspace/src"}}

	if _, err := collectCopyFiles(entries, root, nil); err == nil {
		t.Fatal("the fixture is not over the cap without a .fuseignore")
	}

	ig := writeFuseignore(t, root, "vendor/\n")
	files, _, err := collectCopy(entries, root, ig.decide)
	if err != nil {
		t.Fatalf("an ignored file still counted against the size cap: %v", err)
	}
	if _, ok := files["/workspace/src/keep.bin"]; !ok {
		t.Errorf("the file under the cap was not shipped: %v", keys(files))
	}
}

// An unreadable .fuseignore is reported rather than silently treated as empty:
// shipping a tree the author thought was filtered is the worse failure.
func TestFuseignoreUnreadableFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, fuseignoreName), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFuseignore(dir); err == nil {
		t.Fatal("expected an error for a .fuseignore that cannot be read")
	} else if !strings.Contains(err.Error(), fuseignoreName) {
		t.Errorf("error %q does not name the file", err)
	}
}
