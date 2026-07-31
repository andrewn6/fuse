#!/usr/bin/env bash
# Rebuild rootfs-fused.ext4 from rootfs.ext4 + fused + podman-static + iptables.
# Idempotent. Requires: podman on host (for the iptables bundle extraction),
# sudo, mount, truncate, e2fsck, resize2fs, curl, tar.
#
# Inputs in the working directory:
#   rootfs.ext4          base Firecracker CI rootfs
#   fused                statically linked Linux/amd64 agent binary (the reference agent)
#   fused.service        systemd unit that supervises the agent in the guest
#
# Output:
#   rootfs-fused.ext4    baked image the fc-agent boots per VM
set -euo pipefail

FC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$FC_DIR"

BASE=rootfs.ext4
OUT=rootfs-fused.ext4
SIZE=${FC_ROOTFS_SIZE:-4G}
PODMAN_STATIC_VERSION=${PODMAN_STATIC_VERSION:-v5.8.1}
PODMAN_STATIC_URL="https://github.com/mgoltzsche/podman-static/releases/download/${PODMAN_STATIC_VERSION}/podman-linux-amd64.tar.gz"
DOCKER_COMPOSE_VERSION=${DOCKER_COMPOSE_VERSION:-v2.40.3}
DOCKER_COMPOSE_URL="https://github.com/docker/compose/releases/download/${DOCKER_COMPOSE_VERSION}/docker-compose-linux-x86_64"
MOUNT_POINT=${FC_BAKE_MOUNT:-/tmp/fcbake}
WORK=${FC_BAKE_WORK:-/tmp/fcbake-work}

log() { printf '\033[1;36m[bake] %s\033[0m\n' "$*"; }
need() { command -v "$1" >/dev/null 2>&1 || { echo "missing: $1" >&2; exit 1; }; }
for c in sudo mount umount truncate e2fsck resize2fs curl tar podman; do need "$c"; done

[ -f "$BASE" ]  || { echo "$BASE not found — run ./fc-install.sh first" >&2; exit 1; }
[ -f fused ]   || { echo "fused not found — run ../shared/fc-build-agent.sh (or drop your own agent binary named 'fused' here)" >&2; exit 1; }
[ -f fused.service ] || { echo "fused.service not found — it ships in host-agent/firecracker/; restore it or provide your agent's unit" >&2; exit 1; }

cleanup() {
  sudo -n umount "$MOUNT_POINT" 2>/dev/null || true
}
trap cleanup EXIT

# under `set -e` a failing command aborts with no output, which historically made
# this script look like it exited cleanly mid-bake. always say where it died.
trap 'rc=$?; [ $rc -ne 0 ] && echo "[bake] FAILED at line $LINENO (exit $rc); image is incomplete" >&2' ERR

# e2fsck exits 1 when it corrected errors and 2 when it corrected errors and
# wants a reboot. both are success for our purposes; only >=4 is a real failure.
# without this, `set -e` kills the bake right after the grow, silently.
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

# --- 1. fused + systemd unit + /fuse + /var/tmp + CA bundle ------------------

log "inject fused + systemd unit"
sudo -n mkdir -p \
  "$MOUNT_POINT/fuse" \
  "$MOUNT_POINT/usr/local/bin" \
  "$MOUNT_POINT/var/tmp" \
  "$MOUNT_POINT/var/lib/containers" \
  "$MOUNT_POINT/run/containers" \
  "$MOUNT_POINT/etc/ssl/certs" \
  "$MOUNT_POINT/etc/systemd/system/multi-user.target.wants" \
  "$MOUNT_POINT/etc/containers"
sudo -n chmod 1777 "$MOUNT_POINT/var/tmp"
sudo -n install -m 0755 fused "$MOUNT_POINT/usr/local/bin/fused"
sudo -n install -m 0644 fused.service "$MOUNT_POINT/etc/systemd/system/fused.service"
sudo -n ln -sf /etc/systemd/system/fused.service \
  "$MOUNT_POINT/etc/systemd/system/multi-user.target.wants/fused.service"
echo '# populated by fc-agent start-agent' | \
  sudo -n tee "$MOUNT_POINT/etc/default/fused" >/dev/null

if [ -f /etc/ssl/certs/ca-certificates.crt ]; then
  sudo -n install -m 0644 /etc/ssl/certs/ca-certificates.crt \
    "$MOUNT_POINT/etc/ssl/certs/ca-certificates.crt"
fi

# --- 2. podman-static --------------------------------------------------------

if [ ! -f "$WORK/podman-static.tgz" ]; then
  log "download podman-static $PODMAN_STATIC_VERSION"
  curl -fsSL -o "$WORK/podman-static.tgz" "$PODMAN_STATIC_URL"
fi
log "inject podman-static"
sudo -n tar -xzf "$WORK/podman-static.tgz" -C "$MOUNT_POINT" --strip-components=1

log "enable podman.socket (docker-compose needs the API)"
sudo -n mkdir -p "$MOUNT_POINT/etc/systemd/system/sockets.target.wants"
sudo -n ln -sf /usr/local/lib/systemd/system/podman.socket \
  "$MOUNT_POINT/etc/systemd/system/sockets.target.wants/podman.socket"

# --- 2b. docker-compose v2 (podman compose provider) ------------------------

if [ ! -f "$WORK/docker-compose" ]; then
  log "download docker-compose $DOCKER_COMPOSE_VERSION"
  curl -fsSL -o "$WORK/docker-compose" "$DOCKER_COMPOSE_URL"
fi
log "inject docker-compose as podman compose provider"
sudo -n mkdir -p "$MOUNT_POINT/usr/libexec/docker/cli-plugins"
sudo -n install -m 0755 "$WORK/docker-compose" \
  "$MOUNT_POINT/usr/libexec/docker/cli-plugins/docker-compose"
sudo -n ln -sf /usr/libexec/docker/cli-plugins/docker-compose \
  "$MOUNT_POINT/usr/local/bin/docker-compose"

# --- 3. iptables bundle (extracted from ubuntu:22.04 via host podman) --------

if [ ! -f "$WORK/iptables-full.tar" ]; then
  log "build iptables bundle from ubuntu:22.04"
  # host network: skips netavark firewall setup, which needs nft/iptables on the host
  sudo -n podman run --rm --network=host -v "$WORK":/out docker.io/library/ubuntu:22.04 bash -c '
    set -e
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq >/dev/null
    apt-get install -y --no-install-recommends iptables libnetfilter-conntrack3 >/dev/null 2>&1
    tar -cf /out/iptables-full.tar -C / \
      usr/sbin/iptables usr/sbin/iptables-legacy \
      usr/sbin/iptables-legacy-save usr/sbin/iptables-legacy-restore \
      usr/sbin/iptables-save usr/sbin/iptables-restore \
      usr/sbin/ip6tables usr/sbin/ip6tables-legacy \
      usr/sbin/ip6tables-save usr/sbin/ip6tables-restore \
      usr/sbin/xtables-legacy-multi usr/sbin/xtables-nft-multi \
      usr/sbin/iptables-nft usr/sbin/ip6tables-nft \
      usr/lib/x86_64-linux-gnu/xtables \
      usr/lib/x86_64-linux-gnu/libxtables.so.12 \
      usr/lib/x86_64-linux-gnu/libxtables.so.12.4.0 \
      usr/lib/x86_64-linux-gnu/libip4tc.so.2 \
      usr/lib/x86_64-linux-gnu/libip4tc.so.2.0.0 \
      usr/lib/x86_64-linux-gnu/libip6tc.so.2 \
      usr/lib/x86_64-linux-gnu/libip6tc.so.2.0.0 \
      usr/lib/x86_64-linux-gnu/libnftnl.so.11 \
      usr/lib/x86_64-linux-gnu/libnftnl.so.11.6.0 \
      usr/lib/x86_64-linux-gnu/libmnl.so.0 \
      usr/lib/x86_64-linux-gnu/libmnl.so.0.2.0 \
      usr/lib/x86_64-linux-gnu/libnfnetlink.so.0 \
      usr/lib/x86_64-linux-gnu/libnfnetlink.so.0.2.0 \
      usr/lib/x86_64-linux-gnu/libnetfilter_conntrack.so.3 \
      usr/lib/x86_64-linux-gnu/libnetfilter_conntrack.so.3.8.0
  '
fi
log "inject iptables"
sudo -n tar -xf "$WORK/iptables-full.tar" -C "$MOUNT_POINT"
for n in iptables iptables-save iptables-restore ip6tables ip6tables-save ip6tables-restore; do
  sudo -n ln -sf xtables-legacy-multi "$MOUNT_POINT/usr/sbin/$n"
done

# --- 4. containers.conf / storage.conf ---------------------------------------

log "write containers.conf (host netns + no firewall)"
sudo -n tee "$MOUNT_POINT/etc/containers/containers.conf" >/dev/null <<'CONF'
[containers]
netns = "host"

[engine]
compose_warning_logs = false

[network]
firewall_driver = "none"
CONF

log "write storage.conf (native overlay)"
sudo -n tee "$MOUNT_POINT/etc/containers/storage.conf" >/dev/null <<'CONF'
[storage]
driver = "overlay"
runroot = "/run/containers/storage"
graphroot = "/var/lib/containers/storage"
[storage.options.overlay]
mountopt = "nodev"
CONF

# --- 5. sanity ---------------------------------------------------------------

log "sanity check"
# name the missing artefact. a bare `test` under `set -e` aborts with no output,
# which is how a half-baked image previously reached a boot attempt and only
# failed later with "agent binary not found at /usr/local/bin/fused".
check() {
  sudo -n test "$1" "$MOUNT_POINT$2" \
    || { echo "[bake] sanity check FAILED: $2 missing or wrong type ($1)" >&2; exit 1; }
}
check -x /usr/local/bin/fused
check -x /usr/local/bin/podman
check -x /usr/libexec/docker/cli-plugins/docker-compose
check -L /usr/sbin/iptables
check -f /usr/lib/x86_64-linux-gnu/xtables/libxt_comment.so

sudo -n umount "$MOUNT_POINT"
trap - EXIT

log "done -> $OUT ($(du -h "$OUT" | cut -f1))"
