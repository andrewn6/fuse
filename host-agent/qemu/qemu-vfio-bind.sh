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
#   <count> <kind> <pci_slot> [<pci_slot> ...] [| key=val;key=val;...]
# Example: 1 a100 0000:17:00.0 | uuid=GPU-abc;model=NVIDIA A100-SXM4-80GB;memory_mb=81920;mig_mode=Disabled
#
# The " | key=val;..." suffix is optional per-device metadata (uuid, model,
# memory_mb, mig_mode) captured from a PRE-bind nvidia-smi probe; nvidia-smi is
# blind once devices sit on vfio-pci, so do_bind snapshots it before binding and
# do_list reads that snapshot (VFIO_SMI_SNAPSHOT). When nvidia-smi is absent or
# the group slot is not in the snapshot the suffix is omitted (old format).
#
# qemu-agent.py reads this file (VFIO_INVENTORY): capacity.gpus is the sum of
# counts, capacity.gpu_kind the single kind when homogeneous, and per-device
# metadata surfaces as capacity.gpu_devices. It also picks free devices here at
# VM create time.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VFIO_INVENTORY="${VFIO_INVENTORY:-$SCRIPT_DIR/vfio-inventory.txt}"

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

# -- gpu metadata (pre-bind nvidia-smi snapshot) -----------------------------

# nvidia-smi goes blind once GPUs are bound to vfio-pci, so metadata must be
# captured before the bind loop. snapshot_smi() writes a CSV to VFIO_SMI_SNAPSHOT
# and do_list reads it via smi_meta_for_slot(). Both degrade to no-op / empty
# when nvidia-smi is missing so the listing never fails.

normalize_bus_id() {
  # trim nvidia-smi's 8-digit domain (00000000:17:00.0) to lspci's 0000:17:00.0
  local id="$1"
  id=$(printf '%s' "$id" | tr '[:upper:]' '[:lower:]')
  local head="${id%%:*}" rest="${id#*:}"
  if [ "${#head}" -gt 4 ]; then
    head="${head: -4}"
  fi
  printf '%s:%s' "$head" "$rest"
}

snapshot_smi() {
  # capture per-gpu metadata to $1 (pci_bus_id,uuid,name,memory_mb,mig_mode).
  # never fails: a missing nvidia-smi just leaves an empty file.
  local out="$1"
  : > "$out"
  command -v nvidia-smi >/dev/null 2>&1 || return 0
  nvidia-smi --query-gpu=pci.bus_id,uuid,name,memory.total,mig.mode.current \
    --format=csv,noheader,nounits > "$out" 2>/dev/null || : > "$out"
}

smi_meta_for_slot() {
  # print "bus_id=...;uuid=...;model=...;memory_mb=...;mig_mode=..." for the
  # group's GPU slot from the snapshot, or nothing when unavailable. bus_id is
  # the GPU's own slot (the argument), not the group's first slot, which may be
  # a bridge. Consults the file named by VFIO_SMI_SNAPSHOT; matches on
  # normalized pci bus id.
  local want="$1" snapshot="${VFIO_SMI_SNAPSHOT:-}"
  [ -n "$snapshot" ] && [ -f "$snapshot" ] || return 0
  want=$(normalize_bus_id "$want")
  local line bus uuid model mem mig
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    # split the 5 csv fields; model names have no embedded commas
    IFS=',' read -r bus uuid model mem mig <<< "$line"
    bus=$(normalize_bus_id "$(printf '%s' "$bus" | tr -d ' ')")
    [ "$bus" = "$want" ] || continue
    uuid=$(printf '%s' "$uuid" | sed 's/^ *//;s/ *$//')
    model=$(printf '%s' "$model" | sed 's/^ *//;s/ *$//')
    mem=$(printf '%s' "$mem" | sed 's/^ *//;s/ *$//')
    mig=$(printf '%s' "$mig" | sed 's/^ *//;s/ *$//')
    printf 'bus_id=%s;uuid=%s;model=%s;memory_mb=%s;mig_mode=%s' "$bus" "$uuid" "$model" "$mem" "$mig"
    return 0
  done < "$snapshot"
}

# -- commands ----------------------------------------------------------------

do_list() {
  iommu_enabled || die "IOMMU not enabled. Add intel_iommu=on or amd_iommu=on to kernel cmdline and reboot."
  local slots group slot kind group_slots count
  slots=$(nvidia_gpu_slots)
  [ -n "$slots" ] || { warn "no NVIDIA GPUs detected"; exit 0; }

  declare -A seen
  local meta
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
    count=0
    for s in "${group_slots[@]}"; do
      if lspci -D -s "$s" | grep -qi 'NVIDIA' && lspci -D -s "$s" | grep -qiE 'VGA|3D|Display'; then
        count=$((count + 1))
      fi
    done
    # $slot is the group's primary NVIDIA GPU; look up its pre-bind metadata
    meta=$(smi_meta_for_slot "$slot")
    if [ -n "$meta" ]; then
      printf '%s %s %s | %s\n' "$count" "$kind" "${group_slots[*]}" "$meta"
    else
      printf '%s %s %s\n' "$count" "$kind" "${group_slots[*]}"
    fi
  done
}

do_bind() {
  iommu_enabled || die "IOMMU not enabled. Add intel_iommu=on or amd_iommu=on to kernel cmdline and reboot."
  modprobe vfio-pci 2>/dev/null || sudo modprobe vfio-pci

  local slots slot group group_slot driver inventory_tmp smi_snapshot
  slots=$(nvidia_gpu_slots)
  [ -n "$slots" ] || die "no NVIDIA GPUs detected by lspci"

  # capture nvidia-smi metadata BEFORE binding: once devices sit on vfio-pci
  # nvidia-smi can no longer see them. do_list reads this snapshot below.
  smi_snapshot=$(mktemp "${VFIO_INVENTORY}.smi.XXXXXX")
  snapshot_smi "$smi_snapshot"

  declare -A seen
  for slot in $slots; do
    group=$(iommu_group_of "$slot")
    [ -z "${seen[$group]:-}" ] || continue
    seen[$group]=1
    for group_slot in $(slots_in_group "$group"); do
      driver=$(current_driver "$group_slot")
      if [ "$driver" = "vfio-pci" ]; then
        log "$group_slot already bound to vfio-pci, skip"
        continue
      fi
      log "bind $group_slot from ${driver:-unbound}"
      printf 'vfio-pci' | sudo tee "/sys/bus/pci/devices/$group_slot/driver_override" >/dev/null
      if [ -n "$driver" ]; then
        printf '%s' "$group_slot" | sudo tee "/sys/bus/pci/devices/$group_slot/driver/unbind" >/dev/null
      fi
      printf '%s' "$group_slot" | sudo tee /sys/bus/pci/drivers_probe >/dev/null
      [ "$(current_driver "$group_slot")" = "vfio-pci" ] || die "$group_slot failed to bind to vfio-pci"
    done
  done

  inventory_tmp=$(mktemp "${VFIO_INVENTORY}.XXXXXX")
  # do_list reads the pre-bind snapshot to enrich each line with device metadata
  VFIO_SMI_SNAPSHOT="$smi_snapshot" do_list > "$inventory_tmp"
  mv "$inventory_tmp" "$VFIO_INVENTORY"
  rm -f "$smi_snapshot"
  log "done. Inventory written to $VFIO_INVENTORY:"
  cat "$VFIO_INVENTORY"
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
