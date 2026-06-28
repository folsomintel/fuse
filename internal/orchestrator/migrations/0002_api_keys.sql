-- API keys: revocable, labeled credentials that authenticate callers
-- alongside the static master token (ORCH_AUTH_TOKEN). Every key is
-- full-access; isolation/tenancy is deliberately out of scope here.
--
-- key_hash stores SHA-256(raw key), never the raw key: the orchestrator
-- only ever compares, never recovers, so a hash is sufficient and a DB
-- leak exposes no usable credential. id is a public handle used to list
-- and revoke keys without holding the secret.
CREATE TABLE IF NOT EXISTS orchestrator_api_keys (
    id           TEXT PRIMARY KEY,
    key_hash     BYTEA NOT NULL,
    label        TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL,
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_hash
    ON orchestrator_api_keys (key_hash);
