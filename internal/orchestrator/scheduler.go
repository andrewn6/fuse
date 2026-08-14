package orchestrator

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// HostState captures the scheduling eligibility of a registered host.
type HostState string

const (
	// HostActive means the host is healthy and accepting new VMs.
	HostActive HostState = "active"

	// HostCordoned means the host is not accepting new VMs. Existing
	// VMs are left running. Cordon is the operator's "maintenance
	// soon, stop sending work here" signal.
	HostCordoned HostState = "cordoned"

	// HostDraining means the host is cordoned AND its existing VMs
	// are being evicted by the reconcile loop. Once all VMs are gone
	// the host is eligible for removal. Drain support in reconcile
	// is stubbed in this PR; the state is defined so the data model
	// doesn't need a follow-up migration.
	HostDraining HostState = "draining"
)

// HostCapacity is the resource envelope of a host. It is reported
// by the host agent at registration time and refreshed by heartbeat.
// The scheduler compares Spec against (Capacity - Allocated) to make
// admission decisions.
type HostCapacity struct {
	CPUs      int `json:"cpus"`
	RamMB     int `json:"ram_mb"`
	StorageGB int `json:"storage_gb"`
	VMCount   int `json:"vm_count"` // max concurrent VMs

	// Arch is the host's CPU architecture in GOARCH vocabulary ("amd64",
	// "arm64"). Probed from the host agent like CPUs/RamMB; an operator may
	// also declare it at registration. Empty means amd64: every host that
	// existed before this field was x86_64, so old records stay schedulable
	// without migration.
	Arch string `json:"arch,omitempty"`

	// GPUs is the count of whole GPU devices available on the host.
	// Zero means no GPUs. Only qemu-backed hosts may report GPUs > 0
	// (enforced at registration, see internal/api registerHost).
	GPUs int `json:"gpus,omitempty"`

	// GPUKind identifies the GPU model (e.g. "a100"). Empty when GPUs is 0.
	GPUKind string `json:"gpu_kind,omitempty"`

	// MIGProfiles is fractional GPU capacity: MIG instance count by
	// profile name (e.g. {"1g.10gb": 4}). A GPU in MIG mode stays on the
	// nvidia driver and exports mdev devices, so it is never part of the
	// whole-device GPUs pool — the two counters are independent (D5).
	// Only qemu-backed hosts may advertise MIG profiles.
	//
	// When the host reports per-instance MIG inventory (MIGInstances), this
	// map is a derived summary (count by profile) and the scheduler binds
	// specific instance uuids rather than decrementing a counter. When a
	// host only declares counts (the --mig-profile registration override,
	// or a hand-managed mig-inventory.txt without parent detail), this map
	// remains the scheduling unit so legacy callers do not break.
	MIGProfiles map[string]int `json:"mig_profiles,omitempty"`

	// MIGInstances is the per-instance MIG inventory probed from the host
	// agent (one entry per carved MIG GPU instance). When non-empty, the
	// scheduler switches from count-based to instance-based allocation:
	// fits() checks for free instances of the requested profile on a
	// kind-matching card, and allocate binds concrete uuids to VMs. This
	// is strictly additive — a host that reports no instances falls back
	// to the MIGProfiles count path (issue #41).
	MIGInstances []MIGInstance `json:"mig_instances,omitempty"`

	// GPUDevices is the per-device detail probed from the host agent
	// (one entry per whole GPU). It rides the capacity wire alongside the
	// scalar GPUs/GPUKind counters. In-memory + wire only in this PR; the
	// durable per-device schema is issue #37.
	GPUDevices []GPUDevice `json:"gpu_devices,omitempty"`

	// GPUDeviceUUIDs is the set of device uuids currently bound to VMs. It
	// is populated only on a host's Allocated struct (never on Capacity),
	// and only for hosts that report per-device inventory. fits() derives
	// the free-device set as Capacity.GPUDevices minus this set. It is
	// in-memory only: allocation/deallocation maintain it, and recovery
	// recomputes it from live VM bindings, so it is never persisted as its
	// own column (the durable source of truth is the per-VM gpu_uuids).
	GPUDeviceUUIDs []string `json:"gpu_device_uuids,omitempty"`

	// MIGInstanceUUIDs is the set of MIG instance uuids currently bound to
	// VMs. Populated only on Allocated (never on Capacity), and only for
	// hosts that report per-instance MIG inventory. fits() derives the
	// free-instance set as Capacity.MIGInstances minus this set. In-memory
	// only, like GPUDeviceUUIDs: the durable source of truth is the
	// per-VM mig_instance_uuids binding.
	MIGInstanceUUIDs []string `json:"mig_instance_uuids,omitempty"`
}

// MIGInstance is one carved MIG GPU instance advertised by the host agent.
// Field names mirror the agent's mig_instances payload. The orchestrator
// binds a specific instance uuid to a VM (Spec.MIGInstanceUUIDs) so it knows
// which instance went to which VM — the count-map path (MIGProfiles) cannot.
type MIGInstance struct {
	UUID          string `json:"uuid,omitempty"`
	Profile       string `json:"profile,omitempty"`
	Kind          string `json:"kind,omitempty"`
	ParentGPUUUID string `json:"parent_gpu_uuid,omitempty"`
}

// freeMIG returns the free instance count for a MIG profile given the
// host's capacity and allocated maps.
func freeMIG(capacity, allocated HostCapacity, profile string) int {
	return capacity.MIGProfiles[profile] - allocated.MIGProfiles[profile]
}

// freeMIGInstances returns the capacity MIG instances of the requested profile
// that are not currently bound to a VM (not in allocated.MIGInstanceUUIDs) and
// whose kind matches gpuKind. An empty gpuKind matches any instance. this is
// the per-instance analogue of freeMIG: where freeMIG subtracts counters,
// this filters the concrete instance list. used only when the host reports
// per-instance MIG inventory (Capacity.MIGInstances non-empty).
func freeMIGInstances(capacity, allocated HostCapacity, profile, gpuKind string) []MIGInstance {
	used := make(map[string]struct{}, len(allocated.MIGInstanceUUIDs))
	for _, u := range allocated.MIGInstanceUUIDs {
		used[u] = struct{}{}
	}
	wantProfile := strings.ToLower(profile)
	var free []MIGInstance
	for _, inst := range capacity.MIGInstances {
		if inst.UUID == "" {
			continue
		}
		if !strings.EqualFold(inst.Profile, wantProfile) {
			continue
		}
		if _, taken := used[inst.UUID]; taken {
			continue
		}
		if !migKindMatches(gpuKind, inst, capacity) {
			continue
		}
		free = append(free, inst)
	}
	return free
}

// migKindMatches reports whether a requested gpu kind matches a MIG instance.
// An empty request matches anything. the instance's own Kind field is the
// primary evidence (the agent reports kind per instance). when the instance
// reports no kind, fall back to the parent GPU device's Model, then the host
// scalar GPUKind, so a request like "a100" still matches an instance whose
// parent card is "NVIDIA A100-SXM4-40GB".
func migKindMatches(gpuKind string, inst MIGInstance, capacity HostCapacity) bool {
	if gpuKind == "" {
		return true
	}
	if inst.Kind != "" {
		return strings.Contains(strings.ToLower(inst.Kind), strings.ToLower(gpuKind))
	}
	// fall back to the parent GPU device's model when the instance omits kind.
	if inst.ParentGPUUUID != "" {
		for _, d := range capacity.GPUDevices {
			if d.UUID == inst.ParentGPUUUID {
				return gpuKindMatches(gpuKind, d, capacity.GPUKind)
			}
		}
	}
	// last resort: the host scalar kind.
	return capacity.GPUKind == "" || strings.EqualFold(gpuKind, capacity.GPUKind)
}

// migProfileCounts derives the MIGProfiles count map from a set of bound MIG
// instance uuids, looking each up against the host's per-instance inventory.
// uuids not in the inventory (e.g. an instance since destroyed) are skipped
// rather than counted under an unknown profile. nil in, nil out.
func migProfileCounts(boundUUIDs []string, inventory []MIGInstance) map[string]int {
	if len(boundUUIDs) == 0 {
		return nil
	}
	byUUID := make(map[string]MIGInstance, len(inventory))
	for _, inst := range inventory {
		if inst.UUID != "" {
			byUUID[inst.UUID] = inst
		}
	}
	out := make(map[string]int)
	for _, u := range boundUUIDs {
		inst, ok := byUUID[u]
		if !ok || inst.Profile == "" {
			continue
		}
		out[strings.ToLower(inst.Profile)]++
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// GPUDevice is the per-device detail the host agent probes for a single
// whole GPU. Field names mirror the agent's gpu_devices payload. All fields
// are best-effort: the agent omits any it cannot determine.
type GPUDevice struct {
	UUID          string `json:"uuid,omitempty"`
	Model         string `json:"model,omitempty"`
	PCIBusID      string `json:"pci_bus_id,omitempty"`
	MemoryMB      int    `json:"memory_mb,omitempty"`
	DriverVersion string `json:"driver_version,omitempty"`
	CUDAVersion   string `json:"cuda_version,omitempty"`
	ComputeCap    string `json:"compute_cap,omitempty"`
	MIGCapable    bool   `json:"mig_capable,omitempty"`
	MIGMode       string `json:"mig_mode,omitempty"`
	IOMMUGroup    string `json:"iommu_group,omitempty"`
}

// Host architectures, in GOARCH vocabulary. NormalizeArch maps the uname -m
// spellings agents and operators actually type onto these.
const (
	// ArchAMD64 is x86_64. It is also what an empty arch means everywhere:
	// hosts registered before arch existed are all x86_64.
	ArchAMD64 = "amd64"

	// ArchARM64 is aarch64 / Apple Silicon.
	ArchARM64 = "arm64"
)

// NormalizeArch maps an architecture string onto GOARCH vocabulary. It
// accepts the uname -m spellings ("x86_64", "aarch64") alongside the GOARCH
// ones, lowercased. Empty stays empty (meaning "not set", which the
// scheduler reads as amd64); anything unrecognized is returned lowercased
// as-is so validation can name it in an error.
func NormalizeArch(arch string) string {
	switch strings.ToLower(strings.TrimSpace(arch)) {
	case "":
		return ""
	case "amd64", "x86_64":
		return ArchAMD64
	case "arm64", "aarch64":
		return ArchARM64
	default:
		return strings.ToLower(strings.TrimSpace(arch))
	}
}

// hostArch resolves a host's effective architecture: its reported arch, or
// amd64 when the record predates the field.
func hostArch(h *Host) string {
	if a := NormalizeArch(h.Capacity.Arch); a != "" {
		return a
	}
	return ArchAMD64
}

// HostBackend identifies the virtualization backend a host agent runs.
// It determines which capabilities the host can offer the scheduler
// (e.g. only qemu hosts may advertise GPUs).
type HostBackend string

const (
	// BackendFirecracker is the default backend: microVMs with no GPU
	// passthrough support.
	BackendFirecracker HostBackend = "firecracker"

	// BackendQEMU is a full-VM backend that supports GPU passthrough.
	BackendQEMU HostBackend = "qemu"
)

// fits returns true if the host has enough headroom (capacity minus
// allocated) to place a VM with the given spec.
func fits(capacity, allocated HostCapacity, spec Spec) bool {
	if (capacity.CPUs-allocated.CPUs) < spec.CPUs ||
		(capacity.RamMB-allocated.RamMB) < spec.RamMB ||
		(capacity.StorageGB-allocated.StorageGB) < spec.StorageGB ||
		(capacity.VMCount-allocated.VMCount) < 1 {
		return false
	}
	if spec.GPUs > 0 {
		if spec.GPUProfile != "" {
			// Fractional request: spec.GPUs counts MIG instances of the
			// requested profile, allocated from the host's MIG pool (D5).
			// This pool is independent of the whole-device GPUs pool.
			if len(capacity.MIGInstances) > 0 {
				// per-instance host: require that many free instances of the
				// requested profile on a kind-matching card. this is the
				// granularity change the count-map path cannot make: it filters
				// by instance, not by host (issue #41).
				return len(freeMIGInstances(capacity, allocated, spec.GPUProfile, spec.GPUKind)) >= int(spec.GPUs)
			}
			// legacy count-map host.
			if freeMIG(capacity, allocated, spec.GPUProfile) < int(spec.GPUs) {
				return false
			}
			return hostServesMIG(capacity, spec.GPUKind)
		}
		if len(capacity.GPUDevices) > 0 {
			// per-device host: count free devices whose kind matches the
			// request. free = all capacity devices minus the allocated-uuid
			// set. this makes heterogeneous hosts (mixed models) schedule
			// correctly where scalar subtraction cannot.
			return len(freeMatchingDevices(capacity, allocated, spec.GPUKind)) >= int(spec.GPUs)
		}
		// legacy homogeneous host: scalar count + exact gpu_kind string.
		if capacity.GPUs-allocated.GPUs < int(spec.GPUs) {
			return false
		}
		if spec.GPUKind != "" && spec.GPUKind != capacity.GPUKind {
			return false
		}
	}
	return true
}

// hostServesMIG reports whether a host can carve MIG instances of the
// requested kind. MIG instances come off the host's own GPUs, so when the host
// reports per-device inventory the request must be satisfiable by a SINGLE
// device that is both MIGCapable and a kind match (gpuKindMatches semantics,
// same as the whole-device branch). Both conjuncts must hold of the same card,
// which is why gpuKindMatches must not consult the host scalar kind for a
// device that reported its own Model.
//
// Devices are read from Capacity and not from the free set: a GPU in MIG mode
// stays on the nvidia driver and is never part of the whole-device pool (D5),
// so whole-device allocations say nothing about MIG availability.
//
// Absence of device data is not evidence of incapability. A host that reports
// no per-device inventory (legacy scalar hosts, and MIG pools declared by hand
// with --mig-profile at registration) falls back to the host's scalar GPUKind,
// and a host that reports no kind either is left to the qemu agent to accept
// or reject. Only positive evidence filters a host out here: devices that are
// all non-MIG-capable, or no MIG-capable device of the requested kind.
func hostServesMIG(capacity HostCapacity, gpuKind string) bool {
	if len(capacity.GPUDevices) == 0 {
		// no inventory: ask the same matcher about a device that reports
		// nothing, which is exactly the host-scalar fallback.
		return gpuKindMatches(gpuKind, GPUDevice{}, capacity.GPUKind)
	}
	for _, d := range capacity.GPUDevices {
		if d.MIGCapable && gpuKindMatches(gpuKind, d, capacity.GPUKind) {
			return true
		}
	}
	return false
}

// freeMatchingDevices returns the uuids of capacity devices that are not in
// the allocated-uuid set and whose kind matches gpuKind. An empty gpuKind
// matches any device. Matching is case-insensitive against both the device
// Model and the derived host GPUKind so requests like "a100" match a device
// whose Model is "NVIDIA A100-SXM4-40GB".
func freeMatchingDevices(capacity, allocated HostCapacity, gpuKind string) []string {
	used := make(map[string]struct{}, len(allocated.GPUDeviceUUIDs))
	for _, u := range allocated.GPUDeviceUUIDs {
		used[u] = struct{}{}
	}
	var free []string
	for _, d := range capacity.GPUDevices {
		if d.UUID == "" {
			continue
		}
		if _, taken := used[d.UUID]; taken {
			continue
		}
		if !gpuKindMatches(gpuKind, d, capacity.GPUKind) {
			continue
		}
		free = append(free, d.UUID)
	}
	return free
}

// gpuKindMatches reports whether a requested gpu kind matches a device. An
// empty request matches anything. Otherwise the request must be a
// case-insensitive substring of the device Model, so both "a100" and a full
// model string resolve.
//
// The host's derived GPUKind is only a fallback for a device that reports no
// Model of its own (the qemu agent degrades model to "" when the vfio metadata
// blob omits it). It must never override a Model the device did report: the
// host kind is a fleet-level scalar, so consulting it for a device that
// disagrees makes the match device-independent, and a caller asking two
// questions about one device (this branch and MIGCapable in hostServesMIG)
// could have them answered by two different cards.
func gpuKindMatches(gpuKind string, d GPUDevice, hostKind string) bool {
	if gpuKind == "" {
		return true
	}
	if d.Model != "" {
		return strings.Contains(strings.ToLower(d.Model), strings.ToLower(gpuKind))
	}
	// no device-level evidence: the host scalar is all we know about this card.
	return hostKind == "" || strings.EqualFold(gpuKind, hostKind)
}

// Host is a registered compute host in the fleet. It represents a
// single Firecracker host agent that can provision VMs.
type Host struct {
	ID      string
	URL     string // base URL of the host agent (e.g. https://agent-1.local)
	Token   string // bearer token for this host's agent
	Region  string
	Backend HostBackend // "firecracker" or "qemu"; empty means firecracker (default)
	// Labels are operator-declared key/value pairs supplied at registration
	// and matched against a spec's placement label selectors. They are never
	// probed from the host agent, the same trust model as Capacity.GPUKind.
	Labels    map[string]string
	Capacity  HostCapacity
	Allocated HostCapacity
	State     HostState
	LastSeen  time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// schedulable returns true if the host is healthy and eligible for
// new VM placements. A host must be Active and have been seen
// recently to schedule work to it.
func (h *Host) schedulable() bool {
	return h.State == HostActive
}

// PlacementPolicy controls how the scheduler picks among eligible
// hosts that all have sufficient capacity.
type PlacementPolicy string

const (
	// PlacementBinpack fills hosts as densely as possible. It picks
	// the host with the MOST already-allocated resources that still
	// has room. This minimizes the number of active hosts.
	PlacementBinpack PlacementPolicy = "binpack"

	// PlacementSpread distributes VMs as evenly as possible. It picks
	// the host with the LEAST already-allocated resources. This
	// maximizes isolation between VMs.
	PlacementSpread PlacementPolicy = "spread"
)

// ErrNoCapacity is returned by Schedule when no registered host can
// fit the requested spec. The REST handler maps this to 503.
var ErrNoCapacity = errors.New("no host has sufficient capacity")

// ErrNoHosts is returned by Schedule when the host registry is empty.
var ErrNoHosts = errors.New("no hosts registered")

// ErrHostPinUnschedulable is returned by Schedule when spec.HostID names a
// registered host that no longer clears one of the other gates (cordoned,
// wrong region or backend, mismatched labels, or full). The wrapped message
// names the gate that rejected it. The REST handler maps this to 503; an
// unknown host id is rejected earlier, at the API boundary, as a 404.
var ErrHostPinUnschedulable = errors.New("pinned host cannot take this environment")

// ErrNoLabelMatch is returned by Schedule when spec.Labels matched no
// schedulable host at all, which is a selector mistake rather than a capacity
// shortfall. The REST handler maps this to 503.
var ErrNoLabelMatch = errors.New("no host matches the requested placement labels")

// PlacementDecision records why the scheduler picked a particular
// host. It is attached to the VM record for debugging.
type PlacementDecision struct {
	HostID       string          `json:"host_id"`
	Policy       PlacementPolicy `json:"policy"`
	Candidates   int             `json:"candidates"`    // eligible hosts considered
	HeadroomCPUs int             `json:"headroom_cpus"` // CPUs remaining after placement
	HeadroomRam  int             `json:"headroom_ram"`  // RAM remaining after placement

	// ArtifactLocal reports that the winning host already held the spec's seed
	// artifact, so the environment boots from a local copy with no transfer.
	// When it is false and the spec seeds from an artifact, the caller has to
	// move the artifact to the winner before creating the VM, so this is the
	// field that says whether a transfer is about to happen.
	ArtifactLocal bool `json:"artifact_local,omitempty"`

	// ArtifactHolders is how many of the eligible candidates held the artifact.
	// Zero alongside ArtifactLocal=false means no host that could take the
	// workload had it; a positive count alongside ArtifactLocal=false cannot
	// happen, and reading both is how a decision stays self-explaining.
	ArtifactHolders int `json:"artifact_holders,omitempty"`
}

// PlacementHints carries scheduling preferences that come from fleet state
// rather than from anything the caller asked for. They are kept out of Spec on
// purpose: Spec is persisted with the VM, and a durable record must not carry a
// point-in-time observation about where some bytes happened to live.
type PlacementHints struct {
	// ArtifactHosts are the hosts that already hold the spec's seed artifact.
	//
	// This is a PREFERENCE and never a constraint. A host in this set that
	// cannot fit the workload, is cordoned, is the wrong arch, has the wrong
	// labels, or cannot serve the GPU request is rejected by the ordinary gates
	// before this map is ever consulted; locality only ever breaks a tie among
	// hosts that already qualify. Letting it do more would trade a correct
	// placement for a fast one, which is how a cache hit turns a working create
	// into a failing one.
	ArtifactHosts map[string]bool
}

// Schedule picks the best host for the given spec according to the
// placement policy. It is a pure function with no side effects — the
// caller is responsible for updating the host's Allocated counters
// and persisting the decision.
//
// Filter pipeline:
//  1. If spec.HostID is non-empty, consider only that host; a pin that clears
//     none of the gates below fails with ErrHostPinUnschedulable naming the
//     gate that rejected it.
//  2. Exclude non-schedulable hosts (cordoned, draining).
//  3. If spec.Region is non-empty, exclude hosts in a different region.
//  4. Exclude hosts whose backend cannot serve a GPU request.
//  5. If spec.Labels is non-empty, exclude hosts missing any requested pair.
//  6. Exclude hosts that don't fit the spec (capacity - allocated < spec).
//  7. Among survivors, pick by policy (binpack or spread).
//
// Ties within a policy are broken by host ID for determinism.
func Schedule(spec Spec, hosts []*Host, policy PlacementPolicy) (*Host, PlacementDecision, error) {
	return SchedulePreferring(spec, hosts, policy, PlacementHints{})
}

// SchedulePreferring is Schedule with placement preferences applied.
//
// The preferences are consulted only AFTER the filter pipeline above has run,
// on the hosts that survived it, so nothing in PlacementHints can admit a host
// the gates rejected. Within that surviving set, artifact locality outranks the
// placement policy: seeding from a local artifact is a `cp --reflink=auto` on
// the host, while seeding from a remote one means copying hundreds of megabytes
// to tens of gigabytes across the fleet first. No binpack or spread score is
// worth that difference, and the policy still decides among equally local
// hosts.
func SchedulePreferring(spec Spec, hosts []*Host, policy PlacementPolicy, hints PlacementHints) (*Host, PlacementDecision, error) {
	if len(hosts) == 0 {
		return nil, PlacementDecision{}, ErrNoHosts
	}

	// A pin narrows the candidate set to one host and reports the gate that
	// rejected it, so "pinned to build-3" never fails as a vague capacity
	// shortfall. An unknown host id is caught at the API boundary, but stay
	// defensive: an unregistered pin here is a not-found, not a pin miss.
	if spec.HostID != "" {
		pinned := hostByID(hosts, spec.HostID)
		if pinned == nil {
			return nil, PlacementDecision{}, fmt.Errorf(
				"%w: placement.host %q is not registered", ErrHostNotFound, spec.HostID)
		}
		if reason := hostRejection(pinned, spec); reason != "" {
			return nil, PlacementDecision{}, fmt.Errorf(
				"%w: host %q %s", ErrHostPinUnschedulable, pinned.ID, reason)
		}
		hosts = []*Host{pinned}
	}

	// Filter to eligible candidates.
	type candidate struct {
		host     *Host
		headroom int  // policy-specific score (lower = more packed)
		local    bool // already holds the spec's seed artifact
	}
	var candidates []candidate
	localCount := 0

	for _, h := range hosts {
		if hostRejection(h, spec) != "" {
			continue
		}
		// Score: total "free" resources as a single comparable int.
		// We use CPUs as the primary dimension and RAM as tiebreak
		// because CPUs are the scarcest resource in practice.
		freeCPUs := h.Capacity.CPUs - h.Allocated.CPUs
		freeRAM := h.Capacity.RamMB - h.Allocated.RamMB
		score := freeCPUs*10000 + freeRAM // weight CPUs heavily
		// Locality is read here, on a host that has already passed every gate.
		// A host holding the artifact that could not take the workload never
		// reaches this line, which is what makes the preference incapable of
		// overriding capacity.
		local := hints.ArtifactHosts[h.ID]
		if local {
			localCount++
		}
		candidates = append(candidates, candidate{host: h, headroom: score, local: local})
	}

	if len(candidates) == 0 {
		// A label selector that matched nothing is a selector mistake, not a
		// capacity shortfall: say so, and say how many hosts were considered.
		if len(spec.Labels) > 0 && !anyHostMatchesLabels(hosts, spec.Labels) {
			return nil, PlacementDecision{}, fmt.Errorf(
				"%w: %s matched none of %d schedulable hosts",
				ErrNoLabelMatch, formatLabels(spec.Labels), countSchedulable(hosts))
		}
		return nil, PlacementDecision{}, fmt.Errorf(
			"%w: need %dCPU/%dMB/%dGB, no eligible host fits",
			ErrNoCapacity, spec.CPUs, spec.RamMB, spec.StorageGB)
	}

	// Pick by policy, with artifact locality as the outer comparison.
	best := candidates[0]
	for _, c := range candidates[1:] {
		if c.local != best.local {
			// Every candidate here already fits, so choosing the local one
			// cannot cost an admission; it only ever saves a transfer.
			if c.local {
				best = c
			}
			continue
		}
		switch policy {
		case PlacementBinpack:
			// Most packed = smallest headroom.
			if c.headroom < best.headroom ||
				(c.headroom == best.headroom && c.host.ID < best.host.ID) {
				best = c
			}
		case PlacementSpread, "":
			// Least packed = largest headroom (default).
			if c.headroom > best.headroom ||
				(c.headroom == best.headroom && c.host.ID < best.host.ID) {
				best = c
			}
		}
	}

	decision := PlacementDecision{
		HostID:          best.host.ID,
		Policy:          policy,
		Candidates:      len(candidates),
		HeadroomCPUs:    best.host.Capacity.CPUs - best.host.Allocated.CPUs - spec.CPUs,
		HeadroomRam:     best.host.Capacity.RamMB - best.host.Allocated.RamMB - spec.RamMB,
		ArtifactLocal:   best.local,
		ArtifactHolders: localCount,
	}

	return best.host, decision, nil
}

// hostRejection returns a human-readable reason why h cannot take spec, or ""
// if it can. It is the single definition of the filter gates: the main loop
// uses it as a boolean, and the host-pin path turns the reason into an error
// message so a pin miss names the gate that rejected it.
func hostRejection(h *Host, spec Spec) string {
	switch {
	case !h.schedulable():
		return fmt.Sprintf("is %s, not %s", h.State, HostActive)
	case spec.Region != "" && h.Region != spec.Region:
		return fmt.Sprintf("is in region %q, the spec requires %q", h.Region, spec.Region)
	// arch is a hard gate: an image baked for one architecture cannot boot
	// on the other. An empty spec arch means "any" (the pre-arch behavior);
	// an empty host arch means amd64 (see hostArch).
	case spec.Arch != "" && hostArch(h) != NormalizeArch(spec.Arch):
		return fmt.Sprintf("is %s, the spec requires %s", hostArch(h), NormalizeArch(spec.Arch))
	// gpu envs may only land on qemu hosts (D3). registration rejects
	// gpus > 0 on firecracker hosts, but the scheduler stays defensive
	// against a stale or hand-edited host record.
	case spec.GPUs > 0 && h.Backend != BackendQEMU:
		return fmt.Sprintf("has backend %q, gpu environments require %q", h.Backend, BackendQEMU)
	case !labelsMatch(h.Labels, spec.Labels):
		return "does not have labels " + formatLabels(unmatchedLabels(h.Labels, spec.Labels))
	case !fits(h.Capacity, h.Allocated, spec):
		return fmt.Sprintf("does not fit %dCPU/%dMB/%dGB", spec.CPUs, spec.RamMB, spec.StorageGB)
	}
	return ""
}

// hostByID returns the host with the given id, or nil if it is not in the
// slice.
func hostByID(hosts []*Host, id string) *Host {
	for _, h := range hosts {
		if h.ID == id {
			return h
		}
	}
	return nil
}

// labelsMatch reports whether every requested pair is present on the host
// with the same value. An empty selector matches every host, so a spec
// without placement labels behaves exactly as before.
func labelsMatch(hostLabels, selector map[string]string) bool {
	for k, v := range selector {
		if hostLabels[k] != v {
			return false
		}
	}
	return true
}

// unmatchedLabels returns the subset of the selector the host does not
// satisfy, so an error can name the pairs that actually failed.
func unmatchedLabels(hostLabels, selector map[string]string) map[string]string {
	out := make(map[string]string, len(selector))
	for k, v := range selector {
		if hostLabels[k] != v {
			out[k] = v
		}
	}
	return out
}

// anyHostMatchesLabels reports whether any schedulable host satisfies the
// selector, ignoring the other gates. Used only to distinguish a selector
// mistake from a capacity shortfall.
func anyHostMatchesLabels(hosts []*Host, selector map[string]string) bool {
	for _, h := range hosts {
		if h.schedulable() && labelsMatch(h.Labels, selector) {
			return true
		}
	}
	return false
}

// countSchedulable returns how many hosts cleared the state gate, which is
// the "hosts considered" number an unmatched selector reports.
func countSchedulable(hosts []*Host) int {
	n := 0
	for _, h := range hosts {
		if h.schedulable() {
			n++
		}
	}
	return n
}

// formatLabels renders a label map as a sorted, comma-separated key=value
// list so error messages are stable across runs.
func formatLabels(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + labels[k]
	}
	return strings.Join(parts, ",")
}
