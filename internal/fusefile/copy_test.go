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
