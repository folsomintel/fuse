-- GPU scheduling: hosts advertise a virtualization backend and GPU
-- capacity, VMs record the GPU spec they were provisioned with so a
-- crashed orchestrator can rehydrate allocation state on restart.
-- Default backend='firecracker' preserves existing rows (no GPU support).
ALTER TABLE orchestrator_hosts ADD COLUMN IF NOT EXISTS backend TEXT NOT NULL DEFAULT 'firecracker';
ALTER TABLE orchestrator_hosts ADD COLUMN IF NOT EXISTS gpus_total INTEGER NOT NULL DEFAULT 0;
ALTER TABLE orchestrator_hosts ADD COLUMN IF NOT EXISTS gpu_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE orchestrator_hosts ADD COLUMN IF NOT EXISTS gpus_allocated INTEGER NOT NULL DEFAULT 0;

-- orchestrator_vms (persist spec for recovery)
ALTER TABLE orchestrator_vms ADD COLUMN IF NOT EXISTS gpus INTEGER NOT NULL DEFAULT 0;
ALTER TABLE orchestrator_vms ADD COLUMN IF NOT EXISTS gpu_kind TEXT NOT NULL DEFAULT '';
