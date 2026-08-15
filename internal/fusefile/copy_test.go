package fusefile

import (
	"strings"
	"testing"
)

func TestParseCopyBlock(t *testing.T) {
	src := `version: 1
copy:
  - from: ./start.sh
    to: ./start.sh
  - from: ./src
    to: /workspace/src
  - from: ../shared/config.yaml
    to: /etc/app/config.yaml
run: ./start.sh
`
	f, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(f.Copy) != 3 {
		t.Fatalf("copy: got %d entries, want 3", len(f.Copy))
	}
	want := []CopyEntry{
		{From: "./start.sh", To: "./start.sh"},
		{From: "./src", To: "/workspace/src"},
		{From: "../shared/config.yaml", To: "/etc/app/config.yaml"},
	}
	for i, entry := range want {
		if f.Copy[i] != entry {
			t.Errorf("copy[%d]: got %+v, want %+v", i, f.Copy[i], entry)
		}
	}
}

func TestCompileCopyResolvesGuestPaths(t *testing.T) {
	f := &Fusefile{
		Version:   1,
		Workspace: "/srv/app",
		Copy: []CopyEntry{
			{From: "./start.sh", To: "./start.sh"},
			{From: "./src", To: "src"},
			{From: "../shared/config.yaml", To: "/etc/app/config.yaml"},
		},
	}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	want := []CopySpec{
		{From: "./start.sh", To: "/srv/app/start.sh"},
		{From: "./src", To: "/srv/app/src"},
		{From: "../shared/config.yaml", To: "/etc/app/config.yaml"},
	}
	if len(c.Copy) != len(want) {
		t.Fatalf("copy: got %d entries, want %d", len(c.Copy), len(want))
	}
	for i, spec := range want {
		if c.Copy[i] != spec {
			t.Errorf("copy[%d]: got %+v, want %+v", i, c.Copy[i], spec)
		}
	}

	// the source is named, never read: nothing in the compiler touches the
	// filesystem, so a source that does not exist compiles fine and only
	// fails when the CLI walks it.
	if c.Copy[0].From != "./start.sh" {
		t.Errorf("copy[0].from was rewritten: %q", c.Copy[0].From)
	}
}

func TestCompileCopyDefaultsToWorkspace(t *testing.T) {
	c, err := Compile(&Fusefile{Version: 1, Copy: []CopyEntry{{From: "./app", To: "app"}}})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if got := c.Copy[0].To; got != "/workspace/app" {
		t.Errorf("copy[0].to: got %q, want %q", got, "/workspace/app")
	}
}

func TestCompileRejectsCopyCollisions(t *testing.T) {
	cases := []struct {
		name        string
		copies      []CopyEntry
		wantContain string
	}{
		{
			name: "duplicate guest path across spellings",
			copies: []CopyEntry{
				{From: "./a", To: "./app"},
				{From: "./b", To: "/workspace/app"},
			},
			wantContain: "which copy[0] already writes",
		},
		{
			name:        "reserved guest directory",
			copies:      []CopyEntry{{From: "./secrets", To: "/fuse/secrets.json"}},
			wantContain: "reserved for the guest agent",
		},
		{
			name:        "the reserved directory itself",
			copies:      []CopyEntry{{From: "./secrets", To: "/fuse"}},
			wantContain: "reserved for the guest agent",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(&Fusefile{Version: 1, Copy: tc.copies})
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantContain) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantContain)
			}
		})
	}
}

// A copy entry never reaches the startup script: it is uploaded next to the
// manifest instead, which is the whole reason it is not `files`.
func TestCompileCopyStaysOutOfTheStartupScript(t *testing.T) {
	c, err := Compile(&Fusefile{
		Version: 1,
		Copy:    []CopyEntry{{From: "./src", To: "/workspace/src"}},
		Run:     Command{Shell: "./start.sh"},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if strings.Contains(c.StartupScript, "/workspace/src") {
		t.Errorf("copy entry leaked into the startup script:\n%s", c.StartupScript)
	}
}

func TestParseRejectsBadCopyEntries(t *testing.T) {
	cases := []struct {
		name        string
		src         string
		wantContain string
	}{
		{
			name:        "missing from",
			src:         "version: 1\ncopy:\n  - to: ./app\n",
			wantContain: "copy[0].from: is required",
		},
		{
			name:        "blank from",
			src:         "version: 1\ncopy:\n  - from: \"  \"\n    to: ./app\n",
			wantContain: "copy[0].from: is required",
		},
		{
			name:        "missing to",
			src:         "version: 1\ncopy:\n  - from: ./app\n",
			wantContain: "copy[0].to: is required",
		},
		{
			name:        "traversing to",
			src:         "version: 1\ncopy:\n  - from: ./app\n    to: ../escape\n",
			wantContain: `copy[0].to: must not contain ".." segments`,
		},
		{
			name:        "newline in to",
			src:         "version: 1\ncopy:\n  - from: ./app\n    to: \"a\\nb\"\n",
			wantContain: "copy[0].to: must not contain newlines or NUL bytes",
		},
		{
			name:        "unknown field",
			src:         "version: 1\ncopy:\n  - from: ./app\n    to: ./app\n    mode: \"0755\"\n",
			wantContain: "field mode not found",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.src))
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantContain) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantContain)
			}
		})
	}
}
