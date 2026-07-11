#!/usr/bin/env bash
# Detect, list, and bind NVIDIA GPUs to vfio-pci for QEMU/KVM passthrough.
#
# Usage:
#   ./qemu-vfio-bind.sh                 bind all detected NVIDIA GPUs to vfio-pci
#   ./qemu-vfio-bind.sh --list          print inventory (consumed by qemu-agent.py)
#   ./qemu-vfio-bind.sh --unbind <pci>  unbind a specific device from vfio-pci
#
# Idempotent: already-bound devices are skipped. Requires IOMMU enabled
# (intel_iommu=on or amd_iommu=on on the kernel cmdline + reboot).
#
# Inventory format (--list), one line per IOMMU group containing an NVIDIA GPU:
#   <count> <kind> <pci_slot> [<pci_slot> ...]
# Example: 1 a100 0000:17:00.0
#
# The operator sums lines to populate capacity.gpus and sets capacity.gpu_kind
# when homogeneous. qemu-agent.py reads this file (VFIO_INVENTORY) to pick
# free devices at VM create time.
set -euo pipefail

log()  { printf '\033[1;36m[vfio] %s\033[0m\n' "$*"; }
warn() { printf '\033[1;33m[vfio] %s\033[0m\n' "$*" >&2; }
die()  { printf '\033[1;31m[vfio] %s\033[0m\n' "$*" >&2; exit 1; }

# -- checks ------------------------------------------------------------------

iommu_enabled() {
  [ -d /sys/kernel/iommu_groups ] && \
    [ "$(ls -d /sys/kernel/iommu_groups/*/devices 2>/dev/null | wc -l)" -gt 0 ]
}

nvidia_gpu_slots() {
  # print domain:bus:dev.func for NVIDIA VGA/3D/Display devices
  lspci -D | grep -i 'NVIDIA' | grep -iE 'VGA|3D|Display' | awk '{print $1}'
}

iommu_group_of() {
  local slot="$1"
  basename "$(readlink -f "/sys/bus/pci/devices/$slot/iommu_group")"
}

slots_in_group() {
  local group="$1"
  local d
  for d in /sys/kernel/iommu_groups/"$group"/devices/*; do
    basename "$d"
  done
}

gpu_kind() {
  # derive a lowercase kind tag from the lspci description
  local slot="$1" desc kind
  desc=$(lspci -s "$slot" 2>/dev/null | sed 's/^.*: //')
  kind=$(printf '%s' "$desc" | grep -oE '\[[^]]+\]' | tail -1 | tr -d '[]' | tr '[:upper:]' '[:lower:]' | tr ' ' '-')
  printf '%s' "${kind:-nvidia-gpu}"
}

current_driver() {
  local slot="$1"
  local link
  link=$(readlink "/sys/bus/pci/devices/$slot/driver" 2>/dev/null || true)
  basename "$link"
}

# -- commands ----------------------------------------------------------------

do_list() {
  iommu_enabled || die "IOMMU not enabled. Add intel_iommu=on or amd_iommu=on to kernel cmdline and reboot."
  local slots group slot kind group_slots count
  slots=$(nvidia_gpu_slots)
  [ -n "$slots" ] || { warn "no NVIDIA GPUs detected"; exit 0; }

  declare -A seen
  for slot in $slots; do
    group=$(iommu_group_of "$slot")
    [ -n "$group" ] || continue
    # skip duplicate groups (multiple GPUs in the same group is rare but possible)
    [ -z "${seen[$group]:-}" ] || continue
    seen[$group]=1

    kind=$(gpu_kind "$slot")
    group_slots=()
    local s
    for s in $(slots_in_group "$group"); do
      group_slots+=("$s")
    done
    count=${#group_slots[@]}
    printf '%s %s %s\n' "$count" "$kind" "${group_slots[*]}"
  done
}

do_bind() {
  iommu_enabled || die "IOMMU not enabled. Add intel_iommu=on or amd_iommu=on to kernel cmdline and reboot."
  modprobe vfio-pci 2>/dev/null || sudo modprobe vfio-pci

  local slots slot driver ven dev
  slots=$(nvidia_gpu_slots)
  [ -n "$slots" ] || die "no NVIDIA GPUs detected by lspci"

  for slot in $slots; do
    driver=$(current_driver "$slot")
    if [ "$driver" = "vfio-pci" ]; then
      log "$slot already bound to vfio-pci, skip"
      continue
    fi

    ven=$(cat "/sys/bus/pci/devices/$slot/vendor")   # 0x10de
    dev=$(cat "/sys/bus/pci/devices/$slot/device")
    ven_dev="${ven#0x}:${dev#0x}"

    log "bind $slot ($ven_dev) from ${driver:-unbound}"
    # unbind from current driver
    if [ -n "$driver" ]; then
      echo "$slot" | sudo tee "/sys/bus/pci/devices/$slot/driver/unbind" >/dev/null
    fi
    # register vendor:device with vfio-pci (idempotent: -EEXIST is fine)
    echo "$ven_dev" | sudo tee "/sys/bus/pci/drivers/vfio-pci/new_id" 2>/dev/null || true
    # also override so vfio-pci claims it even if the probe raced
    echo "$ven_dev" | sudo tee "/sys/bus/pci/drivers/vfio-pci/add_id" 2>/dev/null || true
  done

  log "done. Inventory:"
  do_list
}

do_unbind() {
  local slot="${1:?pci slot required (e.g. 0000:17:00.0)}"
  local driver
  driver=$(current_driver "$slot")
  if [ "$driver" != "vfio-pci" ]; then
    log "$slot not bound to vfio-pci (driver=${driver:-none}), skip"
    return
  fi
  log "unbind $slot from vfio-pci"
  echo "$slot" | sudo tee "/sys/bus/pci/drivers/vfio-pci/unbind" >/dev/null
}

# -- main --------------------------------------------------------------------

case "${1:-}" in
  --list)    do_list ;;
  --unbind)  shift; do_unbind "$@" ;;
  ""|bind)   do_bind ;;
  *) die "usage: $0 [--list|--unbind <pci>|bind]" ;;
esac
