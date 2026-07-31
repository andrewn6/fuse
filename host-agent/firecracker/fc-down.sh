#!/usr/bin/env bash
# Shut down the Firecracker microVM and clean up host networking.
set -euo pipefail

FC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SOCK=/tmp/fc.sock
TAP=tap0
HOST_IFACE=$(ip -o route get 8.8.8.8 | awk '{print $5}')

# Try graceful shutdown via API
if [ -S "$SOCK" ]; then
  sudo curl -fsS --unix-socket "$SOCK" -X PUT 'http://localhost/actions' \
    -H 'Content-Type: application/json' \
    -d '{"action_type":"SendCtrlAltDel"}' >/dev/null 2>&1 || true
  sleep 1
fi

sudo pkill -f "firecracker --api-sock $SOCK" 2>/dev/null || true
sudo rm -f "$SOCK"

if ip link show "$TAP" >/dev/null 2>&1; then
  sudo ip link del "$TAP"
  echo "[fc-down] removed $TAP"
fi

# Best-effort iptables cleanup (ignore misses)
sudo iptables -t nat -D POSTROUTING -o "$HOST_IFACE" -j MASQUERADE 2>/dev/null || true
sudo iptables -D FORWARD -i "$TAP" -o "$HOST_IFACE" -j ACCEPT 2>/dev/null || true
sudo iptables -D FORWARD -i "$HOST_IFACE" -o "$TAP" -m state --state RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || true

echo "[fc-down] done"
