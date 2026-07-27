#!/usr/bin/env bash
# release-verify.sh - SHA-256 verification for Fuse release assets.
#
# Sourced (not executed) by the installer/updater scripts so every path that
# downloads a GoReleaser asset checks it the same way before that asset is
# made executable, extracted, run, installed, or used to restart a service.
#
# HTTPS already authenticates the transport. This catches what it does not:
# a truncated or corrupted download, an asset that belongs to a different
# release than the tag we resolved, or a mismatch between checksums.txt and
# the artifact next to it. All three would otherwise replace a running binary.
#
# Every function fails closed: a missing checksums.txt, a missing entry for the
# asset, or no available SHA-256 tool is an error, never a skipped check.
#
# Usage:
#   . "$REPO_ROOT/scripts/lib/release-verify.sh"
#   SUMS="$(mktemp)"
#   fuse_fetch_checksums "$REPO" "$TAG" "$SUMS" || die "..."
#   fuse_verify_asset "$TMPFILE" "fused_Linux_x86_64" "$SUMS" || die "..."

# fuse_sha256 <file> - print the file's lowercase hex SHA-256.
# Prefers coreutils sha256sum (every supported Linux host); shasum is the
# fallback for hosts that ship perl's version instead.
fuse_sha256() {
  local file="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$file" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$file" | awk '{print $1}'
  else
    echo "release-verify: no sha256sum or shasum available; cannot verify downloads" >&2
    return 1
  fi
}

# fuse_fetch_checksums <repo> <tag> <dest> - download checksums.txt for exactly
# this tag. Uses GH_TOKEN when set, matching the asset downloads.
fuse_fetch_checksums() {
  local repo="$1" tag="$2" dest="$3"
  local url="https://github.com/$repo/releases/download/$tag/checksums.txt"

  if [ -n "${GH_TOKEN:-}" ]; then
    curl -fsSL -H "Authorization: Bearer $GH_TOKEN" -o "$dest" "$url" || {
      echo "release-verify: could not download $url" >&2
      return 1
    }
  else
    curl -fsSL -o "$dest" "$url" || {
      echo "release-verify: could not download $url" >&2
      return 1
    }
  fi

  if [ ! -s "$dest" ]; then
    echo "release-verify: $url is empty" >&2
    return 1
  fi
}

# fuse_verify_asset <file> <asset-name> <checksums-file> - verify file against
# the entry for asset-name. asset-name is matched exactly, so a prefix of
# another asset's name can never satisfy the check.
fuse_verify_asset() {
  local file="$1" asset="$2" sums="$3"

  if [ ! -f "$sums" ]; then
    echo "release-verify: checksum file $sums not found" >&2
    return 1
  fi

  # GoReleaser writes "<hash>  <name>"; the binary-mode "<hash> *<name>" form
  # is accepted too so a hand-produced file still verifies.
  local want
  want="$(awk -v a="$asset" '$2 == a || $2 == "*" a {print $1; exit}' "$sums")"
  if [ -z "$want" ]; then
    echo "release-verify: no checksum entry for $asset in $sums" >&2
    return 1
  fi

  local got
  got="$(fuse_sha256 "$file")" || return 1

  if [ "$want" != "$got" ]; then
    echo "release-verify: checksum mismatch for $asset" >&2
    echo "  expected $want" >&2
    echo "  actual   $got" >&2
    return 1
  fi
}
