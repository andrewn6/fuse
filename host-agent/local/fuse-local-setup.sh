#!/usr/bin/env bash
# fuse-local-setup.sh: install and run the fuse local dev stack on a linux
# machine (a dev workstation with /dev/kvm, or the appliance VM `fuse local`
# boots on macOS). embedded in the fuse cli and written to the state dir by
# `fuse local up`, so it is never fetched over the network.
#
# the stack is the real production one: the orchestrator binary from a fuse
# release plus fc-agent.py driving firecracker microvms. nothing here is a
# simulator.
#
# subcommands: install | start | stop | status
#
# env (all consumed at install/start time):
#   FUSE_LOCAL_DIR    state dir (required). layout:
#                       bin/{firecracker,orchestrator}  fc/{vmlinux.bin,rootfs.ext4,ubuntu.id_rsa}
#                       fc-agent.py  *.log  *.pid  env
#   FC_AGENT_TOKEN    bearer token the orchestrator uses to call fc-agent (required by install)
#   ORCH_AUTH_TOKEN   orchestrator master token (required by install)
#   FUSE_VERSION      fuse release tag for orchestrator/fused assets (default: latest)
#   ORCH_PORT         orchestrator listen port (default 8080)
#   FC_AGENT_PORT     fc-agent listen port (default 8090)
#   PUBLIC_HOST       address vm urls are published under. "auto" resolves the
#                     primary interface ip at start time (the appliance case);
#                     default 127.0.0.1 (the same-box dev case).
set -euo pipefail

DIR="${FUSE_LOCAL_DIR:?FUSE_LOCAL_DIR is required}"
ORCH_PORT="${ORCH_PORT:-8080}"
FC_AGENT_PORT="${FC_AGENT_PORT:-8090}"
FUSE_VERSION="${FUSE_VERSION:-latest}"
FC_CI_VERSION="v1.10"
FC_KERNEL="vmlinux-5.10.223"

case "$(uname -m)" in
  x86_64)        ARCH="x86_64";  ASSET_ARCH="x86_64" ;;
  aarch64|arm64) ARCH="aarch64"; ASSET_ARCH="arm64" ;;
  *) echo "[local] unsupported arch $(uname -m)" >&2; exit 1 ;;
esac
CI_BASE="https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/${FC_CI_VERSION}/${ARCH}"

if [ "$FUSE_VERSION" = "latest" ]; then
  REL_BASE="https://github.com/folsomintel/fuse/releases/latest/download"
else
  REL_BASE="https://github.com/folsomintel/fuse/releases/download/${FUSE_VERSION}"
fi

# >>> fuse release-asset checksum verification >>>
# This block is embedded, not sourced: ops/install-orchestrator.sh is
# documented as curl-able and run standalone on a fresh host, so a sibling
# library file is not guaranteed to exist on disk. Keep the copies in
# host-agent/firecracker/fc-update.sh, host-agent/firecracker/fc-agent.sh,
# host-agent/local/fuse-local-setup.sh and ops/install-orchestrator.sh
# byte-identical;
# host-agent/firecracker/test-verify-checksum.sh asserts that and exercises the helper.

# sha256_of <file> - print the file's lowercase sha256, or fail if no tool.
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | cut -d' ' -f1
  else
    return 1
  fi
}

# verify_asset <file> <asset-name> <checksums-file>
# Fails closed. A missing checksum tool, a missing or empty checksums file, a
# missing entry for this exact asset name, or a digest mismatch all return
# non-zero, so the caller must not install, extract, or execute the asset.
verify_asset() {
  local file="$1" name="$2" sums="$3" want="" got="" sum path
  if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
    echo "checksum verification needs sha256sum or shasum; refusing $name" >&2
    return 1
  fi
  if [ ! -f "$file" ]; then
    echo "asset $name is missing; nothing to verify" >&2
    return 1
  fi
  if [ ! -s "$sums" ]; then
    echo "checksums.txt is missing or empty; refusing $name" >&2
    return 1
  fi
  # goreleaser writes "<sha256>  <asset>"; binary-mode tools prefix the name "*".
  while read -r sum path; do
    case "$path" in
      "$name"|"*$name") want="$sum"; break ;;
    esac
  done < "$sums"
  if [ -z "$want" ]; then
    echo "no checksum entry for $name in checksums.txt; refusing it" >&2
    return 1
  fi
  got="$(sha256_of "$file")" || return 1
  if [ "$want" != "$got" ]; then
    echo "checksum mismatch for $name: want $want, got $got" >&2
    return 1
  fi
}
# <<< fuse release-asset checksum verification <<<

log() { echo "[local] $*"; }
die() { echo "[local] $*" >&2; exit 1; }

# resolve_public_host: PUBLIC_HOST env as-is, unless "auto", which picks the
# ip of the interface that routes to the internet (the appliance's eth0).
resolve_public_host() {
  local ph="${PUBLIC_HOST:-127.0.0.1}"
  if [ "$ph" = "auto" ]; then
    ph=$(ip -o route get 8.8.8.8 2>/dev/null | sed -n 's/.*src \([0-9.]*\).*/\1/p')
    [ -n "$ph" ] || die "PUBLIC_HOST=auto but no route to resolve an address from"
  fi
  echo "$ph"
}

fetch() { # fetch <url> <dest>
  log "fetch $1"
  curl -fSL --retry 3 -o "$2.part" "$1" && mv "$2.part" "$2"
}

install_stack() {
  : "${FC_AGENT_TOKEN:?FC_AGENT_TOKEN is required}"
  : "${ORCH_AUTH_TOKEN:?ORCH_AUTH_TOKEN is required}"
  command -v python3 >/dev/null 2>&1 || die "python3 is required (fc-agent.py runs on it)"
  command -v curl >/dev/null 2>&1 || die "curl is required"
  [ -e /dev/kvm ] || die "/dev/kvm not present: firecracker needs kvm (on macOS this script runs inside the appliance VM, which needs nested virtualization: M3 or later + macOS 15+)"
  [ -f "$DIR/fc-agent.py" ] || die "$DIR/fc-agent.py missing: fuse local writes it before calling install"

  mkdir -p "$DIR/bin" "$DIR/fc"

  # firecracker binary, from its upstream release.
  if [ ! -x "$DIR/bin/firecracker" ]; then
    local tag tmp
    tag=$(curl -fsSL https://api.github.com/repos/firecracker-microvm/firecracker/releases/latest | grep '"tag_name"' | cut -d '"' -f4)
    [ -n "$tag" ] || die "could not resolve latest firecracker release tag"
    tmp=$(mktemp -d)
    curl -fSL "https://github.com/firecracker-microvm/firecracker/releases/download/${tag}/firecracker-${tag}-${ARCH}.tgz" | tar -xz -C "$tmp"
    install -m0755 "$tmp/release-${tag}-${ARCH}/firecracker-${tag}-${ARCH}" "$DIR/bin/firecracker"
    rm -rf "$tmp"
    log "installed firecracker $tag"
  fi

  # guest kernel + base rootfs + guest ssh key, from the firecracker ci bucket.
  [ -f "$DIR/fc/vmlinux.bin" ] || fetch "$CI_BASE/$FC_KERNEL" "$DIR/fc/vmlinux.bin"
  [ -f "$DIR/fc/rootfs.ext4" ] || fetch "$CI_BASE/ubuntu-22.04.ext4" "$DIR/fc/rootfs.ext4"
  if [ ! -f "$DIR/fc/ubuntu.id_rsa" ]; then
    fetch "$CI_BASE/ubuntu-22.04.id_rsa" "$DIR/fc/ubuntu.id_rsa"
    chmod 600 "$DIR/fc/ubuntu.id_rsa"
  fi

  # orchestrator from the fuse release archive, checksum-verified. a binary
  # already at bin/orchestrator wins (that's the dev-build injection path).
  if [ ! -x "$DIR/bin/orchestrator" ]; then
    local tmp
    tmp=$(mktemp -d)
    fetch "$REL_BASE/checksums.txt" "$tmp/checksums.txt"
    fetch "$REL_BASE/fuse_Linux_${ASSET_ARCH}.tar.gz" "$tmp/fuse.tar.gz"
    verify_asset "$tmp/fuse.tar.gz" "fuse_Linux_${ASSET_ARCH}.tar.gz" "$tmp/checksums.txt" || die "orchestrator asset failed verification"
    tar -xzf "$tmp/fuse.tar.gz" -C "$tmp" orchestrator
    install -m0755 "$tmp/orchestrator" "$DIR/bin/orchestrator"
    rm -rf "$tmp"
    log "installed orchestrator ($FUSE_VERSION)"
  fi

  # fused, injected straight into the base rootfs so guests need no network
  # fetch at boot (the ci rootfs has no dns configured). marker records which
  # fused is inside so re-running install is a no-op until the asset changes.
  local fused_tmp fused_sum
  fused_tmp=$(mktemp -d)
  if [ ! -f "$DIR/fc/.fused-injected" ] || [ ! -s "$DIR/fc/.fused-injected" ]; then
    fetch "$REL_BASE/checksums.txt" "$fused_tmp/checksums.txt"
    fetch "$REL_BASE/fused_Linux_${ASSET_ARCH}" "$fused_tmp/fused"
    verify_asset "$fused_tmp/fused" "fused_Linux_${ASSET_ARCH}" "$fused_tmp/checksums.txt" || die "fused asset failed verification"
    fused_sum=$(sha256_of "$fused_tmp/fused")
    local mnt
    mnt=$(mktemp -d)
    sudo -n mount -o loop "$DIR/fc/rootfs.ext4" "$mnt" || die "loop-mounting the base rootfs needs passwordless sudo (or run install as root)"
    sudo -n install -m0755 "$fused_tmp/fused" "$mnt/usr/local/bin/fused"
    # the ci rootfs boots with a static ip= config and no resolver; give
    # guests dns so setup scripts (apt, git clone) work out of the box.
    echo "nameserver 1.1.1.1" | sudo -n tee "$mnt/etc/resolv.conf" >/dev/null
    sudo -n umount "$mnt"
    rmdir "$mnt"
    echo "$fused_sum" > "$DIR/fc/.fused-injected"
    log "injected fused into base rootfs"
  fi
  rm -rf "$fused_tmp"

  log "install complete"
}

start_stack() {
  [ -x "$DIR/bin/orchestrator" ] || die "not installed: run install first"
  local public_host
  public_host=$(resolve_public_host)

  sudo -n sysctl -w net.ipv4.ip_forward=1 >/dev/null || die "enabling ip_forward needs passwordless sudo (or run start as root)"

  if [ -f "$DIR/orchestrator.pid" ] && kill -0 "$(cat "$DIR/orchestrator.pid")" 2>/dev/null; then
    log "orchestrator already running (pid $(cat "$DIR/orchestrator.pid"))"
  else
    ORCH_AUTH_TOKEN="${ORCH_AUTH_TOKEN:?ORCH_AUTH_TOKEN is required}" \
    ORCH_LISTEN=":${ORCH_PORT}" \
      nohup "$DIR/bin/orchestrator" >"$DIR/orchestrator.log" 2>&1 &
    echo $! > "$DIR/orchestrator.pid"
    log "orchestrator started on :$ORCH_PORT (pid $!)"
  fi

  if [ -f "$DIR/fc-agent.pid" ] && kill -0 "$(cat "$DIR/fc-agent.pid")" 2>/dev/null; then
    log "fc-agent already running (pid $(cat "$DIR/fc-agent.pid"))"
  else
    FC_AGENT_TOKEN="${FC_AGENT_TOKEN:?FC_AGENT_TOKEN is required}" \
    FC_AGENT_PORT="$FC_AGENT_PORT" \
    FC_DIR="$DIR/fc" \
    FC_BIN="$DIR/bin/firecracker" \
    BASE_ROOTFS="$DIR/fc/rootfs.ext4" \
    PUBLIC_HOST="$public_host" \
      nohup python3 "$DIR/fc-agent.py" >"$DIR/fc-agent.log" 2>&1 &
    echo $! > "$DIR/fc-agent.pid"
    log "fc-agent started on :$FC_AGENT_PORT (pid $!)"
  fi
}

stop_stack() {
  local name
  for name in fc-agent orchestrator; do
    if [ -f "$DIR/$name.pid" ]; then
      kill "$(cat "$DIR/$name.pid")" 2>/dev/null && log "stopped $name" || true
      rm -f "$DIR/$name.pid"
    fi
  done
}

status_stack() {
  local name up=0
  for name in orchestrator fc-agent; do
    if [ -f "$DIR/$name.pid" ] && kill -0 "$(cat "$DIR/$name.pid")" 2>/dev/null; then
      echo "$name: running (pid $(cat "$DIR/$name.pid"))"
      up=$((up+1))
    else
      echo "$name: stopped"
    fi
  done
  [ "$up" = 2 ]
}

case "${1:-}" in
  install) install_stack ;;
  start)   start_stack ;;
  stop)    stop_stack ;;
  status)  status_stack ;;
  *) die "usage: fuse-local-setup.sh install|start|stop|status" ;;
esac
