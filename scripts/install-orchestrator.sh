#!/usr/bin/env bash
# install-orchestrator.sh - install and start the Fuse orchestrator (control
# plane) on a dedicated host, so bring-up is one script instead of
# clone-and-build.
#
# what it does:
#   1. installs the orchestrator binary to /usr/local/bin/orchestrator
#      (from ORCH_BIN_SRC, a local ./orchestrator, or the latest release)
#   2. writes /etc/fuse/orchestrator.env with a generated ORCH_AUTH_TOKEN and
#      TOKEN_ENCRYPTION_KEY (only if the file does not already exist)
#   3. installs the systemd unit and starts it
#   4. prints the exact `fuse connect` command to run next
#
# default posture is a single-node, in-memory store with auth ON: it starts
# immediately and is safe to reach only from trusted networks. for a durable,
# production deploy set DATABASE_URL (Postgres) in the env file and add
# ORCH_REQUIRE_AUTH=true, then restart. for an orchestrator co-located with a
# firecracker host agent, use `host-agent/fc-agent.sh install-orchestrator`
# instead, which wires FIRECRACKER_BASE_URL to the local agent.
#
# usage:
#   sudo ./install-orchestrator.sh
#   ORCH_BIN_SRC=/path/to/orchestrator sudo -E ./install-orchestrator.sh
#   FUSE_REPO=folsomintel/fuse VERSION=v0.4.0 sudo -E ./install-orchestrator.sh
set -euo pipefail

BIN=/usr/local/bin/orchestrator
ENV_DIR=/etc/fuse
ENV_FILE="$ENV_DIR/orchestrator.env"
UNIT=/etc/systemd/system/fuse-orchestrator.service
REPO="${FUSE_REPO:-folsomintel/fuse}"
LISTEN="${ORCH_LISTEN:-:8080}"

log() { echo "[install-orchestrator] $*"; }
die() { echo "[install-orchestrator] error: $*" >&2; exit 1; }

# SHA-256 verification for release downloads. Mandatory: this script installs
# to /usr/local/bin as root, so an unverified archive is the worst thing it
# could extract. A missing library is a hard failure, not a skipped check.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/release-verify.sh
. "$SCRIPT_DIR/lib/release-verify.sh" || die "cannot source $SCRIPT_DIR/lib/release-verify.sh"

[ "$(id -u)" -eq 0 ] || die "run as root (sudo $0)"
command -v openssl >/dev/null 2>&1 || die "openssl is required (token generation)"
command -v systemctl >/dev/null 2>&1 || die "systemctl is required (this installer targets systemd hosts)"

# 1. resolve and install the binary.
case "$(uname -m)" in
  x86_64)        ASSET_ARCH="x86_64" ;;
  aarch64|arm64) ASSET_ARCH="arm64" ;;
  *) die "unsupported arch $(uname -m); set ORCH_BIN_SRC=/path/to/orchestrator" ;;
esac

if [ -n "${ORCH_BIN_SRC:-}" ] && [ -x "$ORCH_BIN_SRC" ]; then
  install -m0755 "$ORCH_BIN_SRC" "$BIN"
  log "installed binary from $ORCH_BIN_SRC"
elif [ -x "./orchestrator" ]; then
  install -m0755 "./orchestrator" "$BIN"
  log "installed binary from ./orchestrator"
else
  command -v curl >/dev/null 2>&1 || die "curl is required to download a release (or set ORCH_BIN_SRC)"
  TAG="${VERSION:-}"
  if [ -z "$TAG" ]; then
    TAG=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
      | grep -oE '"tag_name"[[:space:]]*:[[:space:]]*"[^"]+"' | head -n1 \
      | sed -E 's/.*"([^"]+)"$/\1/')
    [ -n "$TAG" ] || die "could not resolve latest release for $REPO; set VERSION or ORCH_BIN_SRC"
  fi
  TARBALL="fuse_Linux_${ASSET_ARCH}.tar.gz"
  TGZ=$(mktemp); EXDIR=$(mktemp -d); SUMS=$(mktemp)
  trap 'rm -rf "$TGZ" "$EXDIR" "$SUMS"' EXIT
  log "downloading $TARBALL ($TAG)"
  curl -fsSL -o "$TGZ" "https://github.com/$REPO/releases/download/$TAG/$TARBALL" \
    || die "download failed for $TAG ($TARBALL)"
  # Verify before extracting or installing: this runs as root, so a corrupted
  # or mismatched archive must never reach $BIN.
  fuse_fetch_checksums "$REPO" "$TAG" "$SUMS" \
    || die "no checksums.txt for release $TAG — refusing to install $TARBALL"
  fuse_verify_asset "$TGZ" "$TARBALL" "$SUMS" \
    || die "checksum verification failed for $TARBALL ($TAG)"
  log "verified $TARBALL against checksums.txt"
  tar -xzf "$TGZ" -C "$EXDIR" orchestrator
  install -m0755 "$EXDIR/orchestrator" "$BIN"
  log "installed binary from release $TAG"
fi
log "binary -> $BIN ($("$BIN" --version 2>/dev/null || echo '?'))"

# 2. write the env file, generating secrets, only if it does not exist so a
#    re-run never clobbers an operator's edits or rotates their tokens.
mkdir -p "$ENV_DIR"
if [ ! -f "$ENV_FILE" ]; then
  AUTH_TOKEN=$(openssl rand -hex 32)
  ENC_KEY=$(openssl rand -hex 32)
  (umask 077 && cat > "$ENV_FILE" <<EOF
# /etc/fuse/orchestrator.env - Fuse orchestrator config. Edit, then:
#   sudo systemctl restart fuse-orchestrator
ORCH_LISTEN=$LISTEN

# Auth: any non-empty ORCH_AUTH_TOKEN turns auth on. This is the token you
# pass to \`fuse connect --token\`. Keep it secret.
ORCH_AUTH_TOKEN=$AUTH_TOKEN

# Encrypts per-host agent tokens at rest (used once DATABASE_URL is set).
TOKEN_ENCRYPTION_KEY=$ENC_KEY

# State: empty = in-memory (does NOT survive a restart). For production set a
# Postgres URL and uncomment ORCH_REQUIRE_AUTH to fail closed on misconfig.
DATABASE_URL=
# ORCH_REQUIRE_AUTH=true

# Optional: restrict which source networks may reach the API.
# ORCH_ALLOWED_CIDRS=10.0.0.0/8,100.64.0.0/10
EOF
  )
  chmod 600 "$ENV_FILE"
  log "wrote $ENV_FILE (auth token + encryption key generated)"
else
  # shellcheck disable=SC1090
  AUTH_TOKEN=$(grep -E '^ORCH_AUTH_TOKEN=' "$ENV_FILE" | head -n1 | cut -d= -f2- || true)
  log "$ENV_FILE exists - leaving it untouched"
fi

# 3. install the systemd unit (prefer the tracked deploy/ copy, fall back to
#    an inline definition so a curl'd standalone script still works).
if [ -f "$SCRIPT_DIR/../deploy/fuse-orchestrator.service" ]; then
  install -m0644 "$SCRIPT_DIR/../deploy/fuse-orchestrator.service" "$UNIT"
else
  cat > "$UNIT" <<EOF
[Unit]
Description=Fuse orchestrator (control plane)
Documentation=https://github.com/$REPO
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=$ENV_FILE
ExecStart=$BIN
Restart=on-failure
RestartSec=2
NoNewPrivileges=true
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF
fi
log "installed unit -> $UNIT"

systemctl daemon-reload
systemctl enable --now fuse-orchestrator.service
sleep 1
systemctl --no-pager --lines=0 status fuse-orchestrator.service || true

# 4. print the connect command so the operator never has to guess which token
#    goes where.
IP=$(curl -fsS ifconfig.me 2>/dev/null || hostname -I 2>/dev/null | awk '{print $1}' || echo "<this-host>")
PORT="${LISTEN##*:}"
echo
log "orchestrator installed and listening on $LISTEN"
echo
echo "  fuse connect http://${IP}:${PORT} --token ${AUTH_TOKEN:-<see $ENV_FILE>} --master"
echo
