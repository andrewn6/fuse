package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// computerServer serves the computer sub-path, recording the last action body,
// and answers env GETs with a minimal environment so the detail view works.
func computerServer(t *testing.T, display string) (*httptest.Server, *map[string]any) {
	t.Helper()
	var lastAction map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/computer") && r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &lastAction); err != nil {
				t.Errorf("action body is not json: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"screenshot":%q}`, base64.StdEncoding.EncodeToString([]byte("pngbytes")))
		case strings.HasSuffix(r.URL.Path, "/computer") && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, display)
		default:
			fmt.Fprint(w, `{"id":"vm1","state":"running","task_id":"t","url":"10.0.0.4:19551","spec":{}}`)
		}
	}))
	return srv, &lastAction
}

func TestComputerClickSendsAction(t *testing.T) {
	srv, action := computerServer(t, `{}`)
	defer srv.Close()
	cfg := writeConfig(t, srv.URL)

	_, stderr, err := captureBoth(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", cfg, "computer", "click", "vm1", "512", "384"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	want := map[string]any{"action": "left_click", "coordinate": []any{float64(512), float64(384)}}
	if (*action)["action"] != want["action"] {
		t.Fatalf("sent action = %v", *action)
	}
	if fmt.Sprint((*action)["coordinate"]) != fmt.Sprint(want["coordinate"]) {
		t.Fatalf("sent coordinate = %v", (*action)["coordinate"])
	}
	if !strings.Contains(stderr, "clicked (512, 384)") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestComputerClickRejectsNonIntegers(t *testing.T) {
	srv, _ := computerServer(t, `{}`)
	defer srv.Close()
	cfg := writeConfig(t, srv.URL)

	root := newRootCmd()
	root.SetArgs([]string{"--config", cfg, "computer", "click", "vm1", "twelve", "10"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "x must be an integer") {
		t.Fatalf("err = %v, want an x complaint", err)
	}
}

func TestComputerTypeJoinsArgs(t *testing.T) {
	srv, action := computerServer(t, `{}`)
	defer srv.Close()
	cfg := writeConfig(t, srv.URL)

	_, _, err := captureBoth(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", cfg, "computer", "type", "vm1", "hello", "world"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if (*action)["action"] != "type" || (*action)["text"] != "hello world" {
		t.Fatalf("sent = %v", *action)
	}
}

func TestComputerKeySendsCombo(t *testing.T) {
	srv, action := computerServer(t, `{}`)
	defer srv.Close()
	cfg := writeConfig(t, srv.URL)

	_, _, err := captureBoth(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", cfg, "computer", "key", "vm1", "ctrl+shift+t"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if (*action)["action"] != "key" || (*action)["text"] != "ctrl+shift+t" {
		t.Fatalf("sent = %v", *action)
	}
}

func TestComputerScreenshotWritesFile(t *testing.T) {
	srv, action := computerServer(t, `{}`)
	defer srv.Close()
	cfg := writeConfig(t, srv.URL)
	out := filepath.Join(t.TempDir(), "shot.png")

	_, _, err := captureBoth(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", cfg, "computer", "screenshot", "vm1", "--out", out})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if (*action)["action"] != "screenshot" {
		t.Fatalf("sent = %v", *action)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(b) != "pngbytes" {
		t.Fatalf("file = %q, want the decoded screenshot", b)
	}
}

func TestComputerScreenshotStdout(t *testing.T) {
	srv, _ := computerServer(t, `{}`)
	defer srv.Close()
	cfg := writeConfig(t, srv.URL)

	out, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", cfg, "computer", "screenshot", "vm1", "--out", "-"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out != "pngbytes" {
		t.Fatalf("stdout = %q, want raw png bytes", out)
	}
}

// TestEnvGetRendersDesktop checks the detail view's desktop row: present with
// the live geometry when the display is up, absent when the environment has
// no desktop stack at all.
func TestEnvGetRendersDesktop(t *testing.T) {
	srv, _ := computerServer(t, `{"display":":1","up":true,"width":1280,"height":800}`)
	defer srv.Close()
	cfg := writeConfig(t, srv.URL)

	out, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", cfg, "environment", "get", "vm1"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "desktop") || !strings.Contains(out, "1280x800") {
		t.Errorf("output missing the desktop row:\n%s", out)
	}
}

func TestEnvGetNoDesktopRowWithoutDesktop(t *testing.T) {
	srv, _ := computerServer(t, `{"up":false,"error":"xdotool not installed; this is not a desktop image"}`)
	defer srv.Close()
	cfg := writeConfig(t, srv.URL)

	out, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", cfg, "environment", "get", "vm1"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if strings.Contains(out, "desktop") {
		t.Errorf("output has a desktop row for a non-desktop environment:\n%s", out)
	}
}

func TestEnvGetDesktopDownRow(t *testing.T) {
	srv, _ := computerServer(t, `{"display":":1","up":false,"error":"display :1 is not up (no socket at /tmp/.X11-unix/X1)"}`)
	defer srv.Close()
	cfg := writeConfig(t, srv.URL)

	out, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", cfg, "environment", "get", "vm1"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "desktop") || !strings.Contains(out, "down") {
		t.Errorf("output missing the down desktop row:\n%s", out)
	}
}
