#!/usr/bin/env bash
# Boot the Firecracker microVM. Idempotent — safe to re-run.
set -euo pipefail

FC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SOCK=/tmp/fc.sock
TAP=tap0
GUEST_IP=172.16.0.2
HOST_TAP_IP=172.16.0.1
KERNEL="$FC_DIR/vmlinux.bin"
ROOTFS="$FC_DIR/rootfs.ext4"
LOG="$FC_DIR/fc.log"

HOST_IFACE=$(ip -o route get 8.8.8.8 | awk '{print $5}')

echo "[fc-up] host iface: $HOST_IFACE"

# 1. TAP device
if ! ip link show "$TAP" >/dev/null 2>&1; then
  sudo ip tuntap add "$TAP" mode tap
  sudo ip addr add "$HOST_TAP_IP/24" dev "$TAP"
  sudo ip link set "$TAP" up
  echo "[fc-up] created $TAP"
else
  echo "[fc-up] $TAP already exists"
fi

# 2. Forwarding + NAT
sudo sysctl -w net.ipv4.ip_forward=1 >/dev/null
sudo iptables -t nat -C POSTROUTING -o "$HOST_IFACE" -j MASQUERADE 2>/dev/null \
  || sudo iptables -t nat -A POSTROUTING -o "$HOST_IFACE" -j MASQUERADE
sudo iptables -C FORWARD -i "$TAP" -o "$HOST_IFACE" -j ACCEPT 2>/dev/null \
  || sudo iptables -A FORWARD -i "$TAP" -o "$HOST_IFACE" -j ACCEPT
sudo iptables -C FORWARD -i "$HOST_IFACE" -o "$TAP" -m state --state RELATED,ESTABLISHED -j ACCEPT 2>/dev/null \
  || sudo iptables -A FORWARD -i "$HOST_IFACE" -o "$TAP" -m state --state RELATED,ESTABLISHED -j ACCEPT

# 3. Kill any existing firecracker on this socket
if pgrep -f "firecracker --api-sock $SOCK" >/dev/null; then
  echo "[fc-up] existing firecracker running — killing"
  sudo pkill -f "firecracker --api-sock $SOCK" || true
  sleep 1
fi
sudo rm -f "$SOCK"

# 4. Launch Firecracker daemon
sudo nohup firecracker --api-sock "$SOCK" >"$LOG" 2>&1 &
for i in 1 2 3 4 5; do
  [ -S "$SOCK" ] && break
  sleep 0.5
done
[ -S "$SOCK" ] || { echo "[fc-up] firecracker failed to start — see $LOG"; exit 1; }
echo "[fc-up] firecracker started"

api() {
  sudo curl -fsS --unix-socket "$SOCK" -X PUT "http://localhost$1" \
    -H 'Content-Type: application/json' -d "$2" >/dev/null
}

api /boot-source "{\"kernel_image_path\":\"$KERNEL\",\"boot_args\":\"console=ttyS0 reboot=k panic=1 pci=off ip=${GUEST_IP}::${HOST_TAP_IP}:255.255.255.0::eth0:off\"}"
api /drives/rootfs "{\"drive_id\":\"rootfs\",\"path_on_host\":\"$ROOTFS\",\"is_root_device\":true,\"is_read_only\":false}"
api /network-interfaces/eth0 '{"iface_id":"eth0","host_dev_name":"tap0","guest_mac":"06:00:AC:10:00:02"}'
api /machine-config '{"vcpu_count":2,"mem_size_mib":1024}'
api /actions '{"action_type":"InstanceStart"}'

echo "[fc-up] microVM booting at $GUEST_IP — wait ~5s then run ./fc-ssh.sh"
