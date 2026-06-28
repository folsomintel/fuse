-- Orchestrator control-plane state. Single consolidated migration: replaces
-- the original 0001..0005 split which had grown several misnomers, dead
-- columns (gateway_json), and JSON-blob fields where typed columns were
-- cheaper to query.
--
-- Conventions:
--   - text references between tables are loose (no FK constraints) so the
--     orchestrator owns lifecycle ordering in code. Cascade behaviour is
--     enforced by FleetManager, not the database.
--   - integer capacity columns instead of jsonb so the scheduler can
--     filter / sort hosts directly with index-friendly predicates.
--   - tenant_id is a placeholder for multi-tenancy; defaults to '' until
--     the auth layer starts populating it.
--   - all encrypted blobs use bytea (AES-GCM, key from TOKEN_ENCRYPTION_KEY).

-- ── hosts ──────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS orchestrator_hosts (
    host_id              TEXT PRIMARY KEY,
    url                  TEXT NOT NULL,
    token_encrypted      BYTEA,                                    -- agent bearer token, encrypted at rest
    region               TEXT NOT NULL DEFAULT '',
    state                TEXT NOT NULL DEFAULT 'active',           -- active | cordoned
    tenant_id            TEXT NOT NULL DEFAULT '',
    cpus_total           INTEGER NOT NULL DEFAULT 0,
    ram_mb_total         INTEGER NOT NULL DEFAULT 0,
    storage_gb_total     INTEGER NOT NULL DEFAULT 0,
    vm_count_max         INTEGER NOT NULL DEFAULT 0,
    cpus_allocated       INTEGER NOT NULL DEFAULT 0,
    ram_mb_allocated     INTEGER NOT NULL DEFAULT 0,
    storage_gb_allocated INTEGER NOT NULL DEFAULT 0,
    vm_count_allocated   INTEGER NOT NULL DEFAULT 0,
    last_seen_at         TIMESTAMPTZ NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL,
    updated_at           TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS orchestrator_hosts_state_idx     ON orchestrator_hosts (state);
CREATE INDEX IF NOT EXISTS orchestrator_hosts_region_idx    ON orchestrator_hosts (region);
CREATE INDEX IF NOT EXISTS orchestrator_hosts_tenant_idx    ON orchestrator_hosts (tenant_id);

-- ── vms ────────────────────────────────────────────────────────────────
-- network_host stores the externally-reachable DNAT host:port Fuse clients
-- dial. host_id is the loose reference to the placement host (or '' when
-- the orchestrator runs single-provider without a host registry).
CREATE TABLE IF NOT EXISTS orchestrator_vms (
    vm_id                TEXT PRIMARY KEY,
    host_id              TEXT NOT NULL DEFAULT '',
    network_host         TEXT NOT NULL DEFAULT '',
    state                TEXT NOT NULL,                            -- provisioning | running | draining | destroying
    url                  TEXT NOT NULL DEFAULT '',
    task_id              TEXT NOT NULL DEFAULT '',
    tenant_id            TEXT NOT NULL DEFAULT '',
    cpus                 INTEGER NOT NULL DEFAULT 0,
    ram_mb               INTEGER NOT NULL DEFAULT 0,
    storage_gb           INTEGER NOT NULL DEFAULT 0,
    region               TEXT NOT NULL DEFAULT '',
    max_runtime_seconds  INTEGER NOT NULL DEFAULT 0,
    auth_token_encrypted BYTEA,
    secrets_encrypted    BYTEA,                                    -- AES-GCM(json.Marshal(secretMap)); persisted so a crashed orchestrator can re-upload to the guest on recovery
    last_error           TEXT NOT NULL DEFAULT '',
    created_at           TIMESTAMPTZ NOT NULL,
    updated_at           TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS orchestrator_vms_state_idx     ON orchestrator_vms (state);
CREATE INDEX IF NOT EXISTS orchestrator_vms_task_id_idx   ON orchestrator_vms (task_id);
CREATE INDEX IF NOT EXISTS orchestrator_vms_host_id_idx   ON orchestrator_vms (host_id);
CREATE INDEX IF NOT EXISTS orchestrator_vms_tenant_idx    ON orchestrator_vms (tenant_id);

-- ── tasks ──────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS orchestrator_tasks (
    task_id     TEXT PRIMARY KEY,
    vm_id       TEXT NOT NULL DEFAULT '',
    run_status  TEXT NOT NULL,
    retry_count INTEGER NOT NULL DEFAULT 0,
    last_error  TEXT NOT NULL DEFAULT '',
    assigned_at TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS orchestrator_tasks_vm_id_idx       ON orchestrator_tasks (vm_id);
CREATE INDEX IF NOT EXISTS orchestrator_tasks_run_status_idx  ON orchestrator_tasks (run_status);
CREATE INDEX IF NOT EXISTS orchestrator_tasks_assigned_at_idx ON orchestrator_tasks (assigned_at DESC);

-- ── snapshots ──────────────────────────────────────────────────────────
-- mode: snapshot taking strategy (memory | disk | hybrid).
-- state: control-plane lifecycle (pending | ready | failed | deleted).
CREATE TABLE IF NOT EXISTS orchestrator_snapshots (
    snapshot_id        TEXT PRIMARY KEY,
    vm_id              TEXT NOT NULL,
    task_id            TEXT NOT NULL DEFAULT '',
    host_id            TEXT NOT NULL DEFAULT '',
    tenant_id          TEXT NOT NULL DEFAULT '',
    parent_snapshot_id TEXT NOT NULL DEFAULT '',
    mode               TEXT NOT NULL,
    state              TEXT NOT NULL DEFAULT 'ready',
    size_bytes         BIGINT NOT NULL DEFAULT 0,
    retention_until    TIMESTAMPTZ NULL,
    metadata_json      JSONB NOT NULL DEFAULT '{}'::jsonb,
    exports_json       JSONB NOT NULL DEFAULT '[]'::jsonb,
    last_error         TEXT NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ NOT NULL,
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS orchestrator_snapshots_vm_id_idx     ON orchestrator_snapshots (vm_id);
CREATE INDEX IF NOT EXISTS orchestrator_snapshots_state_idx     ON orchestrator_snapshots (state);
CREATE INDEX IF NOT EXISTS orchestrator_snapshots_tenant_idx    ON orchestrator_snapshots (tenant_id);
CREATE INDEX IF NOT EXISTS orchestrator_snapshots_retention_idx
    ON orchestrator_snapshots (retention_until)
    WHERE retention_until IS NOT NULL;

-- ── events ─────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS orchestrator_events (
    id           BIGSERIAL PRIMARY KEY,
    entity_type  TEXT NOT NULL,
    entity_id    TEXT NOT NULL,
    event_type   TEXT NOT NULL,
    payload_json JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS orchestrator_events_entity_idx
    ON orchestrator_events (entity_type, entity_id, created_at DESC);
CREATE INDEX IF NOT EXISTS orchestrator_events_event_type_idx
    ON orchestrator_events (event_type, created_at DESC);

-- ── dead letters ───────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS orchestrator_dead_letters (
    id            BIGSERIAL PRIMARY KEY,
    kind          TEXT NOT NULL,
    entity_id     TEXT NOT NULL,
    task_id       TEXT NOT NULL DEFAULT '',
    reason        TEXT NOT NULL,
    retry_count   INTEGER NOT NULL DEFAULT 0,
    payload_json  JSONB NOT NULL DEFAULT '{}'::jsonb,
    first_seen_at TIMESTAMPTZ NOT NULL,
    last_seen_at  TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS orchestrator_dead_letters_kind_idx     ON orchestrator_dead_letters (kind);
CREATE INDEX IF NOT EXISTS orchestrator_dead_letters_entity_idx   ON orchestrator_dead_letters (entity_id);
CREATE UNIQUE INDEX IF NOT EXISTS orchestrator_dead_letters_kind_entity_unique
    ON orchestrator_dead_letters (kind, entity_id);
