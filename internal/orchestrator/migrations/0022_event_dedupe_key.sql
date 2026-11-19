-- an optional idempotency key per event. a durable step that writes an event
-- and then loses its result before restate journals it runs again; with a
-- key, the second write inserts nothing. NULL (every existing row, and every
-- event written without a key) never conflicts.
ALTER TABLE orchestrator_events
    ADD COLUMN IF NOT EXISTS dedupe_key TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS orchestrator_events_dedupe_key_idx
    ON orchestrator_events (dedupe_key)
    WHERE dedupe_key IS NOT NULL;
