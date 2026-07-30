package orchestrator

import (
	"context"
	"errors"
	"testing"
)

// cpuFleetHost is a plain firecracker host with no GPU, which is the only
// backend a seed snapshot can be booted on.
func cpuFleetHost(id string) Host {
	return Host{
		ID:      id,
		URL:     "http://" + id + ".test",
		Backend: BackendFirecracker,
		Capacity: HostCapacity{
			CPUs:      8,
			RamMB:     4096,
			StorageGB: 100,
			VMCount:   10,
		},
	}
}

// TestProvisionAndAssign_pinnedHostWins is the core of `fuse up --from-build`:
// a seed artifact is host-local, so the vm must land on the pinned host even
// when other hosts are registered and would otherwise be candidates.
func TestProvisionAndAssign_pinnedHostWins(t *testing.T) {
	stub := newStubProvider()
	fm := NewFleetManager(FleetConfig{Provider: stub, Prefix: "fuse-"})
	for _, id := range []string{"h1", "h2", "h3"} {
		if err := fm.RegisterHost(context.Background(), cpuFleetHost(id), stub); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}

	// every host is pinned in turn, so a scheduler that ignored the pin and
	// happened to favour one host cannot pass this by luck.
	for i, want := range []string{"h3", "h1", "h2"} {
		taskID := "task-" + want
		spec := Spec{CPUs: 1, RamMB: 256, SeedSnapshotID: "snap-1", PinnedHostID: want}
		info, err := fm.ProvisionAndAssign(context.Background(), taskID, spec, nil, nil, BootOptions{})
		if err != nil {
			t.Fatalf("provision %d pinned to %s: %v", i, want, err)
		}
		if info.HostID != want {
			t.Fatalf("vm pinned to %s landed on %q", want, info.HostID)
		}
	}
}

// TestProvisionAndAssign_pinnedHostMissingFails asserts the request fails
// rather than silently falling back to another host, which would boot the base
// image instead of the artifact the caller asked for.
func TestProvisionAndAssign_pinnedHostMissingFails(t *testing.T) {
	stub := newStubProvider()
	fm := NewFleetManager(FleetConfig{Provider: stub, Prefix: "fuse-"})
	if err := fm.RegisterHost(context.Background(), cpuFleetHost("h1"), stub); err != nil {
		t.Fatalf("register h1: %v", err)
	}

	spec := Spec{CPUs: 1, RamMB: 256, SeedSnapshotID: "snap-1", PinnedHostID: "gone"}
	_, err := fm.ProvisionAndAssign(context.Background(), "task-1", spec, nil, nil, BootOptions{})
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("err = %v, want ErrNoCapacity for an absent pinned host", err)
	}

	// the failed provision must not leave the vm tracked, or its task id stays
	// unusable for a retry.
	if _, ok := fm.GetVM("fuse-task-1"); ok {
		t.Fatal("failed pinned provision left the vm tracked")
	}
}

// TestProvisionAndAssign_unpinnedStillSchedules guards against the pin filter
// narrowing the candidate set when no pin was asked for.
func TestProvisionAndAssign_unpinnedStillSchedules(t *testing.T) {
	stub := newStubProvider()
	fm := NewFleetManager(FleetConfig{Provider: stub, Prefix: "fuse-"})
	if err := fm.RegisterHost(context.Background(), cpuFleetHost("h1"), stub); err != nil {
		t.Fatalf("register h1: %v", err)
	}

	info, err := fm.ProvisionAndAssign(context.Background(), "task-1",
		Spec{CPUs: 1, RamMB: 256}, nil, nil, BootOptions{})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if info.HostID != "h1" {
		t.Fatalf("vm landed on host %q, want h1", info.HostID)
	}
}
