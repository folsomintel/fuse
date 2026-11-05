-- hitl desktop takeover (issue #313): a recording is now a terminal cast or
-- a desktop session (an input-action log plus periodic screenshots, as
-- json). rows written before this are terminal casts.
ALTER TABLE orchestrator_hitl_recordings
    ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'terminal';
