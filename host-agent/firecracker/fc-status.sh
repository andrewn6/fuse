#!/usr/bin/env bash
# Show health of the Firecracker setup.
set -uo pipefail
FC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SOCK=/tmp/fc.sock

echo "== process =="
pgrep -a firecracker || echo "firecracker not running"
echo
echo "== tap0 =="
ip -br addr show tap0 2>/dev/null || echo "tap0 missing"
echo
echo "== socket =="
[ -S "$SOCK" ] && echo "$SOCK present" || echo "$SOCK missing"
echo
echo "== guest ping =="
ping -c1 -W1 172.16.0.2 >/dev/null && echo "172.16.0.2 reachable" || echo "172.16.0.2 unreachable"
echo
echo "== nat rules =="
sudo iptables -t nat -S POSTROUTING | grep -E 'MASQUERADE' || true
sudo iptables -t nat -S PREROUTING | grep -E 'DNAT' || echo "(no port forwards)"
