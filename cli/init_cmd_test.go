package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/folsomintel/fuse/internal/fusefile"
)

func TestInitWritesParseableFusefile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "Fusefile")
	cfg := filepath.Join(dir, "config.yaml")

	_, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", cfg, "init", "-f", target})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read scaffold: %v", err)
	}

	f, err := fusefile.Parse(data)
	if err != nil {
		t.Fatalf("fusefile.Parse rejected scaffold: %v", err)
	}
	if f.Version != 1 {
		t.Errorf("version = %d, want 1", f.Version)
	}
	if !f.Cache.Enabled {
		t.Errorf("cache.enabled = false, want the scaffold to opt in")
	}
	if len(f.Build) != 1 {
		t.Errorf("build = %v, want the one uncommented step", f.Build)
	}
	if len(f.Setup) != 0 {
		t.Errorf("setup = %v, want the scaffold to use the build key instead", f.Setup)
	}
	svc, ok := f.Services["postgres"]
	if !ok {
		t.Fatalf("services.postgres missing from scaffold")
	}
	if svc.Image != "postgres:16" {
		t.Errorf("services.postgres.image = %q, want postgres:16", svc.Image)
	}

	// Parsing is not enough: the scaffold is what a new user runs `fuse up`
	// on, so it must survive compilation too.
	c, err := fusefile.Compile(f)
	if err != nil {
		t.Fatalf("fusefile.Compile rejected scaffold: %v", err)
	}
	if c.StartupTimeoutSeconds == 0 {
		t.Error("scaffold's startup_timeout did not compile to a bound")
	}

	// The top-level image names a host-baked rootfs, not an OCI ref. The
	// scaffold shipped `ghcr.io/acme/worker:latest` for a while, which fails
	// every create with "base image not found".
	if strings.Contains(string(data), "\nimage: ghcr.io/") {
		t.Error("scaffold sets a top-level image to an OCI reference; it names a host rootfs")
	}
}

// The scaffold's first line points editors at the published schema. It has to
// be line one to be honored, and the url has to be the schema's own $id or
// editors fetch a document that does not exist.
func TestInitScaffoldCarriesSchemaModeline(t *testing.T) {
	first, _, _ := strings.Cut(initScaffold, "\n")

	url, ok := strings.CutPrefix(first, "# yaml-language-server: $schema=")
	if !ok {
		t.Fatalf("scaffold does not start with the schema modeline, got %q", first)
	}

	data, err := os.ReadFile("../schema/fusefile-v1.json")
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var schema struct {
		ID string `json:"$id"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("schema is not well-formed json: %v", err)
	}
	if url != schema.ID {
		t.Errorf("modeline url = %q, want the schema's $id %q", url, schema.ID)
	}
}

func TestInitRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "Fusefile")
	cfg := filepath.Join(dir, "config.yaml")

	sentinel := "# sentinel, do not touch\n"
	if err := os.WriteFile(target, []byte(sentinel), 0o644); err != nil {
		t.Fatal(err)
	}

	root := newRootCmd()
	root.SetArgs([]string{"--config", cfg, "init", "-f", target})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("want already-exists error, got %v", err)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != sentinel {
		t.Fatalf("file content changed without --force: %q", data)
	}

	root2 := newRootCmd()
	root2.SetArgs([]string{"--config", cfg, "init", "-f", target, "--force"})
	if err := root2.Execute(); err != nil {
		t.Fatalf("execute with --force: %v", err)
	}

	data2, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data2) == sentinel {
		t.Fatalf("file content unchanged after --force")
	}
	if _, err := fusefile.Parse(data2); err != nil {
		t.Fatalf("fusefile.Parse rejected forced scaffold: %v", err)
	}
}
