package entspike

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/folsomintel/fuse/internal/entspike/ent"
	"github.com/folsomintel/fuse/internal/entspike/ent/host"
	"github.com/folsomintel/fuse/internal/entspike/ent/vm"
)

// TestEntAgainstRealSchema proves the ent client works against a database
// created by the real hand-written migrations in
// internal/orchestrator/migrations, with no ent auto-migration involved.
// This is the adoption-safety check: ent only ever issues DML against the
// schema ApplyMigrations owns.
func TestEntAgainstRealSchema(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("REAL_SCHEMA_DATABASE_URL")
	if dsn == "" {
		t.Skip("REAL_SCHEMA_DATABASE_URL not set; skipping real-schema ent test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// apply the real migrations in lexical order, like ApplyMigrations
	files, err := filepath.Glob("../orchestrator/migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("find real migrations: %v (found %d)", err, len(files))
	}
	sort.Strings(files)
	for _, f := range files {
		sqlBytes, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := db.ExecContext(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}

	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))

	h, err := client.Host.Create().
		SetID("real-host-1").
		SetURL("https://10.0.0.9:8080").
		SetTokenEncrypted([]byte{0x01, 0x02}).
		SetRegion("bhs").
		SetCpusTotal(64).
		SetLabels(map[string]string{"tier": "baremetal"}).
		SetArch("amd64").
		SetLastSeenAt(time.Now()).
		Save(ctx)
	if err != nil {
		t.Fatalf("create host in real schema: %v", err)
	}

	if _, err := client.VM.Create().
		SetID("real-vm-1").
		SetHostID(h.ID).
		SetState(vm.StateRunning).
		SetCpus(4).
		SetIdleTimeoutSeconds(300).
		SetCreatedAt(time.Now()).
		SetUpdatedAt(time.Now()).
		Save(ctx); err != nil {
		t.Fatalf("create vm in real schema: %v", err)
	}

	got, err := client.Host.Query().Where(host.ID("real-host-1")).Only(ctx)
	if err != nil {
		t.Fatalf("query host: %v", err)
	}
	if got.Labels["tier"] != "baremetal" {
		t.Fatalf("labels_json did not round-trip through real schema: %+v", got.Labels)
	}

	n, err := client.VM.Query().Where(vm.StateEQ(vm.StateRunning)).Count(ctx)
	if err != nil {
		t.Fatalf("count vms: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 running vm, got %d", n)
	}

	// columns ent does not know about (gpu, backend, endpoints_json ...)
	// must have landed with their migration defaults
	var backend string
	if err := db.QueryRowContext(ctx,
		"SELECT backend FROM orchestrator_hosts WHERE host_id = 'real-host-1'").Scan(&backend); err != nil {
		t.Fatalf("read backend column: %v", err)
	}
	if backend != "firecracker" {
		t.Fatalf("backend default mismatch: %q", backend)
	}
}
