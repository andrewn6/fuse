#!/usr/bin/env bash
# Build the reference in-guest agent (fused) for baking into the rootfs.
# Shared by both backends: the Firecracker and QEMU bake scripts each expect
# a `fused` binary sitting next to them (host-agent/firecracker/fused or
# host-agent/qemu/fused).
#
# Produces a static linux/amd64 binary at <dest-dir>/fused, where <dest-dir>
# is the first argument (default: the current directory). Run it from the
# backend directory you're baking for:
#   cd host-agent/firecracker && ../shared/fc-build-agent.sh
#   cd host-agent/qemu        && ../shared/fc-build-agent.sh
#
# Run this on any machine with Go before the bake step. To bake a different
# agent instead, drop your own `fused` binary + `fused.service` into the
# backend dir and skip this.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
DEST_DIR="$(cd "${1:-.}" && pwd)"
OUT="$DEST_DIR/fused"

if ! command -v go >/dev/null 2>&1; then
  cat >&2 <<EOF
[build-agent] Go not found.
  fused is a static linux/amd64 binary, so you can build it anywhere:
   • on a machine that has Go:   ../shared/fc-build-agent.sh && scp fused <host>:~/fuse/host-agent/<backend>/
   • or install Go on this host: ../firecracker/fc-deps.sh --with-go && export PATH=\$PATH:/usr/local/go/bin
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
echo "[build-agent] done: $(ls -lh "$OUT" | awk '{print $5}') static binary"
echo "[build-agent] next: run the bake script in $DEST_DIR"
