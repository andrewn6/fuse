package fusefile

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompileFilesRendersBase64Heredoc(t *testing.T) {
	f := &Fusefile{
		Version: 1,
		Files: []File{
			{Path: "config/app.yaml", Content: "key: value\n"},
			{Path: "/opt/bin/entry.sh", Content: "#!/bin/sh\necho hi\n", Mode: "0755"},
		},
		Run: "cat config/app.yaml",
	}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	script := c.StartupScript
	for _, want := range []string{
		"mkdir -p 'config'\n",
		"base64 -d > 'config/app.yaml' <<'FUSE_FILE_EOF_0'\n",
		base64.StdEncoding.EncodeToString([]byte("key: value\n")),
		"mkdir -p '/opt/bin'\n",
		"base64 -d > '/opt/bin/entry.sh' <<'FUSE_FILE_EOF_1'\n",
		"chmod 0755 '/opt/bin/entry.sh'\n",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("startup script missing %q\n---\n%s", want, script)
		}
	}

	// Files must land before setup and run, which are what read them.
	if idx, runIdx := strings.Index(script, "FUSE_FILE_EOF_0"), strings.Index(script, "cat config/app.yaml"); idx > runIdx {
		t.Errorf("file block rendered after run command:\n%s", script)
	}

	// A file with no mode gets no chmod line at all rather than a default.
	if strings.Contains(script, "chmod 0755 'config/app.yaml'") {
		t.Errorf("unexpected chmod for a file with no mode:\n%s", script)
	}
}

// Content that itself looks like shell, or like the heredoc terminator, must
// survive: it is base64 in transit, so the payload can never terminate its own
// heredoc or be expanded by the shell.
func TestCompileFilesHostileContent(t *testing.T) {
	hostile := "FUSE_FILE_EOF_0\n$(rm -rf /)\n`id`\n'\"\x00\xff"
	f := &Fusefile{Version: 1, Files: []File{{Path: "payload", Content: hostile}}}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	const opener = "<<'FUSE_FILE_EOF_0'\n"
	start := strings.Index(c.StartupScript, opener)
	if start < 0 {
		t.Fatalf("no heredoc opener in:\n%s", c.StartupScript)
	}
	body := c.StartupScript[start+len(opener):]
	end := strings.Index(body, "FUSE_FILE_EOF_0\n")
	if end < 0 {
		t.Fatalf("no heredoc terminator in:\n%s", c.StartupScript)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(body[:end], "\n", ""))
	if err != nil {
		t.Fatalf("payload is not decodable base64: %v", err)
	}
	if string(decoded) != hostile {
		t.Errorf("round trip changed content: got %q, want %q", decoded, hostile)
	}
}

func TestCompileFilesInAllScripts(t *testing.T) {
	f := &Fusefile{
		Version: 1,
		Files:   []File{{Path: "app.conf", Content: "x"}},
		Setup:   []Step{{Run: "echo setup"}},
		Run:     "echo run",
	}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	// RunScript carries the files too: `fuse up --from-build` skips setup,
	// but the author may have edited a file since the bake.
	for name, script := range map[string]string{
		"StartupScript": c.StartupScript,
		"BuildScript":   c.BuildScript,
		"RunScript":     c.RunScript,
	} {
		if !strings.Contains(script, "base64 -d > 'app.conf'") {
			t.Errorf("%s missing the file block:\n%s", name, script)
		}
	}
}

// A Fusefile whose only content is files still compiles to a real script;
// previously an empty setup and run meant there was nothing to emit.
func TestCompileFilesOnlyStillEmitsScript(t *testing.T) {
	f := &Fusefile{Version: 1, Files: []File{{Path: "only.txt", Content: "x"}}}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if c.StartupScript == "" {
		t.Fatal("StartupScript is empty for a files-only Fusefile")
	}
	// BuildScript stays empty: there is no setup work to bake, and files are
	// rewritten on every boot regardless.
	if c.BuildScript != "" {
		t.Errorf("BuildScript = %q, want empty for a Fusefile with no setup", c.BuildScript)
	}
}

func TestCompileFilesErrors(t *testing.T) {
	cases := []struct {
		name        string
		files       []File
		wantContain string
	}{
		{
			name:        "missing path",
			files:       []File{{Content: "x"}},
			wantContain: "files[0].path: is required",
		},
		{
			name:        "parent traversal",
			files:       []File{{Path: "../escape", Content: "x"}},
			wantContain: `files[0].path: must not contain ".." segments`,
		},
		{
			name:        "traversal inside an absolute path",
			files:       []File{{Path: "/opt/../../etc/shadow", Content: "x"}},
			wantContain: `files[0].path: must not contain ".." segments`,
		},
		{
			name:        "directory path",
			files:       []File{{Path: "/etc/", Content: "x"}},
			wantContain: "is a directory, not a file",
		},
		{
			name:        "newline in path",
			files:       []File{{Path: "a\nb", Content: "x"}},
			wantContain: "must not contain newlines or NUL bytes",
		},
		{
			name:        "invalid mode",
			files:       []File{{Path: "a", Content: "x", Mode: "rwxr-xr-x"}},
			wantContain: `files[0].mode: invalid octal mode`,
		},
		{
			name:        "setuid mode rejected",
			files:       []File{{Path: "a", Content: "x", Mode: "4755"}},
			wantContain: `files[0].mode: invalid octal mode`,
		},
		{
			name:        "duplicate path",
			files:       []File{{Path: "a", Content: "x"}, {Path: "./a", Content: "y"}},
			wantContain: `files[1].path: "./a" is already written by files[0]`,
		},
		{
			name:        "over the size cap",
			files:       []File{{Path: "big", Content: strings.Repeat("x", MaxFilesBytes+1)}},
			wantContain: "over the 65536 byte limit",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(&Fusefile{Version: 1, Files: tc.files})
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantContain) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantContain)
			}
		})
	}
}

// The cap is on the sum, not on any single entry: several individually legal
// files must not add up past it.
func TestCompileFilesSizeCapIsCumulative(t *testing.T) {
	half := strings.Repeat("x", MaxFilesBytes/2+1)
	_, err := Compile(&Fusefile{Version: 1, Files: []File{
		{Path: "a", Content: half},
		{Path: "b", Content: half},
	}})
	if err == nil || !strings.Contains(err.Error(), "over the") {
		t.Fatalf("expected a cumulative size error, got %v", err)
	}
}

func TestParseFilesBodyRules(t *testing.T) {
	cases := []struct {
		name        string
		yaml        string
		wantContain string
	}{
		{
			name: "both source and content",
			yaml: "version: 1\nfiles:\n  - path: a\n    source: ./a\n    content: x\n",

			wantContain: "files[0]: source and content are mutually exclusive",
		},
		{
			name:        "neither source nor content",
			yaml:        "version: 1\nfiles:\n  - path: a\n",
			wantContain: "files[0]: source or content is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantContain) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantContain)
			}
		})
	}
}

func TestResolveFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "local.conf"), []byte("from disk\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	f := &Fusefile{Version: 1, Files: []File{
		{Path: "app.conf", Source: "local.conf"},
		{Path: "literal.conf", Content: "inline\n"},
	}}
	if err := ResolveFiles(f, dir); err != nil {
		t.Fatalf("ResolveFiles: %v", err)
	}

	if f.Files[0].Content != "from disk\n" {
		t.Errorf("Content = %q, want %q", f.Files[0].Content, "from disk\n")
	}
	// Source is cleared so the resolved Fusefile still satisfies the
	// "exactly one body" rule if it is re-validated.
	if f.Files[0].Source != "" {
		t.Errorf("Source = %q, want it cleared after resolution", f.Files[0].Source)
	}
	if f.Files[1].Content != "inline\n" {
		t.Errorf("literal content was modified: %q", f.Files[1].Content)
	}
}

// An empty source file resolves to empty content, which must compile rather
// than trip the "source or content is required" rule that Parse applies before
// resolution.
func TestResolveFilesEmptySourceCompiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "empty"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f := &Fusefile{Version: 1, Files: []File{{Path: "empty", Source: "empty"}}}
	if err := ResolveFiles(f, dir); err != nil {
		t.Fatalf("ResolveFiles: %v", err)
	}
	if _, err := Compile(f); err != nil {
		t.Fatalf("Compile after resolving an empty source: %v", err)
	}
}

func TestResolveFilesMissingSource(t *testing.T) {
	f := &Fusefile{Version: 1, Files: []File{{Path: "a", Source: "nope.conf"}}}
	err := ResolveFiles(f, t.TempDir())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "files[0].source") {
		t.Errorf("error %q does not name the offending entry", err.Error())
	}
}
