#!/usr/bin/env bash
# End-to-end smoke test: SSH in, fix DNS if needed, verify outbound connectivity.
set -euo pipefail
FC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SSH="$FC_DIR/fc-ssh.sh"

echo "[test] waiting for SSH..."
for i in $(seq 1 20); do
  "$SSH" true 2>/dev/null && break
  sleep 1
done

echo "[test] ensuring DNS + default route in guest"
"$SSH" bash -s <<'EOF'
set -e
ip route show default | grep -q '172.16.0.1' || ip route add default via 172.16.0.1 2>/dev/null || true
grep -q '1.1.1.1' /etc/resolv.conf 2>/dev/null || echo 'nameserver 1.1.1.1' > /etc/resolv.conf
EOF

echo "[test] hostname:"; "$SSH" hostname
echo "[test] kernel:";  "$SSH" uname -r
echo "[test] outbound HTTP:"
"$SSH" 'curl -sS -o /dev/null -w "example.com -> %{http_code}\n" http://example.com'
echo "[test] OK"
