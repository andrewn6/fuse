package api

import (
	"strings"
	"time"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// toAPIEnvironment converts an orchestrator VMInfo into the wire shape.
func toAPIEnvironment(v orchestrator.VMInfo) Environment {
	return Environment{
		ID:        v.ID,
		State:     string(v.State),
		TaskID:    v.TaskID,
		HostID:    v.HostID,
		URL:       v.URL,
		Spec:      toAPIResourceSpec(v.Spec),
		CreatedAt: v.CreatedAt,
		UpdatedAt: v.UpdatedAt,
		Error:     v.Error,
		Endpoints: toAPIEndpoints(v.Endpoints),
		Health:    toAPIHealth(v.Health),
	}
}

// toAPIHealth converts the orchestrator's health verdict into the wire shape.
// Nil in, nil out, so an environment with no probe (or none read back yet)
// omits the field entirely rather than reporting an empty state.
func toAPIHealth(h *orchestrator.HealthStatus) *Health {
	if h == nil {
		return nil
	}
	return &Health{
		State:    string(h.State),
		Since:    h.Since,
		Failures: h.Failures,
		Message:  h.Message,
	}
}

// toOrchestratorHealthcheck converts a wire healthcheck into the orchestrator
// type. Nil in, nil out: a create request with no healthcheck must produce a
// nil spec, since that is what tells the boot path to ship no probe file and
// the reconcile loop not to poll the guest.
func toOrchestratorHealthcheck(hc *HealthcheckSpec) *orchestrator.HealthcheckSpec {
	if hc == nil {
		return nil
	}
	out := &orchestrator.HealthcheckSpec{
		Exec:               append([]string(nil), hc.Exec...),
		IntervalSeconds:    hc.IntervalSeconds,
		TimeoutSeconds:     hc.TimeoutSeconds,
		Retries:            hc.Retries,
		StartPeriodSeconds: hc.StartPeriodSeconds,
	}
	if hc.HTTP != nil {
		out.HTTP = &orchestrator.HealthcheckHTTP{Port: hc.HTTP.Port, Path: hc.HTTP.Path}
	}
	return out
}

// toOrchestratorDesktop converts a wire desktop block into the orchestrator
// type. Nil in, nil out: a create request with no desktop must produce a nil
// spec, since that is what tells the boot path to ship no geometry file.
func toOrchestratorDesktop(d *DesktopSpec) *orchestrator.DesktopSpec {
	if d == nil {
		return nil
	}
	return &orchestrator.DesktopSpec{Width: d.Width, Height: d.Height}
}

// toAPIResourceSpec converts an orchestrator.Spec into the wire shape.
// The duration → seconds coercion here is one-way intentional: the
// wire contract uses seconds for ergonomics (no Go-specific parser).
func toAPIResourceSpec(s orchestrator.Spec) ResourceSpec {
	return ResourceSpec{
		CPUs:               int32(s.CPUs),
		RamMB:              int32(s.RamMB),
		StorageGB:          int32(s.StorageGB),
		Region:             s.Region,
		Arch:               s.Arch,
		MaxRuntimeSeconds:  int64(s.MaxRuntime.Seconds()),
		IdleTimeoutSeconds: int64(s.IdleTimeout.Seconds()),
		Image:              s.Image,
		GPUs:               s.GPUs,
		GPUKind:            s.GPUKind,
		GPUProfile:         s.GPUProfile,
		HostID:             s.HostID,
		Labels:             copyLabels(s.Labels),
	}
}

// toOrchestratorSpec converts a wire spec into the orchestrator type.
// GPUProfile is lowercased so profile matching against host inventory is
// case-insensitive end to end.
func toOrchestratorSpec(s ResourceSpec) orchestrator.Spec {
	return orchestrator.Spec{
		CPUs:        int(s.CPUs),
		RamMB:       int(s.RamMB),
		StorageGB:   int(s.StorageGB),
		Region:      s.Region,
		Arch:        orchestrator.NormalizeArch(s.Arch),
		MaxRuntime:  time.Duration(s.MaxRuntimeSeconds) * time.Second,
		IdleTimeout: time.Duration(s.IdleTimeoutSeconds) * time.Second,
		Image:       s.Image,
		GPUs:        s.GPUs,
		GPUKind:     s.GPUKind,
		GPUProfile:  strings.ToLower(s.GPUProfile),
		HostID:      s.HostID,
		Labels:      copyLabels(s.Labels),
	}
}

// copyLabels clones a label map so neither side of a conversion aliases the
// other. Nil (and empty) in, nil out, so the json omitempty behavior and the
// scheduler's "no selector" fast path are both preserved.
func copyLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// toOrchestratorExpose converts wire expose entries into the orchestrator type.
func toOrchestratorExpose(in []ExposeSpec) []orchestrator.ExposeSpec {
	if len(in) == 0 {
		return nil
	}
	out := make([]orchestrator.ExposeSpec, len(in))
	for i, e := range in {
		out[i] = orchestrator.ExposeSpec{Port: e.Port, As: e.As}
	}
	return out
}

// toAPIEndpoints converts orchestrator endpoints into the wire shape.
func toAPIEndpoints(in []orchestrator.Endpoint) []Endpoint {
	if len(in) == 0 {
		return nil
	}
	out := make([]Endpoint, len(in))
	for i, e := range in {
		out[i] = Endpoint{As: e.As, URL: e.URL, Port: e.Port}
	}
	return out
}

// toAPIHost converts an orchestrator.Host into the wire shape.
func toAPIHost(h orchestrator.Host) HostInfo {
	return HostInfo{
		ID:      h.ID,
		URL:     h.URL,
		Region:  h.Region,
		Backend: string(h.Backend),
		Labels:  copyLabels(h.Labels),
		State:   string(h.State),
		Capacity: HostCapacity{
			CPUs:         h.Capacity.CPUs,
			RamMB:        h.Capacity.RamMB,
			StorageGB:    h.Capacity.StorageGB,
			VMCount:      h.Capacity.VMCount,
			Arch:         h.Capacity.Arch,
			GPUs:         h.Capacity.GPUs,
			GPUKind:      h.Capacity.GPUKind,
			MIGProfiles:  copyMIGProfiles(h.Capacity.MIGProfiles),
			MIGInstances: toAPIMIGInstances(h.Capacity.MIGInstances),
			GPUDevices:   toAPIGPUDevices(h.Capacity.GPUDevices),
		},
		Allocated: HostCapacity{
			CPUs:             h.Allocated.CPUs,
			RamMB:            h.Allocated.RamMB,
			StorageGB:        h.Allocated.StorageGB,
			VMCount:          h.Allocated.VMCount,
			GPUs:             h.Allocated.GPUs,
			GPUKind:          h.Allocated.GPUKind,
			MIGProfiles:      copyMIGProfiles(h.Allocated.MIGProfiles),
			MIGInstanceUUIDs: append([]string(nil), h.Allocated.MIGInstanceUUIDs...),
		},
		LastSeen:  h.LastSeen,
		CreatedAt: h.CreatedAt,
		UpdatedAt: h.UpdatedAt,
	}
}

// copyMIGProfiles clones a MIG profile map so wire responses never alias
// the fleet's live allocation state. Nil (and empty) in, nil out, so the
// json omitempty behavior is preserved.
func copyMIGProfiles(in map[string]int) map[string]int {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// toAPIGPUDevices maps the orchestrator's per-device GPU list to the wire
// shape. Returns nil for an empty input so the field is omitted from JSON.
func toAPIGPUDevices(devs []orchestrator.GPUDevice) []GPUDevice {
	if len(devs) == 0 {
		return nil
	}
	out := make([]GPUDevice, len(devs))
	for i, d := range devs {
		out[i] = GPUDevice{
			UUID:          d.UUID,
			Model:         d.Model,
			PCIBusID:      d.PCIBusID,
			MemoryMB:      d.MemoryMB,
			DriverVersion: d.DriverVersion,
			CUDAVersion:   d.CUDAVersion,
			ComputeCap:    d.ComputeCap,
			MIGCapable:    d.MIGCapable,
			MIGMode:       d.MIGMode,
			IOMMUGroup:    d.IOMMUGroup,
		}
	}
	return out
}

// toAPIMIGInstances maps the orchestrator's per-instance MIG inventory to the
// wire shape. Returns nil for an empty input so the field is omitted from JSON.
func toAPIMIGInstances(insts []orchestrator.MIGInstance) []MIGInstance {
	if len(insts) == 0 {
		return nil
	}
	out := make([]MIGInstance, len(insts))
	for i, inst := range insts {
		out[i] = MIGInstance{
			UUID:          inst.UUID,
			Profile:       inst.Profile,
			Kind:          inst.Kind,
			ParentGPUUUID: inst.ParentGPUUUID,
		}
	}
	return out
}

// snapshotKind renders a record's kind for the wire, resolving the empty
// value to "disk".
//
// Empty is not a third state: it is a row written before the column existed,
// or a provider with only one kind to report. Resolving it here rather than
// letting it through as "" means every read path answers a caller that
// branches on kind, instead of handing it a value it has to special-case.
func snapshotKind(k orchestrator.SnapshotKind) string {
	if k == "" {
		return string(orchestrator.SnapshotKindDisk)
	}
	return string(k)
}

// toAPISnapshot converts a persisted SnapshotRecord into the wire shape.
// Any unknown metadata fields are dropped — the wire schema only
// exposes a subset today.
func toAPISnapshot(s orchestrator.SnapshotRecord) Snapshot {
	out := Snapshot{
		ID:               s.SnapshotID,
		VMID:             s.VMID,
		TaskID:           s.TaskID,
		TenantID:         s.TenantID,
		ParentSnapshotID: s.ParentSnapshotID,
		Mode:             string(s.Mode),
		LayerKey:         s.LayerKey,
		Arch:             s.Arch,
		Digest:           s.Digest,
		Kind:             snapshotKind(s.Kind),
		State:            string(s.State),
		CreatedAt:        s.CreatedAt,
		UpdatedAt:        s.UpdatedAt,
		RetentionUntil:   s.RetentionUntil,
		SizeBytes:        s.SizeBytes,
		LastError:        s.LastError,
	}
	// Best-effort comment extraction. The orchestrator snapshot
	// metadata is a free-form JSON blob; we only surface a "comment"
	// field if it's present and a string. Missing or malformed
	// metadata falls through as empty.
	if len(s.Metadata) > 0 {
		if comment := extractStringField(s.Metadata, "comment"); comment != "" {
			out.Comment = comment
		}
		if exportRef := extractStringField(s.Metadata, "export_ref"); exportRef != "" {
			out.ExportRef = exportRef
		}
		if name := extractStringField(s.Metadata, "name"); name != "" {
			out.Name = name
		}
	}
	if len(s.Exports) > 0 {
		out.Exports = make([]SnapshotExport, 0, len(s.Exports))
		for _, export := range s.Exports {
			out.Exports = append(out.Exports, SnapshotExport{
				Destination: export.Destination,
				Status:      string(export.Status),
				RequestedAt: export.RequestedAt,
				UpdatedAt:   export.UpdatedAt,
				LastError:   export.LastError,
			})
		}
	}
	return out
}
