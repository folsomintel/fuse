# D6: MIG slices and the snapshot/fork guardrail

status: accepted (2026-07-19)
sibling to: D4 (snapshot/fork refusal for gpu passthrough), D5 (mdev MIG
fractional passthrough)

## context

D4 refuses snapshot and fork for any environment whose `spec.GPUs > 0`. the
rationale is that a vfio-pci passthrough gpu cannot be checkpointed: the guest
holds a real pci device whose state lives on the host, outside the vm's memory
image, so a snapshot is incomplete by construction.

issue #41 (per-instance MIG allocation) raises a question D4 did not have to
answer. a MIG slice is not a vfio-pci device: it is a mediated device (mdev)
bound via `vfio-pci sysfsdev`, and the question of whether mdev is checkpointable
is separate from the vfio-pci question. so a MIG-only spec (`spec.GPUProfile`
set, `spec.GPUUUIDs` empty, `spec.MIGInstanceUUIDs` populated) is arguably a
different case from a whole-gpu spec.

three options were on the table:

1. keep blanket refusal: any gpu (whole or MIG) is refused.
2. gate on `len(spec.MIGInstanceUUIDs) > 0` vs `len(spec.GPUUUIDs) > 0`,
   allowing snapshot/fork for MIG-only specs.
3. per-provider capability flag: refuse based on whether the environment's
   provider reports itself snapshot-capable.

## decision

**keep blanket refusal (option 1).** snapshot and fork stay refused for any
environment with `spec.GPUs > 0`, regardless of whether the gpu is a whole
device or a MIG slice.

## rationale

- the qemu provider's `remoteEnv` deliberately does not implement
  `SnapshotCapable`, and the qemu host agent's snapshot endpoints return 501
  unconditionally. neither has been validated for mdev checkpointing on real
  hardware (A100/H100). allowing MIG-only snapshot/fork would advertise a
  capability that has never been tested and that the agent explicitly refuses.
- mdev checkpointing is not obviously correct: the mdev instance's backing
  state on the parent gpu (the carve-out and its compute-instance state) is
  host-side and not part of the guest memory image. a "snapshot" that omits
  it would restore a guest that believes it holds a MIG slice the host no
  longer has carved, which is a worse failure mode than a clean refusal.
- consistency is cheap and safe: an operator who reads "gpus cannot be
  snapshotted" should not have to learn a second rule ("except MIG slices,
  sometimes"). the cost of refusal is that MIG environments must be recreated
  rather than forked, which is the same cost whole-gpu environments already pay.
- the per-instance allocation work in issue #41 is orthogonal to the guardrail.
  bundling a guardrail change into it would couple a scheduler change to an
  unvalidated checkpointing assumption. keeping them separate means the
  per-instance work can land and be validated on real hardware before anyone
  revisits whether MIG snapshot/fork is safe.

## consequences

- `gpu_guardrail_test.go` is unchanged: the existing whole-device refusal tests
  stay, and no MIG-only relaxation test is added (there is nothing to relax).
- a MIG-only environment that a user tries to snapshot or fork gets the same
  gpu-specific unsupported error as a whole-gpu environment.
- this decision is revisitable. if mdev checkpointing is validated on real
  hardware, the path is: add a `SnapshotCapable`-style flag on the qemu
  environment gated on `len(spec.MIGInstanceUUIDs) > 0 && len(spec.GPUUUIDs) == 0`,
  implement the agent-side snapshot path, and add a per-branch test. that work
  is deferred until the hardware validation exists.

## what this decision does not block

the rest of issue #41 (per-instance MIG allocation: agent reporting, scheduler
granularity switch, schema, recover recompute, sdk/cli/docs) is independent of
this guardrail and lands in this PR regardless. only the guardrail code is
held back.