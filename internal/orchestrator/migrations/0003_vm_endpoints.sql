-- Ingress: published endpoints for a vm (e.g. Fusefile `expose` entries),
-- persisted as a json array of {as, url, port} so a crashed orchestrator can
-- recover them on restart without re-asking the provider. Empty array default
-- keeps existing rows and the byte-for-byte no-expose case simple.
ALTER TABLE orchestrator_vms
    ADD COLUMN IF NOT EXISTS endpoints_json TEXT NOT NULL DEFAULT '[]';
