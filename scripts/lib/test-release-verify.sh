#!/usr/bin/env bash
# Tests for scripts/lib/release-verify.sh. No network and no root: the fetch
# helper is exercised through fuse_verify_asset, which is the part that decides
# whether an asset gets installed.
#
#   ./scripts/lib/test-release-verify.sh
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=release-verify.sh
. "$HERE/release-verify.sh"

PASS=0
FAIL=0

ok()   { printf '  ok   %s\n' "$1"; PASS=$((PASS + 1)); }
bad()  { printf '  FAIL %s\n' "$1"; FAIL=$((FAIL + 1)); }

# expect_ok <name> <cmd...> - the command must succeed.
expect_ok() {
  local name="$1"; shift
  if "$@" 2>/dev/null; then ok "$name"; else bad "$name (expected success)"; fi
}

# expect_fail <name> <cmd...> - the command must fail.
expect_fail() {
  local name="$1"; shift
  if "$@" 2>/dev/null; then bad "$name (expected failure)"; else ok "$name"; fi
}

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

ASSET="fuse_Linux_x86_64.tar.gz"
printf 'pretend release archive\n' > "$WORK/$ASSET"
HASH="$(fuse_sha256 "$WORK/$ASSET")"

# A second asset whose name is a prefix of nothing else, used to prove entries
# are matched by exact name.
OTHER="fused_Linux_x86_64"
printf 'pretend guest agent\n' > "$WORK/$OTHER"
OTHER_HASH="$(fuse_sha256 "$WORK/$OTHER")"

SUMS="$WORK/checksums.txt"
printf '%s  %s\n%s  %s\n' "$HASH" "$ASSET" "$OTHER_HASH" "$OTHER" > "$SUMS"

echo "release-verify"

expect_ok "valid asset verifies" \
  fuse_verify_asset "$WORK/$ASSET" "$ASSET" "$SUMS"

expect_ok "second asset in the same file verifies" \
  fuse_verify_asset "$WORK/$OTHER" "$OTHER" "$SUMS"

# Tampered: same length, different bytes. This is the case HTTPS cannot catch,
# because the file is served exactly as it sits in the release.
printf 'pretend release archivX\n' > "$WORK/tampered"
expect_fail "tampered asset is rejected" \
  fuse_verify_asset "$WORK/tampered" "$ASSET" "$SUMS"

# Truncated: what a dropped connection leaves behind.
printf 'pretend' > "$WORK/truncated"
expect_fail "truncated asset is rejected" \
  fuse_verify_asset "$WORK/truncated" "$ASSET" "$SUMS"

expect_fail "missing checksum file is rejected" \
  fuse_verify_asset "$WORK/$ASSET" "$ASSET" "$WORK/nope.txt"

expect_fail "missing entry for the asset is rejected" \
  fuse_verify_asset "$WORK/$ASSET" "fuse_Linux_arm64.tar.gz" "$SUMS"

# An entry for a different asset must not satisfy this one even though both
# hashes are present in the file.
expect_fail "another asset's entry does not satisfy this asset" \
  fuse_verify_asset "$WORK/$OTHER" "$ASSET" "$SUMS"

# Binary-mode format ("<hash> *<name>") as produced by sha256sum -b.
printf '%s *%s\n' "$HASH" "$ASSET" > "$WORK/binary-mode.txt"
expect_ok "binary-mode checksum lines verify" \
  fuse_verify_asset "$WORK/$ASSET" "$ASSET" "$WORK/binary-mode.txt"

# An empty checksums file has no entry, so it must fail rather than pass
# vacuously.
: > "$WORK/empty.txt"
expect_fail "empty checksum file is rejected" \
  fuse_verify_asset "$WORK/$ASSET" "$ASSET" "$WORK/empty.txt"

# A tag with no checksums.txt published: the fetch fails, so nothing installs.
expect_fail "unreachable checksums.txt fails closed" \
  fuse_fetch_checksums "folsomintel/fuse" "v0.0.0-does-not-exist" "$WORK/fetched.txt"

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
