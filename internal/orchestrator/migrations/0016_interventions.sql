-- human-in-the-loop interventions (issue #309).
--
-- an intervention is a request for a human: it is raised pending and moves
-- once to approved, rejected, expired or cancelled. postgres is the source
-- of truth, so an orchestrator restart or a dropped client loses nothing and
-- the caller resumes by id.
--
-- content columns (payload, edited_payload, result, reason) are sealed with
-- the orchestrator's TOKEN_ENCRYPTION_KEY when one is configured; `sealed`
-- records which, so a dev row written without a key still reads back.
-- metadata stays plaintext so the deadline sweep and list filters work
-- without decrypting anything.
CREATE TABLE IF NOT EXISTS orchestrator_interventions (
    intervention_id TEXT PRIMARY KEY,
    env_id          TEXT NOT NULL,
    kind            TEXT NOT NULL,
    gate            TEXT NOT NULL DEFAULT '',
    state           TEXT NOT NULL,
    sealed          BOOLEAN NOT NULL DEFAULT FALSE,
    payload         BYTEA,
    edited_payload  BYTEA,
    result          BYTEA,
    reason          BYTEA,
    outcome         TEXT NOT NULL DEFAULT '',
    triggered_by    TEXT NOT NULL DEFAULT '{}',
    decided_by      TEXT NOT NULL DEFAULT '{}',
    decided_at      TIMESTAMPTZ,
    on_timeout      TEXT NOT NULL DEFAULT 'reject',
    deadline        TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS orchestrator_interventions_env_state
    ON orchestrator_interventions (env_id, state);
CREATE INDEX IF NOT EXISTS orchestrator_interventions_state_deadline
    ON orchestrator_interventions (state, deadline);

-- grants let one approval cover repeats of the same action. the table lands
-- here so the schema is in place; nothing reads or writes it until the exec
-- and lifecycle gates (#311).
CREATE TABLE IF NOT EXISTS orchestrator_hitl_grants (
    grant_id        TEXT PRIMARY KEY,
    env_id          TEXT NOT NULL,
    gate            TEXT NOT NULL,
    match_json      TEXT NOT NULL DEFAULT '{}',
    scope           TEXT NOT NULL,
    intervention_id TEXT NOT NULL DEFAULT '',
    expires_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS orchestrator_hitl_grants_env
    ON orchestrator_hitl_grants (env_id);

-- the hitl policy the vm was created with, as json. null means hitl is off,
-- which is every row that predates this column.
ALTER TABLE orchestrator_vms
    ADD COLUMN IF NOT EXISTS hitl_spec_json TEXT;
