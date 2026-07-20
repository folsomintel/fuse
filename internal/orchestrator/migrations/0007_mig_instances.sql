-- per-instance MIG allocation, issue #41: today the orchestrator allocates MIG
-- by profile count (--mig-profile 1g.10gb=4) and the qemu agent picks the
-- actual mdev uuid, so the control plane does not know which instance went to
-- which VM. this migration adds the durable store for per-instance inventory
-- and binding, mirroring 0006_gpu_devices.sql's whole-device pattern:
--
--   hosts: mig_instances_json is the per-instance MIG inventory probed from the
--   host agent (one entry per carved GPU instance: uuid, profile, kind, parent
--   gpu uuid). mig_profiles_json (0005) stays as the derived count-by-profile
--   summary for back-compat; a host that reports instances derives the count
--   map from it, and a host that only declares counts keeps the count path.
--
--   vms: mig_instance_uuids is the concrete MIG instance uuid(s) this VM is
--   bound to, persisted so the binding survives an orchestrator restart and
--   recover can recompute the host's allocated set from live VMs (#39 pattern).
--
-- both are jsonb ('[]' = none) because pgx stdlib has no native text[] scan path
-- without an extra dependency, matching gpu_devices_json / gpu_uuids. additive +
-- back-compat: existing rows default to '[]', so non-MIG and count-only hosts
-- load with no data loss and the count-based scheduler path keeps working.
ALTER TABLE orchestrator_hosts ADD COLUMN IF NOT EXISTS mig_instances_json JSONB NOT NULL DEFAULT '[]';

ALTER TABLE orchestrator_vms ADD COLUMN IF NOT EXISTS mig_instance_uuids JSONB NOT NULL DEFAULT '[]';