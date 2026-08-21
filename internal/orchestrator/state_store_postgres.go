package orchestrator

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/folsomintel/fuse/internal/entspike/ent"
	entvm "github.com/folsomintel/fuse/internal/entspike/ent/vm"
)

const migrationsTableName = "orchestrator_schema_migrations"

//go:embed migrations/*.sql
var migrationFiles embed.FS

// PostgresStateStore persists orchestrator state in Postgres.
// The vm entity goes through the ent client (first entity of the ent
// adoption, see internal/entspike); everything else is still raw sql.
// Both share the same *sql.DB, and migrations stay owned by
// ApplyMigrations: ent issues DML only, never DDL.
type PostgresStateStore struct {
	db   *sql.DB
	entc *ent.Client
}

// NewPostgresStateStore creates a Postgres-backed state store.
func NewPostgresStateStore(db *sql.DB) *PostgresStateStore {
	return &PostgresStateStore{
		db:   db,
		entc: ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db))),
	}
}

// ApplyMigrations creates and upgrades orchestrator state tables.
func (s *PostgresStateStore) ApplyMigrations(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`, migrationsTableName)); err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}

	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		version := entry.Name()
		var exists bool
		if err := s.db.QueryRowContext(ctx,
			fmt.Sprintf("SELECT EXISTS(SELECT 1 FROM %s WHERE version=$1)", migrationsTableName),
			version,
		).Scan(&exists); err != nil {
			return fmt.Errorf("check migration %s: %w", version, err)
		}
		if exists {
			continue
		}

		sqlBytes, err := migrationFiles.ReadFile(filepath.ToSlash(filepath.Join("migrations", version)))
		if err != nil {
			return fmt.Errorf("read migration %s: %w", version, err)
		}

		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration tx %s: %w", version, err)
		}

		if _, err := tx.ExecContext(ctx, string(sqlBytes)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO %s(version) VALUES($1)", migrationsTableName),
			version,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %s: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", version, err)
		}
	}

	return nil
}

func (s *PostgresStateStore) UpsertVM(ctx context.Context, vm VMRecord) error {
	maxRuntimeSeconds := int(vm.Spec.MaxRuntime.Seconds())
	idleTimeoutSeconds := int(vm.Spec.IdleTimeout.Seconds())
	endpoints := vm.Endpoints
	if endpoints == nil {
		endpoints = []Endpoint{}
	}
	endpointsJSON, err := json.Marshal(endpoints)
	if err != nil {
		return fmt.Errorf("marshal endpoints for vm %s: %w", vm.ID, err)
	}
	// per-vm gpu binding: the concrete device uuids bound to this vm. nil is
	// coerced to [] so the column never holds a json null.
	gpuUUIDs := vm.Spec.GPUUUIDs
	if gpuUUIDs == nil {
		gpuUUIDs = []string{}
	}
	gpuUUIDsJSON, err := json.Marshal(gpuUUIDs)
	if err != nil {
		return fmt.Errorf("marshal gpu uuids for vm %s: %w", vm.ID, err)
	}
	// per-vm MIG instance binding: the concrete MIG instance uuids bound to
	// this vm, nil-coerced to [] (mirrors gpu_uuids).
	migInstanceUUIDs := vm.Spec.MIGInstanceUUIDs
	if migInstanceUUIDs == nil {
		migInstanceUUIDs = []string{}
	}
	migInstanceUUIDsJSON, err := json.Marshal(migInstanceUUIDs)
	if err != nil {
		return fmt.Errorf("marshal mig instance uuids for vm %s: %w", vm.ID, err)
	}
	err = s.entc.VM.Create().
		SetID(vm.ID).
		SetHostID(vm.HostID).
		SetNetworkHost(vm.NetworkHost).
		SetState(entvm.State(vm.State)).
		SetURL(vm.URL).
		SetTaskID(vm.TaskID).
		SetTenantID(vm.TenantID).
		SetCpus(vm.Spec.CPUs).
		SetRAMMB(vm.Spec.RamMB).
		SetStorageGB(vm.Spec.StorageGB).
		SetRegion(vm.Spec.Region).
		SetMaxRuntimeSeconds(maxRuntimeSeconds).
		SetIdleTimeoutSeconds(idleTimeoutSeconds).
		SetAuthTokenEncrypted(vm.AuthTokenEncrypted).
		SetSecretsEncrypted(vm.SecretsEncrypted).
		SetLastError(vm.LastError).
		SetEndpoints(json.RawMessage(endpointsJSON)).
		SetGpus(vm.Spec.GPUs).
		SetGpuKind(vm.Spec.GPUKind).
		SetGpuProfile(vm.Spec.GPUProfile).
		SetGpuUuids(json.RawMessage(gpuUUIDsJSON)).
		SetMigInstanceUuids(json.RawMessage(migInstanceUUIDsJSON)).
		SetCreatedAt(vm.CreatedAt.UTC()).
		SetUpdatedAt(vm.UpdatedAt.UTC()).
		OnConflictColumns(entvm.FieldID).
		UpdateNewValues().
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("upsert vm %s: %w", vm.ID, err)
	}
	return nil
}

func (s *PostgresStateStore) DeleteVM(ctx context.Context, vmID string) error {
	// Delete().Where(), not DeleteOneID: stays a no-op for a missing row,
	// same as the plain sql DELETE it replaces
	if _, err := s.entc.VM.Delete().Where(entvm.ID(vmID)).Exec(ctx); err != nil {
		return fmt.Errorf("delete vm %s: %w", vmID, err)
	}
	return nil
}

func (s *PostgresStateStore) ListVMs(ctx context.Context) ([]VMRecord, error) {
	rows, err := s.entc.VM.Query().All(ctx)
	if err != nil {
		return nil, fmt.Errorf("list vms: %w", err)
	}

	var out []VMRecord
	for _, row := range rows {
		record := VMRecord{
			ID:                 row.ID,
			HostID:             row.HostID,
			NetworkHost:        row.NetworkHost,
			State:              VMState(row.State),
			URL:                row.URL,
			TaskID:             row.TaskID,
			TenantID:           row.TenantID,
			AuthTokenEncrypted: row.AuthTokenEncrypted,
			SecretsEncrypted:   row.SecretsEncrypted,
			LastError:          row.LastError,
			CreatedAt:          row.CreatedAt,
			UpdatedAt:          row.UpdatedAt,
		}
		record.Spec.CPUs = row.Cpus
		record.Spec.RamMB = row.RAMMB
		record.Spec.StorageGB = row.StorageGB
		record.Spec.Region = row.Region
		record.Spec.GPUs = row.Gpus
		record.Spec.GPUKind = row.GpuKind
		record.Spec.GPUProfile = row.GpuProfile
		if row.MaxRuntimeSeconds > 0 {
			record.Spec.MaxRuntime = time.Duration(row.MaxRuntimeSeconds) * time.Second
		}
		if row.IdleTimeoutSeconds > 0 {
			record.Spec.IdleTimeout = time.Duration(row.IdleTimeoutSeconds) * time.Second
		}
		if len(row.Endpoints) > 0 {
			if err := json.Unmarshal(row.Endpoints, &record.Endpoints); err != nil {
				return nil, fmt.Errorf("unmarshal endpoints for vm %s: %w", record.ID, err)
			}
		}
		// gpu_uuids defaults to '[]'; a legacy null or empty scan leaves
		// GPUUUIDs nil (no per-device binding), which is the correct signal.
		if len(row.GpuUuids) > 0 {
			if err := json.Unmarshal(row.GpuUuids, &record.Spec.GPUUUIDs); err != nil {
				return nil, fmt.Errorf("unmarshal gpu uuids for vm %s: %w", record.ID, err)
			}
		}
		// mig_instance_uuids defaults to '[]'; an empty scan leaves
		// MIGInstanceUUIDs nil (count-map host, no per-instance binding).
		if len(row.MigInstanceUuids) > 0 {
			if err := json.Unmarshal(row.MigInstanceUuids, &record.Spec.MIGInstanceUUIDs); err != nil {
				return nil, fmt.Errorf("unmarshal mig instance uuids for vm %s: %w", record.ID, err)
			}
		}
		out = append(out, record)
	}
	return out, nil
}

func (s *PostgresStateStore) UpsertTask(ctx context.Context, task TaskRecord) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO orchestrator_tasks (
			task_id, vm_id, run_status, retry_count, last_error, assigned_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (task_id) DO UPDATE SET
			vm_id=EXCLUDED.vm_id,
			run_status=EXCLUDED.run_status,
			retry_count=EXCLUDED.retry_count,
			last_error=EXCLUDED.last_error,
			assigned_at=EXCLUDED.assigned_at,
			updated_at=EXCLUDED.updated_at
	`,
		task.TaskID,
		task.VMID,
		string(task.RunStatus),
		task.RetryCount,
		task.LastError,
		task.AssignedAt.UTC(),
		task.UpdatedAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("upsert task %s: %w", task.TaskID, err)
	}
	return nil
}

func (s *PostgresStateStore) ListTasks(ctx context.Context) ([]TaskRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT task_id, vm_id, run_status, retry_count, last_error, assigned_at, updated_at
		FROM orchestrator_tasks
	`)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []TaskRecord
	for rows.Next() {
		var (
			record TaskRecord
			status string
		)
		if err := rows.Scan(
			&record.TaskID,
			&record.VMID,
			&status,
			&record.RetryCount,
			&record.LastError,
			&record.AssignedAt,
			&record.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan task row: %w", err)
		}
		record.RunStatus = TaskRunStatus(status)
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tasks: %w", err)
	}
	return out, nil
}

func (s *PostgresStateStore) UpsertSnapshot(ctx context.Context, snapshot SnapshotRecord) error {
	if len(snapshot.Metadata) == 0 {
		snapshot.Metadata = json.RawMessage(`{}`)
	}
	if snapshot.State == "" {
		snapshot.State = SnapshotStateReady
	}
	if snapshot.Exports == nil {
		snapshot.Exports = []SnapshotExportRecord{}
	}
	exportsJSON, err := json.Marshal(snapshot.Exports)
	if err != nil {
		return fmt.Errorf("marshal snapshot exports %s: %w", snapshot.SnapshotID, err)
	}
	// The column defaults to 'disk', but naming it in the INSERT bypasses that
	// default, so an unset Kind has to be normalized here or every row written
	// through this path lands as '' instead: a value the migration
	// deliberately never creates and no reader expects.
	kind := snapshot.Kind
	if kind == "" {
		kind = SnapshotKindDisk
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO orchestrator_snapshots (
			snapshot_id, vm_id, task_id, host_id, tenant_id, parent_snapshot_id, mode, state, size_bytes,
			retention_until, metadata_json, exports_json, last_error, created_at, updated_at,
			layer_key, arch, digest, kind
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
		ON CONFLICT (snapshot_id) DO UPDATE SET
			vm_id=EXCLUDED.vm_id,
			task_id=EXCLUDED.task_id,
			host_id=EXCLUDED.host_id,
			tenant_id=EXCLUDED.tenant_id,
			parent_snapshot_id=EXCLUDED.parent_snapshot_id,
			mode=EXCLUDED.mode,
			state=EXCLUDED.state,
			size_bytes=EXCLUDED.size_bytes,
			retention_until=EXCLUDED.retention_until,
			metadata_json=EXCLUDED.metadata_json,
			exports_json=EXCLUDED.exports_json,
			last_error=EXCLUDED.last_error,
			created_at=EXCLUDED.created_at,
			updated_at=EXCLUDED.updated_at,
			layer_key=EXCLUDED.layer_key,
			arch=EXCLUDED.arch,
			digest=EXCLUDED.digest,
			kind=EXCLUDED.kind
	`,
		snapshot.SnapshotID,
		snapshot.VMID,
		snapshot.TaskID,
		snapshot.HostID,
		snapshot.TenantID,
		snapshot.ParentSnapshotID,
		string(snapshot.Mode),
		string(snapshot.State),
		snapshot.SizeBytes,
		snapshot.RetentionUntil,
		[]byte(snapshot.Metadata),
		exportsJSON,
		snapshot.LastError,
		snapshot.CreatedAt.UTC(),
		snapshot.UpdatedAt.UTC(),
		snapshot.LayerKey,
		snapshot.Arch,
		snapshot.Digest,
		string(kind),
	)
	if err != nil {
		return fmt.Errorf("upsert snapshot %s: %w", snapshot.SnapshotID, err)
	}
	return nil
}

func (s *PostgresStateStore) GetSnapshot(ctx context.Context, snapshotID string) (SnapshotRecord, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT snapshot_id, vm_id, task_id, host_id, tenant_id, parent_snapshot_id, mode, state, size_bytes,
		       retention_until, metadata_json, exports_json, last_error, created_at, updated_at,
		       layer_key, arch, digest, kind
		FROM orchestrator_snapshots
		WHERE snapshot_id=$1
	`, snapshotID)

	record, err := scanSnapshotRow(row.Scan)
	if err != nil {
		if err == sql.ErrNoRows {
			return SnapshotRecord{}, fmt.Errorf("snapshot %s not found", snapshotID)
		}
		return SnapshotRecord{}, fmt.Errorf("get snapshot %s: %w", snapshotID, err)
	}
	return record, nil
}

func (s *PostgresStateStore) ListSnapshots(ctx context.Context) ([]SnapshotRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT snapshot_id, vm_id, task_id, host_id, tenant_id, parent_snapshot_id, mode, state, size_bytes,
		       retention_until, metadata_json, exports_json, last_error, created_at, updated_at,
		       layer_key, arch, digest, kind
		FROM orchestrator_snapshots
	`)
	if err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SnapshotRecord
	for rows.Next() {
		record, err := scanSnapshotRow(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan snapshot row: %w", err)
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate snapshots: %w", err)
	}
	return out, nil
}

func (s *PostgresStateStore) DeleteSnapshot(ctx context.Context, snapshotID string) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM orchestrator_snapshots WHERE snapshot_id=$1", snapshotID); err != nil {
		return fmt.Errorf("delete snapshot %s: %w", snapshotID, err)
	}
	return nil
}

func (s *PostgresStateStore) ListSnapshotsByVM(ctx context.Context, vmID string) ([]SnapshotRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT snapshot_id, vm_id, task_id, host_id, tenant_id, parent_snapshot_id, mode, state, size_bytes,
		       retention_until, metadata_json, exports_json, last_error, created_at, updated_at,
		       layer_key, arch, digest, kind
		FROM orchestrator_snapshots
		WHERE vm_id=$1
	`, vmID)
	if err != nil {
		return nil, fmt.Errorf("list snapshots for vm %s: %w", vmID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []SnapshotRecord
	for rows.Next() {
		record, err := scanSnapshotRow(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan snapshot row: %w", err)
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate snapshots for vm %s: %w", vmID, err)
	}
	return out, nil
}

// ListSnapshotsByDigest serves the artifact index off
// orchestrator_snapshots_digest_idx. The `digest <> ”` predicate matches the
// index's partial predicate, so this stays an index probe rather than degrading
// into a scan as snapshots accumulate; it is also a correctness guard, since
// every snapshot written by an agent that does not hash carries an empty digest
// and must not read as "this host holds artifact ”".
//
// Ordering by snapshot_id (not created_at) is deliberate: the caller uses this
// to choose a peer to pull from, and a stable order means a retry asks the same
// host again instead of spreading half-finished transfers over the fleet.
func (s *PostgresStateStore) ListSnapshotsByDigest(ctx context.Context, tenantID, digest string) ([]SnapshotRecord, error) {
	if digest == "" {
		return nil, nil
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT snapshot_id, vm_id, task_id, host_id, tenant_id, parent_snapshot_id, mode, state, size_bytes,
		       retention_until, metadata_json, exports_json, last_error, created_at, updated_at,
		       layer_key, arch, digest, kind
		FROM orchestrator_snapshots
		WHERE tenant_id=$1
		  AND digest=$2
		  AND digest <> ''
		  AND state=$3
		ORDER BY snapshot_id
	`, tenantID, digest, string(SnapshotStateReady))
	if err != nil {
		return nil, fmt.Errorf("list snapshots by digest: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SnapshotRecord
	for rows.Next() {
		record, err := scanSnapshotRow(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan snapshot row: %w", err)
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate snapshots by digest: %w", err)
	}
	return out, nil
}

// FindSnapshotByLayerKey serves the layer cache lookup off
// orchestrator_snapshots_layer_key_idx. The `layer_key <> ”` predicate is not
// redundant with the equality above it: it is what makes the query match the
// index's partial predicate, so this stays a single index probe instead of
// degrading into a scan as build artifacts accumulate.
//
// The layer key is not unique by design (two concurrent builds of the same
// recipe both insert), so duplicates are resolved here rather than rejected at
// write time: newest ready artifact wins, with snapshot_id as the tiebreak so
// this agrees exactly with MemoryStateStore and with sortSnapshotRecords.
//
// tenant_id is a security boundary, not a filter for convenience. Another
// tenant's artifact carries whatever their build baked in, so a cross-tenant
// row has to read as a miss.
func (s *PostgresStateStore) FindSnapshotByLayerKey(ctx context.Context, tenantID, layerKey, arch string) (SnapshotRecord, bool, error) {
	// an empty layer key is not a key, it is every non-layer snapshot in the
	// table. never let it reach the query.
	if layerKey == "" {
		return SnapshotRecord{}, false, nil
	}

	row := s.db.QueryRowContext(ctx, `
		SELECT snapshot_id, vm_id, task_id, host_id, tenant_id, parent_snapshot_id, mode, state, size_bytes,
		       retention_until, metadata_json, exports_json, last_error, created_at, updated_at,
		       layer_key, arch, digest, kind
		FROM orchestrator_snapshots
		WHERE tenant_id=$1
		  AND layer_key=$2
		  AND layer_key <> ''
		  AND arch=$3
		  AND state=$4
		ORDER BY created_at DESC, snapshot_id DESC
		LIMIT 1
	`, tenantID, layerKey, arch, string(SnapshotStateReady))

	record, err := scanSnapshotRow(row.Scan)
	if err != nil {
		if err == sql.ErrNoRows {
			return SnapshotRecord{}, false, nil
		}
		return SnapshotRecord{}, false, fmt.Errorf("find snapshot by layer key %s: %w", layerKey, err)
	}
	return record, true, nil
}

// SnapshotQuotaUsage aggregates count and size for tenantID directly in
// SQL, scoped to the same states/mode enforceSnapshotQuota cares about, so
// per-tenant quota checks never require a full-table scan.
func (s *PostgresStateStore) SnapshotQuotaUsage(ctx context.Context, tenantID string) (SnapshotQuotaUsage, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(size_bytes), 0)
		FROM orchestrator_snapshots
		WHERE tenant_id=$1
		  AND mode <> $2
		  AND state IN ($3, $4, $5)
	`, tenantID, string(SnapshotModeBuild), string(SnapshotStateCreating), string(SnapshotStateReady), string(SnapshotStateRestoring))

	var usage SnapshotQuotaUsage
	if err := row.Scan(&usage.Count, &usage.Bytes); err != nil {
		return SnapshotQuotaUsage{}, fmt.Errorf("snapshot quota usage for tenant %s: %w", tenantID, err)
	}
	return usage, nil
}

func scanSnapshotRow(scan func(dest ...any) error) (SnapshotRecord, error) {
	var (
		record       SnapshotRecord
		mode         string
		state        string
		kind         string
		retentionRaw sql.NullTime
		exportsJSON  []byte
	)
	if err := scan(
		&record.SnapshotID,
		&record.VMID,
		&record.TaskID,
		&record.HostID,
		&record.TenantID,
		&record.ParentSnapshotID,
		&mode,
		&state,
		&record.SizeBytes,
		&retentionRaw,
		&record.Metadata,
		&exportsJSON,
		&record.LastError,
		&record.CreatedAt,
		&record.UpdatedAt,
		&record.LayerKey,
		&record.Arch,
		&record.Digest,
		&kind,
	); err != nil {
		return SnapshotRecord{}, err
	}
	record.Mode = SnapshotMode(mode)
	record.State = SnapshotState(state)
	// '' cannot come from the column (NOT NULL DEFAULT 'disk'), but it can
	// come from a row written before that default existed on some other
	// deployment path, and disk is the safe reading either way.
	record.Kind = SnapshotKind(kind)
	if record.Kind == "" {
		record.Kind = SnapshotKindDisk
	}
	if retentionRaw.Valid {
		t := retentionRaw.Time
		record.RetentionUntil = &t
	}
	if len(exportsJSON) == 0 {
		record.Exports = []SnapshotExportRecord{}
		return record, nil
	}
	if err := json.Unmarshal(exportsJSON, &record.Exports); err != nil {
		return SnapshotRecord{}, fmt.Errorf("unmarshal snapshot exports %s: %w", record.SnapshotID, err)
	}
	return record, nil
}

func (s *PostgresStateStore) UpsertDeadLetter(ctx context.Context, entry DeadLetterRecord) error {
	if len(entry.Payload) == 0 {
		entry.Payload = json.RawMessage(`{}`)
	}
	now := time.Now()
	if entry.FirstSeenAt.IsZero() {
		entry.FirstSeenAt = now
	}
	if entry.LastSeenAt.IsZero() {
		entry.LastSeenAt = now
	}

	err := s.db.QueryRowContext(ctx, `
		INSERT INTO orchestrator_dead_letters (
			kind, entity_id, task_id, reason, retry_count, payload_json, first_seen_at, last_seen_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (kind, entity_id) DO UPDATE SET
			task_id=EXCLUDED.task_id,
			reason=EXCLUDED.reason,
			retry_count=GREATEST(orchestrator_dead_letters.retry_count, EXCLUDED.retry_count),
			payload_json=EXCLUDED.payload_json,
			last_seen_at=EXCLUDED.last_seen_at
		RETURNING id
	`,
		string(entry.Kind),
		entry.EntityID,
		entry.TaskID,
		entry.Reason,
		entry.RetryCount,
		[]byte(entry.Payload),
		entry.FirstSeenAt.UTC(),
		entry.LastSeenAt.UTC(),
	).Scan(&entry.ID)
	if err != nil {
		return fmt.Errorf("upsert dead letter %s/%s: %w", entry.Kind, entry.EntityID, err)
	}
	return nil
}

func (s *PostgresStateStore) ListDeadLetters(ctx context.Context) ([]DeadLetterRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, kind, entity_id, task_id, reason, retry_count, payload_json, first_seen_at, last_seen_at
		FROM orchestrator_dead_letters
		ORDER BY last_seen_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("list dead letters: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []DeadLetterRecord
	for rows.Next() {
		var (
			record  DeadLetterRecord
			kind    string
			payload []byte
		)
		if err := rows.Scan(
			&record.ID,
			&kind,
			&record.EntityID,
			&record.TaskID,
			&record.Reason,
			&record.RetryCount,
			&payload,
			&record.FirstSeenAt,
			&record.LastSeenAt,
		); err != nil {
			return nil, fmt.Errorf("scan dead letter row: %w", err)
		}
		record.Kind = DeadLetterKind(kind)
		record.Payload = json.RawMessage(payload)
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate dead letters: %w", err)
	}
	return out, nil
}

func (s *PostgresStateStore) UpsertHost(ctx context.Context, h HostRecord) error {
	migProfilesJSON, err := marshalMIGProfiles(h.Capacity.MIGProfiles)
	if err != nil {
		return fmt.Errorf("marshal mig profiles for host %s: %w", h.ID, err)
	}
	migAllocatedJSON, err := marshalMIGProfiles(h.Allocated.MIGProfiles)
	if err != nil {
		return fmt.Errorf("marshal mig allocation for host %s: %w", h.ID, err)
	}
	// per-device inventory rides as a jsonb array; default to [] so the
	// column never holds null and empty hosts round-trip cleanly.
	gpuDevices := h.Capacity.GPUDevices
	if gpuDevices == nil {
		gpuDevices = []GPUDevice{}
	}
	gpuDevicesJSON, err := json.Marshal(gpuDevices)
	if err != nil {
		return fmt.Errorf("marshal gpu devices for host %s: %w", h.ID, err)
	}
	// per-instance MIG inventory rides as a jsonb array too. nil coerces to []
	// so the column never holds a json null and count-only hosts round-trip
	// cleanly (the scheduler treats an empty MIGInstances as "use the count
	// map", which is the back-compat path).
	migInstances := h.Capacity.MIGInstances
	if migInstances == nil {
		migInstances = []MIGInstance{}
	}
	migInstancesJSON, err := json.Marshal(migInstances)
	if err != nil {
		return fmt.Errorf("marshal mig instances for host %s: %w", h.ID, err)
	}
	labelsJSON, err := marshalHostLabels(h.Labels)
	if err != nil {
		return fmt.Errorf("marshal labels for host %s: %w", h.ID, err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO orchestrator_hosts (
			host_id, url, token_encrypted, region, state, tenant_id,
			cpus_total, ram_mb_total, storage_gb_total, vm_count_max,
			cpus_allocated, ram_mb_allocated, storage_gb_allocated, vm_count_allocated,
			last_seen_at, created_at, updated_at,
			backend, gpus_total, gpu_kind, gpus_allocated,
			mig_profiles_json, mig_allocated_json, gpu_devices_json, mig_instances_json,
			labels_json, arch
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27)
		ON CONFLICT (host_id) DO UPDATE SET
			url=EXCLUDED.url,
			token_encrypted=EXCLUDED.token_encrypted,
			region=EXCLUDED.region,
			state=EXCLUDED.state,
			tenant_id=EXCLUDED.tenant_id,
			cpus_total=EXCLUDED.cpus_total,
			ram_mb_total=EXCLUDED.ram_mb_total,
			storage_gb_total=EXCLUDED.storage_gb_total,
			vm_count_max=EXCLUDED.vm_count_max,
			cpus_allocated=EXCLUDED.cpus_allocated,
			ram_mb_allocated=EXCLUDED.ram_mb_allocated,
			storage_gb_allocated=EXCLUDED.storage_gb_allocated,
			vm_count_allocated=EXCLUDED.vm_count_allocated,
			last_seen_at=EXCLUDED.last_seen_at,
			updated_at=EXCLUDED.updated_at,
			backend=EXCLUDED.backend,
			gpus_total=EXCLUDED.gpus_total,
			gpu_kind=EXCLUDED.gpu_kind,
			gpus_allocated=EXCLUDED.gpus_allocated,
			mig_profiles_json=EXCLUDED.mig_profiles_json,
			mig_allocated_json=EXCLUDED.mig_allocated_json,
			gpu_devices_json=EXCLUDED.gpu_devices_json,
			mig_instances_json=EXCLUDED.mig_instances_json,
			labels_json=EXCLUDED.labels_json,
			arch=EXCLUDED.arch
	`,
		h.ID,
		h.URL,
		h.TokenEncrypted,
		h.Region,
		string(h.State),
		h.TenantID,
		h.Capacity.CPUs,
		h.Capacity.RamMB,
		h.Capacity.StorageGB,
		h.Capacity.VMCount,
		h.Allocated.CPUs,
		h.Allocated.RamMB,
		h.Allocated.StorageGB,
		h.Allocated.VMCount,
		h.LastSeen.UTC(),
		h.CreatedAt.UTC(),
		h.UpdatedAt.UTC(),
		string(h.Backend),
		h.Capacity.GPUs,
		h.Capacity.GPUKind,
		h.Allocated.GPUs,
		migProfilesJSON,
		migAllocatedJSON,
		string(gpuDevicesJSON),
		string(migInstancesJSON),
		labelsJSON,
		h.Capacity.Arch,
	)
	if err != nil {
		return fmt.Errorf("upsert host %s: %w", h.ID, err)
	}
	return nil
}

// marshalMIGProfiles renders a MIG profile map as the JSON object stored in
// the mig_*_json columns. Nil/empty maps store as "{}" to match the column
// default, so unmigrated rows and hosts without MIG capacity look identical.
func marshalMIGProfiles(m map[string]int) (string, error) {
	if len(m) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// unmarshalMIGProfiles is the inverse of marshalMIGProfiles: "{}" (or empty)
// yields nil so in-memory state never carries useless empty maps.
func unmarshalMIGProfiles(s string) (map[string]int, error) {
	if s == "" || s == "{}" {
		return nil, nil
	}
	var m map[string]int
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, err
	}
	if len(m) == 0 {
		return nil, nil
	}
	return m, nil
}

// marshalHostLabels renders operator-declared host labels as the JSON object
// stored in labels_json. Nil/empty maps store as "{}" to match the column
// default, so unmigrated rows and hosts without labels look identical.
func marshalHostLabels(m map[string]string) (string, error) {
	if len(m) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// unmarshalHostLabels is the inverse of marshalHostLabels: "{}" (or empty)
// yields nil so in-memory state never carries useless empty maps.
func unmarshalHostLabels(s string) (map[string]string, error) {
	if s == "" || s == "{}" {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, err
	}
	if len(m) == 0 {
		return nil, nil
	}
	return m, nil
}

func (s *PostgresStateStore) DeleteHost(ctx context.Context, hostID string) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM orchestrator_hosts WHERE host_id=$1", hostID); err != nil {
		return fmt.Errorf("delete host %s: %w", hostID, err)
	}
	return nil
}

// hostsSelect is the canonical projection for ListHosts and GetHost.
const hostsSelect = `
	SELECT host_id, url, token_encrypted, region, state, tenant_id,
	       cpus_total, ram_mb_total, storage_gb_total, vm_count_max,
	       cpus_allocated, ram_mb_allocated, storage_gb_allocated, vm_count_allocated,
	       last_seen_at, created_at, updated_at,
	       backend, gpus_total, gpu_kind, gpus_allocated,
	       mig_profiles_json, mig_allocated_json, gpu_devices_json, mig_instances_json,
	       labels_json, arch
	FROM orchestrator_hosts`

// scanHost maps a row from hostsSelect onto a HostRecord. Both
// ListHosts and GetHost share this so the column order can only diverge
// in one place if the schema ever changes.
func scanHost(scan func(...any) error) (HostRecord, error) {
	var (
		record           HostRecord
		state            string
		backend          string
		migProfilesJSON  string
		migAllocatedJSON string
		gpuDevicesJSON   []byte
		migInstancesJSON []byte
		labelsJSON       string
	)
	if err := scan(
		&record.ID,
		&record.URL,
		&record.TokenEncrypted,
		&record.Region,
		&state,
		&record.TenantID,
		&record.Capacity.CPUs,
		&record.Capacity.RamMB,
		&record.Capacity.StorageGB,
		&record.Capacity.VMCount,
		&record.Allocated.CPUs,
		&record.Allocated.RamMB,
		&record.Allocated.StorageGB,
		&record.Allocated.VMCount,
		&record.LastSeen,
		&record.CreatedAt,
		&record.UpdatedAt,
		&backend,
		&record.Capacity.GPUs,
		&record.Capacity.GPUKind,
		&record.Allocated.GPUs,
		&migProfilesJSON,
		&migAllocatedJSON,
		&gpuDevicesJSON,
		&migInstancesJSON,
		&labelsJSON,
		&record.Capacity.Arch,
	); err != nil {
		return HostRecord{}, err
	}
	record.State = HostState(state)
	record.Backend = HostBackend(backend)
	var err error
	if record.Capacity.MIGProfiles, err = unmarshalMIGProfiles(migProfilesJSON); err != nil {
		return HostRecord{}, fmt.Errorf("unmarshal mig profiles for host %s: %w", record.ID, err)
	}
	if record.Allocated.MIGProfiles, err = unmarshalMIGProfiles(migAllocatedJSON); err != nil {
		return HostRecord{}, fmt.Errorf("unmarshal mig allocation for host %s: %w", record.ID, err)
	}
	if record.Labels, err = unmarshalHostLabels(labelsJSON); err != nil {
		return HostRecord{}, fmt.Errorf("unmarshal labels for host %s: %w", record.ID, err)
	}
	// gpu_devices_json defaults to '[]' but a legacy null or empty scan
	// leaves GPUDevices nil, which the scheduler treats as "no per-device
	// inventory" (legacy scalar path). only unmarshal a real array.
	if len(gpuDevicesJSON) > 0 {
		if err := json.Unmarshal(gpuDevicesJSON, &record.Capacity.GPUDevices); err != nil {
			return HostRecord{}, fmt.Errorf("unmarshal gpu devices for host %s: %w", record.ID, err)
		}
	}
	// mig_instances_json defaults to '[]'; an empty/legacy scan leaves
	// MIGInstances nil, which the scheduler treats as "use the count map"
	// (the back-compat path). only unmarshal a real array.
	if len(migInstancesJSON) > 0 {
		if err := json.Unmarshal(migInstancesJSON, &record.Capacity.MIGInstances); err != nil {
			return HostRecord{}, fmt.Errorf("unmarshal mig instances for host %s: %w", record.ID, err)
		}
	}
	return record, nil
}

func (s *PostgresStateStore) ListHosts(ctx context.Context) ([]HostRecord, error) {
	rows, err := s.db.QueryContext(ctx, hostsSelect)
	if err != nil {
		return nil, fmt.Errorf("list hosts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []HostRecord
	for rows.Next() {
		record, err := scanHost(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan host row: %w", err)
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate hosts: %w", err)
	}
	return out, nil
}

func (s *PostgresStateStore) GetHost(ctx context.Context, hostID string) (HostRecord, error) {
	row := s.db.QueryRowContext(ctx, hostsSelect+" WHERE host_id=$1", hostID)
	record, err := scanHost(row.Scan)
	if err != nil {
		return HostRecord{}, fmt.Errorf("get host %s: %w", hostID, err)
	}
	return record, nil
}

func (s *PostgresStateStore) AppendEvent(ctx context.Context, event EventRecord) error {
	if len(event.Payload) == 0 {
		event.Payload = json.RawMessage(`{}`)
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now()
	}
	if err := s.db.QueryRowContext(ctx, `
		INSERT INTO orchestrator_events (entity_type, entity_id, event_type, payload_json, created_at)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING id
	`, event.EntityType, event.EntityID, event.EventType, []byte(event.Payload), event.CreatedAt.UTC()).Scan(&event.ID); err != nil {
		return fmt.Errorf("append event: %w", err)
	}
	return nil
}
