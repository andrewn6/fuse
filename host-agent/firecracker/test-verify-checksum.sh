#!/usr/bin/env bash
# Unit test for the release-asset checksum verification helper that guards the
# installer/updater download paths.
#
# The helper is embedded (not sourced) in three scripts, so this test extracts
# the block from each, asserts the copies have not drifted, and then exercises the
# helper against the cases that matter: a good asset, a tampered one, a
# truncated one, a missing checksums file, an empty one, a missing entry, and a
# host with no checksum tool on PATH.
#
# No network, no root, no VMs:  ./host-agent/firecracker/test-verify-checksum.sh
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BEGIN='# >>> fuse release-asset checksum verification >>>'
END='# <<< fuse release-asset checksum verification <<<'

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

PASS=0
FAIL=0

# check <name> <want-rc> <got-rc>
check() {
  if [ "$2" = "$3" ]; then
    PASS=$((PASS + 1)); printf '  ok    %s\n' "$1"
  else
    FAIL=$((FAIL + 1)); printf '  FAIL  %s (want rc=%s, got rc=%s)\n' "$1" "$2" "$3"
  fi
}

# rc <cmd...> - run quietly and echo the exit status.
rc() { "$@" >/dev/null 2>&1; echo $?; }

extract() { awk -v b="$BEGIN" -v e="$END" '$0==b{f=1} f{print} $0==e{f=0}' "$1"; }

printf '\n== helper copies are in sync ==\n'
A="$ROOT/host-agent/firecracker/fc-update.sh"
B="$ROOT/ops/install-orchestrator.sh"
C="$ROOT/host-agent/firecracker/fc-agent.sh"
extract "$A" > "$WORK/a.sh"
extract "$B" > "$WORK/b.sh"
extract "$C" > "$WORK/c.sh"
[ -s "$WORK/a.sh" ] || { echo "  FAIL  no helper block found in $A"; exit 1; }
[ -s "$WORK/b.sh" ] || { echo "  FAIL  no helper block found in $B"; exit 1; }
[ -s "$WORK/c.sh" ] || { echo "  FAIL  no helper block found in $C"; exit 1; }
check "fc-update.sh and install-orchestrator.sh carry the same helper" 0 "$(rc cmp -s "$WORK/a.sh" "$WORK/b.sh")"
check "fc-agent.sh carries the same helper" 0 "$(rc cmp -s "$WORK/a.sh" "$WORK/c.sh")"

# shellcheck source=/dev/null
source "$WORK/a.sh"

printf '\n== fixtures ==\n'
ASSET="fuse_Linux_x86_64.tar.gz"
printf 'pretend release tarball\n' > "$WORK/$ASSET"
GOOD="$(sha256_of "$WORK/$ASSET")"
[ -n "$GOOD" ] || { echo "  FAIL  sha256_of produced nothing"; exit 1; }
printf '  asset %s sha256=%s\n' "$ASSET" "$GOOD"

SUMS="$WORK/checksums.txt"
{
  printf '%s  fused_Linux_x86_64\n' "0000000000000000000000000000000000000000000000000000000000000000"
  printf '%s  %s\n' "$GOOD" "$ASSET"
} > "$SUMS"

printf '\n== verification cases ==\n'

# 1. the happy path: the published checksum matches the downloaded asset.
check "valid checksum is accepted" 0 "$(rc verify_asset "$WORK/$ASSET" "$ASSET" "$SUMS")"

# 2. tampered: same size, different bytes.
cp "$WORK/$ASSET" "$WORK/tampered"
printf 'evil\n' >> "$WORK/tampered"
check "tampered asset is rejected" 1 "$(rc verify_asset "$WORK/tampered" "$ASSET" "$SUMS")"

# 3. truncated download.
head -c 4 "$WORK/$ASSET" > "$WORK/truncated"
check "truncated asset is rejected" 1 "$(rc verify_asset "$WORK/truncated" "$ASSET" "$SUMS")"

# 4. the release published no checksums.txt at all.
check "missing checksums file is rejected" 1 "$(rc verify_asset "$WORK/$ASSET" "$ASSET" "$WORK/nope.txt")"

# 5. checksums.txt exists but is empty (e.g. a 0-byte download).
: > "$WORK/empty.txt"
check "empty checksums file is rejected" 1 "$(rc verify_asset "$WORK/$ASSET" "$ASSET" "$WORK/empty.txt")"

# 6. checksums.txt has no line for this asset name.
check "missing asset entry is rejected" 1 "$(rc verify_asset "$WORK/$ASSET" "fuse_Linux_arm64.tar.gz" "$SUMS")"

# 7. a near-miss name must not match by prefix or suffix.
printf '%s  not-%s-either\n' "$GOOD" "$ASSET" > "$WORK/nearmiss.txt"
check "similar-but-different asset name is rejected" 1 "$(rc verify_asset "$WORK/$ASSET" "$ASSET" "$WORK/nearmiss.txt")"

# 8. binary-mode checksum files write "*name"; that entry must still match.
printf '%s *%s\n' "$GOOD" "$ASSET" > "$WORK/binmode.txt"
check "binary-mode (*name) entry is accepted" 0 "$(rc verify_asset "$WORK/$ASSET" "$ASSET" "$WORK/binmode.txt")"

# 9. the asset itself never arrived.
check "absent asset file is rejected" 1 "$(rc verify_asset "$WORK/absent" "$ASSET" "$SUMS")"

# 10. no sha256sum and no shasum on PATH: fail closed, never silently skip.
mkdir -p "$WORK/emptybin"
# shellcheck disable=SC2123  # overriding PATH in this subshell is the point
NOTOOL="$(PATH="$WORK/emptybin"; verify_asset "$WORK/$ASSET" "$ASSET" "$SUMS" >/dev/null 2>&1; echo $?)"
check "missing checksum tool fails closed" 1 "$NOTOOL"

printf '\n%s passed, %s failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ] || exit 1
printf '\033[1;32mOK\033[0m\n'
