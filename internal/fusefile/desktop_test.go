package fusefile

import (
	"reflect"
	"strings"
	"testing"
)

// parseDesktop parses a Fusefile that is just a version line plus the given
// desktop block, so each case reads as only the block under test.
func parseDesktop(t *testing.T, body string) (*Fusefile, error) {
	t.Helper()
	return Parse([]byte("version: 1\n" + body))
}

func TestParseDesktop(t *testing.T) {
	f, err := parseDesktop(t, `
desktop:
  width: 1280
  height: 800
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := &Desktop{Width: 1280, Height: 800}
	if !reflect.DeepEqual(f.Desktop, want) {
		t.Fatalf("desktop = %+v, want %+v", f.Desktop, want)
	}
}

// an absent block must stay absent: nil is what tells every layer below that
// the author asked for no desktop, and a zero-valued struct would not.
func TestParseDesktopAbsentIsNil(t *testing.T) {
	f, err := parseDesktop(t, "run: echo hi\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.Desktop != nil {
		t.Fatalf("desktop = %+v, want nil", f.Desktop)
	}
}

func TestParseDesktopRejections(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "missing width",
			body: "desktop:\n  height: 800\n",
			want: "desktop.width: must be between",
		},
		{
			name: "missing height",
			body: "desktop:\n  width: 1280\n",
			want: "desktop.height: must be between",
		},
		{
			name: "width below floor",
			body: "desktop:\n  width: 100\n  height: 800\n",
			want: "desktop.width: must be between 320 and 3840, got 100",
		},
		{
			name: "height above ceiling",
			body: "desktop:\n  width: 1280\n  height: 9000\n",
			want: "desktop.height: must be between 320 and 3840, got 9000",
		},
		{
			name: "negative width",
			body: "desktop:\n  width: -1\n  height: 800\n",
			want: "desktop.width: must be between",
		},
		{
			name: "unknown field",
			body: "desktop:\n  width: 1280\n  height: 800\n  display: 2\n",
			want: "display",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseDesktop(t, tc.body)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestCompileDesktop(t *testing.T) {
	f, err := parseDesktop(t, `
desktop:
  width: 1920
  height: 1080
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	want := &DesktopSpec{Width: 1920, Height: 1080}
	if !reflect.DeepEqual(c.Desktop, want) {
		t.Fatalf("compiled desktop = %+v, want %+v", c.Desktop, want)
	}
}

// an absent block compiles to nil, which is what tells the orchestrator to
// write no /fuse/desktop.json and the image to keep its baked default.
func TestCompileDesktopAbsentIsNil(t *testing.T) {
	f, err := parseDesktop(t, "run: echo hi\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if c.Desktop != nil {
		t.Fatalf("compiled desktop = %+v, want nil", c.Desktop)
	}
}
