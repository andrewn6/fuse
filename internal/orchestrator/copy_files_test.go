package orchestrator

import (
	"context"
	"strings"
	"testing"
)

// A caller file (a Fusefile `copy` entry) must be in the guest before the
// startup script runs, since setup and run are what read it.
func TestBootUploadsCallerFilesBeforeStartupScript(t *testing.T) {
	p := newBootMockProvider()
	opts := BootOptions{
		StartupScript: "cat /workspace/src/main.go",
		Files:         map[string][]byte{"/workspace/src/main.go": []byte("package main\n")},
	}

	if _, err := Boot(context.Background(), p, Spec{Name: "vm-copy"}, []byte(`{"version":"1"}`), nil, opts, nil); err != nil {
		t.Fatalf("Boot: %v", err)
	}

	env := p.envs["vm-copy"]
	if got := string(env.uploads["/workspace/src/main.go"]); got != "package main\n" {
		t.Fatalf("caller file in guest = %q, want %q", got, "package main\n")
	}
	if env.uploadsAtFirstExec == 0 {
		t.Fatal("the startup script ran before any file was uploaded")
	}
	if _, ok := env.uploads[fuseManifestPath]; !ok {
		t.Fatal("caller files displaced the profile's manifest upload")
	}
}

// A restored environment gets its caller files again: the restore path
// re-uploads AgentSpec.Files, and the artifact it restored from may predate an
// edit to them.
func TestBootRestoreReuploadsCallerFiles(t *testing.T) {
	p := newBootMockProvider()
	env := &bootMockEnv{name: "vm-restore", url: "http://vm-restore", checkpoints: []Checkpoint{{ID: "cp-1"}}}
	p.envs["vm-restore"] = env

	opts := BootOptions{Files: map[string][]byte{"/workspace/app.py": []byte("print(1)\n")}}
	result, err := Boot(context.Background(), p, Spec{Name: "vm-restore"}, []byte(`{"version":"1"}`), nil, opts, nil)
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	if !result.FromCache {
		t.Fatal("expected a restore, got a fresh provision")
	}
	if got := string(env.uploads["/workspace/app.py"]); got != "print(1)\n" {
		t.Fatalf("caller file after restore = %q, want %q", got, "print(1)\n")
	}
}

// A caller can name any guest path it likes; the profile's own files still
// win. The API rejects a path under /fuse before it ever reaches Boot, so this
// is the second half of that guarantee, not the first.
func TestFusedAgentSpecCallerFilesCannotOverwriteProfileFiles(t *testing.T) {
	manifest := []byte(`{"version":"1","machine":{"workspace":"/workspace"},"services":{}}`)
	opts := BootOptions{Files: map[string][]byte{
		fuseSecretsPath:         []byte(`{"stolen":"yes"}`),
		fuseManifestPath:        []byte(`{"version":"evil"}`),
		"/workspace/src/app.go": []byte("package main\n"),
	}}

	spec := FusedAgentSpec(manifest, map[string]string{"PG": "hunter2"}, nil, opts)

	if got := string(spec.Files[fuseSecretsPath]); strings.Contains(got, "stolen") {
		t.Fatalf("caller overwrote %s: %s", fuseSecretsPath, got)
	}
	if got := string(spec.Files[fuseSecretsPath]); !strings.Contains(got, "hunter2") {
		t.Fatalf("%s = %s, want the resolved secret map", fuseSecretsPath, got)
	}
	if got := string(spec.Files[fuseManifestPath]); got != string(manifest) {
		t.Fatalf("caller overwrote %s: %s", fuseManifestPath, got)
	}
	if got := string(spec.Files["/workspace/src/app.go"]); got != "package main\n" {
		t.Fatalf("caller file was dropped: %q", got)
	}
}

// A fork inherits the source's disk, so it must not be handed caller files a
// second time: ForkEnvironment uploads credentials and nothing else.
func TestForkEnvironmentUploadsOnlyCredentials(t *testing.T) {
	provider := newCredForkProvider()
	fm := NewFleetManager(FleetConfig{
		Provider:           provider,
		Prefix:             "fuse-",
		TokenEncryptionKey: testEncryptionKey(),
	})
	srcID := provisionSnapshotTestVM(t, fm, "task-fork-copy")

	newID, err := fm.ForkEnvironment(context.Background(), srcID, ForkOptions{})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}

	credentials := map[string]bool{fuseTLSCertPath: true, fuseTLSKeyPath: true, fuseAuthTokenPath: true}
	uploaded := provider.envs[newID].uploadedPaths
	if len(uploaded) != len(credentials) {
		t.Fatalf("fork uploaded %v, want only the credential files", uploaded)
	}
	for _, path := range uploaded {
		if !credentials[path] {
			t.Errorf("fork uploaded %s; a fork inherits the source disk and should only re-mint credentials", path)
		}
	}
}
