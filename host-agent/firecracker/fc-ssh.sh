#!/usr/bin/env bash
# SSH into the guest. Passes through any extra args as a remote command.
set -euo pipefail
FC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec ssh -i "$FC_DIR/ubuntu.id_rsa" \
  -o StrictHostKeyChecking=no \
  -o UserKnownHostsFile=/dev/null \
  -o LogLevel=ERROR \
  -o ConnectTimeout=5 \
  root@172.16.0.2 "$@"
