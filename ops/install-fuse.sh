#!/usr/bin/env bash
# install-fuse.sh - install the `fuse` operator CLI from a GitHub release.
#
# The point of this script, beyond convenience: curl does not set the
# com.apple.quarantine extended attribute, and Homebrew casks do. A CLI
# installed this way is never quarantined, so macOS Gatekeeper never runs its
# first-launch check on it and never reports the binary as unverifiable. That
# makes this the path that works on macOS without an Apple Developer ID
# signature or notarization.
#
# The release archive is verified against the release's own checksums.txt
# before anything is extracted or installed.
#
# usage:
#   curl -fsSL https://raw.githubusercontent.com/folsomintel/fuse/main/ops/install-fuse.sh | bash
#   ./install-fuse.sh
#
# env knobs:
#   VERSION           release tag to install (default: latest, e.g. v0.9.1)
#   FUSE_INSTALL_DIR  where to put the binary (default: /usr/local/bin if
#                     writable, else $HOME/.local/bin)
#   FUSE_REPO         owner/name to install from (default: folsomintel/fuse)
#   GH_TOKEN          optional GitHub token (API rate limits / private forks)
set -euo pipefail

REPO="${FUSE_REPO:-folsomintel/fuse}"

log()  { printf '[install-fuse] %s\n' "$*"; }
die()  { printf '[install-fuse] error: %s\n' "$*" >&2; exit 1; }

# >>> fuse release-asset checksum verification >>>
# Embedded rather than sourced: this script is meant to be piped from curl on a
# machine with no checkout, so a sibling library file is not guaranteed to be
# on disk. The same helper is embedded in host-agent/firecracker/fc-update.sh,
# host-agent/firecracker/fc-agent.sh, and ops/install-orchestrator.sh; keep the bodies
# in step.

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

curl_dl() {
  if [ -n "${GH_TOKEN:-}" ]; then
    curl -fsSL -H "Authorization: Bearer $GH_TOKEN" "$@"
  else
    curl -fsSL "$@"
  fi
}

command -v curl >/dev/null 2>&1 || die "curl is required"
command -v tar  >/dev/null 2>&1 || die "tar is required"

# 1. resolve platform. These must match .goreleaser.yaml's `cli` archive
#    name_template, which titlecases the OS and rewrites amd64 -> x86_64.
case "$(uname -s)" in
  Darwin) ASSET_OS="Darwin" ;;
  Linux)  ASSET_OS="Linux" ;;
  *) die "unsupported OS $(uname -s); build from source with: go build ./cli" ;;
esac
case "$(uname -m)" in
  x86_64|amd64)  ASSET_ARCH="x86_64" ;;
  aarch64|arm64) ASSET_ARCH="arm64" ;;
  i386|i686)     ASSET_ARCH="i386" ;;
  *) die "unsupported arch $(uname -m); build from source with: go build ./cli" ;;
esac

# 2. resolve the release tag.
TAG="${VERSION:-}"
if [ -z "$TAG" ]; then
  TAG=$(curl_dl "https://api.github.com/repos/$REPO/releases/latest" \
    | grep -oE '"tag_name"[[:space:]]*:[[:space:]]*"[^"]+"' | head -n1 \
    | sed -E 's/.*"([^"]+)"$/\1/') || true
  [ -n "$TAG" ] || die "could not resolve the latest release for $REPO; set VERSION=vX.Y.Z"
fi

# 3. pick an install dir. Prefer /usr/local/bin, fall back to ~/.local/bin so a
#    non-root user gets a working install instead of a permission error.
if [ -n "${FUSE_INSTALL_DIR:-}" ]; then
  INSTALL_DIR="$FUSE_INSTALL_DIR"
elif [ -w /usr/local/bin ] 2>/dev/null; then
  INSTALL_DIR=/usr/local/bin
else
  INSTALL_DIR="$HOME/.local/bin"
fi
mkdir -p "$INSTALL_DIR" || die "cannot create $INSTALL_DIR; set FUSE_INSTALL_DIR"

ASSET="fuse-cli_${ASSET_OS}_${ASSET_ARCH}.tar.gz"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# 4. download the archive and the checksums for this exact tag.
log "downloading $ASSET ($TAG)"
curl_dl -o "$TMP/$ASSET" "https://github.com/$REPO/releases/download/$TAG/$ASSET" \
  || die "download failed for $TAG ($ASSET)"
curl_dl -o "$TMP/checksums.txt" "https://github.com/$REPO/releases/download/$TAG/checksums.txt" \
  || die "release $TAG publishes no checksums.txt - refusing to install unverified assets"

# 5. verify before extracting or installing.
verify_asset "$TMP/$ASSET" "$ASSET" "$TMP/checksums.txt" \
  || die "checksum verification failed for $ASSET ($TAG)"
log "sha256 verified: $ASSET"

tar -xzf "$TMP/$ASSET" -C "$TMP" fuse || die "archive $ASSET has no 'fuse' binary"
install -m0755 "$TMP/fuse" "$INSTALL_DIR/fuse" \
  || die "could not install to $INSTALL_DIR; set FUSE_INSTALL_DIR to a writable path"

log "installed $("$INSTALL_DIR/fuse" --version 2>/dev/null || echo "$TAG") -> $INSTALL_DIR/fuse"

# 6. tell the user if the install dir is not on PATH, since a silent
#    "command not found" after a successful install is a confusing place to end.
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *)
    echo
    log "$INSTALL_DIR is not on your PATH. Add it:"
    echo "    export PATH=\"$INSTALL_DIR:\$PATH\""
    ;;
esac

echo
log "next: point the CLI at an orchestrator"
echo "    fuse connect http://<orchestrator>:8080 --token <token>"
