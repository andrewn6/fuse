package orchestrator

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func host(id string, cpus, ramMB, storageGB, vmCount int, state HostState) *Host {
	return &Host{
		ID:       id,
		URL:      "http://" + id + ".test",
		Region:   "us-east",
		Capacity: HostCapacity{CPUs: cpus, RamMB: ramMB, StorageGB: storageGB, VMCount: vmCount},
		State:    state,
		LastSeen: time.Now(),
	}
}

func hostWithAlloc(id string, capCPU, capRAM, allocCPU, allocRAM int) *Host {
	return &Host{
		ID:        id,
		URL:       "http://" + id + ".test",
		Region:    "us-east",
		Capacity:  HostCapacity{CPUs: capCPU, RamMB: capRAM, StorageGB: 100, VMCount: 10},
		Allocated: HostCapacity{CPUs: allocCPU, RamMB: allocRAM},
		State:     HostActive,
		LastSeen:  time.Now(),
	}
}

func spec(cpus, ramMB, storageGB int) Spec {
	return Spec{CPUs: cpus, RamMB: ramMB, StorageGB: storageGB}
}

func TestSchedule_emptyHostsReturnsErrNoHosts(t *testing.T) {
	_, _, err := Schedule(spec(1, 256, 10), nil, PlacementSpread)
	if !errors.Is(err, ErrNoHosts) {
		t.Errorf("err = %v, want ErrNoHosts", err)
	}
}

func TestSchedule_allCordonedReturnsErrNoCapacity(t *testing.T) {
	hosts := []*Host{
		host("h1", 8, 4096, 100, 10, HostCordoned),
		host("h2", 8, 4096, 100, 10, HostDraining),
	}
	_, _, err := Schedule(spec(1, 256, 10), hosts, PlacementSpread)
	if !errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, want ErrNoCapacity", err)
	}
}

func TestSchedule_noFitReturnsErrNoCapacity(t *testing.T) {
	hosts := []*Host{
		host("h1", 2, 1024, 50, 10, HostActive),
	}
	// Request more CPUs than the host has.
	_, _, err := Schedule(spec(4, 256, 10), hosts, PlacementSpread)
	if !errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, want ErrNoCapacity", err)
	}
}

func TestSchedule_regionFilterExcludesMismatch(t *testing.T) {
	h := host("h1", 8, 4096, 100, 10, HostActive)
	h.Region = "eu-west"

	s := spec(1, 256, 10)
	s.Region = "us-east" // doesn't match h1's region

	_, _, err := Schedule(s, []*Host{h}, PlacementSpread)
	if !errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, want ErrNoCapacity (region mismatch)", err)
	}
}

func TestSchedule_binpackPicksMostPacked(t *testing.T) {
	hosts := []*Host{
		hostWithAlloc("h1", 8, 4096, 6, 3072), // 2 free CPUs, 1024 free RAM
		hostWithAlloc("h2", 8, 4096, 2, 1024), // 6 free CPUs, 3072 free RAM
	}
	picked, dec, err := Schedule(spec(1, 256, 10), hosts, PlacementBinpack)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "h1" {
		t.Errorf("binpack picked %s, want h1 (most packed)", picked.ID)
	}
	if dec.Candidates != 2 {
		t.Errorf("candidates = %d, want 2", dec.Candidates)
	}
	if dec.Policy != PlacementBinpack {
		t.Errorf("policy = %s", dec.Policy)
	}
}

func TestSchedule_spreadPicksLeastPacked(t *testing.T) {
	hosts := []*Host{
		hostWithAlloc("h1", 8, 4096, 6, 3072), // 2 free CPUs
		hostWithAlloc("h2", 8, 4096, 2, 1024), // 6 free CPUs
	}
	picked, _, err := Schedule(spec(1, 256, 10), hosts, PlacementSpread)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "h2" {
		t.Errorf("spread picked %s, want h2 (least packed)", picked.ID)
	}
}

func TestSchedule_tieBreaksByID(t *testing.T) {
	hosts := []*Host{
		hostWithAlloc("b-host", 8, 4096, 4, 2048),
		hostWithAlloc("a-host", 8, 4096, 4, 2048), // same allocation, lower ID
	}
	picked, _, err := Schedule(spec(1, 256, 10), hosts, PlacementSpread)
	if err != nil {
		t.Fatal(err)
	}
	// Same headroom → tiebreak by ID (alphabetical).
	if picked.ID != "a-host" {
		t.Errorf("tiebreak picked %s, want a-host", picked.ID)
	}
}

func TestSchedule_vmCountLimitEnforced(t *testing.T) {
	h := host("h1", 64, 65536, 1000, 2, HostActive)
	h.Allocated = HostCapacity{VMCount: 2} // at capacity for VM count
	_, _, err := Schedule(spec(1, 256, 10), []*Host{h}, PlacementSpread)
	if !errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, want ErrNoCapacity (VM count full)", err)
	}
}

func TestSchedule_decisionRecordsHeadroom(t *testing.T) {
	hosts := []*Host{
		hostWithAlloc("h1", 8, 4096, 2, 1024),
	}
	_, dec, err := Schedule(spec(2, 512, 10), hosts, PlacementSpread)
	if err != nil {
		t.Fatal(err)
	}
	// After placement: 8-2-2=4 CPUs, 4096-1024-512=2560 RAM.
	if dec.HeadroomCPUs != 4 {
		t.Errorf("headroom CPUs = %d, want 4", dec.HeadroomCPUs)
	}
	if dec.HeadroomRam != 2560 {
		t.Errorf("headroom RAM = %d, want 2560", dec.HeadroomRam)
	}
}

func TestSchedule_emptyPolicyDefaultsToSpread(t *testing.T) {
	hosts := []*Host{
		hostWithAlloc("h1", 8, 4096, 6, 3072),
		hostWithAlloc("h2", 8, 4096, 2, 1024),
	}
	picked, _, err := Schedule(spec(1, 256, 10), hosts, "")
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "h2" {
		t.Errorf("empty policy picked %s, want h2 (spread default)", picked.ID)
	}
}

func TestFits_variousCombinations(t *testing.T) {
	cap := HostCapacity{CPUs: 8, RamMB: 4096, StorageGB: 100, VMCount: 5}
	tests := []struct {
		name      string
		allocated HostCapacity
		spec      Spec
		want      bool
	}{
		{"empty alloc, small spec", HostCapacity{}, Spec{CPUs: 1, RamMB: 256, StorageGB: 10}, true},
		{"exact fit", HostCapacity{CPUs: 7, RamMB: 3840, StorageGB: 90, VMCount: 4}, Spec{CPUs: 1, RamMB: 256, StorageGB: 10}, true},
		{"CPU overcommit", HostCapacity{CPUs: 8}, Spec{CPUs: 1}, false},
		{"RAM overcommit", HostCapacity{RamMB: 4096}, Spec{RamMB: 1}, false},
		{"storage overcommit", HostCapacity{StorageGB: 100}, Spec{StorageGB: 1}, false},
		{"VM count full", HostCapacity{VMCount: 5}, Spec{CPUs: 1, RamMB: 1, StorageGB: 1}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := fits(cap, tc.allocated, tc.spec)
			if got != tc.want {
				t.Errorf("fits = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHostCapacity_GPUFieldsRoundTripJSON(t *testing.T) {
	in := HostCapacity{CPUs: 4, RamMB: 1024, StorageGB: 50, VMCount: 2, GPUs: 2, GPUKind: "a100"}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !containsJSONField(data, `"gpus":2`) || !containsJSONField(data, `"gpu_kind":"a100"`) {
		t.Fatalf("marshaled json missing gpu fields: %s", data)
	}

	var out HostCapacity
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Errorf("round trip = %+v, want %+v", out, in)
	}
}

func TestHostCapacity_GPUFieldsOmittedWhenZero(t *testing.T) {
	in := HostCapacity{CPUs: 4, RamMB: 1024, StorageGB: 50, VMCount: 2}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if containsJSONField(data, `"gpus"`) || containsJSONField(data, `"gpu_kind"`) {
		t.Fatalf("expected gpu fields to be omitted, got: %s", data)
	}

	var out HostCapacity
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.GPUs != 0 || out.GPUKind != "" {
		t.Errorf("defaults = %+v, want zero values", out)
	}
}

func TestHostBackend_DefaultsToFirecracker(t *testing.T) {
	// A Host built without an explicit Backend (e.g. legacy record) must
	// behave as firecracker — this is asserted at the registration
	// boundary in internal/api, but the zero value itself must be the
	// empty string so callers can detect "unset" and apply the default.
	var h Host
	if h.Backend != "" {
		t.Errorf("zero-value Backend = %q, want empty string", h.Backend)
	}
}

func TestHostBackend_Constants(t *testing.T) {
	if BackendFirecracker != "firecracker" {
		t.Errorf("BackendFirecracker = %q, want firecracker", BackendFirecracker)
	}
	if BackendQEMU != "qemu" {
		t.Errorf("BackendQEMU = %q, want qemu", BackendQEMU)
	}
}

// containsJSONField is a small substring helper so the round-trip tests
// can assert on exact key:value presence without decoding into a map
// (which would hide omitempty behavior behind Go's zero-value defaults).
func containsJSONField(data []byte, field string) bool {
	return bytes.Contains(data, []byte(field))
}

func gpuHost(id string, backend HostBackend, gpus int, kind string) *Host {
	h := host(id, 8, 4096, 100, 10, HostActive)
	h.Backend = backend
	h.Capacity.GPUs = gpus
	h.Capacity.GPUKind = kind
	return h
}

func gpuSpec(gpus int32, kind string) Spec {
	s := spec(1, 256, 10)
	s.GPUs = gpus
	s.GPUKind = kind
	return s
}

func TestSchedule_gpuRequiresGPUHost(t *testing.T) {
	// host with GPUs:0 must be rejected for a GPUs:1 request, even if cpu/ram fit
	h := gpuHost("h1", BackendQEMU, 0, "")
	_, _, err := Schedule(gpuSpec(1, ""), []*Host{h}, PlacementSpread)
	if !errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, want ErrNoCapacity", err)
	}
}

func TestSchedule_gpuNeverLandsOnFirecrackerHost(t *testing.T) {
	// registration rejects gpus>0 on firecracker hosts, but the scheduler
	// stays defensive against a stale or hand-edited host record
	h := gpuHost("h1", BackendFirecracker, 2, "a100")
	_, _, err := Schedule(gpuSpec(1, ""), []*Host{h}, PlacementSpread)
	if !errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, want ErrNoCapacity (firecracker host)", err)
	}
}

func TestSchedule_gpuPlacedOnQEMUHostWithFreeGPUs(t *testing.T) {
	hosts := []*Host{
		gpuHost("fc-1", BackendFirecracker, 0, ""),
		gpuHost("gpu-1", BackendQEMU, 2, "a100"),
	}
	picked, _, err := Schedule(gpuSpec(1, ""), hosts, PlacementSpread)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "gpu-1" {
		t.Errorf("picked %s, want gpu-1", picked.ID)
	}
}

func TestSchedule_gpuKindMustMatch(t *testing.T) {
	h100 := gpuHost("h100-1", BackendQEMU, 2, "h100")
	_, _, err := Schedule(gpuSpec(1, "a100"), []*Host{h100}, PlacementSpread)
	if !errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, want ErrNoCapacity (kind mismatch)", err)
	}

	a100 := gpuHost("a100-1", BackendQEMU, 2, "a100")
	picked, _, err := Schedule(gpuSpec(1, "a100"), []*Host{h100, a100}, PlacementSpread)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "a100-1" {
		t.Errorf("picked %s, want a100-1", picked.ID)
	}
}

func TestSchedule_gpuEmptyKindMatchesAnyKind(t *testing.T) {
	h := gpuHost("h1", BackendQEMU, 1, "h100")
	picked, _, err := Schedule(gpuSpec(1, ""), []*Host{h}, PlacementSpread)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "h1" {
		t.Errorf("picked %s, want h1", picked.ID)
	}
}

func TestFits_migProfileUsesMIGPool(t *testing.T) {
	cap := HostCapacity{
		CPUs: 8, RamMB: 4096, StorageGB: 100, VMCount: 5,
		GPUs: 0, GPUKind: "a100",
		MIGProfiles: map[string]int{"1g.10gb": 4},
	}
	tests := []struct {
		name      string
		allocated HostCapacity
		spec      Spec
		want      bool
	}{
		{
			name: "profile fits free pool",
			spec: Spec{CPUs: 1, RamMB: 256, StorageGB: 10, GPUs: 2, GPUProfile: "1g.10gb"},
			want: true,
		},
		{
			name:      "profile exhausted",
			allocated: HostCapacity{MIGProfiles: map[string]int{"1g.10gb": 4}},
			spec:      Spec{CPUs: 1, RamMB: 256, StorageGB: 10, GPUs: 1, GPUProfile: "1g.10gb"},
			want:      false,
		},
		{
			name: "unknown profile",
			spec: Spec{CPUs: 1, RamMB: 256, StorageGB: 10, GPUs: 1, GPUProfile: "2g.20gb"},
			want: false,
		},
		{
			name: "whole-device request ignores mig pool",
			spec: Spec{CPUs: 1, RamMB: 256, StorageGB: 10, GPUs: 1},
			want: false, // GPUs capacity is 0
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := fits(cap, tc.allocated, tc.spec)
			if got != tc.want {
				t.Errorf("fits = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSchedule_migProfilePlacedOnMatchingHost(t *testing.T) {
	whole := gpuHost("whole-1", BackendQEMU, 2, "a100")
	mig := gpuHost("mig-1", BackendQEMU, 0, "a100")
	mig.Capacity.MIGProfiles = map[string]int{"1g.10gb": 4}

	spec := gpuSpec(1, "a100")
	spec.GPUProfile = "1g.10gb"

	picked, _, err := Schedule(spec, []*Host{whole, mig}, PlacementSpread)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "mig-1" {
		t.Errorf("picked %s, want mig-1", picked.ID)
	}
}

func TestSchedule_gpuAllocatedDevicesAreNotReused(t *testing.T) {
	// single-gpu host with its device already allocated cannot take another
	h := gpuHost("h1", BackendQEMU, 1, "a100")
	h.Allocated.GPUs = 1
	_, _, err := Schedule(gpuSpec(1, ""), []*Host{h}, PlacementSpread)
	if !errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, want ErrNoCapacity (gpu already allocated)", err)
	}
}

func TestSchedule_gpuRequestExceedingFreeInventoryFails(t *testing.T) {
	h := gpuHost("h1", BackendQEMU, 2, "a100")
	_, _, err := Schedule(gpuSpec(4, ""), []*Host{h}, PlacementSpread)
	if !errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, want ErrNoCapacity (not enough free gpus)", err)
	}
}

func TestSchedule_zeroGPUSpecSchedulesOnFirecrackerUnchanged(t *testing.T) {
	// regression: non-gpu envs keep scheduling on firecracker hosts
	h := host("h1", 8, 4096, 100, 10, HostActive)
	picked, _, err := Schedule(spec(1, 256, 10), []*Host{h}, PlacementSpread)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "h1" {
		t.Errorf("picked %s, want h1", picked.ID)
	}
}

func TestSchedule_zeroGPUSpecMayUseQEMUGPUHost(t *testing.T) {
	// d3: non-gpu workloads may run on qemu gpu hosts
	h := gpuHost("gpu-1", BackendQEMU, 2, "a100")
	picked, _, err := Schedule(spec(1, 256, 10), []*Host{h}, PlacementSpread)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "gpu-1" {
		t.Errorf("picked %s, want gpu-1", picked.ID)
	}
}

func TestFits_gpuCombinations(t *testing.T) {
	cap := HostCapacity{CPUs: 8, RamMB: 4096, StorageGB: 100, VMCount: 5, GPUs: 2, GPUKind: "a100"}
	tests := []struct {
		name      string
		allocated HostCapacity
		spec      Spec
		want      bool
	}{
		{"gpu fits", HostCapacity{}, Spec{CPUs: 1, RamMB: 256, StorageGB: 10, GPUs: 1}, true},
		{"gpu exact fit", HostCapacity{}, Spec{CPUs: 1, RamMB: 256, StorageGB: 10, GPUs: 2}, true},
		{"gpu overcommit", HostCapacity{GPUs: 2}, Spec{CPUs: 1, RamMB: 256, StorageGB: 10, GPUs: 1}, false},
		{"kind match", HostCapacity{}, Spec{CPUs: 1, RamMB: 256, StorageGB: 10, GPUs: 1, GPUKind: "a100"}, true},
		{"kind mismatch", HostCapacity{}, Spec{CPUs: 1, RamMB: 256, StorageGB: 10, GPUs: 1, GPUKind: "h100"}, false},
		{"kind ignored when no gpu requested", HostCapacity{}, Spec{CPUs: 1, RamMB: 256, StorageGB: 10, GPUKind: "h100"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := fits(cap, tc.allocated, tc.spec)
			if got != tc.want {
				t.Errorf("fits = %v, want %v", got, tc.want)
			}
		})
	}
}

// deviceHost builds a per-device host (Capacity.GPUDevices populated). The
// scalar GPUs count and GPUKind are derived as back-compat aggregates.
func deviceHost(id string, backend HostBackend, devices ...GPUDevice) *Host {
	h := host(id, 8, 4096, 100, 10, HostActive)
	h.Backend = backend
	h.Capacity.GPUDevices = devices
	h.Capacity.GPUs = len(devices)
	if len(devices) > 0 {
		h.Capacity.GPUKind = devices[0].Model
	}
	return h
}

func TestFits_perDeviceCountsFreeMatchingDevices(t *testing.T) {
	cap := HostCapacity{
		CPUs: 8, RamMB: 4096, StorageGB: 100, VMCount: 5, GPUs: 2,
		GPUDevices: []GPUDevice{
			{UUID: "gpu-a", Model: "NVIDIA A100-SXM4-40GB"},
			{UUID: "gpu-b", Model: "NVIDIA A100-SXM4-40GB"},
		},
	}
	// one device already bound: only one free, so a 2-gpu request fails but a
	// 1-gpu request fits.
	alloc := HostCapacity{GPUDeviceUUIDs: []string{"gpu-a"}}
	if fits(cap, alloc, Spec{CPUs: 1, RamMB: 256, StorageGB: 10, GPUs: 2}) {
		t.Error("fits = true, want false (only one free device)")
	}
	if !fits(cap, alloc, Spec{CPUs: 1, RamMB: 256, StorageGB: 10, GPUs: 1}) {
		t.Error("fits = false, want true (one free device)")
	}
}

func TestFits_heterogeneousHostMatchesByModel(t *testing.T) {
	// mixed-model host: an a100 request must only count the a100 device, even
	// though the scalar free count is 2.
	cap := HostCapacity{
		CPUs: 8, RamMB: 4096, StorageGB: 100, VMCount: 5, GPUs: 2,
		GPUDevices: []GPUDevice{
			{UUID: "gpu-a", Model: "NVIDIA A100-SXM4-40GB"},
			{UUID: "gpu-h", Model: "NVIDIA H100-SXM5-80GB"},
		},
	}
	if !fits(cap, HostCapacity{}, Spec{CPUs: 1, RamMB: 256, StorageGB: 10, GPUs: 1, GPUKind: "a100"}) {
		t.Error("fits = false, want true (one a100 free)")
	}
	if fits(cap, HostCapacity{}, Spec{CPUs: 1, RamMB: 256, StorageGB: 10, GPUs: 2, GPUKind: "a100"}) {
		t.Error("fits = true, want false (only one a100 on a mixed host)")
	}
	// once the a100 is bound, no a100 is free.
	alloc := HostCapacity{GPUDeviceUUIDs: []string{"gpu-a"}}
	if fits(cap, alloc, Spec{CPUs: 1, RamMB: 256, StorageGB: 10, GPUs: 1, GPUKind: "a100"}) {
		t.Error("fits = true, want false (the only a100 is allocated)")
	}
}

func TestSchedule_heterogeneousHostPlacesByKind(t *testing.T) {
	h := deviceHost("gpu-1", BackendQEMU,
		GPUDevice{UUID: "gpu-a", Model: "NVIDIA A100-SXM4-40GB"},
		GPUDevice{UUID: "gpu-h", Model: "NVIDIA H100-SXM5-80GB"},
	)
	picked, _, err := Schedule(gpuSpec(1, "h100"), []*Host{h}, PlacementSpread)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "gpu-1" {
		t.Errorf("picked %s, want gpu-1", picked.ID)
	}
	// a kind the host does not carry must not fit.
	if _, _, err := Schedule(gpuSpec(1, "v100"), []*Host{h}, PlacementSpread); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, want ErrNoCapacity (no v100 device)", err)
	}
}

// migDevice builds a MIG-capable per-device entry (mig_mode reported, so the
// agent flags it mig_capable) for the given model.
func migDevice(uuid, model string) GPUDevice {
	return GPUDevice{UUID: uuid, Model: model, MIGCapable: true, MIGMode: "Enabled"}
}

func migSpec(gpus int32, kind, profile string) Spec {
	s := gpuSpec(gpus, kind)
	s.GPUProfile = profile
	return s
}

func TestFits_migHonorsGPUKind(t *testing.T) {
	// per-device mig host carrying only a100s. the mig pool alone is not
	// enough: the requested kind has to be on the host too.
	cap := HostCapacity{
		CPUs: 8, RamMB: 4096, StorageGB: 100, VMCount: 5,
		GPUKind:     "a100",
		MIGProfiles: map[string]int{"1g.10gb": 4},
		GPUDevices:  []GPUDevice{migDevice("gpu-a", "NVIDIA A100-SXM4-40GB")},
	}
	tests := []struct {
		name string
		spec Spec
		want bool
	}{
		{"matching kind", migSpec(1, "a100", "1g.10gb"), true},
		{"mismatched kind", migSpec(1, "h100", "1g.10gb"), false},
		{"empty kind matches any", migSpec(1, "", "1g.10gb"), true},
		{"matching kind but pool exhausted", migSpec(5, "a100", "1g.10gb"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := fits(cap, HostCapacity{}, tc.spec); got != tc.want {
				t.Errorf("fits = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFits_migRejectsNonMIGCapableDevices(t *testing.T) {
	// host advertises a mig pool but every device it reports says
	// mig_capable=false. the pool is a misconfiguration, so no mig request
	// may land here, kind or no kind.
	cap := HostCapacity{
		CPUs: 8, RamMB: 4096, StorageGB: 100, VMCount: 5,
		GPUKind:     "l40s",
		MIGProfiles: map[string]int{"1g.10gb": 4},
		GPUDevices: []GPUDevice{
			{UUID: "gpu-a", Model: "NVIDIA L40S", MIGCapable: false, MIGMode: "N/A"},
		},
	}
	if fits(cap, HostCapacity{}, migSpec(1, "l40s", "1g.10gb")) {
		t.Error("fits = true, want false (device is not mig capable)")
	}
	if fits(cap, HostCapacity{}, migSpec(1, "", "1g.10gb")) {
		t.Error("fits = true, want false (no mig capable device, empty kind)")
	}
	// the same host still serves whole-device requests.
	if !fits(cap, HostCapacity{}, gpuSpec(1, "l40s")) {
		t.Error("fits = false, want true (whole-device request is unaffected)")
	}
}

func TestFits_migMixedHostOnlyMatchesMIGCapableKind(t *testing.T) {
	// one mig-capable a100 alongside a non-mig-capable l40s. a mig request
	// pinned to l40s must not land here even though the host carries an l40s.
	//
	// GPUKind is set on purpose: the probe leaves it empty on a mixed host, but
	// an operator's declared --gpu-kind overrides the probe, so a scalar kind
	// alongside per-device inventory is a real shape. It is also the shape that
	// exercises the host-kind fallback in gpuKindMatches. Without the fallback
	// being gated on an empty device Model, the a100's MIGCapable and the host's
	// l40s label would satisfy the two conjuncts from two different cards and
	// admit this request.
	cap := HostCapacity{
		CPUs: 8, RamMB: 4096, StorageGB: 100, VMCount: 5,
		GPUKind:     "l40s",
		MIGProfiles: map[string]int{"1g.10gb": 4},
		GPUDevices: []GPUDevice{
			migDevice("gpu-a", "NVIDIA A100-SXM4-40GB"),
			{UUID: "gpu-l", Model: "NVIDIA L40S", MIGCapable: false, MIGMode: "N/A"},
		},
	}
	if !fits(cap, HostCapacity{}, migSpec(1, "a100", "1g.10gb")) {
		t.Error("fits = false, want true (mig capable a100 present)")
	}
	if fits(cap, HostCapacity{}, migSpec(1, "l40s", "1g.10gb")) {
		t.Error("fits = true, want false (l40s is not mig capable)")
	}
}

func TestFits_migModellessDeviceFallsBackToHostKind(t *testing.T) {
	// the qemu agent degrades model to "" when the vfio metadata blob omits it,
	// so a device can be reported with a uuid and mig_capable but no model. the
	// host scalar kind is then the only kind evidence there is and must still
	// admit a matching request, otherwise these hosts stop placing entirely.
	cap := HostCapacity{
		CPUs: 8, RamMB: 4096, StorageGB: 100, VMCount: 5,
		GPUKind:     "a100",
		MIGProfiles: map[string]int{"1g.10gb": 4},
		GPUDevices: []GPUDevice{
			{UUID: "gpu-a", Model: "", MIGCapable: true, MIGMode: "Enabled"},
		},
	}
	if !fits(cap, HostCapacity{}, migSpec(1, "a100", "1g.10gb")) {
		t.Error("fits = false, want true (modelless device falls back to host kind)")
	}
	if fits(cap, HostCapacity{}, migSpec(1, "h100", "1g.10gb")) {
		t.Error("fits = true, want false (host kind is a100)")
	}
	// whole-device requests take the same fallback.
	if !fits(cap, HostCapacity{}, gpuSpec(1, "a100")) {
		t.Error("fits = false, want true (whole-device modelless fallback)")
	}
}

func TestFits_migLegacyHostWithoutDeviceInventoryStillPlaces(t *testing.T) {
	// legacy/profiles-only host: mig pool declared at registration, no
	// per-device inventory reported. absence of device data must not read as
	// "not mig capable", or every currently-working mig host breaks.
	scalar := HostCapacity{
		CPUs: 8, RamMB: 4096, StorageGB: 100, VMCount: 5,
		GPUKind:     "a100",
		MIGProfiles: map[string]int{"1g.10gb": 4},
	}
	if !fits(scalar, HostCapacity{}, migSpec(1, "a100", "1g.10gb")) {
		t.Error("fits = false, want true (scalar kind matches)")
	}
	if !fits(scalar, HostCapacity{}, migSpec(1, "", "1g.10gb")) {
		t.Error("fits = false, want true (no kind requested)")
	}
	if fits(scalar, HostCapacity{}, migSpec(1, "h100", "1g.10gb")) {
		t.Error("fits = true, want false (scalar kind mismatch)")
	}

	// host that reports neither devices nor a kind: nothing to match on, so
	// the qemu agent stays the enforcement point.
	kindless := scalar
	kindless.GPUKind = ""
	if !fits(kindless, HostCapacity{}, migSpec(1, "a100", "1g.10gb")) {
		t.Error("fits = false, want true (unknown host kind must not be ruled out)")
	}
}

func TestSchedule_migProfileRoutesToMatchingKind(t *testing.T) {
	// end to end through Schedule: two mig hosts offering the same profile on
	// different cards. the request must reach the one carrying the right card
	// instead of failing later at the qemu agent.
	a100 := deviceHost("mig-a100", BackendQEMU, migDevice("gpu-a", "NVIDIA A100-SXM4-40GB"))
	a100.Capacity.GPUs = 0
	a100.Capacity.MIGProfiles = map[string]int{"1g.10gb": 4}

	h100 := deviceHost("mig-h100", BackendQEMU, migDevice("gpu-h", "NVIDIA H100-SXM5-80GB"))
	h100.Capacity.GPUs = 0
	h100.Capacity.MIGProfiles = map[string]int{"1g.10gb": 4}

	picked, _, err := Schedule(migSpec(1, "h100", "1g.10gb"), []*Host{a100, h100}, PlacementSpread)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "mig-h100" {
		t.Errorf("picked %s, want mig-h100", picked.ID)
	}

	// a kind neither host carries has nowhere to go.
	_, _, err = Schedule(migSpec(1, "a40", "1g.10gb"), []*Host{a100, h100}, PlacementSpread)
	if !errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, want ErrNoCapacity (no host carries an a40)", err)
	}
}

func TestSchedule_migProfileSkipsNonMIGCapableHost(t *testing.T) {
	bad := deviceHost("mig-l40s", BackendQEMU,
		GPUDevice{UUID: "gpu-l", Model: "NVIDIA L40S", MIGCapable: false, MIGMode: "N/A"})
	bad.Capacity.GPUs = 0
	bad.Capacity.MIGProfiles = map[string]int{"1g.10gb": 4}

	_, _, err := Schedule(migSpec(1, "", "1g.10gb"), []*Host{bad}, PlacementSpread)
	if !errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, want ErrNoCapacity (host reports no mig capable device)", err)
	}
}

// migInstanceHost builds a per-instance MIG host: capacity carries concrete
// MIGInstance entries (one per carved GPU instance) instead of just a count
// map. The MIGProfiles map is derived as a summary so the host still reports
// the legacy field for back-compat readers.
func migInstanceHost(id string, instances ...MIGInstance) *Host {
	h := host(id, 8, 4096, 100, 10, HostActive)
	h.Backend = BackendQEMU
	h.Capacity.MIGInstances = instances
	profiles := make(map[string]int, len(instances))
	for _, inst := range instances {
		profiles[strings.ToLower(inst.Profile)]++
	}
	h.Capacity.MIGProfiles = profiles
	if len(instances) > 0 {
		h.Capacity.GPUKind = instances[0].Kind
	}
	return h
}

func TestFits_migInstancesCountFreeByProfileAndKind(t *testing.T) {
	cap := HostCapacity{
		CPUs: 8, RamMB: 4096, StorageGB: 100, VMCount: 5,
		MIGInstances: []MIGInstance{
			{UUID: "m1", Profile: "1g.10gb", Kind: "a100", ParentGPUUUID: "gpu-a"},
			{UUID: "m2", Profile: "1g.10gb", Kind: "a100", ParentGPUUUID: "gpu-a"},
			{UUID: "m3", Profile: "2g.20gb", Kind: "a100", ParentGPUUUID: "gpu-a"},
		},
	}
	// one 1g.10gb already bound: only one free, so a 2-instance request fails
	// but a 1-instance request fits.
	alloc := HostCapacity{MIGInstanceUUIDs: []string{"m1"}}
	if fits(cap, alloc, migSpec(2, "a100", "1g.10gb")) {
		t.Error("fits = true, want false (only one free 1g.10gb instance)")
	}
	if !fits(cap, alloc, migSpec(1, "a100", "1g.10gb")) {
		t.Error("fits = false, want true (one free 1g.10gb instance)")
	}
	// a different profile is untouched by the m1 binding.
	if !fits(cap, alloc, migSpec(1, "a100", "2g.20gb")) {
		t.Error("fits = false, want true (2g.20gb pool is separate)")
	}
}

func TestFits_migInstancesRejectMismatchedKind(t *testing.T) {
	cap := HostCapacity{
		CPUs: 8, RamMB: 4096, StorageGB: 100, VMCount: 5,
		MIGInstances: []MIGInstance{
			{UUID: "m1", Profile: "1g.10gb", Kind: "a100", ParentGPUUUID: "gpu-a"},
		},
	}
	if fits(cap, HostCapacity{}, migSpec(1, "h100", "1g.10gb")) {
		t.Error("fits = true, want false (instance kind is a100, request is h100)")
	}
	// empty kind matches anything, same as the device path.
	if !fits(cap, HostCapacity{}, migSpec(1, "", "1g.10gb")) {
		t.Error("fits = false, want true (empty kind matches any instance)")
	}
}

func TestFits_migInstanceKindFallsBackToParentDevice(t *testing.T) {
	// instance omits Kind; the parent GPU device's Model is the evidence.
	cap := HostCapacity{
		CPUs: 8, RamMB: 4096, StorageGB: 100, VMCount: 5,
		GPUDevices: []GPUDevice{
			{UUID: "gpu-a", Model: "NVIDIA A100-SXM4-40GB"},
		},
		MIGInstances: []MIGInstance{
			{UUID: "m1", Profile: "1g.10gb", ParentGPUUUID: "gpu-a"},
		},
	}
	if !fits(cap, HostCapacity{}, migSpec(1, "a100", "1g.10gb")) {
		t.Error("fits = false, want true (parent device model matches a100)")
	}
	if fits(cap, HostCapacity{}, migSpec(1, "h100", "1g.10gb")) {
		t.Error("fits = true, want false (parent device model is a100)")
	}
}

func TestSchedule_migInstanceHostPlacesAndBindsUUIDs(t *testing.T) {
	h := migInstanceHost("mig-1",
		MIGInstance{UUID: "m1", Profile: "1g.10gb", Kind: "a100", ParentGPUUUID: "gpu-a"},
		MIGInstance{UUID: "m2", Profile: "1g.10gb", Kind: "a100", ParentGPUUUID: "gpu-a"},
	)
	picked, _, err := Schedule(migSpec(1, "a100", "1g.10gb"), []*Host{h}, PlacementSpread)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "mig-1" {
		t.Errorf("picked %s, want mig-1", picked.ID)
	}
	// over-request: only 2 instances of the profile exist, ask for 3.
	if _, _, err := Schedule(migSpec(3, "a100", "1g.10gb"), []*Host{h}, PlacementSpread); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, want ErrNoCapacity (not enough instances)", err)
	}
}

func TestSchedule_migInstanceHostExhaustedFallsBackToCount(t *testing.T) {
	// sanity: a per-instance host whose instances are all bound still
	// rejects, it does not silently route to the count-map path.
	h := migInstanceHost("mig-1",
		MIGInstance{UUID: "m1", Profile: "1g.10gb", Kind: "a100"},
	)
	h.Allocated.MIGInstanceUUIDs = []string{"m1"}
	if _, _, err := Schedule(migSpec(1, "a100", "1g.10gb"), []*Host{h}, PlacementSpread); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, want ErrNoCapacity (only instance is bound)", err)
	}
}

// ── placement: host pin and label selectors (issue #102) ──────────

// labelledHost is a plain cpu host with declared placement labels.
func labelledHost(id string, labels map[string]string) *Host {
	h := host(id, 8, 4096, 100, 10, HostActive)
	h.Labels = labels
	return h
}

func TestSchedule_hostPinHonored(t *testing.T) {
	// h2 is emptier, so spread would pick it without the pin.
	hosts := []*Host{
		hostWithAlloc("h1", 8, 4096, 4, 2048),
		hostWithAlloc("h2", 8, 4096, 0, 0),
	}
	s := spec(1, 256, 10)
	s.HostID = "h1"
	picked, decision, err := Schedule(s, hosts, PlacementSpread)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if picked.ID != "h1" {
		t.Errorf("picked %s, want h1 (the pinned host)", picked.ID)
	}
	if decision.HostID != "h1" || decision.Candidates != 1 {
		t.Errorf("decision = %+v, want host h1 with 1 candidate", decision)
	}
}

func TestSchedule_hostPinToUnknownHostIsNotFound(t *testing.T) {
	hosts := []*Host{host("h1", 8, 4096, 100, 10, HostActive)}
	s := spec(1, 256, 10)
	s.HostID = "nope"
	_, _, err := Schedule(s, hosts, PlacementSpread)
	if !errors.Is(err, ErrHostNotFound) {
		t.Fatalf("err = %v, want ErrHostNotFound", err)
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("err = %q, want it to name the requested host", err)
	}
}

func TestSchedule_hostPinToCordonedHostNamesTheGate(t *testing.T) {
	hosts := []*Host{
		host("h1", 8, 4096, 100, 10, HostCordoned),
		host("h2", 8, 4096, 100, 10, HostActive),
	}
	s := spec(1, 256, 10)
	s.HostID = "h1"
	_, _, err := Schedule(s, hosts, PlacementSpread)
	if !errors.Is(err, ErrHostPinUnschedulable) {
		t.Fatalf("err = %v, want ErrHostPinUnschedulable", err)
	}
	// a pin miss must never read as a capacity shortfall, and must name both
	// the host and the gate that rejected it.
	if errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, must not also be ErrNoCapacity", err)
	}
	if !strings.Contains(err.Error(), "h1") || !strings.Contains(err.Error(), string(HostCordoned)) {
		t.Errorf("err = %q, want it to name host h1 and the cordoned state", err)
	}
}

func TestSchedule_hostPinThatDoesNotFitNamesCapacity(t *testing.T) {
	hosts := []*Host{host("h1", 2, 1024, 50, 10, HostActive)}
	s := spec(4, 256, 10)
	s.HostID = "h1"
	_, _, err := Schedule(s, hosts, PlacementSpread)
	if !errors.Is(err, ErrHostPinUnschedulable) {
		t.Fatalf("err = %v, want ErrHostPinUnschedulable", err)
	}
	if !strings.Contains(err.Error(), "does not fit") {
		t.Errorf("err = %q, want it to name the capacity gate", err)
	}
}

func TestSchedule_hostPinToWrongLabelsNamesUnmatchedPairs(t *testing.T) {
	hosts := []*Host{labelledHost("h1", map[string]string{"disk": "ssd"})}
	s := spec(1, 256, 10)
	s.HostID = "h1"
	s.Labels = map[string]string{"disk": "nvme"}
	_, _, err := Schedule(s, hosts, PlacementSpread)
	if !errors.Is(err, ErrHostPinUnschedulable) {
		t.Fatalf("err = %v, want ErrHostPinUnschedulable", err)
	}
	if !strings.Contains(err.Error(), "disk=nvme") {
		t.Errorf("err = %q, want it to name the unmatched pair", err)
	}
}

func TestSchedule_labelSelectorPicksMatchingHost(t *testing.T) {
	// h1 is emptier, so spread would pick it without the selector.
	h1 := hostWithAlloc("h1", 8, 4096, 0, 0)
	h2 := hostWithAlloc("h2", 8, 4096, 4, 2048)
	h2.Labels = map[string]string{"disk": "nvme", "tier": "build"}
	s := spec(1, 256, 10)
	s.Labels = map[string]string{"disk": "nvme"}
	picked, _, err := Schedule(s, []*Host{h1, h2}, PlacementSpread)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if picked.ID != "h2" {
		t.Errorf("picked %s, want h2 (the only labelled host)", picked.ID)
	}
}

func TestSchedule_labelSelectorRequiresEveryPair(t *testing.T) {
	// every requested pair is a gate: a host with only one of them is out.
	hosts := []*Host{labelledHost("h1", map[string]string{"disk": "nvme"})}
	s := spec(1, 256, 10)
	s.Labels = map[string]string{"disk": "nvme", "tier": "build"}
	_, _, err := Schedule(s, hosts, PlacementSpread)
	if !errors.Is(err, ErrNoLabelMatch) {
		t.Fatalf("err = %v, want ErrNoLabelMatch", err)
	}
	if errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, must not also be ErrNoCapacity", err)
	}
	// the message reports the selector and how many hosts were considered.
	if !strings.Contains(err.Error(), "disk=nvme,tier=build") {
		t.Errorf("err = %q, want the sorted selector in the message", err)
	}
	if !strings.Contains(err.Error(), "1 schedulable hosts") {
		t.Errorf("err = %q, want the count of hosts considered", err)
	}
}

func TestSchedule_labelMissOnFullHostsStillReportsCapacity(t *testing.T) {
	// a host that matches the labels but has no room is a capacity
	// shortfall, not a selector mistake.
	h := labelledHost("h1", map[string]string{"disk": "nvme"})
	h.Allocated = HostCapacity{CPUs: 8, RamMB: 4096}
	s := spec(4, 256, 10)
	s.Labels = map[string]string{"disk": "nvme"}
	_, _, err := Schedule(s, []*Host{h}, PlacementSpread)
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("err = %v, want ErrNoCapacity", err)
	}
}

func TestSchedule_pinPlusLabelsBothApply(t *testing.T) {
	h1 := labelledHost("h1", map[string]string{"disk": "nvme"})
	h2 := labelledHost("h2", map[string]string{"disk": "nvme"})
	s := spec(1, 256, 10)
	s.HostID = "h2"
	s.Labels = map[string]string{"disk": "nvme"}
	picked, _, err := Schedule(s, []*Host{h1, h2}, PlacementSpread)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if picked.ID != "h2" {
		t.Errorf("picked %s, want h2", picked.ID)
	}
}

func TestSchedule_emptyPlacementIsUnchanged(t *testing.T) {
	// no pin and no selector: labelled and unlabelled hosts are equally
	// eligible and the policy alone decides, exactly as before placement.
	h1 := hostWithAlloc("h1", 8, 4096, 4, 2048)
	h2 := hostWithAlloc("h2", 8, 4096, 0, 0)
	h1.Labels = map[string]string{"disk": "nvme"}
	s := spec(1, 256, 10)
	picked, decision, err := Schedule(s, []*Host{h1, h2}, PlacementSpread)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if picked.ID != "h2" {
		t.Errorf("spread picked %s, want h2 (the emptiest)", picked.ID)
	}
	if decision.Candidates != 2 {
		t.Errorf("candidates = %d, want 2 (a host's labels never gate an empty selector)", decision.Candidates)
	}
	picked, _, err = Schedule(s, []*Host{h1, h2}, PlacementBinpack)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if picked.ID != "h1" {
		t.Errorf("binpack picked %s, want h1 (the most packed)", picked.ID)
	}
}

func TestNormalizeArch(t *testing.T) {
	cases := map[string]string{
		"":        "",
		"amd64":   ArchAMD64,
		"x86_64":  ArchAMD64,
		"X86_64":  ArchAMD64,
		"arm64":   ArchARM64,
		"aarch64": ArchARM64,
		" ARM64 ": ArchARM64,
		"riscv64": "riscv64",
	}
	for in, want := range cases {
		if got := NormalizeArch(in); got != want {
			t.Errorf("NormalizeArch(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSchedule_archFilterExcludesMismatch(t *testing.T) {
	h := host("h1", 8, 4096, 100, 10, HostActive)
	h.Capacity.Arch = ArchARM64

	s := spec(1, 256, 10)
	s.Arch = ArchAMD64

	_, _, err := Schedule(s, []*Host{h}, PlacementSpread)
	if !errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, want ErrNoCapacity (arch mismatch)", err)
	}
}

func TestSchedule_archMatchedHostWins(t *testing.T) {
	amd := host("h-amd", 8, 4096, 100, 10, HostActive)
	arm := host("h-arm", 8, 4096, 100, 10, HostActive)
	arm.Capacity.Arch = ArchARM64

	s := spec(1, 256, 10)
	s.Arch = ArchARM64

	picked, _, err := Schedule(s, []*Host{amd, arm}, PlacementSpread)
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if picked.ID != "h-arm" {
		t.Errorf("picked %s, want h-arm", picked.ID)
	}
}

func TestSchedule_emptyHostArchMeansAMD64(t *testing.T) {
	// hosts registered before the arch field are all x86_64: an empty
	// host arch must satisfy an amd64 request and reject an arm64 one.
	legacy := host("h-legacy", 8, 4096, 100, 10, HostActive)

	s := spec(1, 256, 10)
	s.Arch = ArchAMD64
	if _, _, err := Schedule(s, []*Host{legacy}, PlacementSpread); err != nil {
		t.Errorf("amd64 spec on legacy host: %v, want scheduled", err)
	}

	s.Arch = ArchARM64
	if _, _, err := Schedule(s, []*Host{legacy}, PlacementSpread); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("arm64 spec on legacy host: err = %v, want ErrNoCapacity", err)
	}
}

func TestSchedule_emptySpecArchMatchesAnyHost(t *testing.T) {
	arm := host("h-arm", 8, 4096, 100, 10, HostActive)
	arm.Capacity.Arch = ArchARM64

	if _, _, err := Schedule(spec(1, 256, 10), []*Host{arm}, PlacementSpread); err != nil {
		t.Errorf("no-arch spec on arm64 host: %v, want scheduled", err)
	}
}
