package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDesktopSpec(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "desktop.json")

	// missing file is the ordinary "no desktop declared" case
	spec, err := loadDesktopSpec(path)
	if err != nil || spec != nil {
		t.Fatalf("missing file: spec=%+v err=%v, want nil/nil", spec, err)
	}

	// empty path means the feature is off entirely
	spec, err = loadDesktopSpec("")
	if err != nil || spec != nil {
		t.Fatalf("empty path: spec=%+v err=%v, want nil/nil", spec, err)
	}

	if err := os.WriteFile(path, []byte(`{"width":1280,"height":800}`), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, err = loadDesktopSpec(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if spec.Width != 1280 || spec.Height != 800 {
		t.Fatalf("spec = %+v, want 1280x800", spec)
	}

	// a file that exists but is unusable is an error, not a silent default
	for name, body := range map[string]string{
		"malformed": `{`,
		"zeroes":    `{"width":0,"height":800}`,
		"negative":  `{"width":1280,"height":-1}`,
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := loadDesktopSpec(path); err == nil {
			t.Fatalf("%s desktop.json should be an error", name)
		} else if !strings.Contains(err.Error(), "desktop") {
			t.Fatalf("%s error should name the file: %v", name, err)
		}
	}
}

func TestNeedsDisplayRestart(t *testing.T) {
	spec := &desktopSpec{Width: 1280, Height: 800}
	if needsDisplayRestart(nil, 1024, 768) {
		t.Fatal("nil spec must never restart the display")
	}
	if needsDisplayRestart(spec, 1280, 800) {
		t.Fatal("matching geometry must not restart the display")
	}
	if !needsDisplayRestart(spec, 1024, 768) {
		t.Fatal("mismatched geometry must restart the display")
	}
}
