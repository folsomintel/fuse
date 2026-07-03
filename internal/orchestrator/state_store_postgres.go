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

	_ "github.com/jackc/pgx/v5/stdlib"
)

const migrationsTableName = "orchestrator_schema_migrations"

//go:embed migrations/*.sql
var migrationFiles embed.FS

// PostgresStateStore persists orchestrator state in Postgres.
type PostgresStateStore struct {
	db *sql.DB
}

// NewPostgresStateStore creates a Postgres-backed state store.
func NewPostgresStateStore(db *sql.DB) *PostgresStateStore {
	return &PostgresStateStore{db: db}
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
	endpoints := vm.Endpoints
	if endpoints == nil {
		endpoints = []Endpoint{}
	}
	endpointsJSON, err := json.Marshal(endpoints)
	if err != nil {
		return fmt.Errorf("marshal endpoints for vm %s: %w", vm.ID, err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO orchestrator_vms (
			vm_id, host_id, network_host, state, url, task_id, tenant_id,
			cpus, ram_mb, storage_gb, region, max_runtime_seconds,
			auth_token_encrypted, secrets_encrypted, last_error, endpoints_json, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
		ON CONFLICT (vm_id) DO UPDATE SET
			host_id=EXCLUDED.host_id,
			network_host=EXCLUDED.network_host,
			state=EXCLUDED.state,
			url=EXCLUDED.url,
			task_id=EXCLUDED.task_id,
			tenant_id=EXCLUDED.tenant_id,
			cpus=EXCLUDED.cpus,
			ram_mb=EXCLUDED.ram_mb,
			storage_gb=EXCLUDED.storage_gb,
			region=EXCLUDED.region,
			max_runtime_seconds=EXCLUDED.max_runtime_seconds,
			auth_token_encrypted=EXCLUDED.auth_token_encrypted,
			secrets_encrypted=EXCLUDED.secrets_encrypted,
			last_error=EXCLUDED.last_error,
			endpoints_json=EXCLUDED.endpoints_json,
			created_at=EXCLUDED.created_at,
			updated_at=EXCLUDED.updated_at
	`,
		vm.ID,
		vm.HostID,
		vm.NetworkHost,
		string(vm.State),
		vm.URL,
		vm.TaskID,
		vm.TenantID,
		vm.Spec.CPUs,
		vm.Spec.RamMB,
		vm.Spec.StorageGB,
		vm.Spec.Region,
		maxRuntimeSeconds,
		vm.AuthTokenEncrypted,
		vm.SecretsEncrypted,
		vm.LastError,
		string(endpointsJSON),
		vm.CreatedAt.UTC(),
		vm.UpdatedAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("upsert vm %s: %w", vm.ID, err)
	}
	return nil
}

func (s *PostgresStateStore) DeleteVM(ctx context.Context, vmID string) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM orchestrator_vms WHERE vm_id=$1", vmID); err != nil {
		return fmt.Errorf("delete vm %s: %w", vmID, err)
	}
	return nil
}

func (s *PostgresStateStore) ListVMs(ctx context.Context) ([]VMRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT vm_id, host_id, network_host, state, url, task_id, tenant_id,
		       cpus, ram_mb, storage_gb, region, max_runtime_seconds,
		       auth_token_encrypted, secrets_encrypted, last_error, endpoints_json, created_at, updated_at
		FROM orchestrator_vms
	`)
	if err != nil {
		return nil, fmt.Errorf("list vms: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []VMRecord
	for rows.Next() {
		var (
			record            VMRecord
			state             string
			maxRuntimeSeconds int
			endpointsJSON     string
		)
		if err := rows.Scan(
			&record.ID,
			&record.HostID,
			&record.NetworkHost,
			&state,
			&record.URL,
			&record.TaskID,
			&record.TenantID,
			&record.Spec.CPUs,
			&record.Spec.RamMB,
			&record.Spec.StorageGB,
			&record.Spec.Region,
			&maxRuntimeSeconds,
			&record.AuthTokenEncrypted,
			&record.SecretsEncrypted,
			&record.LastError,
			&endpointsJSON,
			&record.CreatedAt,
			&record.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan vm row: %w", err)
		}
		record.State = VMState(state)
		if maxRuntimeSeconds > 0 {
			record.Spec.MaxRuntime = time.Duration(maxRuntimeSeconds) * time.Second
		}
		if endpointsJSON != "" {
			if err := json.Unmarshal([]byte(endpointsJSON), &record.Endpoints); err != nil {
				return nil, fmt.Errorf("unmarshal endpoints for vm %s: %w", record.ID, err)
			}
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate vms: %w", err)
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
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO orchestrator_snapshots (
			snapshot_id, vm_id, task_id, host_id, tenant_id, parent_snapshot_id, mode, state, size_bytes,
			retention_until, metadata_json, exports_json, last_error, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
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
			updated_at=EXCLUDED.updated_at
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
	)
	if err != nil {
		return fmt.Errorf("upsert snapshot %s: %w", snapshot.SnapshotID, err)
	}
	return nil
}

func (s *PostgresStateStore) GetSnapshot(ctx context.Context, snapshotID string) (SnapshotRecord, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT snapshot_id, vm_id, task_id, host_id, tenant_id, parent_snapshot_id, mode, state, size_bytes,
		       retention_until, metadata_json, exports_json, last_error, created_at, updated_at
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
		       retention_until, metadata_json, exports_json, last_error, created_at, updated_at
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

func scanSnapshotRow(scan func(dest ...any) error) (SnapshotRecord, error) {
	var (
		record       SnapshotRecord
		mode         string
		state        string
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
	); err != nil {
		return SnapshotRecord{}, err
	}
	record.Mode = SnapshotMode(mode)
	record.State = SnapshotState(state)
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
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO orchestrator_hosts (
			host_id, url, token_encrypted, region, state, tenant_id,
			cpus_total, ram_mb_total, storage_gb_total, vm_count_max,
			cpus_allocated, ram_mb_allocated, storage_gb_allocated, vm_count_allocated,
			last_seen_at, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
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
			updated_at=EXCLUDED.updated_at
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
	)
	if err != nil {
		return fmt.Errorf("upsert host %s: %w", h.ID, err)
	}
	return nil
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
	       last_seen_at, created_at, updated_at
	FROM orchestrator_hosts`

// scanHost maps a row from hostsSelect onto a HostRecord. Both
// ListHosts and GetHost share this so the column order can only diverge
// in one place if the schema ever changes.
func scanHost(scan func(...any) error) (HostRecord, error) {
	var (
		record HostRecord
		state  string
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
	); err != nil {
		return HostRecord{}, err
	}
	record.State = HostState(state)
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
