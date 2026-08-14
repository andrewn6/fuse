package orchestrator

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func gpuFleetHost(id string, gpus int, kind string) Host {
	return Host{
		ID:      id,
		URL:     "http://" + id + ".test",
		Backend: BackendQEMU,
		Capacity: HostCapacity{
			CPUs:      8,
			RamMB:     4096,
			StorageGB: 100,
			VMCount:   10,
			GPUs:      gpus,
			GPUKind:   kind,
		},
	}
}

func deviceFleetHost(id string, devices ...GPUDevice) Host {
	h := gpuFleetHost(id, len(devices), "")
	h.Capacity.GPUDevices = devices
	if len(devices) > 0 {
		h.Capacity.GPUKind = devices[0].Model
	}
	return h
}

func TestAllocateOnHost_bindsConcreteUUIDsAndReleasesThem(t *testing.T) {
	stub := newStubProvider()
	fm := NewFleetManager(FleetConfig{Provider: stub, Prefix: "gpu-"})
	host := deviceFleetHost("h1",
		GPUDevice{UUID: "gpu-a", Model: "a100"},
		GPUDevice{UUID: "gpu-b", Model: "a100"},
	)
	if err := fm.RegisterHost(context.Background(), host, stub); err != nil {
		t.Fatal(err)
	}

	v := &vm{spec: Spec{CPUs: 1, RamMB: 256, GPUs: 1, GPUKind: "a100"}}
	fm.mu.Lock()
	fm.allocateOnHost("h1", v)
	fm.mu.Unlock()

	if len(v.spec.GPUUUIDs) != 1 {
		t.Fatalf("v.spec.GPUUUIDs = %v, want one bound uuid", v.spec.GPUUUIDs)
	}
	bound := v.spec.GPUUUIDs[0]
	if bound != "gpu-a" && bound != "gpu-b" {
		t.Errorf("bound uuid %q not one of the host devices", bound)
	}
	h := findHost(t, fm, "h1")
	if h.Allocated.GPUs != 1 || len(h.Allocated.GPUDeviceUUIDs) != 1 || h.Allocated.GPUDeviceUUIDs[0] != bound {
		t.Errorf("host allocated = %+v, want the single bound uuid", h.Allocated)
	}

	// a second env must bind the OTHER device, not reuse the taken one.
	v2 := &vm{spec: Spec{CPUs: 1, RamMB: 256, GPUs: 1, GPUKind: "a100"}}
	fm.mu.Lock()
	fm.allocateOnHost("h1", v2)
	fm.mu.Unlock()
	if len(v2.spec.GPUUUIDs) != 1 || v2.spec.GPUUUIDs[0] == bound {
		t.Errorf("second bind = %v, want the other device (not %q)", v2.spec.GPUUUIDs, bound)
	}

	// deallocate the first vm: only its uuid is released.
	fm.mu.Lock()
	fm.deallocateOnHost("h1", v.spec)
	fm.mu.Unlock()
	h = findHost(t, fm, "h1")
	if h.Allocated.GPUs != 1 || len(h.Allocated.GPUDeviceUUIDs) != 1 || h.Allocated.GPUDeviceUUIDs[0] != v2.spec.GPUUUIDs[0] {
		t.Errorf("after dealloc host allocated = %+v, want only the second uuid", h.Allocated)
	}
}

func findHost(t *testing.T, fm *FleetManager, id string) Host {
	t.Helper()
	for _, h := range fm.ListHosts() {
		if h.ID == id {
			return h
		}
	}
	t.Fatalf("host %s not found", id)
	return Host{}
}

func TestAllocateOnHost_incrementsGPUCounter(t *testing.T) {
	stub := newStubProvider()
	fm := NewFleetManager(FleetConfig{Provider: stub, Prefix: "gpu-"})
	if err := fm.RegisterHost(context.Background(), gpuFleetHost("h1", 2, "a100"), stub); err != nil {
		t.Fatal(err)
	}

	fm.mu.Lock()
	fm.allocateOnHost("h1", &vm{spec: Spec{CPUs: 1, RamMB: 256, GPUs: 1}})
	fm.mu.Unlock()

	if got := findHost(t, fm, "h1").Allocated.GPUs; got != 1 {
		t.Errorf("Allocated.GPUs = %d, want 1", got)
	}
}

func TestDeallocateOnHost_decrementsGPUCounter(t *testing.T) {
	stub := newStubProvider()
	fm := NewFleetManager(FleetConfig{Provider: stub, Prefix: "gpu-"})
	if err := fm.RegisterHost(context.Background(), gpuFleetHost("h1", 2, "a100"), stub); err != nil {
		t.Fatal(err)
	}

	fm.mu.Lock()
	fm.allocateOnHost("h1", &vm{spec: Spec{CPUs: 1, RamMB: 256, GPUs: 2}})
	fm.deallocateOnHost("h1", Spec{CPUs: 1, RamMB: 256, GPUs: 1})
	fm.mu.Unlock()

	if got := findHost(t, fm, "h1").Allocated.GPUs; got != 1 {
		t.Errorf("Allocated.GPUs = %d, want 1", got)
	}
}

func TestDeallocateOnHost_clampsGPUCounterAtZero(t *testing.T) {
	stub := newStubProvider()
	fm := NewFleetManager(FleetConfig{Provider: stub, Prefix: "gpu-"})
	if err := fm.RegisterHost(context.Background(), gpuFleetHost("h1", 1, "a100"), stub); err != nil {
		t.Fatal(err)
	}

	// deallocate without a prior allocation must clamp at zero,
	// matching the cpu/ram counter behavior
	fm.mu.Lock()
	fm.deallocateOnHost("h1", Spec{CPUs: 1, RamMB: 256, GPUs: 1})
	fm.mu.Unlock()

	if got := findHost(t, fm, "h1").Allocated.GPUs; got != 0 {
		t.Errorf("Allocated.GPUs = %d, want 0 (clamped)", got)
	}
}

func TestSingleGPUHost_secondGPUEnvHasNoCapacity(t *testing.T) {
	stub := newStubProvider()
	fm := NewFleetManager(FleetConfig{Provider: stub, Prefix: "gpu-"})
	if err := fm.RegisterHost(context.Background(), gpuFleetHost("h1", 1, "a100"), stub); err != nil {
		t.Fatal(err)
	}

	// first env takes the only device
	fm.mu.Lock()
	fm.allocateOnHost("h1", &vm{spec: Spec{CPUs: 1, RamMB: 256, GPUs: 1}})
	fm.mu.Unlock()

	// second gpu env must not fit on the same host
	_, _, err := Schedule(Spec{CPUs: 1, RamMB: 256, StorageGB: 10, GPUs: 1}, fm.activeHosts(), PlacementSpread)
	if !errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, want ErrNoCapacity (gpu already allocated)", err)
	}
}

func TestProvisionAndDestroy_gpuEnvRoundTripsAllocation(t *testing.T) {
	defaultProvider := newMockProvider()
	hostProvider := newMockProvider()
	fm := NewFleetManager(FleetConfig{Provider: defaultProvider, Prefix: "gpu-"})
	ctx := context.Background()
	if err := fm.RegisterHost(ctx, gpuFleetHost("h1", 1, "a100"), hostProvider); err != nil {
		t.Fatal(err)
	}

	manifest := []byte(`{"version":"1","services":{}}`)
	info, err := fm.ProvisionAndAssign(ctx, "gpu-task", Spec{CPUs: 1, RamMB: 256, GPUs: 1}, manifest, nil, BootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := findHost(t, fm, "h1").Allocated.GPUs; got != 1 {
		t.Errorf("Allocated.GPUs after boot = %d, want 1", got)
	}
	if defaultProvider.count() != 0 || hostProvider.count() != 1 {
		t.Fatalf("provider counts after boot = default %d, host %d; want 0, 1", defaultProvider.count(), hostProvider.count())
	}

	if err := fm.DestroyVM(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
	if got := findHost(t, fm, "h1").Allocated.GPUs; got != 0 {
		t.Errorf("Allocated.GPUs after destroy = %d, want 0", got)
	}
	if hostProvider.count() != 0 {
		t.Fatalf("host provider count after destroy = %d, want 0", hostProvider.count())
	}
}

func TestProvisionGPUWithoutRegisteredHostReturnsNoCapacity(t *testing.T) {
	defaultProvider := newMockProvider()
	fm := NewFleetManager(FleetConfig{Provider: defaultProvider, Prefix: "gpu-"})

	_, err := fm.ProvisionAndAssign(context.Background(), "gpu-task", Spec{GPUs: 1}, []byte(`{}`), nil, BootOptions{})
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("err = %v, want ErrNoCapacity", err)
	}
	if defaultProvider.count() != 0 {
		t.Fatalf("default provider count = %d, want 0", defaultProvider.count())
	}
}

func TestConcurrentGPUProvisionReservesBeforeBoot(t *testing.T) {
	defaultProvider := newMockProvider()
	hostProvider := newMockProvider()
	createStarted := make(chan struct{})
	releaseCreate := make(chan struct{})
	var once sync.Once
	hostProvider.createFn = func(_ context.Context, spec Spec) (Environment, error) {
		once.Do(func() { close(createStarted) })
		<-releaseCreate
		env := &mockEnv{name: spec.Name, url: "http://" + spec.Name + ".test"}
		hostProvider.envs[spec.Name] = env
		return env, nil
	}
	fm := NewFleetManager(FleetConfig{Provider: defaultProvider, Prefix: "gpu-"})
	if err := fm.RegisterHost(context.Background(), gpuFleetHost("h1", 1, "a100"), hostProvider); err != nil {
		t.Fatal(err)
	}

	firstDone := make(chan error, 1)
	go func() {
		_, err := fm.ProvisionAndAssign(context.Background(), "first", Spec{GPUs: 1}, []byte(`{}`), nil, BootOptions{})
		firstDone <- err
	}()
	<-createStarted

	_, secondErr := fm.ProvisionAndAssign(context.Background(), "second", Spec{GPUs: 1}, []byte(`{}`), nil, BootOptions{})
	if !errors.Is(secondErr, ErrNoCapacity) {
		t.Fatalf("second provision err = %v, want ErrNoCapacity", secondErr)
	}
	close(releaseCreate)
	if err := <-firstDone; err != nil {
		t.Fatalf("first provision: %v", err)
	}
}

func migInstanceFleetHost(id string, instances ...MIGInstance) Host {
	h := gpuFleetHost(id, 0, "")
	h.Capacity.MIGInstances = instances
	profiles := make(map[string]int, len(instances))
	for _, inst := range instances {
		profiles[inst.Profile]++
	}
	h.Capacity.MIGProfiles = profiles
	if len(instances) > 0 {
		h.Capacity.GPUKind = instances[0].Kind
	}
	return h
}

func TestAllocateOnHost_bindsConcreteMIGInstanceUUIDs(t *testing.T) {
	stub := newStubProvider()
	fm := NewFleetManager(FleetConfig{Provider: stub, Prefix: "gpu-"})
	host := migInstanceFleetHost("h1",
		MIGInstance{UUID: "m1", Profile: "1g.10gb", Kind: "a100", ParentGPUUUID: "gpu-a"},
		MIGInstance{UUID: "m2", Profile: "1g.10gb", Kind: "a100", ParentGPUUUID: "gpu-a"},
		MIGInstance{UUID: "m3", Profile: "2g.20gb", Kind: "a100", ParentGPUUUID: "gpu-a"},
	)
	if err := fm.RegisterHost(context.Background(), host, stub); err != nil {
		t.Fatal(err)
	}

	v := &vm{spec: Spec{CPUs: 1, RamMB: 256, GPUs: 1, GPUKind: "a100", GPUProfile: "1g.10gb"}}
	fm.mu.Lock()
	fm.allocateOnHost("h1", v)
	fm.mu.Unlock()

	if len(v.spec.MIGInstanceUUIDs) != 1 {
		t.Fatalf("v.spec.MIGInstanceUUIDs = %v, want one bound uuid", v.spec.MIGInstanceUUIDs)
	}
	bound := v.spec.MIGInstanceUUIDs[0]
	if bound != "m1" && bound != "m2" {
		t.Errorf("bound uuid %q not a 1g.10gb instance", bound)
	}
	h := findHost(t, fm, "h1")
	if len(h.Allocated.MIGInstanceUUIDs) != 1 || h.Allocated.MIGInstanceUUIDs[0] != bound {
		t.Errorf("host allocated = %v, want the single bound uuid", h.Allocated.MIGInstanceUUIDs)
	}
	// the derived count map must reflect the binding, not the legacy counter.
	if h.Allocated.MIGProfiles["1g.10gb"] != 1 {
		t.Errorf("MIGProfiles[1g.10gb] = %d, want 1 (derived from bound uuids)", h.Allocated.MIGProfiles["1g.10gb"])
	}

	// a second env must bind the OTHER 1g.10gb instance, never reuse, and
	// never touch the 2g.20gb pool.
	v2 := &vm{spec: Spec{CPUs: 1, RamMB: 256, GPUs: 1, GPUKind: "a100", GPUProfile: "1g.10gb"}}
	fm.mu.Lock()
	fm.allocateOnHost("h1", v2)
	fm.mu.Unlock()
	if len(v2.spec.MIGInstanceUUIDs) != 1 || v2.spec.MIGInstanceUUIDs[0] == bound {
		t.Errorf("second bind = %v, want the other 1g.10gb instance", v2.spec.MIGInstanceUUIDs)
	}
	h = findHost(t, fm, "h1")
	if h.Allocated.MIGProfiles["1g.10gb"] != 2 {
		t.Errorf("MIGProfiles[1g.10gb] = %d, want 2 after second bind", h.Allocated.MIGProfiles["1g.10gb"])
	}

	// deallocate the first vm: only its uuid is released, and the count map
	// is re-derived from the survivors so it cannot drift.
	fm.mu.Lock()
	fm.deallocateOnHost("h1", v.spec)
	fm.mu.Unlock()
	h = findHost(t, fm, "h1")
	if len(h.Allocated.MIGInstanceUUIDs) != 1 || h.Allocated.MIGInstanceUUIDs[0] != v2.spec.MIGInstanceUUIDs[0] {
		t.Errorf("after dealloc host allocated = %v, want only the second uuid", h.Allocated.MIGInstanceUUIDs)
	}
	if h.Allocated.MIGProfiles["1g.10gb"] != 1 {
		t.Errorf("MIGProfiles[1g.10gb] = %d, want 1 after release", h.Allocated.MIGProfiles["1g.10gb"])
	}
}

func TestDeallocateOnHost_migInstanceReleasesAcrossProfiles(t *testing.T) {
	stub := newStubProvider()
	fm := NewFleetManager(FleetConfig{Provider: stub, Prefix: "gpu-"})
	host := migInstanceFleetHost("h1",
		MIGInstance{UUID: "m1", Profile: "1g.10gb", Kind: "a100"},
		MIGInstance{UUID: "m3", Profile: "2g.20gb", Kind: "a100"},
	)
	if err := fm.RegisterHost(context.Background(), host, stub); err != nil {
		t.Fatal(err)
	}

	v1 := &vm{spec: Spec{CPUs: 1, RamMB: 256, GPUs: 1, GPUKind: "a100", GPUProfile: "1g.10gb"}}
	v2 := &vm{spec: Spec{CPUs: 1, RamMB: 256, GPUs: 1, GPUKind: "a100", GPUProfile: "2g.20gb"}}
	fm.mu.Lock()
	fm.allocateOnHost("h1", v1)
	fm.allocateOnHost("h1", v2)
	fm.mu.Unlock()

	h := findHost(t, fm, "h1")
	if h.Allocated.MIGProfiles["1g.10gb"] != 1 || h.Allocated.MIGProfiles["2g.20gb"] != 1 {
		t.Fatalf("MIGProfiles = %v, want one of each profile", h.Allocated.MIGProfiles)
	}

	// releasing the 2g.20gb vm must leave the 1g.10gb binding untouched and
	// drop only the 2g.20gb count.
	fm.mu.Lock()
	fm.deallocateOnHost("h1", v2.spec)
	fm.mu.Unlock()
	h = findHost(t, fm, "h1")
	if len(h.Allocated.MIGInstanceUUIDs) != 1 || h.Allocated.MIGInstanceUUIDs[0] != v1.spec.MIGInstanceUUIDs[0] {
		t.Errorf("after release allocated = %v, want only the 1g.10gb uuid", h.Allocated.MIGInstanceUUIDs)
	}
	if _, ok := h.Allocated.MIGProfiles["2g.20gb"]; ok {
		t.Errorf("MIGProfiles still has 2g.20gb = %d, want it removed", h.Allocated.MIGProfiles["2g.20gb"])
	}
	if h.Allocated.MIGProfiles["1g.10gb"] != 1 {
		t.Errorf("MIGProfiles[1g.10gb] = %d, want 1 (untouched by the 2g.20gb release)", h.Allocated.MIGProfiles["1g.10gb"])
	}
}

// TestReconcileOrphan_destroysAndReclaimsHostAllocation checks #68's Part B:
// destroying an orphan must not leave the host's allocation inflated. An
// orphan is untracked, so it carries no Spec for deallocateOnHost to subtract;
// its stale counter can only drain by re-deriving host allocation after the
// destroy. We simulate a VM that was allocated on its host then lost its vm
// record to a crash (leaving the host counter inflated) while its sandbox
// lingers in the provider as an orphan, and assert the host allocation is
// reclaimed to zero once reconcile destroys the orphan.
func TestReconcileOrphan_destroysAndReclaimsHostAllocation(t *testing.T) {
	stub := newStubProvider()
	fm := NewFleetManager(FleetConfig{Provider: stub, Prefix: "gpu-"})
	host := gpuFleetHost("h1", 2, "a100")
	if err := fm.RegisterHost(context.Background(), host, stub); err != nil {
		t.Fatal(err)
	}

	// Simulate a VM that was allocated on h1 then lost its vm record to a
	// crash, leaving h1's counter inflated. The sandbox lingers in the
	// provider as an untracked orphan.
	fm.mu.Lock()
	fm.allocateOnHost("h1", &vm{spec: Spec{
		Name: "gpu-orphan", CPUs: 1, RamMB: 256, StorageGB: 10, GPUs: 1, GPUKind: "a100",
	}})
	stub.envs["gpu-orphan"] = &stubLoadEnv{name: "gpu-orphan", url: "http://gpu-orphan.test"}
	fm.mu.Unlock()

	if got := findHost(t, fm, "h1").Allocated.GPUs; got != 1 {
		t.Fatalf("pre-reconcile allocated GPUs = %d, want 1 (fixture must inflate the host)", got)
	}

	fm.reconcile(context.Background())

	h := findHost(t, fm, "h1")
	if h.Allocated.GPUs != 0 {
		t.Errorf("post-reconcile allocated GPUs = %d, want 0 (orphan destroy must reclaim host allocation)", h.Allocated.GPUs)
	}
	if h.Allocated.VMCount != 0 {
		t.Errorf("post-reconcile VMCount = %d, want 0", h.Allocated.VMCount)
	}
	stub.mu.Lock()
	orphanGone := len(stub.envs) == 0
	stub.mu.Unlock()
	if !orphanGone {
		t.Errorf("orphan not destroyed: %d envs remain", len(stub.envs))
	}
}
