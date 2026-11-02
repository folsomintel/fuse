-- hitl takeover (issue #312): recorded terminal sessions.
--
-- each row is one shell session a human opened during a takeover, as an
-- asciinema v2 cast. data is sealed with TOKEN_ENCRYPTION_KEY when the
-- orchestrator has one (sealed = true), so the cast replays only through
-- the orchestrator. the audit row that closes the session links it by id.
CREATE TABLE IF NOT EXISTS orchestrator_hitl_recordings (
    id              TEXT PRIMARY KEY,
    intervention_id TEXT NOT NULL,
    env_id          TEXT NOT NULL,
    actor_json      JSONB NOT NULL,
    started_at      TIMESTAMPTZ NOT NULL,
    ended_at        TIMESTAMPTZ NOT NULL,
    size_bytes      BIGINT NOT NULL,
    truncated       BOOLEAN NOT NULL DEFAULT FALSE,
    sealed          BOOLEAN NOT NULL DEFAULT FALSE,
    data            BYTEA NOT NULL
);

CREATE INDEX IF NOT EXISTS orchestrator_hitl_recordings_intervention
    ON orchestrator_hitl_recordings (intervention_id);

-- recording_id links a takeover_session row to its cast. empty for every
-- other event, and left out of the row hash when empty, so older rows
-- still verify.
ALTER TABLE orchestrator_hitl_audit
    ADD COLUMN IF NOT EXISTS recording_id TEXT NOT NULL DEFAULT '';
