-- Shared memory seed: the seed snapshot whose memory image a vm shares with
-- the other vms resumed from it on the same host. Allocation charges that
-- image once per host plus a private share per vm, and recovery re-derives
-- the host totals from this column, so it is persisted like the gpu bindings.
--
-- Default '' is the pre-sharing behaviour (memory charged in full) for every
-- existing row.
ALTER TABLE orchestrator_vms
    ADD COLUMN IF NOT EXISTS shared_mem_seed TEXT NOT NULL DEFAULT '';
