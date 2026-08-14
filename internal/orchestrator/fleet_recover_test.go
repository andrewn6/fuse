package orchestrator

import (
	"context"
	"testing"
)

// recoverFleet builds a fresh FleetManager wired to the given store and a
// host provider factory that hands back the supplied stub, mirroring how a
// real orchestrator reattaches per-host providers on restart. Every
// recovered host resolves to the same stub, which is fine because the tests
// register a single host.
func recoverFleet(store StateStore, stub Provider) *FleetManager {
	return NewFleetManager(FleetConfig{
		Provider:   stub,
		Prefix:     "gpu-",
		StateStore: store,
		HostProviderFactory: func(_ string, _ string, _ HostBackend) Provider {
			return stub
		},
	})
}

// seedRunningVM persists a Running VM assigned to hostID and pre-seeds the
// stub provider with its env so recovery keeps it live (a Running VM whose
// env is missing from the provider is demoted to Destroying).
func seedRunningVM(t *testing.T, store StateStore, stub *stubLoadProvider, id, hostID string, spec Spec) {
	t.Helper()
	stub.mu.Lock()
	stub.envs[id] = &stubLoadEnv{name: id, url: "http://" + id + ".test"}
	stub.mu.Unlock()
	spec.Name = id
	if err := store.UpsertVM(context.Background(), VMRecord{
		ID:     id,
		HostID: hostID,
		State:  VMStateRunning,
		URL:    "http://" + id + ".test",
		Spec:   spec,
	}); err != nil {
		t.Fatalf("seed vm %s: %v", id, err)
	}
}

// TestRecoverRederivesGPUAllocationFromLiveVMs checks that a restart derives
// a per-device host's Allocated (uuid set + scalar count) purely from the
// live VM bindings.
func TestRecoverRederivesGPUAllocationFromLiveVMs(t *testing.T) {
	store := NewMemoryStateStore()
	stub := newStubProvider()

	// first boot: register a two-device host and persist a live VM bound to
	// one of its devices.
	seed := recoverFleet(store, stub)
	host := deviceFleetHost("h1",
		GPUDevice{UUID: "gpu-a", Model: "a100"},
		GPUDevice{UUID: "gpu-b", Model: "a100"},
	)
	if err := seed.RegisterHost(context.Background(), host, stub); err != nil {
		t.Fatal(err)
	}
	seedRunningVM(t, store, stub, "gpu-vm-1", "h1", Spec{
		CPUs: 2, RamMB: 512, StorageGB: 10, GPUs: 1, GPUKind: "a100",
		GPUUUIDs: []string{"gpu-a"},
	})

	// restart: a fresh manager over the same store recovers state.
	fm := recoverFleet(store, stub)
	if err := fm.recoverState(context.Background()); err != nil {
		t.Fatalf("recoverState: %v", err)
	}

	h := findHost(t, fm, "h1")
	if h.Allocated.CPUs != 2 || h.Allocated.RamMB != 512 || h.Allocated.StorageGB != 10 || h.Allocated.VMCount != 1 {
		t.Errorf("scalar allocated = %+v, want cpus=2 ram=512 storage=10 vmcount=1", h.Allocated)
	}
	if h.Allocated.GPUs != 1 {
		t.Errorf("allocated GPUs = %d, want 1", h.Allocated.GPUs)
	}
	if len(h.Allocated.GPUDeviceUUIDs) != 1 || h.Allocated.GPUDeviceUUIDs[0] != "gpu-a" {
		t.Errorf("allocated uuids = %v, want [gpu-a]", h.Allocated.GPUDeviceUUIDs)
	}
}

// TestRecoverHealsInjectedGPUDrift persists a host whose Allocated counter is
// deliberately wrong (drift from a crash mid write-behind) alongside a single
// live VM, and asserts recovery re-derives the correct values from the VM.
func TestRecoverHealsInjectedGPUDrift(t *testing.T) {
	store := NewMemoryStateStore()
	stub := newStubProvider()

	// persist a host record with a bogus Allocated: it claims both devices
	// are bound and 4 cpus in use, but only one live VM (2 cpus, gpu-a)
	// actually exists.
	drifted := HostRecord{
		ID:      "h1",
		URL:     "http://h1.test",
		Backend: BackendQEMU,
		State:   HostActive,
		Capacity: HostCapacity{
			CPUs: 8, RamMB: 4096, StorageGB: 100, VMCount: 10,
			GPUs:    2,
			GPUKind: "a100",
			GPUDevices: []GPUDevice{
				{UUID: "gpu-a", Model: "a100"},
				{UUID: "gpu-b", Model: "a100"},
			},
		},
		Allocated: HostCapacity{
			CPUs: 4, RamMB: 2048, StorageGB: 40, VMCount: 2,
			GPUs:           2,
			GPUDeviceUUIDs: []string{"gpu-a", "gpu-b"},
		},
	}
	if err := store.UpsertHost(context.Background(), drifted); err != nil {
		t.Fatal(err)
	}
	seedRunningVM(t, store, stub, "gpu-vm-1", "h1", Spec{
		CPUs: 2, RamMB: 512, StorageGB: 10, GPUs: 1, GPUKind: "a100",
		GPUUUIDs: []string{"gpu-a"},
	})

	fm := recoverFleet(store, stub)
	if err := fm.recoverState(context.Background()); err != nil {
		t.Fatalf("recoverState: %v", err)
	}

	h := findHost(t, fm, "h1")
	if h.Allocated.CPUs != 2 || h.Allocated.RamMB != 512 || h.Allocated.StorageGB != 10 || h.Allocated.VMCount != 1 {
		t.Errorf("healed scalar allocated = %+v, want cpus=2 ram=512 storage=10 vmcount=1 (drift not healed)", h.Allocated)
	}
	if h.Allocated.GPUs != 1 {
		t.Errorf("healed GPUs = %d, want 1 (drift not healed)", h.Allocated.GPUs)
	}
	if len(h.Allocated.GPUDeviceUUIDs) != 1 || h.Allocated.GPUDeviceUUIDs[0] != "gpu-a" {
		t.Errorf("healed uuids = %v, want [gpu-a] (stale gpu-b not released)", h.Allocated.GPUDeviceUUIDs)
	}
}

// TestRecoverExcludesDestroyingVMFromRecompute checks that a VM demoted to
// Destroying during recovery does not contribute to the recomputed
// allocation, so its GPU device is treated as free again. The Destroying
// demotion is triggered by leaving its env out of the provider.
func TestRecoverExcludesDestroyingVMFromRecompute(t *testing.T) {
	store := NewMemoryStateStore()
	stub := newStubProvider()

	seed := recoverFleet(store, stub)
	host := deviceFleetHost("h1",
		GPUDevice{UUID: "gpu-a", Model: "a100"},
		GPUDevice{UUID: "gpu-b", Model: "a100"},
	)
	if err := seed.RegisterHost(context.Background(), host, stub); err != nil {
		t.Fatal(err)
	}

	// live VM: env present, stays Running and counts toward the recompute.
	seedRunningVM(t, store, stub, "gpu-live", "h1", Spec{
		CPUs: 2, RamMB: 512, GPUs: 1, GPUKind: "a100",
		GPUUUIDs: []string{"gpu-a"},
	})

	// doomed VM: persisted as Running and bound to gpu-b, but its env is
	// absent from the provider, so recovery demotes it to Destroying. It
	// must be excluded from the sums.
	if err := store.UpsertVM(context.Background(), VMRecord{
		ID:     "gpu-doomed",
		HostID: "h1",
		State:  VMStateRunning,
		Spec: Spec{
			Name: "gpu-doomed", CPUs: 2, RamMB: 512, GPUs: 1, GPUKind: "a100",
			GPUUUIDs: []string{"gpu-b"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	fm := recoverFleet(store, stub)
	if err := fm.recoverState(context.Background()); err != nil {
		t.Fatalf("recoverState: %v", err)
	}

	// sanity: the doomed VM was actually demoted.
	if info, ok := fm.GetVM("gpu-doomed"); !ok || info.State != VMStateDestroying {
		t.Fatalf("gpu-doomed state = %v ok=%v, want destroying", info.State, ok)
	}

	h := findHost(t, fm, "h1")
	if h.Allocated.CPUs != 2 || h.Allocated.VMCount != 1 {
		t.Errorf("allocated = %+v, want only the live vm counted (cpus=2 vmcount=1)", h.Allocated)
	}
	if h.Allocated.GPUs != 1 {
		t.Errorf("allocated GPUs = %d, want 1 (destroying vm's device excluded)", h.Allocated.GPUs)
	}
	if len(h.Allocated.GPUDeviceUUIDs) != 1 || h.Allocated.GPUDeviceUUIDs[0] != "gpu-a" {
		t.Errorf("allocated uuids = %v, want [gpu-a] (gpu-b freed with the destroying vm)", h.Allocated.GPUDeviceUUIDs)
	}
}

// TestRecoverRederivesMIGAllocationFromLiveVMs checks that a restart derives a
// per-instance MIG host's Allocated (bound uuid set + derived count map)
// purely from the live VM bindings, not a persisted counter.
func TestRecoverRederivesMIGAllocationFromLiveVMs(t *testing.T) {
	store := NewMemoryStateStore()
	stub := newStubProvider()

	seed := recoverFleet(store, stub)
	host := migInstanceFleetHost("h1",
		MIGInstance{UUID: "m1", Profile: "1g.10gb", Kind: "a100", ParentGPUUUID: "gpu-a"},
		MIGInstance{UUID: "m2", Profile: "1g.10gb", Kind: "a100", ParentGPUUUID: "gpu-a"},
		MIGInstance{UUID: "m3", Profile: "2g.20gb", Kind: "a100", ParentGPUUUID: "gpu-a"},
	)
	if err := seed.RegisterHost(context.Background(), host, stub); err != nil {
		t.Fatal(err)
	}
	seedRunningVM(t, store, stub, "mig-vm-1", "h1", Spec{
		CPUs: 2, RamMB: 512, StorageGB: 10, GPUs: 1, GPUKind: "a100",
		GPUProfile:       "1g.10gb",
		MIGInstanceUUIDs: []string{"m1"},
	})

	fm := recoverFleet(store, stub)
	if err := fm.recoverState(context.Background()); err != nil {
		t.Fatalf("recoverState: %v", err)
	}

	h := findHost(t, fm, "h1")
	if len(h.Allocated.MIGInstanceUUIDs) != 1 || h.Allocated.MIGInstanceUUIDs[0] != "m1" {
		t.Errorf("allocated mig uuids = %v, want [m1]", h.Allocated.MIGInstanceUUIDs)
	}
	if h.Allocated.MIGProfiles["1g.10gb"] != 1 {
		t.Errorf("MIGProfiles[1g.10gb] = %d, want 1 (derived from bound uuid)", h.Allocated.MIGProfiles["1g.10gb"])
	}
	// whole-device pool is untouched on a MIG-only host.
	if h.Allocated.GPUs != 0 {
		t.Errorf("allocated GPUs = %d, want 0 (mig vm must not count as whole device)", h.Allocated.GPUs)
	}
}

// TestRecoverHealsInjectedMIGDrift persists a per-instance host whose
// Allocated counter claims a stale bound uuid (drift from a crash mid
// write-behind) alongside a single live VM bound to a different instance, and
// asserts recovery re-derives the bound set from the VM so the stale uuid is
// released.
func TestRecoverHealsInjectedMIGDrift(t *testing.T) {
	store := NewMemoryStateStore()
	stub := newStubProvider()

	drifted := HostRecord{
		ID:      "h1",
		URL:     "http://h1.test",
		Backend: BackendQEMU,
		State:   HostActive,
		Capacity: HostCapacity{
			CPUs: 8, RamMB: 4096, StorageGB: 100, VMCount: 10,
			MIGInstances: []MIGInstance{
				{UUID: "m1", Profile: "1g.10gb", Kind: "a100"},
				{UUID: "m2", Profile: "1g.10gb", Kind: "a100"},
			},
			MIGProfiles: map[string]int{"1g.10gb": 2},
		},
		Allocated: HostCapacity{
			CPUs: 4, RamMB: 2048, VMCount: 2,
			MIGInstanceUUIDs: []string{"m2"}, // stale: no live vm holds m2
			MIGProfiles:      map[string]int{"1g.10gb": 1},
		},
	}
	if err := store.UpsertHost(context.Background(), drifted); err != nil {
		t.Fatal(err)
	}
	seedRunningVM(t, store, stub, "mig-vm-1", "h1", Spec{
		CPUs: 2, RamMB: 512, StorageGB: 10, GPUs: 1, GPUKind: "a100",
		GPUProfile:       "1g.10gb",
		MIGInstanceUUIDs: []string{"m1"},
	})

	fm := recoverFleet(store, stub)
	if err := fm.recoverState(context.Background()); err != nil {
		t.Fatalf("recoverState: %v", err)
	}

	h := findHost(t, fm, "h1")
	if len(h.Allocated.MIGInstanceUUIDs) != 1 || h.Allocated.MIGInstanceUUIDs[0] != "m1" {
		t.Errorf("healed mig uuids = %v, want [m1] (stale m2 released)", h.Allocated.MIGInstanceUUIDs)
	}
	if h.Allocated.MIGProfiles["1g.10gb"] != 1 {
		t.Errorf("healed MIGProfiles[1g.10gb] = %d, want 1", h.Allocated.MIGProfiles["1g.10gb"])
	}
}

// TestRecoverRebuildsLegacyMIGCountMap checks that a legacy count-map host
// (no per-instance inventory) rebuilds MIGProfiles from the live VM
// profile+count, so a persisted counter drift heals there too.
func TestRecoverRebuildsLegacyMIGCountMap(t *testing.T) {
	store := NewMemoryStateStore()
	stub := newStubProvider()

	seed := recoverFleet(store, stub)
	host := gpuFleetHost("h1", 0, "a100")
	host.Capacity.MIGProfiles = map[string]int{"1g.10gb": 4}
	if err := seed.RegisterHost(context.Background(), host, stub); err != nil {
		t.Fatal(err)
	}
	seedRunningVM(t, store, stub, "mig-vm-1", "h1", Spec{
		CPUs: 2, RamMB: 512, StorageGB: 10, GPUs: 2, GPUKind: "a100",
		GPUProfile: "1g.10gb",
	})

	fm := recoverFleet(store, stub)
	if err := fm.recoverState(context.Background()); err != nil {
		t.Fatalf("recoverState: %v", err)
	}

	h := findHost(t, fm, "h1")
	if h.Allocated.MIGProfiles["1g.10gb"] != 2 {
		t.Errorf("MIGProfiles[1g.10gb] = %d, want 2 (rebuilt from vm count)", h.Allocated.MIGProfiles["1g.10gb"])
	}
	if len(h.Allocated.MIGInstanceUUIDs) != 0 {
		t.Errorf("MIGInstanceUUIDs = %v, want empty on a count-map host", h.Allocated.MIGInstanceUUIDs)
	}
}

// TestRecoverReattachesProvisioningInterruptedByCrash verifies the core #68
// fix: a VM persisted in VMStateProvisioning when the orchestrator crashed
// must be re-attached to its still-live provider sandbox (and promoted to
// Running) rather than destroyed. The sandbox survives the orchestrator
// crash, so provider.Get succeeds and the in-flight work — including the
// uploadfiles/retry that previously 404'd against a destroyed sandbox —
// continues against the live environment, and its task stays assigned.
func TestRecoverReattachesProvisioningInterruptedByCrash(t *testing.T) {
	store := NewMemoryStateStore()
	stub := newStubProvider()

	vmID := "gpu-vm-1"
	// The sandbox survived the crash: it already exists in the provider.
	stub.mu.Lock()
	stub.envs[vmID] = &stubLoadEnv{name: vmID, url: "http://" + vmID + ".test"}
	stub.mu.Unlock()
	if err := store.UpsertVM(context.Background(), VMRecord{
		ID:     vmID,
		State:  VMStateProvisioning,
		TaskID: "task-1",
		Spec:   Spec{Name: vmID, CPUs: 2, RamMB: 512, StorageGB: 10, GPUs: 1, GPUKind: "a100"},
	}); err != nil {
		t.Fatalf("seed vm: %v", err)
	}

	// Restart: a fresh manager recovers state over the same store.
	fm := recoverFleet(store, stub)
	if err := fm.recoverState(context.Background()); err != nil {
		t.Fatalf("recoverState: %v", err)
	}

	fm.mu.RLock()
	v, ok := fm.vms[vmID]
	fm.mu.RUnlock()
	if !ok {
		t.Fatal("vm not recovered into fleet")
	}
	if v.state != VMStateRunning {
		t.Errorf("state = %q, want %s (in-flight provision must be re-attached, not destroyed)", v.state, VMStateRunning)
	}
	if v.taskID != "task-1" {
		t.Errorf("taskID = %q, want task retained for the re-attached vm", v.taskID)
	}
	stub.mu.Lock()
	_, stillAlive := stub.envs[vmID]
	stub.mu.Unlock()
	if !stillAlive {
		t.Error("provider sandbox was destroyed; a re-attached in-flight provision must keep its sandbox alive")
	}
}

// TestRecoverProvisioningWithoutSandboxIsCleanedUp checks that the Provisioning
// re-attach branch still falls back to the pre-existing teardown when the
// sandbox never materialised in the provider: the VM is marked Destroying and
// its task is released for retry.
func TestRecoverProvisioningWithoutSandboxIsCleanedUp(t *testing.T) {
	store := NewMemoryStateStore()
	stub := newStubProvider()

	vmID := "gpu-vm-1"
	// Sandbox NOT seeded: provider.Get must fail.
	if err := store.UpsertVM(context.Background(), VMRecord{
		ID:     vmID,
		State:  VMStateProvisioning,
		TaskID: "task-1",
		Spec:   Spec{Name: vmID},
	}); err != nil {
		t.Fatalf("seed vm: %v", err)
	}

	fm := recoverFleet(store, stub)
	if err := fm.recoverState(context.Background()); err != nil {
		t.Fatalf("recoverState: %v", err)
	}

	fm.mu.RLock()
	v, ok := fm.vms[vmID]
	fm.mu.RUnlock()
	if !ok {
		t.Fatal("vm not recovered into fleet")
	}
	if v.state != VMStateDestroying {
		t.Errorf("state = %q, want %s (lost provision must be cleaned up for retry)", v.state, VMStateDestroying)
	}
	if v.taskID != "" {
		t.Errorf("taskID = %q, want empty (task must be released for retry)", v.taskID)
	}
}
