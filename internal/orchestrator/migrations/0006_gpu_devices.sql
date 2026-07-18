-- per-gpu device inventory: replace the scalar gpus_total/gpu_kind model
-- with durable per-device detail. each host row carries a json array of
-- GPUDevice records (uuid, model, pci bus, memory, mig capability, etc.)
-- probed by the host agent. gpus_total/gpu_kind stay as derived back-compat
-- aggregates so existing readers and homogeneous-gpu hosts keep working.
-- vms record the specific device uuid(s) they hold so allocation can bind
-- concrete devices (the per-vm binding logic lands in issue #38). the binding
-- is stored as a jsonb array of uuid strings, matching the json round-trip the
-- state store already uses for endpoints and gpu_devices_json (the pgx stdlib
-- driver has no native text[] scan path without an extra dependency).
--
-- additive + back-compat: existing rows default to '[]', so non-gpu and
-- homogeneous-gpu hosts load with no data loss.
ALTER TABLE orchestrator_hosts ADD COLUMN IF NOT EXISTS gpu_devices_json JSONB NOT NULL DEFAULT '[]';

ALTER TABLE orchestrator_vms ADD COLUMN IF NOT EXISTS gpu_uuids JSONB NOT NULL DEFAULT '[]';
