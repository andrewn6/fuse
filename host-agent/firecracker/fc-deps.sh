#!/usr/bin/env bash
# Install everything a Fuse Firecracker host needs to run fc-install.sh,
# fc-bake-rootfs.sh, and fc-agent.py.
#
# Supports apt (Ubuntu/Debian) and dnf (Fedora/RHEL). Idempotent — safe to
# re-run. Run on the HOST that will run microVMs.
#
# Usage:
#   ./fc-deps.sh             # install runtime deps + check KVM
#   ./fc-deps.sh --with-go   # also install Go (to build fused on this host)
#
# Firecracker itself is fetched by fc-install.sh, not here.
set -euo pipefail

GO_VERSION="${GO_VERSION:-1.26.5}"   # keep in sync with go.mod
WITH_GO=0
[ "${1:-}" = "--with-go" ] && WITH_GO=1

log()  { printf '\033[1;36m[deps] %s\033[0m\n' "$*"; }
ok()   { printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; }
warn() { printf '\033[1;33m  ! %s\033[0m\n' "$*" >&2; }
die()  { printf '\033[1;31m  ✗ %s\033[0m\n' "$*" >&2; exit 1; }

# sudo wrapper (no-op if already root).
SUDO=""
if [ "$(id -u)" -ne 0 ]; then
  command -v sudo >/dev/null 2>&1 || die "need root or sudo"
  SUDO="sudo"
fi

ARCH="$(uname -m)"   # x86_64 / aarch64

# --- package install ---------------------------------------------------------
# Common tool set across distros; package names differ per manager.
if command -v apt-get >/dev/null 2>&1; then
  log "apt: installing host dependencies"
  export DEBIAN_FRONTEND=noninteractive
  $SUDO apt-get update -y
  $SUDO apt-get install -y --no-install-recommends \
    podman python3 curl ca-certificates tar \
    iptables nftables iproute2 e2fsprogs util-linux coreutils \
    openssh-client procps openssl
elif command -v dnf >/dev/null 2>&1; then
  log "dnf: installing host dependencies"
  $SUDO dnf install -y \
    podman python3 curl ca-certificates tar \
    iptables nftables iproute e2fsprogs util-linux coreutils \
    openssh-clients procps-ng openssl
else
  die "unsupported package manager (need apt-get or dnf); install manually: podman python3 curl tar iptables nftables iproute2 e2fsprogs util-linux openssh-client procps openssl"
fi
ok "packages installed"

# --- verify the tools the scripts actually shell out to ----------------------
log "verifying required tools"
missing=0
for t in podman python3 curl tar iptables nft ip e2fsck resize2fs truncate mount umount setsid ssh pgrep openssl awk; do
  if command -v "$t" >/dev/null 2>&1; then
    ok "$t"
  else
    warn "missing: $t"
    missing=1
  fi
done
[ "$missing" -eq 0 ] || die "some required tools are still missing (see above)"

# --- KVM (Firecracker requires hardware virtualization) ----------------------
log "checking KVM"
if [ ! -e /dev/kvm ]; then
  warn "/dev/kvm not present — this host cannot run Firecracker."
  warn "Use a bare-metal box or a VM/instance with nested virtualization enabled."
else
  ok "/dev/kvm present"
  if [ -r /dev/kvm ] && [ -w /dev/kvm ]; then
    ok "/dev/kvm is read/writable by $(id -un)"
  else
    warn "/dev/kvm not accessible by $(id -un); adding you to the 'kvm' group"
    getent group kvm >/dev/null 2>&1 || $SUDO groupadd kvm || true
    $SUDO usermod -aG kvm "$(id -un)" || warn "could not add to kvm group; run firecracker as root or fix /dev/kvm perms"
    warn "log out and back in (or run 'newgrp kvm') for the group change to take effect"
  fi
fi

# --- optional: Go toolchain (to build fused on this host) --------------------
if [ "$WITH_GO" -eq 1 ]; then
  log "installing Go ${GO_VERSION}"
  case "$ARCH" in
    x86_64)  GOARCH=amd64 ;;
    aarch64) GOARCH=arm64 ;;
    *) die "unsupported arch for Go install: $ARCH" ;;
  esac
  TARBALL="go${GO_VERSION}.linux-${GOARCH}.tar.gz"
  TMP="$(mktemp -d)"
  curl -fsSL -o "$TMP/$TARBALL" "https://go.dev/dl/$TARBALL"
  $SUDO rm -rf /usr/local/go
  $SUDO tar -C /usr/local -xzf "$TMP/$TARBALL"
  rm -rf "$TMP"
  if /usr/local/go/bin/go version >/dev/null 2>&1; then
    ok "$('/usr/local/go/bin/go' version)"
    warn "add Go to PATH:  export PATH=\$PATH:/usr/local/go/bin"
  else
    die "Go install verification failed"
  fi
fi

printf '\n\033[1;32m[deps] done.\033[0m next: ./fc-install.sh  (then ../shared/fc-build-agent.sh && ./fc-bake-rootfs.sh)\n'
