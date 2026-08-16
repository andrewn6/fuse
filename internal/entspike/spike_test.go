package entspike

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/folsomintel/fuse/internal/entspike/ent"
	"github.com/folsomintel/fuse/internal/entspike/ent/host"
	"github.com/folsomintel/fuse/internal/entspike/ent/vm"
)

// same convention as state_store_postgres_test.go: skip without DATABASE_URL
func openClient(t *testing.T) *ent.Client {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping ent spike test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { client.Close() })
	return client
}

func TestEntSpikeCRUD(t *testing.T) {
	ctx := context.Background()
	client := openClient(t)

	// auto-migrate the throwaway test database
	if err := client.Schema.Create(ctx); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		client.VM.Delete().ExecX(ctx)
		client.Host.Delete().ExecX(ctx)
	})

	h, err := client.Host.Create().
		SetID("host-1").
		SetURL("https://10.0.0.5:8080").
		SetTokenEncrypted([]byte{0xde, 0xad, 0xbe, 0xef}).
		SetRegion("bhs").
		SetCpusTotal(32).
		SetRAMMBTotal(131072).
		SetLabels(map[string]string{"gpu": "none", "tier": "baremetal"}).
		SetArch("amd64").
		SetLastSeenAt(time.Now()).
		Save(ctx)
	if err != nil {
		t.Fatalf("create host: %v", err)
	}

	for _, spec := range []struct {
		id    string
		state vm.State
	}{
		{"vm-1", vm.StateRunning},
		{"vm-2", vm.StateRunning},
		{"vm-3", vm.StateDraining},
	} {
		_, err := client.VM.Create().
			SetID(spec.id).
			SetHostID(h.ID).
			SetState(spec.state).
			SetCpus(4).
			SetRAMMB(8192).
			Save(ctx)
		if err != nil {
			t.Fatalf("create %s: %v", spec.id, err)
		}
	}

	// typed predicate query: running vms on host-1
	running, err := client.VM.Query().
		Where(vm.HostID(h.ID), vm.StateEQ(vm.StateRunning)).
		All(ctx)
	if err != nil {
		t.Fatalf("query running vms: %v", err)
	}
	if len(running) != 2 {
		t.Fatalf("want 2 running vms, got %d", len(running))
	}

	// typed atomic increment, the scheduler allocation pattern
	h2, err := client.Host.UpdateOneID(h.ID).
		AddCpusAllocated(8).
		AddRAMMBAllocated(16384).
		AddVMCountAllocated(2).
		Save(ctx)
	if err != nil {
		t.Fatalf("update allocation: %v", err)
	}
	if h2.CpusAllocated != 8 || h2.VMCountAllocated != 2 {
		t.Fatalf("allocation mismatch: %+v", h2)
	}

	// json labels round-trip
	got, err := client.Host.Query().Where(host.ID("host-1")).Only(ctx)
	if err != nil {
		t.Fatalf("reload host: %v", err)
	}
	if got.Labels["tier"] != "baremetal" {
		t.Fatalf("labels did not round-trip: %+v", got.Labels)
	}
	if string(got.TokenEncrypted) != "\xde\xad\xbe\xef" {
		t.Fatalf("bytea did not round-trip: %x", got.TokenEncrypted)
	}

	// invalid enum value is rejected before it hits the database
	_, err = client.VM.Create().SetID("vm-bad").SetState(vm.State("exploded")).Save(ctx)
	if err == nil {
		t.Fatal("expected enum validation error, got nil")
	}
}
