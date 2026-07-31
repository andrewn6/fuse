#!/usr/bin/env bash
# Smoke test for a booted QEMU GPU VM: SSH in, verify the passed-through GPU
# is visible via nvidia-smi. Hardware-gated: skips cleanly (exit 0) when no
# guest is reachable, so CI never fails on a box without a GPU.
#
# Usage: FUSE_GPU_TEST=1 ./qemu-test.sh <guest_ip>
set -euo pipefail
QEMU_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SSH_KEY="$QEMU_DIR/ubuntu.id_rsa"

SSH=(ssh -i "$SSH_KEY"
     -o StrictHostKeyChecking=no
     -o UserKnownHostsFile=/dev/null
     -o LogLevel=ERROR
     -o ConnectTimeout=5
     -o BatchMode=yes)

if [ "${FUSE_GPU_TEST:-0}" != "1" ]; then
  echo "[test] SKIP: no vfio hardware. Set FUSE_GPU_TEST=1 and provide a guest IP to run."
  exit 0
fi

GUEST_IP="${1:-}"
[ -n "$GUEST_IP" ] || { echo "usage: FUSE_GPU_TEST=1 $0 <guest_ip>" >&2; exit 1; }
[ -f "$SSH_KEY" ] || { echo "[test] $SSH_KEY not found; run ./qemu-install.sh first" >&2; exit 1; }

echo "[test] waiting for SSH on $GUEST_IP..."
for i in $(seq 1 20); do
  "${SSH[@]}" "root@$GUEST_IP" true 2>/dev/null && break
  sleep 1
done

echo "[test] nvidia-smi:"
"${SSH[@]}" "root@$GUEST_IP" nvidia-smi -L || { echo "[test] FAIL: nvidia-smi not found or no GPU visible" >&2; exit 1; }

echo "[test] hostname:"; "${SSH[@]}" "root@$GUEST_IP" hostname
echo "[test] kernel:";  "${SSH[@]}" "root@$GUEST_IP" uname -r
echo "[test] OK"
