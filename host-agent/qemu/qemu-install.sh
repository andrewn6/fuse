#!/usr/bin/env bash
# One-shot installer for the QEMU/KVM GPU host: installs qemu-system, OVMF
# firmware, qemu-utils, downloads a base Ubuntu cloud image, and generates SSH
# keys. Safe to re-run; skips anything already present.
set -euo pipefail
QEMU_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$QEMU_DIR"

log() { printf '\033[1;36m[install] %s\033[0m\n' "$*"; }

# 1. apt packages
need_pkgs=()
command -v qemu-system-x86_64 >/dev/null 2>&1 || need_pkgs+=(qemu-system-x86)
[ -f /usr/share/OVMF/OVMF_CODE.fd ] || need_pkgs+=(ovmf)
command -v qemu-nbd >/dev/null 2>&1 || need_pkgs+=(qemu-utils)
command -v iptables >/dev/null 2>&1 || need_pkgs+=(iptables)
if [ ${#need_pkgs[@]} -gt 0 ]; then
  log "apt install: ${need_pkgs[*]}"
  sudo apt-get update -qq
  sudo apt-get install -y "${need_pkgs[@]}"
fi

# 2. SSH keypair (shared with fc-agent; reuse if already present)
if [ ! -f ubuntu.id_rsa ]; then
  log "generate SSH keypair"
  ssh-keygen -t rsa -b 2048 -N "" -f ubuntu.id_rsa
fi
chmod 600 ubuntu.id_rsa
if [ ! -f ubuntu.id_rsa.pub ]; then
  log "derive missing SSH public key"
  ssh-keygen -y -f ubuntu.id_rsa > ubuntu.id_rsa.pub
fi

# 3. base rootfs (Ubuntu 22.04 cloud image, qcow2)
if [ ! -f rootfs.qcow2 ]; then
  log "download Ubuntu 22.04 cloud image"
  curl -fsSL -o rootfs.qcow2 \
    "https://cloud-images.ubuntu.com/jammy/current/jammy-server-cloudimg-amd64.img"
fi

# 4. fused binary
if [ ! -f fused ]; then
  log "note: place your fused binary here (run ../shared/fc-build-agent.sh or copy one)"
  log "      the bake step (qemu-bake-cuda-rootfs.sh) requires it"
fi

log "ready. Next: ./qemu-vfio-bind.sh && ./qemu-bake-cuda-rootfs.sh <driver-version>"
