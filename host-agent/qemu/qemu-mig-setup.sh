#!/usr/bin/env bash
# Enable NVIDIA MIG mode, carve GPU instances, and emit the mig-inventory.txt
# the qemu agent reads at create time. Sibling to qemu-vfio-bind.sh: where the
# vfio script binds whole GPUs to vfio-pci, this script carves MIG slices out of
# a GPU that stays on the nvidia driver.
#
# MIG and vfio-pci bind are mutually exclusive on the same card: a GPU in MIG
# mode stays on the nvidia driver and exports mdev devices, so it is never part
# of the whole-device vfio pool (decision D5). run this BEFORE
# qemu-vfio-bind.sh on a card you intend to slice, and never run both against
# the same GPU.
#
# usage:
#   ./qemu-mig-setup.sh --profile 1g.10gb=4 [--profile 2g.20gb=1]
#                       enable MIG, create the requested GPU instances, persist
#                       the layout, and refresh mig-inventory.txt
#   ./qemu-mig-setup.sh                  re-apply the persisted layout (reboot path)
#   ./qemu-mig-setup.sh --list           print current inventory (consumed by qemu-agent.py)
#   ./qemu-mig-setup.sh --unbind <uuid>  destroy the MIG GPU instance with this uuid
#
# idempotent: enabling MIG on an already-enabled GPU is a no-op, and creating a
# GPU instance that already exists for that profile is skipped (best effort).
# requires the nvidia driver and nvidia-smi; GPUs must still be on the nvidia
# driver (not vfio-pci), since nvidia-smi is blind to vfio-bound devices.
#
# inventory format (--list), one line per MIG GPU instance:
#   <profile> <kind> <uuid> [<parent_gpu_uuid>]
# example: 1g.10gb a100 MIG-a1b2c3 GPU-d4e5f6
# the optional 4th field is the parent GPU uuid; the agent reads 3 or 4 fields.
#
# qemu-agent.py reads this file (MIG_INVENTORY): host_capacity reports each
# instance as a per-instance MIG inventory entry, and pick_mig_devices picks
# free instances of a profile to attach at create time.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
QEMU_DIR="${QEMU_DIR:-/home/ubuntu/qemu}"
MIG_INVENTORY="${MIG_INVENTORY:-$QEMU_DIR/mig-inventory.txt}"
MIG_LAYOUT_CONF="${MIG_LAYOUT_CONF:-$QEMU_DIR/mig-layout.conf}"

log()  { printf '\033[1;36m[mig] %s\033[0m\n' "$*"; }
warn() { printf '\033[1;33m[mig] %s\033[0m\n' "$*" >&2; }
die()  { printf '\033[1;31m[mig] %s\033[0m\n' "$*" >&2; exit 1; }

require_smi() {
  command -v nvidia-smi >/dev/null 2>&1 || die "nvidia-smi not found. MIG setup requires the nvidia driver with MIG support (A100/H100)."
}

# -- helpers ------------------------------------------------------------------

# gpu_kind <pci_slot|index> -> lowercase kind tag. the kind is derived from the
# model name so it matches the gpu_kind the orchestrator requests (e.g. a100).
gpu_kind() {
  local idx="$1" model kind
  model=$(nvidia-smi --query-gpu=name --format=csv,noheader,nounits -i "$idx" 2>/dev/null | head -1)
  kind=$(printf '%s' "$model" | tr '[:upper:]' '[:lower:]' | tr ' ' '-')
  printf '%s' "${kind:-nvidia-gpu}"
}

# parent_gpu_uuids populates the associative array PARENT_UUID[index]=GPU-uuid
# from nvidia-smi so inventory lines can carry the parent gpu uuid.
declare -A PARENT_UUID
load_parent_uuids() {
  local idx uuid
  while IFS=',' read -r idx uuid; do
    [ -n "$idx" ] || continue
    PARENT_UUID["$idx"]="$uuid"
  done < <(nvidia-smi --query-gpu=index,uuid --format=csv,noheader,nounits 2>/dev/null || true)
}

# mig_enabled <gpu_index> -> "Enabled"|"Disabled"|"N/A"
mig_mode() {
  local idx="$1"
  nvidia-smi --query-gpu=mig.mode.current --format=csv,noheader,nounits -i "$idx" 2>/dev/null | head -1 || printf 'N/A'
}

# enable_mig turns MIG mode on for every GPU that supports it. already-enabled
# GPUs are skipped. a GPU whose pending mode differs from current requires a
# reset to apply; we warn and leave it for the operator rather than resetting
# live hardware automatically.
enable_mig() {
  local idx mode pending
  for idx in $(nvidia-smi --query-gpu=index --format=csv,noheader,nounits 2>/dev/null || true); do
    mode=$(mig_mode "$idx")
    [ "$mode" = "Enabled" ] && { log "gpu $idx: MIG already enabled, skip"; continue; }
    [ "$mode" = "N/A" ] && { warn "gpu $idx: MIG not supported, skip"; continue; }
    log "gpu $idx: enable MIG"
    nvidia-smi -i "$idx" -mig 1 >/dev/null 2>&1 || {
      pending=$(nvidia-smi --query-gpu=mig.mode.pending --format=csv,noheader,nounits -i "$idx" 2>/dev/null | head -1 || true)
      die "gpu $idx: failed to enable MIG (current=$mode pending=${pending:-?}). a pending change needs a GPU reset: run 'nvidia-smi -i $idx -r' then re-run this script."
    }
  done
}

# create_instances <profile=count ...> carves GPU instances of each requested
# profile. idempotent: existing instances of a profile count toward the
# requested total so a re-run only creates what is missing.
create_instances() {
  local profile count have gpu
  for entry in "$@"; do
    profile="${entry%%=*}"
    count="${entry##*=}"
    [ -n "$profile" ] && [ -n "$count" ] || die "invalid --profile $entry (expected profile=count, e.g. 1g.10gb=4)"
    have=$(count_instances "$profile")
    if [ "$have" -ge "$count" ]; then
      log "$profile: $have/$count instances already present, skip"
      continue
    fi
    local need=$((count - have))
    log "$profile: create $need instance(s) ($have/$count present)"
    for gpu in $(nvidia-smi --query-gpu=index --format=csv,noheader,nounits 2>/dev/null || true); do
      [ "$need" -le 0 ] && break
      [ "$(mig_mode "$gpu")" = "Enabled" ] || continue
      # -C also creates a default compute instance covering the whole GPU
      # instance, which is what a single-tenant MIG slice expects.
      if nvidia-smi mig -cgi "$profile" -C -gpu "$gpu" >/dev/null 2>&1; then
        need=$((need - 1))
      fi
    done
    [ "$need" -le 0 ] || warn "$profile: created $((count - need))/$count instances (insufficient MIG capacity on this host)"
  done
}

# count_instances <profile> -> number of GPU instances of this profile across
# all enabled GPUs, parsed from `nvidia-smi mig -lgi -c` (JSON). never fails.
count_instances() {
  local profile="$1"
  nvidia-smi mig -lgi -c 2>/dev/null \
    | grep -c "\"profile\":\"$profile\"" || true
}

# write_layout <profile=count ...> persists the requested layout so a reboot
# (or a bare re-run) re-applies the same instances.
write_layout() {
  mkdir -p "$(dirname "$MIG_LAYOUT_CONF")"
  : > "$MIG_LAYOUT_CONF"
  local entry
  for entry in "$@"; do
    printf '%s\n' "$entry" >> "$MIG_LAYOUT_CONF"
  done
  log "layout written to $MIG_LAYOUT_CONF"
}

# read_layout echoes the persisted profile=count entries, one per line, or
# nothing if no layout is persisted.
read_layout() {
  [ -f "$MIG_LAYOUT_CONF" ] || return 0
  grep -v '^[[:space:]]*$' "$MIG_LAYOUT_CONF" 2>/dev/null || true
}

# -- commands -----------------------------------------------------------------

# do_list emits mig-inventory.txt from live `nvidia-smi mig -lgi -c` output.
# format: <profile> <kind> <uuid> <parent_gpu_uuid>. degrades to an empty
# inventory when no GPU instances exist (e.g. MIG enabled but nothing carved).
do_list() {
  require_smi
  load_parent_uuids
  local tmp
  tmp=$(mktemp "${MIG_INVENTORY}.XXXXXX")
  : > "$tmp"
  local line gpu_idx profile inst_uuid parent kind
  # `nvidia-smi mig -lgi -c` prints one JSON object per GPU instance. flatten it
  # to one line per object and pull the fields we need with sed so we never
  # need a JSON parser dependency.
  nvidia-smi mig -lgi -c 2>/dev/null | tr -d '\n' \
    | sed 's/}/}\n/g' \
    | while IFS= read -r obj; do
      [ -n "$obj" ] || continue
      profile=$(printf '%s' "$obj" | sed -n 's/.*"profile":"\([^"]*\)".*/\1/p')
      inst_uuid=$(printf '%s' "$obj" | sed -n 's/.*"uuid":"\([^"]*\)".*/\1/p')
      gpu_idx=$(printf '%s' "$obj" | sed -n 's/.*"gpu_instance_id":\([0-9][0-9]*\).*/\1/p')
      [ -n "$profile" ] && [ -n "$inst_uuid" ] && [ -n "$gpu_idx" ] || continue
      kind=$(gpu_kind "$gpu_idx")
      parent="${PARENT_UUID[$gpu_idx]:-}"
      if [ -n "$parent" ]; then
        printf '%s %s %s %s\n' "$profile" "$kind" "$inst_uuid" "$parent" >> "$tmp"
      else
        printf '%s %s %s\n' "$profile" "$kind" "$inst_uuid" >> "$tmp"
      fi
    done
  mv "$tmp" "$MIG_INVENTORY"
  log "inventory written to $MIG_INVENTORY ($(wc -l < "$MIG_INVENTORY") instance(s)):"
  cat "$MIG_INVENTORY"
}

do_apply() {
  require_smi
  local profiles=()
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --profile) profiles+=("$2"); shift 2 ;;
      *) die "unknown argument $1 (expected --profile profile=count)" ;;
    esac
  done
  # if no profiles were given, re-apply the persisted layout (reboot path).
  if [ "${#profiles[@]}" -eq 0 ]; then
    local persisted
    persisted=$(read_layout)
    [ -n "$persisted" ] || die "no --profile given and no layout persisted at $MIG_LAYOUT_CONF. pass --profile 1g.10gb=4 to carve instances."
    while IFS= read -r entry; do
      [ -n "$entry" ] && profiles+=("$entry")
    done <<< "$persisted"
    log "re-applying persisted layout ($(printf '%s ' "${profiles[@]}"))"
  fi
  enable_mig
  create_instances "${profiles[@]}"
  write_layout "${profiles[@]}"
  do_list
}

do_unbind() {
  require_smi
  local uuid="${1:?MIG GPU instance uuid required (e.g. MIG-a1b2c3)}"
  # find the gpu index + instance id carrying this uuid, then destroy it.
  local line gpu_idx inst_id
  line=$(nvidia-smi mig -lgi -c 2>/dev/null | tr -d '\n' | sed 's/}/}\n/g' \
    | grep "\"uuid\":\"$uuid\"" || true)
  [ -n "$line" ] || { warn "no MIG instance with uuid $uuid"; exit 0; }
  gpu_idx=$(printf '%s' "$line" | sed -n 's/.*"gpu_instance_id":\([0-9][0-9]*\).*/\1/p')
  inst_id=$(printf '%s' "$line" | sed -n 's/.*"instance_id":\([0-9][0-9]*\).*/\1/p')
  [ -n "$gpu_idx" ] && [ -n "$inst_id" ] || die "could not resolve gpu/instance id for $uuid"
  log "destroy GPU instance $inst_id on gpu $gpu_idx (uuid $uuid)"
  nvidia-smi mig -dgi -i "$inst_id" -gpu "$gpu_idx" >/dev/null 2>&1 \
    || die "failed to destroy instance $uuid (is it still attached to a vm?)"
  do_list
}

# -- main ---------------------------------------------------------------------

case "${1:-}" in
  --list)        shift; do_list ;;
  --unbind)      shift; do_unbind "$@" ;;
  --profile)     do_apply "$@" ;;
  "")            do_apply "$@" ;;
  *) die "usage: $0 [--profile profile=count ...] | --list | --unbind <uuid>" ;;
esac