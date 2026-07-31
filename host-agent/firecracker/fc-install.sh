#!/usr/bin/env bash
# One-shot installer: downloads firecracker + kernel + rootfs into this dir.
# Safe to re-run — skips anything already present.
set -euo pipefail
FC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$FC_DIR"

ARCH=$(uname -m)
CI_BASE="https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.10/${ARCH}"

if ! command -v firecracker >/dev/null 2>&1; then
  echo "[install] firecracker binary"
  TAG=$(curl -fsSL https://api.github.com/repos/firecracker-microvm/firecracker/releases/latest | grep tag_name | cut -d '"' -f4)
  TMP=$(mktemp -d)
  curl -fsSL "https://github.com/firecracker-microvm/firecracker/releases/download/${TAG}/firecracker-${TAG}-${ARCH}.tgz" | tar -xz -C "$TMP"
  sudo install -m0755 "$TMP/release-${TAG}-${ARCH}/firecracker-${TAG}-${ARCH}" /usr/local/bin/firecracker
  rm -rf "$TMP"
fi

[ -f vmlinux.bin ]  || curl -fsSL -o vmlinux.bin  "$CI_BASE/vmlinux-5.10.223"
[ -f rootfs.ext4 ]  || curl -fsSL -o rootfs.ext4  "$CI_BASE/ubuntu-22.04.ext4"
[ -f ubuntu.id_rsa ] || { curl -fsSL -o ubuntu.id_rsa "$CI_BASE/ubuntu-22.04.id_rsa"; chmod 600 ubuntu.id_rsa; }

echo "[install] ready. Next: ./fc-up.sh && ./fc-test.sh"
