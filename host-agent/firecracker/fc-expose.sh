#!/usr/bin/env bash
# Expose a guest TCP port to the outside world.
# Usage: ./fc-expose.sh <host_port> <guest_ip> <guest_port>    (add rule)
#        ./fc-expose.sh -d <host_port> <guest_ip> <guest_port> (remove rule)
#
# Idempotent: each rule is added only if not already present (checked via
# iptables -C) and removed only if present, so re-running this script for
# the same port is always safe. guest_ip is per-VM (see fc-agent.py's
# setup_tap, which allocates 10.200.<idx>.2 per VM) -- unlike an earlier
# version of this script there is no hardcoded guest ip. Mirrors the rule
# set fc-agent.py's own add_agent_forward uses for the fixed agent port.
set -euo pipefail
HOST_IFACE=$(ip -o route get 8.8.8.8 | awk '{print $5}')

MODE=add
if [ "${1:-}" = "-d" ]; then MODE=del; shift; fi

HOST_PORT="${1:?host port required}"
GUEST_IP="${2:?guest ip required}"
GUEST_PORT="${3:?guest port required}"

# prerouting dnat: <host>:host_port -> guest_ip:guest_port
if sudo iptables -t nat -C PREROUTING -i "$HOST_IFACE" -p tcp --dport "$HOST_PORT" -j DNAT --to-destination "$GUEST_IP:$GUEST_PORT" 2>/dev/null; then
  [ "$MODE" = del ] && sudo iptables -t nat -D PREROUTING -i "$HOST_IFACE" -p tcp --dport "$HOST_PORT" -j DNAT --to-destination "$GUEST_IP:$GUEST_PORT"
else
  [ "$MODE" = add ] && sudo iptables -t nat -I PREROUTING -i "$HOST_IFACE" -p tcp --dport "$HOST_PORT" -j DNAT --to-destination "$GUEST_IP:$GUEST_PORT"
fi

# loopback dnat, so 127.0.0.1:host_port also reaches the guest
if sudo iptables -t nat -C OUTPUT -o lo -p tcp --dport "$HOST_PORT" -j DNAT --to-destination "$GUEST_IP:$GUEST_PORT" 2>/dev/null; then
  [ "$MODE" = del ] && sudo iptables -t nat -D OUTPUT -o lo -p tcp --dport "$HOST_PORT" -j DNAT --to-destination "$GUEST_IP:$GUEST_PORT"
else
  [ "$MODE" = add ] && sudo iptables -t nat -I OUTPUT -o lo -p tcp --dport "$HOST_PORT" -j DNAT --to-destination "$GUEST_IP:$GUEST_PORT"
fi

# forward: allow the dnat'd traffic through to the guest
if sudo iptables -C FORWARD -p tcp -d "$GUEST_IP" --dport "$GUEST_PORT" -j ACCEPT 2>/dev/null; then
  [ "$MODE" = del ] && sudo iptables -D FORWARD -p tcp -d "$GUEST_IP" --dport "$GUEST_PORT" -j ACCEPT
else
  [ "$MODE" = add ] && sudo iptables -I FORWARD -p tcp -d "$GUEST_IP" --dport "$GUEST_PORT" -j ACCEPT
fi

# input: allow the host port itself (packets destined for the host before dnat rewrites them)
if sudo iptables -C INPUT -p tcp --dport "$HOST_PORT" -j ACCEPT 2>/dev/null; then
  [ "$MODE" = del ] && sudo iptables -D INPUT -p tcp --dport "$HOST_PORT" -j ACCEPT
else
  [ "$MODE" = add ] && sudo iptables -I INPUT -p tcp --dport "$HOST_PORT" -j ACCEPT
fi

if [ "$MODE" = del ]; then
  echo "[expose] removed forward for ${GUEST_IP}:${GUEST_PORT} (host port ${HOST_PORT})"
else
  echo "[expose] ${HOST_PORT} -> ${GUEST_IP}:${GUEST_PORT}"
fi
