package orchestrator

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// openTestPostgres returns a migrated PostgresStateStore backed by
// DATABASE_URL, or skips the test if the env var is unset. This mirrors
// the env var the orchestrator binary itself uses to opt into Postgres
// (see orchestrator/main.go), so a developer can run this test
// locally with: DATABASE_URL=... go test ./internal/orchestrator/
func openTestPostgres(t *testing.T) *PostgresStateStore {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping postgres state store test")
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("ping database: %v", err)
	}

	store := NewPostgresStateStore(db)
	if err := store.ApplyMigrations(context.Background()); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return store
}

// TestPostgresStateStore_HostAndVMGPUFields verifies that a qemu host's
// GPU capacity/allocation and backend, and a VM's GPU spec, survive an
// upsert + read round trip through Postgres.
func TestPostgresStateStore_HostAndVMGPUFields(t *testing.T) {
	store := openTestPostgres(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	hostID := "host-gpu-test"
	vmID := "vm-gpu-test"

	// clean up any leftovers from a previous failed run before asserting.
	t.Cleanup(func() {
		_ = store.DeleteVM(ctx, vmID)
		_ = store.DeleteHost(ctx, hostID)
	})

	host := HostRecord{
		ID:      hostID,
		URL:     "https://qemu-host.local",
		Region:  "us-east-1",
		State:   HostActive,
		Backend: BackendQEMU,
		Capacity: HostCapacity{
			CPUs: 8, RamMB: 32768, StorageGB: 200, VMCount: 4, GPUs: 2, GPUKind: "a100",
			GPUDevices: []GPUDevice{
				{UUID: "gpu-a", Model: "NVIDIA A100-SXM4-40GB", MemoryMB: 40960},
				{UUID: "gpu-b", Model: "NVIDIA A100-SXM4-40GB", MemoryMB: 40960},
			},
		},
		Allocated: HostCapacity{
			GPUs: 1,
		},
		LastSeen:  now,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.UpsertHost(ctx, host); err != nil {
		t.Fatalf("upsert host: %v", err)
	}

	vm := VMRecord{
		ID:          vmID,
		HostID:      hostID,
		NetworkHost: "10.0.0.5:2222",
		State:       VMStateRunning,
		Spec: Spec{
			CPUs:      2,
			RamMB:     4096,
			StorageGB: 20,
			GPUs:      1,
			GPUKind:   "a100",
			GPUUUIDs:  []string{"gpu-a"},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.UpsertVM(ctx, vm); err != nil {
		t.Fatalf("upsert vm: %v", err)
	}

	gotHost, err := store.GetHost(ctx, hostID)
	if err != nil {
		t.Fatalf("get host: %v", err)
	}
	if gotHost.Backend != BackendQEMU {
		t.Errorf("host backend = %q, want %q", gotHost.Backend, BackendQEMU)
	}
	if gotHost.Capacity.GPUs != 2 {
		t.Errorf("host capacity gpus = %d, want 2", gotHost.Capacity.GPUs)
	}
	if gotHost.Capacity.GPUKind != "a100" {
		t.Errorf("host capacity gpu_kind = %q, want %q", gotHost.Capacity.GPUKind, "a100")
	}
	if gotHost.Allocated.GPUs != 1 {
		t.Errorf("host allocated gpus = %d, want 1", gotHost.Allocated.GPUs)
	}
	if len(gotHost.Capacity.GPUDevices) != 2 {
		t.Fatalf("host capacity gpu_devices = %v, want 2 devices", gotHost.Capacity.GPUDevices)
	}
	if gotHost.Capacity.GPUDevices[0].UUID != "gpu-a" || gotHost.Capacity.GPUDevices[1].UUID != "gpu-b" {
		t.Errorf("host gpu device uuids = %q,%q, want gpu-a,gpu-b",
			gotHost.Capacity.GPUDevices[0].UUID, gotHost.Capacity.GPUDevices[1].UUID)
	}

	vms, err := store.ListVMs(ctx)
	if err != nil {
		t.Fatalf("list vms: %v", err)
	}
	var gotVM *VMRecord
	for i := range vms {
		if vms[i].ID == vmID {
			gotVM = &vms[i]
			break
		}
	}
	if gotVM == nil {
		t.Fatalf("vm %s not found in ListVMs result", vmID)
	}
	if gotVM.Spec.GPUs != 1 {
		t.Errorf("vm spec gpus = %d, want 1", gotVM.Spec.GPUs)
	}
	if gotVM.Spec.GPUKind != "a100" {
		t.Errorf("vm spec gpu_kind = %q, want %q", gotVM.Spec.GPUKind, "a100")
	}
	if len(gotVM.Spec.GPUUUIDs) != 1 || gotVM.Spec.GPUUUIDs[0] != "gpu-a" {
		t.Errorf("vm spec gpu_uuids = %v, want [gpu-a]", gotVM.Spec.GPUUUIDs)
	}
}

// TestPostgresStateStore_HostBackendDefaultsToFirecracker verifies two
// things about the migration's DEFAULT 'firecracker':
//  1. A pre-existing row written before this migration (no backend/gpu
//     columns supplied) reads back with backend="firecracker" and
//     gpus_total=0, so old rows keep working after the schema change.
//  2. A HostRecord that explicitly sets Backend (as FleetManager.RegisterHost
//     always does before calling UpsertHost) round-trips unchanged.
func TestPostgresStateStore_HostBackendDefaultsToFirecracker(t *testing.T) {
	store := openTestPostgres(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)

	t.Run("legacy row backfills to firecracker default", func(t *testing.T) {
		hostID := "host-legacy-row-test"
		t.Cleanup(func() { _ = store.DeleteHost(ctx, hostID) })

		// Insert a row the way it would have looked before this migration:
		// no backend/gpu columns in the statement at all, so Postgres
		// applies each column's DEFAULT.
		_, err := store.db.ExecContext(ctx, `
			INSERT INTO orchestrator_hosts (
				host_id, url, token_encrypted, region, state, tenant_id,
				cpus_total, ram_mb_total, storage_gb_total, vm_count_max,
				cpus_allocated, ram_mb_allocated, storage_gb_allocated, vm_count_allocated,
				last_seen_at, created_at, updated_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		`,
			hostID, "https://fc-host.local", []byte(nil), "", string(HostActive), "",
			4, 8192, 50, 2,
			0, 0, 0, 0,
			now.UTC(), now.UTC(), now.UTC(),
		)
		if err != nil {
			t.Fatalf("insert legacy-shaped row: %v", err)
		}

		got, err := store.GetHost(ctx, hostID)
		if err != nil {
			t.Fatalf("get host: %v", err)
		}
		if got.Backend != BackendFirecracker {
			t.Errorf("host backend = %q, want %q", got.Backend, BackendFirecracker)
		}
		if got.Capacity.GPUs != 0 {
			t.Errorf("host capacity gpus = %d, want 0", got.Capacity.GPUs)
		}
		if got.Allocated.GPUs != 0 {
			t.Errorf("host allocated gpus = %d, want 0", got.Allocated.GPUs)
		}
	})

	t.Run("explicit firecracker backend round-trips", func(t *testing.T) {
		hostID := "host-explicit-backend-test"
		t.Cleanup(func() { _ = store.DeleteHost(ctx, hostID) })

		host := HostRecord{
			ID:        hostID,
			URL:       "https://fc-host.local",
			State:     HostActive,
			Backend:   BackendFirecracker,
			Capacity:  HostCapacity{CPUs: 4, RamMB: 8192, StorageGB: 50, VMCount: 2},
			LastSeen:  now,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if err := store.UpsertHost(ctx, host); err != nil {
			t.Fatalf("upsert host: %v", err)
		}

		got, err := store.GetHost(ctx, hostID)
		if err != nil {
			t.Fatalf("get host: %v", err)
		}
		if got.Backend != BackendFirecracker {
			t.Errorf("host backend = %q, want %q", got.Backend, BackendFirecracker)
		}
		if got.Capacity.GPUs != 0 {
			t.Errorf("host capacity gpus = %d, want 0", got.Capacity.GPUs)
		}
	})
}
