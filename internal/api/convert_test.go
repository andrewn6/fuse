package api

import (
	"strings"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// TestToAPIHost_MIGInstancesRoundTrip checks that per-instance MIG inventory
// on capacity and the bound-uuid set on allocated both flow through to the
// wire shape, so SDK clients see which MIG instances a host offers and which
// are currently bound.
func TestToAPIHost_MIGInstancesRoundTrip(t *testing.T) {
	h := orchestrator.Host{
		ID:      "h1",
		URL:     "http://h1.test",
		Backend: orchestrator.BackendQEMU,
		State:   orchestrator.HostActive,
		Capacity: orchestrator.HostCapacity{
			CPUs: 8, RamMB: 4096, StorageGB: 100, VMCount: 10, GPUKind: "a100",
			MIGInstances: []orchestrator.MIGInstance{
				{UUID: "m1", Profile: "1g.10gb", Kind: "a100", ParentGPUUUID: "gpu-a"},
				{UUID: "m2", Profile: "1g.10gb", Kind: "a100", ParentGPUUUID: "gpu-a"},
			},
		},
		Allocated: orchestrator.HostCapacity{
			MIGInstanceUUIDs: []string{"m1"},
			MIGProfiles:      map[string]int{"1g.10gb": 1},
		},
	}

	info := toAPIHost(h)
	if len(info.Capacity.MIGInstances) != 2 {
		t.Fatalf("capacity mig_instances = %v, want 2 entries", info.Capacity.MIGInstances)
	}
	got := info.Capacity.MIGInstances[0]
	if got.UUID != "m1" || got.Profile != "1g.10gb" || got.Kind != "a100" || got.ParentGPUUUID != "gpu-a" {
		t.Errorf("first mig_instance = %+v, want full field round-trip", got)
	}
	if len(info.Allocated.MIGInstanceUUIDs) != 1 || info.Allocated.MIGInstanceUUIDs[0] != "m1" {
		t.Errorf("allocated mig_instance_uuids = %v, want [m1]", info.Allocated.MIGInstanceUUIDs)
	}
	// the returned slices must not alias the orchestrator's backing arrays,
	// so a later mutation can't leak into a previously-serialized response.
	info.Capacity.MIGInstances[0].UUID = "mutated"
	if h.Capacity.MIGInstances[0].UUID == "mutated" {
		t.Error("capacity mig_instances aliases the orchestrator slice")
	}
	info.Allocated.MIGInstanceUUIDs[0] = "mutated"
	if h.Allocated.MIGInstanceUUIDs[0] == "mutated" {
		t.Error("allocated mig_instance_uuids aliases the orchestrator slice")
	}
}

// TestPlacementSpecRoundTrip checks that a host pin and label selectors
// survive both directions of the single conversion point, and that neither
// side aliases the other's label map.
func TestPlacementSpecRoundTrip(t *testing.T) {
	wire := ResourceSpec{
		CPUs: 2, RamMB: 1024,
		HostID: "build-3",
		Labels: map[string]string{"disk": "nvme"},
	}
	spec := toOrchestratorSpec(wire)
	if spec.HostID != "build-3" {
		t.Errorf("spec.HostID = %q, want build-3", spec.HostID)
	}
	if spec.Labels["disk"] != "nvme" {
		t.Errorf("spec.Labels = %v, want disk=nvme", spec.Labels)
	}
	spec.Labels["disk"] = "ssd"
	if wire.Labels["disk"] != "nvme" {
		t.Errorf("toOrchestratorSpec aliased the wire label map")
	}

	back := toAPIResourceSpec(spec)
	if back.HostID != "build-3" || back.Labels["disk"] != "ssd" {
		t.Errorf("round trip = %+v, want the placement fields preserved", back)
	}
}

// TestPlacementSpecEmptyStaysNil keeps the omitempty behavior and the
// scheduler's "no selector" fast path: an empty spec must convert to nil
// labels, not an allocated empty map.
func TestPlacementSpecEmptyStaysNil(t *testing.T) {
	spec := toOrchestratorSpec(ResourceSpec{CPUs: 2})
	if spec.HostID != "" || spec.Labels != nil {
		t.Errorf("spec placement = %q/%v, want empty and nil", spec.HostID, spec.Labels)
	}
	if back := toAPIResourceSpec(spec); back.HostID != "" || back.Labels != nil {
		t.Errorf("wire placement = %q/%v, want empty and nil", back.HostID, back.Labels)
	}
}

// TestToAPIHost_LabelsRoundTrip checks host labels reach the wire shape and
// that the response never aliases the fleet's live label map.
func TestToAPIHost_LabelsRoundTrip(t *testing.T) {
	h := orchestrator.Host{
		ID:       "build-3",
		Labels:   map[string]string{"disk": "nvme"},
		Capacity: orchestrator.HostCapacity{CPUs: 8, RamMB: 4096, StorageGB: 100, VMCount: 10},
	}
	info := toAPIHost(h)
	if info.Labels["disk"] != "nvme" {
		t.Fatalf("labels = %v, want disk=nvme", info.Labels)
	}
	info.Labels["disk"] = "ssd"
	if h.Labels["disk"] != "nvme" {
		t.Errorf("toAPIHost aliased the host label map")
	}
	if got := toAPIHost(orchestrator.Host{ID: "h2"}); got.Labels != nil {
		t.Errorf("labels = %v, want nil for a host with none", got.Labels)
	}
}

// TestResourceSpecTimeoutsRoundTrip checks that both duration knobs survive
// the wire -> orchestrator -> wire trip, in seconds either side.
func TestResourceSpecTimeoutsRoundTrip(t *testing.T) {
	in := ResourceSpec{MaxRuntimeSeconds: 14400, IdleTimeoutSeconds: 900}

	spec := toOrchestratorSpec(in)
	if spec.MaxRuntime != 4*time.Hour {
		t.Errorf("MaxRuntime = %v, want 4h", spec.MaxRuntime)
	}
	if spec.IdleTimeout != 15*time.Minute {
		t.Errorf("IdleTimeout = %v, want 15m", spec.IdleTimeout)
	}

	out := toAPIResourceSpec(spec)
	if out.MaxRuntimeSeconds != in.MaxRuntimeSeconds {
		t.Errorf("max_runtime_seconds = %d, want %d", out.MaxRuntimeSeconds, in.MaxRuntimeSeconds)
	}
	if out.IdleTimeoutSeconds != in.IdleTimeoutSeconds {
		t.Errorf("idle_timeout_seconds = %d, want %d", out.IdleTimeoutSeconds, in.IdleTimeoutSeconds)
	}
}

func TestValidateTimeoutSpec(t *testing.T) {
	cases := []struct {
		name    string
		spec    ResourceSpec
		wantErr bool
	}{
		{"zero is fine", ResourceSpec{}, false},
		{"one minute idle", ResourceSpec{IdleTimeoutSeconds: 60}, false},
		{"both set", ResourceSpec{MaxRuntimeSeconds: 3600, IdleTimeoutSeconds: 900}, false},
		{"negative max runtime", ResourceSpec{MaxRuntimeSeconds: -3600}, true},
		{"negative idle timeout", ResourceSpec{IdleTimeoutSeconds: -900}, true},
		{"sub-minute idle timeout", ResourceSpec{IdleTimeoutSeconds: 10}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTimeoutSpec(tc.spec)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// TestToOrchestratorHealthcheck checks the create-request probe reaches the
// orchestrator intact, and that a request without one produces a nil spec:
// nil is what tells the boot path to ship no probe file and the reconcile loop
// not to poll the guest.
func TestToOrchestratorHealthcheck(t *testing.T) {
	got := toOrchestratorHealthcheck(&HealthcheckSpec{
		HTTP:               &HealthcheckHTTP{Port: 8080, Path: "/healthz"},
		IntervalSeconds:    5,
		TimeoutSeconds:     2,
		Retries:            12,
		StartPeriodSeconds: 10,
	})
	if got == nil {
		t.Fatal("healthcheck = nil, want a spec")
	}
	if got.HTTP == nil || got.HTTP.Port != 8080 || got.HTTP.Path != "/healthz" {
		t.Errorf("http = %+v, want port 8080 path /healthz", got.HTTP)
	}
	if got.IntervalSeconds != 5 || got.TimeoutSeconds != 2 || got.Retries != 12 || got.StartPeriodSeconds != 10 {
		t.Errorf("timings = %+v, want 5/2/12/10", got)
	}

	if toOrchestratorHealthcheck(nil) != nil {
		t.Error("a nil healthcheck must convert to nil, not to an empty spec")
	}
}

// TestToAPIHealth checks the guest's verdict reaches the wire, and that an
// environment with no verdict omits the field rather than reporting an empty
// state a client would have to defend against.
func TestToAPIHealth(t *testing.T) {
	since := time.Now().UTC().Truncate(time.Second)
	got := toAPIHealth(&orchestrator.HealthStatus{
		State:    orchestrator.HealthFailing,
		Since:    since,
		Failures: 3,
		Message:  "connection refused",
	})
	if got == nil {
		t.Fatal("health = nil, want a verdict")
	}
	if got.State != "failing" || got.Failures != 3 || got.Message != "connection refused" {
		t.Errorf("health = %+v", got)
	}
	if !got.Since.Equal(since) {
		t.Errorf("since = %s, want %s", got.Since, since)
	}

	if toAPIHealth(nil) != nil {
		t.Error("a nil verdict must convert to nil so the field is omitted")
	}
}

// TestValidateHealthcheck holds raw SDK callers to the same rules Fusefile
// authors get, since the compiler's checks run client-side and are trivially
// bypassed.
func TestValidateHealthcheck(t *testing.T) {
	cases := []struct {
		name string
		spec *HealthcheckSpec
		want string
	}{
		{name: "absent", spec: nil},
		{name: "http only", spec: &HealthcheckSpec{HTTP: &HealthcheckHTTP{Port: 80}}},
		{name: "exec only", spec: &HealthcheckSpec{Exec: []string{"/ready"}}},
		{
			name: "both",
			spec: &HealthcheckSpec{HTTP: &HealthcheckHTTP{Port: 80}, Exec: []string{"/ready"}},
			want: "mutually exclusive",
		},
		{name: "neither", spec: &HealthcheckSpec{Retries: 3}, want: "http or exec is required"},
		{
			name: "port out of range",
			spec: &HealthcheckSpec{HTTP: &HealthcheckHTTP{Port: 70000}},
			want: "between 1 and 65535",
		},
		{
			name: "path without a slash",
			spec: &HealthcheckSpec{HTTP: &HealthcheckHTTP{Port: 80, Path: "healthz"}},
			want: "must start with a slash",
		},
		{
			name: "empty exec argument",
			spec: &HealthcheckSpec{Exec: []string{"/ready", " "}},
			want: "must not be empty",
		},
		{
			name: "negative retries",
			spec: &HealthcheckSpec{Exec: []string{"/ready"}, Retries: -1},
			want: "must not be negative",
		},
		{
			name: "timeout above interval",
			spec: &HealthcheckSpec{Exec: []string{"/ready"}, IntervalSeconds: 2, TimeoutSeconds: 5},
			want: "must not exceed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHealthcheck(tc.spec)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("rejected a valid healthcheck: %v", err)
			case tc.want == "":
				return
			case err == nil:
				t.Fatalf("accepted %s", tc.name)
			case !strings.Contains(err.Error(), tc.want):
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestToAPISnapshot_kindDefaultsToDisk pins the empty-to-disk resolution.
// Empty is not a third kind: it is a row written before the column existed, or
// a provider with only one kind to report. Letting "" onto the wire would push
// the special case into every client, and the SDKs document kind as always
// being one of the two values.
func TestToAPISnapshot_kindDefaultsToDisk(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   orchestrator.SnapshotKind
		want string
	}{
		{"empty reads as disk", "", "disk"},
		{"disk passes through", orchestrator.SnapshotKindDisk, "disk"},
		{"live passes through", orchestrator.SnapshotKindLive, "live"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := toAPISnapshot(orchestrator.SnapshotRecord{SnapshotID: "s1", Kind: tc.in})
			if got.Kind != tc.want {
				t.Errorf("kind = %q, want %q", got.Kind, tc.want)
			}
		})
	}
}
