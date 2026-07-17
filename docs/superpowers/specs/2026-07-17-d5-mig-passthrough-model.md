# D5: MIG passthrough model

source: https://github.com/folsomintel/fuse/issues/32
epic: https://github.com/folsomintel/fuse/issues/31

## decision

**vGPU / mediated-device (mdev) fractional passthrough.**

the allocation unit becomes `mig_uuid` (one MIG GPU instance exposed as an
mdev device), not the whole physical `gpu_uuid`. fuse packs multiple tenants
onto one physical A100/H100/H200 by passing each VM
`vfio-pci,sysfsdev=/sys/bus/mdev/devices/<uuid>` instead of
`vfio-pci,host=<slot>`.

## options considered

1. **whole-GPU vfio-pci + in-guest MIG.** fuse keeps allocating a whole
   physical GPU (`vfio-pci,host=<slot>`, `pick_gpu_slots` unchanged); the
   guest enables MIG and carves GPU instances internally. no mdev, no
   licensing, no scheduler rewrite — inventory just advertises
   mig-capable/mig-mode. rejected: fuse cannot pack multiple tenants onto one
   physical GPU, which is the point of the epic.
2. **vGPU / mdev fractional passthrough (chosen).** individual MIG instances
   are the allocation unit. requires NVIDIA vGPU (GRID) licensing, mdev
   lifecycle management on the host agent, and a fractional scheduler with
   per-instance accounting. breaks the current whole-IOMMU-group assumption
   in `pick_gpu_slots` and forces a revisit of the D4 snapshot/fork refusal.
3. **container-runtime provider.** MIG via `CUDA_VISIBLE_DEVICES` in
   containers rather than VMs. rejected: a different isolation model that
   does not fit fuse's VM-centric design; would be a new provider, not an
   evolution of the qemu path.

## why fractional

- the epic's goal is fractional/MIG allocation, not just richer whole-device
  inventory. option 1 leaves multi-tenant packing permanently off the table.
- option 2 is the only VM-native path where `gpu_profile` (#42) is a real
  allocation unit rather than advisory metadata.
- the licensing/cost overhead of NVIDIA vGPU is accepted as the price of
  fleet density on A100/H100-class hardware.

## licensing / cost implications

- NVIDIA vGPU (GRID) software licensing is required on every MIG host: a
  per-GPU (or per-CCU) subscription plus the vGPU host driver package, which
  replaces the plain datacenter driver.
- hosts that only ever serve whole-GPU workloads can stay on the unlicensed
  whole-device path; the mdev path is additive, keyed off host capability.
- CI/dev coverage needs at least one licensed MIG-capable host (or the
  nvidia vGPU eval program) — pure-software emulation of mdev is not
  available.

## resulting allocation unit

`mig_uuid`. per-MIG-instance inventory rows carry: profile (`1g.10gb`,
`2g.20gb`, ...), memory, parent `gpu_uuid`, and `mig_uuid`. the scheduler
matches a requested `gpu_profile` against free instances and binds the VM to
a specific `mig_uuid`; whole-GPU requests (`gpu: N` with no profile) continue
to allocate unpartitioned physical devices.

## implications vs current code

| area | today | after D5 |
|---|---|---|
| agent device path | `vfio-pci,host=<slot>` per whole IOMMU group (`pick_gpu_slots` L164, `start_qemu` L427, `host-agent/qemu-agent.py`) | `vfio-pci,sysfsdev=/sys/bus/mdev/devices/<uuid>` per MIG instance |
| scheduler fit | integer headroom + exact `gpu_kind` string (`fits()`, `internal/orchestrator/scheduler.go:64`) | match requested profile against free per-MIG rows; bind by `mig_uuid` |
| allocation accounting | `Allocated.GPUs` integer counter | per-instance occupancy on MIG rows (parent GPU derived) |
| inventory | scalar `gpus_total`/`gpu_kind` (migration `0004_gpu_hosts.sql`) | per-MIG rows: profile, memory, parent `gpu_uuid`, `mig_uuid` (phase-2 schema #37 must anticipate these) |
| request surface | `gpu` + `gpu_kind` | + `gpu_profile` — tracked in #42 |
| D4 snapshot/fork block | refuse when `GPUs > 0` (`fork.go:99`, `snapshots.go:98`) | keep the refusal for mdev-passed devices initially; NVIDIA vGPU supports suspend/resume in some configurations, so #41 may relax it later behind a capability flag |
| D3 qemu-only guard | GPU VMs only on qemu hosts (`scheduler.go:172`) | unchanged; mdev is qemu-only too |

## phase-3 issue breakdown (to create under #41)

1. host-agent: MIG mode enable/reset (`nvidia-smi -mig 1`), GI/CI
   create/destroy (`mig -cgi`/`-cci`), persist configured profiles across
   reboot.
2. host-agent: mdev lifecycle — create/destroy mdev devices for GPU
   instances, replace `pick_gpu_slots` whole-group selection with per-`mig_uuid`
   allocation, emit `vfio-pci,sysfsdev=...` in `start_qemu`, propagate the
   device UUID into the guest (`CUDA_VISIBLE_DEVICES`).
3. orchestrator: per-MIG inventory rows + allocate/deallocate by `mig_uuid`
   (extends the phase-2 per-device model from #37/#38).
4. orchestrator: admission — requested `gpu_profile` must fit a free GI
   placement on some host; reject otherwise.
5. guardrail: revisit D4 for MIG slices (`fork.go`, `snapshots.go`,
   `gpu_guardrail_test.go`) — gate on passthrough type instead of a blanket
   `GPUs > 0` check.
6. docs: MIG section in `gpu-host-setup.mdx` (stub reserved by #40).
7. request surface: `gpu_profile` through fusefile → sdk → api →
   orchestrator → qemu — already tracked as #42, unblocked by this decision:
   `gpu_profile` is the real allocation unit, not advisory metadata.
