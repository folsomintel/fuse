-- Egress: the resolved outbound policy for a vm, persisted as a small json
-- object of {mode, provider, protocol, endpoint} so a restarted orchestrator
-- still knows which backend to release on teardown and what endpoint the
-- environment reports. Health is live state and is never written here.
--
-- Default '{}' reads as direct egress, which is byte-for-byte the pre-egress
-- behaviour for every existing row. No CHECK constraint on the shape: the
-- store owns it, same as endpoints_json. No index: nothing queries by it.
ALTER TABLE orchestrator_vms
    ADD COLUMN IF NOT EXISTS egress_json TEXT NOT NULL DEFAULT '{}';
