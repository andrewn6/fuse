package main

import (
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
	if len(f.Setup) != 1 {
		t.Errorf("setup = %v, want the one uncommented step", f.Setup)
	}
	svc, ok := f.Services["postgres"]
	if !ok {
		t.Fatalf("services.postgres missing from scaffold")
	}
	if svc.Image != "postgres:16" {
		t.Errorf("services.postgres.image = %q, want postgres:16", svc.Image)
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
