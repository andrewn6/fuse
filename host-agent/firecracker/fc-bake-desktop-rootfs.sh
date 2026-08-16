#!/usr/bin/env bash
# Layer a desktop onto rootfs-fused.ext4, producing rootfs-desktop.ext4.
# Idempotent. Requires: podman on host (for the desktop bundle extraction),
# sudo, mount, truncate, e2fsck, resize2fs, tar, chroot.
#
# Inputs:
#   rootfs-fused.ext4      baked image from ./fc-bake-rootfs.sh (working dir)
#   fuse-display-run       xvfb runner script (ships in host-agent/firecracker/)
#   ops/systemd/fuse-display.service   systemd unit for the display
#   ops/systemd/fuse-wm.service        systemd unit for the window manager (mutter)
#   ops/systemd/fuse-panel.service     systemd unit for the panel (tint2)
#
# Output:
#   rootfs-desktop.ext4    desktop image; place it in the images dir to use it
#                          from a Fusefile (mkdir -p images && cp
#                          rootfs-desktop.ext4 images/desktop.ext4, then
#                          `image: desktop`)
#
# The guest cannot apt-get (the CI rootfs ships an empty dpkg status), so the
# desktop stack is installed into an ubuntu:22.04 container on the host - the
# same release the rootfs is built from - and the resulting files are extracted
# into the image, the same way the base bake obtains iptables.
set -euo pipefail

FC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$FC_DIR"

# systemd units live with the repo's other service files
OPS_SYSTEMD="$(cd "$FC_DIR/../../ops/systemd" && pwd)"

BASE=rootfs-fused.ext4
OUT=rootfs-desktop.ext4
SIZE=${FC_DESKTOP_ROOTFS_SIZE:-8G}
GEOMETRY=${FC_DESKTOP_GEOMETRY:-1024x768x24}
MOUNT_POINT=${FC_BAKE_MOUNT:-/tmp/fcbake-desktop}
WORK=${FC_BAKE_WORK:-/tmp/fcbake-work}

# everything the desktop needs, resolved with dependencies by apt inside the
# bundle container. firefox-esr comes from the mozillateam ppa because the
# archive "firefox" package on 22.04 is a snap shim, and snaps cannot run in
# the guest.
DESKTOP_PACKAGES="xvfb x11-utils x11-xserver-utils xauth
  xdotool scrot xclip
  mutter tint2
  pcmanfm xterm
  dbus dbus-x11
  fonts-dejavu-core fonts-liberation
  firefox-esr"

log() { printf '\033[1;36m[bake] %s\033[0m\n' "$*"; }
need() { command -v "$1" >/dev/null 2>&1 || { echo "missing: $1" >&2; exit 1; }; }
for c in sudo mount umount truncate e2fsck resize2fs tar podman chroot; do need "$c"; done

[ -f "$BASE" ] || { echo "$BASE not found — run ./fc-bake-rootfs.sh first" >&2; exit 1; }
[ -f fuse-display-run ] || { echo "fuse-display-run not found — it ships in host-agent/firecracker/; restore it" >&2; exit 1; }
for u in fuse-display fuse-wm fuse-panel; do
  [ -f "$OPS_SYSTEMD/$u.service" ] || { echo "$u.service not found — it ships in ops/systemd/; restore it" >&2; exit 1; }
done

cleanup() {
  sudo -n umount "$MOUNT_POINT/dev" 2>/dev/null || true
  sudo -n umount "$MOUNT_POINT/proc" 2>/dev/null || true
  sudo -n umount "$MOUNT_POINT" 2>/dev/null || true
}
trap cleanup EXIT

# under `set -e` a failing command aborts with no output, which historically made
# the base bake look like it exited cleanly mid-bake. always say where it died.
trap 'rc=$?; [ $rc -ne 0 ] && echo "[bake] FAILED at line $LINENO (exit $rc); image is incomplete" >&2' ERR

# e2fsck exits 1 when it corrected errors and 2 when it corrected errors and
# wants a reboot. both are success for our purposes; only >=4 is a real failure.
fsck_ok() {
  local rc=0
  sudo -n e2fsck -f -y "$1" >/dev/null || rc=$?
  [ "$rc" -le 2 ] || { echo "[bake] e2fsck failed on $1 (exit $rc)" >&2; return 1; }
}

mkdir -p "$WORK"

log "copy base -> $OUT"
cp -f "$BASE" "$OUT"

log "grow $OUT to $SIZE"
sudo -n truncate -s "$SIZE" "$OUT"
fsck_ok "$OUT"
sudo -n resize2fs "$OUT" >/dev/null

log "mount loopback at $MOUNT_POINT"
sudo -n mkdir -p "$MOUNT_POINT"
sudo -n mount -o loop "$OUT" "$MOUNT_POINT"

# --- 1. desktop bundle (built in ubuntu:22.04 via host podman) ---------------

if [ ! -f "$WORK/desktop-full.tar" ]; then
  log "build desktop bundle from ubuntu:22.04 (this downloads a browser; takes a while)"
  # host network: skips netavark firewall setup, which needs nft/iptables on the host.
  # software-properties-common is installed before the package snapshot so the
  # bundle diff contains only the desktop stack, not the ppa tooling.
  sudo -n podman run --rm --network=host \
    -v "$WORK":/out -e DESKTOP_PACKAGES="$DESKTOP_PACKAGES" \
    docker.io/library/ubuntu:22.04 bash -c '
    set -e
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq >/dev/null
    apt-get install -y --no-install-recommends software-properties-common ca-certificates gnupg >/dev/null
    add-apt-repository -y ppa:mozillateam/ppa >/dev/null 2>&1
    apt-get update -qq >/dev/null
    dpkg-query -W -f="\${Package}\n" | sort > /tmp/before
    apt-get install -y --no-install-recommends $DESKTOP_PACKAGES >/dev/null
    dpkg-query -W -f="\${Package}\n" | sort > /tmp/after
    comm -13 /tmp/before /tmp/after > /tmp/new-pkgs
    # union with the full dependency closure of DESKTOP_PACKAGES, not just the
    # names themselves: dbus (for dbus-run-session) and its library
    # libdbus-1-3 are both pulled in transitively by software-properties-common
    # earlier, so neither shows up as "new" here even though the desktop
    # stack depends on their files.
    apt-cache depends --recurse --no-recommends --no-suggests --no-conflicts \
      --no-breaks --no-replaces --no-enhances $DESKTOP_PACKAGES 2>/dev/null \
      | grep -E "^[a-zA-Z0-9]" | sort -u > /tmp/desktop-closure
    { cat /tmp/new-pkgs /tmp/desktop-closure; for p in $DESKTOP_PACKAGES; do echo "$p"; done; } \
      | sort -u > /tmp/wanted-pkgs
    while read -r p; do dpkg -L "$p" 2>/dev/null; done < /tmp/wanted-pkgs | sort -u > /tmp/paths
    # gtk apps need the caches the package triggers generated in this
    # container; dpkg does not own them, so they are listed by hand
    glib-compile-schemas /usr/share/glib-2.0/schemas 2>/dev/null || true
    {
      while read -r f; do
        if [ -f "$f" ] || [ -L "$f" ]; then printf "%s\n" "$f"; fi
      done < /tmp/paths
      [ -f /usr/share/glib-2.0/schemas/gschemas.compiled ] && echo /usr/share/glib-2.0/schemas/gschemas.compiled
      find /usr/lib/x86_64-linux-gnu/gdk-pixbuf-2.0 -name loaders.cache 2>/dev/null || true
      find /usr/lib/x86_64-linux-gnu/gtk-3.0 -name immodules.cache 2>/dev/null || true
      find /usr/share/icons -name icon-theme.cache 2>/dev/null || true
      find /usr/share/mime -type f 2>/dev/null || true
      true
    } | sort -u > /tmp/manifest
    tar -cf /out/desktop-full.tar --no-recursion -T /tmp/manifest
  '
fi
log "inject desktop bundle"
sudo -n tar -xf "$WORK/desktop-full.tar" -C "$MOUNT_POINT"

# the bundle added a few hundred shared libraries; refresh the loader cache so
# the guest does not depend on the compiled-in fallback search path
if command -v ldconfig >/dev/null 2>&1; then
  log "refresh guest ld cache"
  sudo -n ldconfig -r "$MOUNT_POINT" 2>/dev/null \
    || echo "[bake] WARNING: ldconfig -r failed; guest falls back to default lib paths" >&2
fi

# --- 2. display runner + systemd units ---------------------------------------

log "inject display runner + systemd units"
sudo -n install -m 0755 fuse-display-run "$MOUNT_POINT/usr/local/bin/fuse-display-run"
for u in fuse-display fuse-wm fuse-panel; do
  sudo -n install -m 0644 "$OPS_SYSTEMD/$u.service" "$MOUNT_POINT/etc/systemd/system/$u.service"
  sudo -n ln -sf "/etc/systemd/system/$u.service" \
    "$MOUNT_POINT/etc/systemd/system/multi-user.target.wants/$u.service"
done

log "write /etc/default/fuse-desktop (geometry $GEOMETRY)"
sudo -n tee "$MOUNT_POINT/etc/default/fuse-desktop" >/dev/null <<CONF
# baked default; a fusefile desktop block overrides this via /fuse/desktop.json
FUSE_DISPLAY_GEOMETRY=$GEOMETRY
CONF

# order the agent after the desktop so its computer surface never races a
# display that is still starting. drop-in, not a unit edit: start-agent writes
# its own drop-in (override.conf) into the same directory and both must apply.
log "order fused after the desktop"
sudo -n mkdir -p "$MOUNT_POINT/etc/systemd/system/fused.service.d"
sudo -n tee "$MOUNT_POINT/etc/systemd/system/fused.service.d/10-desktop.conf" >/dev/null <<'CONF'
[Unit]
After=fuse-display.service fuse-wm.service
Wants=fuse-display.service fuse-wm.service
CONF

# --- 3. sanity ---------------------------------------------------------------

log "sanity check"
check() {
  sudo -n test "$1" "$MOUNT_POINT$2" \
    || { echo "[bake] sanity check FAILED: $2 missing or wrong type ($1)" >&2; exit 1; }
}
check -x /usr/local/bin/fused
check -x /usr/bin/Xvfb
check -x /usr/bin/xdpyinfo
check -x /usr/bin/xdotool
check -x /usr/bin/scrot
check -x /usr/bin/xclip
check -x /usr/bin/mutter
check -x /usr/bin/tint2
check -x /usr/bin/pcmanfm
check -e /usr/bin/firefox-esr
check -x /usr/local/bin/fuse-display-run
check -f /etc/systemd/system/fuse-display.service
check -L /etc/systemd/system/multi-user.target.wants/fuse-display.service
check -L /etc/systemd/system/multi-user.target.wants/fuse-wm.service
check -L /etc/systemd/system/multi-user.target.wants/fuse-panel.service
check -f /etc/systemd/system/fused.service.d/10-desktop.conf

# boot the display stack in a chroot and capture a real screenshot, so a bundle
# with a missing library fails here instead of at first VM boot. skippable for
# bake hosts where chroot X refuses to start (FC_DESKTOP_SKIP_DISPLAY_CHECK=1).
if [ "${FC_DESKTOP_SKIP_DISPLAY_CHECK:-0}" != "1" ]; then
  log "sanity: boot Xvfb in a chroot and capture a screenshot"
  sudo -n mount -t proc proc "$MOUNT_POINT/proc"
  sudo -n mount --bind /dev "$MOUNT_POINT/dev"
  rc=0
  sudo -n chroot "$MOUNT_POINT" /bin/sh -c '
    set -e
    Xvfb :99 -screen 0 640x480x24 -nolisten tcp >/dev/null 2>&1 &
    xpid=$!
    trap "kill $xpid 2>/dev/null; wait $xpid 2>/dev/null; true" EXIT
    i=0
    while [ ! -S /tmp/.X11-unix/X99 ]; do
      i=$((i+1)); [ $i -gt 50 ] && exit 1; sleep 0.2
    done
    DISPLAY=:99 xdpyinfo >/dev/null
    DISPLAY=:99 scrot -o /tmp/fc-bake-check.png
    test -s /tmp/fc-bake-check.png
  ' || rc=$?
  sudo -n rm -f "$MOUNT_POINT/tmp/fc-bake-check.png" \
    "$MOUNT_POINT/tmp/.X99-lock" "$MOUNT_POINT/tmp/.X11-unix/X99"
  sudo -n umount "$MOUNT_POINT/dev"
  sudo -n umount "$MOUNT_POINT/proc"
  [ "$rc" -eq 0 ] || { echo "[bake] display sanity check FAILED (exit $rc)" >&2; exit 1; }
fi

sudo -n umount "$MOUNT_POINT"
trap - EXIT

log "done -> $OUT ($(du -h "$OUT" | cut -f1))"
