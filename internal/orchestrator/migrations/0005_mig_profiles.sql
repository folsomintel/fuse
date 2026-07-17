-- Fractional GPU (MIG) allocation, decision D5: hosts advertise MIG
-- instance capacity by profile ("1g.10gb": 4) and track per-profile
-- allocation; VMs record the MIG profile they were provisioned with so a
-- restarted orchestrator rehydrates fractional allocation state. JSON
-- object columns ('{}' = no MIG capacity) because the profile set is
-- open-ended, unlike the fixed whole-device gpus_total counter.
ALTER TABLE orchestrator_hosts ADD COLUMN IF NOT EXISTS mig_profiles_json TEXT NOT NULL DEFAULT '{}';
ALTER TABLE orchestrator_hosts ADD COLUMN IF NOT EXISTS mig_allocated_json TEXT NOT NULL DEFAULT '{}';

-- orchestrator_vms (persist spec for recovery)
ALTER TABLE orchestrator_vms ADD COLUMN IF NOT EXISTS gpu_profile TEXT NOT NULL DEFAULT '';
