-- hitl access (issue #310): scoped, expiring api keys, actors and a
-- tamper-evident audit log.
--
-- scopes null is a full-access key, which is every key that predates this
-- migration, so nothing changes for them. a key with scopes is limited to
-- the hitl routes; env_id further limits it to one environment and every
-- environment forked from it. actor_id/actor_name pin the actor every
-- request on the key is recorded as. created_by is the principal that
-- minted it ("master" or a key id).
ALTER TABLE orchestrator_api_keys
    ADD COLUMN IF NOT EXISTS scopes     TEXT[],
    ADD COLUMN IF NOT EXISTS env_id     TEXT,
    ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS actor_id   TEXT,
    ADD COLUMN IF NOT EXISTS actor_name TEXT,
    ADD COLUMN IF NOT EXISTS created_by TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_api_keys_env
    ON orchestrator_api_keys (env_id) WHERE env_id IS NOT NULL;

-- append-only audit of every raise, decision, expiry, cancel and invite.
-- rows hold who, when and from where, never content. each row stores the
-- hash of the row before it (prev_hash) and its own (hash), so an edited or
-- deleted row breaks the chain at that point. seq is assigned under an
-- advisory lock by the writer, not by a sequence, so it has no gaps.
CREATE TABLE IF NOT EXISTS orchestrator_hitl_audit (
    seq             BIGINT PRIMARY KEY,
    event           TEXT NOT NULL,
    intervention_id TEXT NOT NULL DEFAULT '',
    env_id          TEXT NOT NULL DEFAULT '',
    actor_id        TEXT NOT NULL DEFAULT '',
    actor_name      TEXT NOT NULL DEFAULT '',
    actor_source    TEXT NOT NULL DEFAULT '',
    key_id          TEXT NOT NULL DEFAULT '',
    remote_addr     TEXT NOT NULL DEFAULT '',
    at              TIMESTAMPTZ NOT NULL,
    prev_hash       BYTEA NOT NULL,
    hash            BYTEA NOT NULL
);

CREATE INDEX IF NOT EXISTS orchestrator_hitl_audit_env
    ON orchestrator_hitl_audit (env_id, seq);
CREATE INDEX IF NOT EXISTS orchestrator_hitl_audit_actor
    ON orchestrator_hitl_audit (actor_id, seq);

-- ancestors is the vm's fork lineage as a json array of vm ids, oldest
-- first, so an invite for an environment also covers its forks. null for
-- every row that predates it, which reads as no ancestors.
--
-- hitl_keys_encrypted holds the hitl.notify signing secrets, sealed with
-- TOKEN_ENCRYPTION_KEY, so deliveries stay signed across a restart and
-- follow a fork or migrate.
ALTER TABLE orchestrator_vms
    ADD COLUMN IF NOT EXISTS ancestors_json TEXT,
    ADD COLUMN IF NOT EXISTS hitl_keys_encrypted BYTEA;
