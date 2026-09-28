-- Huge pages: whether a vm's guest memory is backed by 2M pages. It is fixed
-- when the vm boots and read again at migrate time (only a 2M guest can be
-- migrated lazily), so it is persisted like the rest of the spec.
--
-- Default false is byte-for-byte the pre-huge-pages behaviour for every
-- existing row.
ALTER TABLE orchestrator_vms
    ADD COLUMN IF NOT EXISTS huge_pages BOOLEAN NOT NULL DEFAULT FALSE;
