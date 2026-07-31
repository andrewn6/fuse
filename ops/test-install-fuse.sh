#!/usr/bin/env bash
# Tests for ops/install-fuse.sh. No network: a fake `curl` on PATH serves a
# synthetic release, so the platform mapping and the fail-closed verification
# are exercised without touching GitHub.
#
#   ./ops/test-install-fuse.sh
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALLER="$HERE/install-fuse.sh"

PASS=0
FAIL=0
ok()  { printf '  ok   %s\n' "$1"; PASS=$((PASS + 1)); }
bad() { printf '  FAIL %s\n' "$1"; FAIL=$((FAIL + 1)); }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# --- a fake release, served by a fake curl ----------------------------------
# RELEASE_DIR stands in for the release's asset list: whatever file the fake
# curl is asked for, it copies from here. A missing file is a 404.
RELEASE_DIR="$WORK/release"
mkdir -p "$RELEASE_DIR"

# A "fuse" binary that just prints a version, tarred the way goreleaser does.
printf '#!/bin/sh\necho "fuse version 9.9.9"\n' > "$WORK/fuse"
chmod +x "$WORK/fuse"

make_asset() {
  # make_asset <asset-name> - build the tarball and record its real checksum.
  tar -czf "$RELEASE_DIR/$1" -C "$WORK" fuse
}

sha_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
  else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# The fake curl: understands `-o <dest> <url>` and ignores the rest.
mkdir -p "$WORK/bin"
cat > "$WORK/bin/curl" <<'SHIM'
#!/usr/bin/env bash
dest=""; url=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) dest="$2"; shift 2 ;;
    -H) shift 2 ;;
    -*) shift ;;
    *)  url="$1"; shift ;;
  esac
done
name="${url##*/}"
src="$FAKE_RELEASE_DIR/$name"
[ -f "$src" ] || exit 22          # curl's exit code for HTTP 4xx with -f
if [ -n "$dest" ]; then cp "$src" "$dest"; else cat "$src"; fi
SHIM
chmod +x "$WORK/bin/curl"

run_installer() {
  # run_installer <install-dir> - run with the fake curl first on PATH.
  PATH="$WORK/bin:$PATH" FAKE_RELEASE_DIR="$RELEASE_DIR" \
    VERSION=v9.9.9 FUSE_INSTALL_DIR="$1" FUSE_REPO=folsomintel/fuse \
    bash "$INSTALLER" >"$WORK/out.log" 2>&1
}

# The asset name the installer should ask for on this machine, mirroring
# .goreleaser.yaml's cli name_template.
case "$(uname -s)" in
  Darwin) OS="Darwin" ;;
  Linux)  OS="Linux" ;;
  *) echo "unsupported test OS $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64)  ARCH="x86_64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  i386|i686)     ARCH="i386" ;;
  *) echo "unsupported test arch $(uname -m)" >&2; exit 1 ;;
esac
ASSET="fuse-cli_${OS}_${ARCH}.tar.gz"

echo "install-fuse ($ASSET)"

# --- 1. happy path -----------------------------------------------------------
make_asset "$ASSET"
printf '%s  %s\n' "$(sha_of "$RELEASE_DIR/$ASSET")" "$ASSET" > "$RELEASE_DIR/checksums.txt"
DEST="$WORK/dest-ok"
if run_installer "$DEST" && [ -x "$DEST/fuse" ] && "$DEST/fuse" | grep -q "9.9.9"; then
  ok "a valid asset installs and runs"
else
  bad "a valid asset installs and runs"; cat "$WORK/out.log"
fi

# The installer must resolve the platform to the goreleaser asset name. If this
# mapping ever drifts the fake curl 404s and the install fails, so reaching
# here at all proves the name matched.
grep -q "$ASSET" "$WORK/out.log" && ok "requests the goreleaser asset name" \
  || bad "requests the goreleaser asset name"

# --- 2. tampered asset -------------------------------------------------------
# Same name, different bytes: the case HTTPS cannot catch.
printf '#!/bin/sh\necho pwned\n' > "$WORK/fuse"
chmod +x "$WORK/fuse"
make_asset "$ASSET"                       # rebuilt, checksums.txt now stale
DEST="$WORK/dest-tampered"
if run_installer "$DEST"; then
  bad "a tampered asset is refused"
else
  [ ! -e "$DEST/fuse" ] && ok "a tampered asset is refused, nothing installed" \
    || bad "a tampered asset is refused, nothing installed"
fi

# --- 3. missing checksums.txt ------------------------------------------------
printf '#!/bin/sh\necho "fuse version 9.9.9"\n' > "$WORK/fuse"
chmod +x "$WORK/fuse"
make_asset "$ASSET"
rm -f "$RELEASE_DIR/checksums.txt"
DEST="$WORK/dest-nosums"
if run_installer "$DEST"; then
  bad "a release with no checksums.txt is refused"
else
  [ ! -e "$DEST/fuse" ] && ok "a release with no checksums.txt is refused" \
    || bad "a release with no checksums.txt is refused"
fi

# --- 4. no entry for this asset ----------------------------------------------
printf '%s  %s\n' "$(sha_of "$RELEASE_DIR/$ASSET")" "some-other-asset.tar.gz" \
  > "$RELEASE_DIR/checksums.txt"
DEST="$WORK/dest-noentry"
if run_installer "$DEST"; then
  bad "a checksums.txt without this asset is refused"
else
  [ ! -e "$DEST/fuse" ] && ok "a checksums.txt without this asset is refused" \
    || bad "a checksums.txt without this asset is refused"
fi

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
