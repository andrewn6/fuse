#!/usr/bin/env bash
# Self-host auto-update: bring this Firecracker host to the latest Fuse code.
# Idempotent — a no-op when already current.
#
# Flow:
#   1. git pull the checkout (fc-agent.py, host-agent/ scripts, cmd/fused source)
#   2. obtain the `fused` agent binary: download the latest release asset if a
#      release is published, otherwise BUILD it from the pulled source (needs Go)
#   3. re-bake the guest rootfs so new microVMs run the new agent
#   4. restart the host agent
#   5. (optional) update + restart a co-located orchestrator service
#
# Run by the systemd timer (`./fc-agent.sh install-updater`) or by hand. Public
# repo — no token needed; set GH_TOKEN to avoid GitHub API rate limits.
#
# Env knobs:
#   FUSE_REPO          owner/name for releases (default: folsomintel/fuse)
#   GH_TOKEN           optional GitHub token (rate limits / private forks)
#   FC_FORCE=1         re-bake/restart even if already current
#   FC_SKIP_REBAKE=1   update the binary but don't re-bake/restart
#   FUSE_ORCH_SERVICE  systemd unit of a co-located orchestrator to also update
#   FUSE_ORCH_BIN      path to that orchestrator binary (required with the above)
set -euo pipefail

FC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="${FUSE_REPO:-folsomintel/fuse}"
MARKER="$FC_DIR/.fc-version"

log()  { printf '\033[1;36m[update] %s\033[0m\n' "$*"; }
ok()   { printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; }
warn() { printf '\033[1;33m  ! %s\033[0m\n' "$*" >&2; }
die()  { printf '\033[1;31m  ✗ %s\033[0m\n' "$*" >&2; exit 1; }

# GitHub API GET that does NOT hard-fail on 4xx (so "no releases yet" -> 404 is
# handled, not fatal). Echoes the body; returns curl's exit only on transport
# errors.
curl_api() {
  if [ -n "${GH_TOKEN:-}" ]; then
    curl -sSL -H "Authorization: Bearer $GH_TOKEN" "$@"
  else
    curl -sSL "$@"
  fi
}
# Asset download that DOES hard-fail on 4xx.
curl_dl() {
  if [ -n "${GH_TOKEN:-}" ]; then
    curl -fsSL -H "Authorization: Bearer $GH_TOKEN" "$@"
  else
    curl -fsSL "$@"
  fi
}

# >>> fuse release-asset checksum verification >>>
# This block is embedded, not sourced: scripts/install-orchestrator.sh is
# documented as curl-able and run standalone on a fresh host, so a sibling
# library file is not guaranteed to exist on disk. Keep the copies in
# host-agent/fc-update.sh, host-agent/fc-agent.sh and
# scripts/install-orchestrator.sh byte-identical;
# host-agent/test-verify-checksum.sh asserts that and exercises the helper.

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

# One temp dir for every download this run makes, removed on any exit so a
# rejected asset is never left lying around next to the real binary.
FC_TMP="$(mktemp -d)"
trap 'rm -rf "$FC_TMP"' EXIT
CHECKSUMS=""

# fetch_checksums <tag> - download checksums.txt for that exact release tag,
# once per run. Fatal if absent: without it nothing from the release can be
# verified, and installing unverified assets is what this guards against.
fetch_checksums() {
  if [ -n "$CHECKSUMS" ]; then return 0; fi
  local dest="$FC_TMP/checksums.txt"
  curl_dl -o "$dest" "https://github.com/$REPO/releases/download/$1/checksums.txt" \
    || die "release $1 publishes no checksums.txt - refusing to install unverified assets"
  CHECKSUMS="$dest"
}

case "$(uname -m)" in
  x86_64)        ASSET_ARCH="x86_64" ;;
  aarch64|arm64) ASSET_ARCH="arm64" ;;
  *) die "unsupported arch $(uname -m)" ;;
esac

# --- 1. update the checkout first (so we build/bake from the latest source) ---
REPO_ROOT="$FC_DIR/.."
if git -C "$REPO_ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  log "git pull"
  git -C "$REPO_ROOT" fetch --tags --quiet || warn "git fetch failed"
  if git -C "$REPO_ROOT" pull --ff-only --quiet; then ok "checkout updated"; else warn "git pull --ff-only failed (local changes?) — continuing with current source"; fi
fi

# --- 2. resolve the latest release tag (non-fatal: none yet => source build) --
log "checking latest release of $REPO"
RELEASE_JSON="$(curl_api "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null || true)"
LATEST="$(printf '%s' "$RELEASE_JSON" | grep -oE '"tag_name"[[:space:]]*:[[:space:]]*"[^"]+"' | head -n1 | sed -E 's/.*"([^"]+)"$/\1/')"

if [ -n "$LATEST" ]; then
  VERSION="$LATEST"
else
  GITSHA="$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"
  VERSION="src-$GITSHA"
  log "no published release for $REPO — will build fused from source ($VERSION)"
fi

# --- 3. idempotency: skip if already at this version and fused is present ------
INSTALLED="$(cat "$MARKER" 2>/dev/null || echo none)"
log "installed=$INSTALLED  target=$VERSION"
if [ "$INSTALLED" = "$VERSION" ] && [ -x "$FC_DIR/fused" ] && [ "${FC_FORCE:-0}" != "1" ]; then
  ok "already up to date (FC_FORCE=1 to re-apply)"
  exit 0
fi

# --- 4. obtain the fused binary -----------------------------------------------
if [ -n "$LATEST" ]; then
  ASSET="fused_Linux_${ASSET_ARCH}"
  log "downloading $ASSET ($LATEST)"
  TMP="$FC_TMP/$ASSET"
  if curl_dl -o "$TMP" "https://github.com/$REPO/releases/download/$LATEST/$ASSET"; then
    # Verify before chmod/exec/mv: a truncated or tampered asset must never
    # become executable or replace the installed agent.
    fetch_checksums "$LATEST"
    verify_asset "$TMP" "$ASSET" "$CHECKSUMS" || die "checksum verification failed for $ASSET ($LATEST)"
    ok "sha256 verified: $ASSET"
    chmod +x "$TMP"
    "$TMP" --version >/dev/null 2>&1 || die "downloaded fused is not runnable on this host (arch mismatch?)"
    mv "$TMP" "$FC_DIR/fused"
    ok "fused (release $LATEST) -> $("$FC_DIR/fused" --version 2>/dev/null || echo '?')"
  else
    warn "release $LATEST has no $ASSET asset — falling back to a source build"
    rm -f "$TMP"
    LATEST=""
  fi
fi
if [ -z "$LATEST" ]; then
  command -v go >/dev/null 2>&1 || die "no release published and Go is not installed — run ./fc-deps.sh --with-go (then re-run), or publish a release"
  log "building fused from source"
  "$FC_DIR/fc-build-agent.sh"
  ok "fused (source) -> $("$FC_DIR/fused" --version 2>/dev/null || echo '?')"
fi

if [ "${FC_SKIP_REBAKE:-0}" = "1" ]; then
  warn "FC_SKIP_REBAKE=1 — not re-baking/restarting"
else
  # --- 5. re-bake so new microVMs boot the new agent --------------------------
  log "re-baking rootfs"
  "$FC_DIR/fc-bake-rootfs.sh"
  ok "rootfs re-baked"

  # --- 6. restart the host agent (re-attaches running VMs) --------------------
  log "restarting fc-agent"
  if systemctl is-enabled fc-agent.service >/dev/null 2>&1; then
    sudo -n systemctl restart fc-agent.service
  else
    "$FC_DIR/fc-agent.sh" restart
  fi
  ok "fc-agent restarted"
fi

# --- 7. optional: update a co-located orchestrator service (release only) ------
if [ -n "${FUSE_ORCH_SERVICE:-}" ]; then
  [ -n "${FUSE_ORCH_BIN:-}" ] || die "FUSE_ORCH_SERVICE set but FUSE_ORCH_BIN is not"
  if [ -n "$LATEST" ]; then
    log "updating orchestrator -> $FUSE_ORCH_BIN"
    TARBALL="fuse_Linux_${ASSET_ARCH}.tar.gz"
    TGZ="$FC_TMP/$TARBALL"; EXDIR="$FC_TMP/orchestrator-extract"; mkdir -p "$EXDIR"
    curl_dl -o "$TGZ" "https://github.com/$REPO/releases/download/$LATEST/$TARBALL" || die "orchestrator download failed"
    # Verify before extract/install/restart: on failure die here, leaving the
    # running orchestrator binary and its service untouched.
    fetch_checksums "$LATEST"
    verify_asset "$TGZ" "$TARBALL" "$CHECKSUMS" || die "checksum verification failed for $TARBALL ($LATEST)"
    ok "sha256 verified: $TARBALL"
    tar -xzf "$TGZ" -C "$EXDIR" orchestrator
    sudo -n install -m 0755 "$EXDIR/orchestrator" "$FUSE_ORCH_BIN"
    sudo -n systemctl restart "$FUSE_ORCH_SERVICE"
    ok "orchestrator updated + restarted ($("$FUSE_ORCH_BIN" --version 2>/dev/null || echo '?'))"
  elif command -v go >/dev/null 2>&1; then
    log "building orchestrator from source -> $FUSE_ORCH_BIN"
    CGO_ENABLED=0 go -C "$REPO_ROOT" build -ldflags='-s -w' -o "$FC_DIR/.orchestrator.new" ./cmd/orchestrator
    sudo -n install -m 0755 "$FC_DIR/.orchestrator.new" "$FUSE_ORCH_BIN"; rm -f "$FC_DIR/.orchestrator.new"
    sudo -n systemctl restart "$FUSE_ORCH_SERVICE"
    ok "orchestrator (source) updated + restarted"
  else
    warn "skipping orchestrator update (no release and no Go)"
  fi
fi

# --- 8. record the applied version --------------------------------------------
printf '%s\n' "$VERSION" > "$MARKER"
printf '\n\033[1;32m[update] applied %s -> %s\033[0m\n' "$INSTALLED" "$VERSION"
