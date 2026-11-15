-- fusefile builds run by the orchestrator (issue #351). the row is the read
-- model GET /v1/builds/{id} answers from; restate, when on, owns only the
-- in-flight steps. the whole record is one jsonb document: nothing filters
-- on its fields but the id.
CREATE TABLE IF NOT EXISTS orchestrator_builds (
    build_id   TEXT PRIMARY KEY,
    tenant_id  TEXT NOT NULL DEFAULT '',
    state      TEXT NOT NULL,
    record     JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
