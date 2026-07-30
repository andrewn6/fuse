#!/usr/bin/env bash
# Build the reference in-guest agent (fused) for baking into the rootfs.
#
# Produces a static linux/amd64 binary at host-agent/fused (the input fc-bake-rootfs.sh
# expects). The systemd unit host-agent/fused.service is committed alongside it.
#
# Run this on any machine with Go before ./fc-bake-rootfs.sh. To bake a different
# agent instead, drop your own `fused` binary + `fused.service` here and skip this.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="$REPO_ROOT/host-agent/fused"

if ! command -v go >/dev/null 2>&1; then
  cat >&2 <<'EOF'
[build-agent] Go not found.
  fused is a static linux/amd64 binary, so you can build it anywhere:
   • on a machine that has Go:   ./fc-build-agent.sh && scp host-agent/fused <host>:~/fuse/host-agent/
   • or install Go on this host: ./fc-deps.sh --with-go && export PATH=$PATH:/usr/local/go/bin
EOF
  exit 1
fi

echo "[build-agent] building reference agent -> $OUT (linux/amd64, static)"
# go build -o refuses to overwrite an existing file it did not produce ("already
# exists and is not an object file"), which wedges every rebuild once a stale or
# hand-dropped fused is sitting here. clear it first.
rm -f "$OUT"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go -C "$REPO_ROOT" build \
  -ldflags='-s -w' -o "$OUT" ./fused

chmod 0755 "$OUT"
echo "[build-agent] done: $(cd "$REPO_ROOT/host-agent" && ls -lh fused | awk '{print $5}') static binary"
echo "[build-agent] next: ./fc-bake-rootfs.sh"
