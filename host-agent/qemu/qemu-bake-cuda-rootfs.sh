#!/usr/bin/env bash
# Reference bake: builds a CUDA-capable rootfs (qcow2) from an Ubuntu cloud image
# by injecting NVIDIA driver + CUDA toolkit + fused + podman + docker-compose +
# iptables. QEMU counterpart to ../firecracker/fc-bake-rootfs.sh; uses qemu-nbd
# for qcow2 instead of loopback mount.
#
# Usage: ./qemu-bake-cuda-rootfs.sh <driver-version>
#   driver-version   NVIDIA driver branch (e.g. 535, 550, 560). Required.
#
# Requires: sudo, qemu-nbd, curl, e2fsck, resize2fs.
#
# Inputs in the working directory:
#   rootfs.qcow2        base Ubuntu cloud image (from qemu-install.sh)
#   fused               statically linked agent binary
#   fused.service       systemd unit that supervises the agent in the guest
#
# Output:
#   rootfs-cuda.qcow2   baked image the qemu-agent boots per VM
#   vmlinuz.bin         guest kernel extracted from the image (for qemu -kernel)
set -euo pipefail

QEMU_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$QEMU_DIR"

# -- validate args -----------------------------------------------------------

DRIVER_VERSION="${1:-}"
[ -n "$DRIVER_VERSION" ] || {
  echo "usage: $0 <driver-version>  (e.g. 535, 550, 560)" >&2
  echo "ERROR: driver version is required -- refusing to silently pick a default" >&2
  exit 1
}

BASE=rootfs.qcow2
OUT=rootfs-cuda.qcow2
SIZE=${QEMU_ROOTFS_SIZE:-20G}
UBUNTU_REL=${UBUNTU_REL:-ubuntu2204}
NBD_DEV=${QEMU_NBD_DEV:-/dev/nbd0}
MOUNT_POINT=${QEMU_BAKE_MOUNT:-/tmp/qemubake}
WORK=${QEMU_BAKE_WORK:-/tmp/qemubake-work}
PODMAN_STATIC_VERSION=${PODMAN_STATIC_VERSION:-v5.8.1}
PODMAN_STATIC_URL="https://github.com/mgoltzsche/podman-static/releases/download/${PODMAN_STATIC_VERSION}/podman-linux-amd64.tar.gz"
DOCKER_COMPOSE_VERSION=${DOCKER_COMPOSE_VERSION:-v2.40.3}
DOCKER_COMPOSE_URL="https://github.com/docker/compose/releases/download/${DOCKER_COMPOSE_VERSION}/docker-compose-linux-x86_64"

log()  { printf '\033[1;36m[bake] %s\033[0m\n' "$*"; }
need() { command -v "$1" >/dev/null 2>&1 || { echo "missing: $1" >&2; exit 1; }; }
for c in sudo qemu-nbd curl resize2fs parted; do need "$c"; done

[ -f "$BASE" ] || { echo "$BASE not found -- run ./qemu-install.sh first" >&2; exit 1; }
[ -f fused ]   || { echo "fused not found -- run ../shared/fc-build-agent.sh (or drop your own agent binary named 'fused' here)" >&2; exit 1; }
[ -f fused.service ] || { echo "fused.service not found -- it ships in host-agent/qemu/" >&2; exit 1; }
[ -f ubuntu.id_rsa.pub ] || { echo "ubuntu.id_rsa.pub not found -- run ./qemu-install.sh first" >&2; exit 1; }

cleanup() {
  sudo -n umount "$MOUNT_POINT/sys" 2>/dev/null || true
  sudo -n umount "$MOUNT_POINT/proc" 2>/dev/null || true
  sudo -n umount "$MOUNT_POINT/dev" 2>/dev/null || true
  sudo -n umount "$MOUNT_POINT" 2>/dev/null || true
  sudo -n qemu-nbd --disconnect "$NBD_DEV" 2>/dev/null || true
}
trap cleanup EXIT

mkdir -p "$WORK"

# -- 1. copy + resize base image ----------------------------------------------

log "copy $BASE -> $OUT"
cp -f "$BASE" "$OUT"

log "grow $OUT to $SIZE"
sudo -n qemu-img resize "$OUT" "$SIZE"

# -- 2. mount via qemu-nbd ----------------------------------------------------

log "connect $NBD_DEV"
sudo -n modprobe nbd max_part=8
sudo -n qemu-nbd --connect="$NBD_DEV" "$OUT"
sleep 1

# find the root partition (cloud images have GPT: p1=BIOS grub, p2=root or p1=root)
ROOT_PART="${NBD_DEV}p1"
if ! sudo -n blkid "$ROOT_PART" >/dev/null 2>&1; then
  ROOT_PART="${NBD_DEV}p2"
fi
sudo -n blkid "$ROOT_PART" >/dev/null 2>&1 || { echo "cannot find root partition on $NBD_DEV" >&2; exit 1; }

log "mount $ROOT_PART at $MOUNT_POINT"
sudo -n mkdir -p "$MOUNT_POINT"
sudo -n e2fsck -f -y "$ROOT_PART" >/dev/null 2>&1 || true
sudo -n resize2fs "$ROOT_PART" >/dev/null 2>&1 || true
sudo -n mount "$ROOT_PART" "$MOUNT_POINT"

# -- 3. extract guest kernel for qemu -kernel --------------------------------

log "extract guest kernel"
GUEST_KERNEL=$(sudo -n find "$MOUNT_POINT/boot" -name 'vmlinuz-*-generic' -type f | sort -V | tail -1 || true)
if [ -n "$GUEST_KERNEL" ]; then
  sudo -n cp "$GUEST_KERNEL" vmlinuz.bin
  sudo -n chmod 644 vmlinuz.bin
  KERNEL_VER=$(basename "$GUEST_KERNEL" | sed 's/vmlinuz-//')
  log "guest kernel: $KERNEL_VER"
else
  log "warning: no kernel found in image; copy one manually to vmlinuz.bin"
fi

# -- 4. NVIDIA driver + CUDA toolkit -----------------------------------------

log "inject NVIDIA driver $DRIVER_VERSION + CUDA toolkit"
sudo -n mkdir -p "$MOUNT_POINT/tmp/nvidia"
curl -fsSL -o "$WORK/cuda-keyring.deb" \
  "https://developer.download.nvidia.com/compute/cuda/repos/${UBUNTU_REL}/x86_64/cuda-keyring_1.1-1_all.deb"
sudo -n install -m 0644 "$WORK/cuda-keyring.deb" "$MOUNT_POINT/tmp/nvidia/cuda-keyring.deb"
sudo -n cp /etc/resolv.conf "$MOUNT_POINT/etc/resolv.conf"
sudo -n mount --bind /dev "$MOUNT_POINT/dev"
sudo -n mount --bind /proc "$MOUNT_POINT/proc"
sudo -n mount --bind /sys "$MOUNT_POINT/sys"

sudo -n chroot "$MOUNT_POINT" bash -c "
    set -e
    export DEBIAN_FRONTEND=noninteractive
    dpkg -i /tmp/nvidia/cuda-keyring.deb
    apt-get update -qq
    apt-get install -y --no-install-recommends linux-headers-${KERNEL_VER:-generic} build-essential dkms
    apt-get install -y nvidia-driver-${DRIVER_VERSION}
    apt-get install -y cuda-toolkit
    apt-get clean
    rm -rf /var/lib/apt/lists/*
  " || { echo "NVIDIA driver/CUDA install failed" >&2; exit 1; }

sudo -n umount "$MOUNT_POINT/dev" "$MOUNT_POINT/proc" "$MOUNT_POINT/sys" 2>/dev/null || true

# -- 5. fused + systemd unit + /fuse + dirs -----------------------------------

log "inject fused + systemd unit"
sudo -n mkdir -p \
  "$MOUNT_POINT/fuse" \
  "$MOUNT_POINT/usr/local/bin" \
  "$MOUNT_POINT/var/tmp" \
  "$MOUNT_POINT/var/lib/containers" \
  "$MOUNT_POINT/run/containers" \
  "$MOUNT_POINT/etc/ssl/certs" \
  "$MOUNT_POINT/root/.ssh" \
  "$MOUNT_POINT/etc/systemd/system/multi-user.target.wants" \
  "$MOUNT_POINT/etc/containers"
sudo -n chmod 1777 "$MOUNT_POINT/var/tmp"
sudo -n chmod 0700 "$MOUNT_POINT/root/.ssh"
sudo -n install -m 0600 ubuntu.id_rsa.pub "$MOUNT_POINT/root/.ssh/authorized_keys"
sudo -n install -m 0755 fused "$MOUNT_POINT/usr/local/bin/fused"
sudo -n install -m 0644 fused.service "$MOUNT_POINT/etc/systemd/system/fused.service"
sudo -n ln -sf /etc/systemd/system/fused.service \
  "$MOUNT_POINT/etc/systemd/system/multi-user.target.wants/fused.service"
echo '# populated by qemu-agent start-agent' | \
  sudo -n tee "$MOUNT_POINT/etc/default/fused" >/dev/null

if [ -f /etc/ssl/certs/ca-certificates.crt ]; then
  sudo -n install -m 0644 /etc/ssl/certs/ca-certificates.crt \
    "$MOUNT_POINT/etc/ssl/certs/ca-certificates.crt"
fi

# -- 6. podman-static --------------------------------------------------------

if [ ! -f "$WORK/podman-static.tgz" ]; then
  log "download podman-static $PODMAN_STATIC_VERSION"
  curl -fsSL -o "$WORK/podman-static.tgz" "$PODMAN_STATIC_URL"
fi
log "inject podman-static"
sudo -n tar -xzf "$WORK/podman-static.tgz" -C "$MOUNT_POINT" --strip-components=1

sudo -n mkdir -p "$MOUNT_POINT/etc/systemd/system/sockets.target.wants"
sudo -n ln -sf /usr/local/lib/systemd/system/podman.socket \
  "$MOUNT_POINT/etc/systemd/system/sockets.target.wants/podman.socket"

# -- 7. docker-compose v2 (podman compose provider) --------------------------

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

# -- 8. iptables bundle ------------------------------------------------------

if [ ! -f "$WORK/iptables-full.tar" ]; then
  log "build iptables bundle from ubuntu:22.04"
  if command -v podman >/dev/null 2>&1; then
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
  else
    log "warning: podman not found on host; iptables bundle not injected"
    log "         guest rootfs must already have iptables installed"
  fi
fi
if [ -f "$WORK/iptables-full.tar" ]; then
  log "inject iptables"
  sudo -n tar -xf "$WORK/iptables-full.tar" -C "$MOUNT_POINT"
  for n in iptables iptables-save iptables-restore ip6tables ip6tables-save ip6tables-restore; do
    sudo -n ln -sf xtables-legacy-multi "$MOUNT_POINT/usr/sbin/$n"
  done
fi

# -- 9. containers.conf / storage.conf ---------------------------------------

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

# -- 10. sanity --------------------------------------------------------------

log "sanity check"
sudo -n test -x "$MOUNT_POINT/usr/local/bin/fused" || { echo "fused missing" >&2; exit 1; }
sudo -n test -x "$MOUNT_POINT/usr/local/bin/podman" || { echo "podman missing" >&2; exit 1; }
sudo -n test -d "$MOUNT_POINT/usr/lib/x86_64-linux-gnu/xtables" || echo "warning: xtables dir missing (ok if iptables skipped)"
if sudo -n test -f "$MOUNT_POINT/usr/lib/x86_64-linux-gnu/libcuda.so.1"; then
  log "NVIDIA userspace driver present"
else
  log "warning: libcuda.so.1 not found -- driver may not have installed correctly"
fi

# -- 11. cleanup -------------------------------------------------------------

sudo -n umount "$MOUNT_POINT"
sudo -n qemu-nbd --disconnect "$NBD_DEV"
trap - EXIT

log "done -> $OUT ($(du -h "$OUT" | cut -f1))"
log "kernel -> vmlinuz.bin"
log "next: register the host and deploy with 'fuse up' (resources.gpu: 1)"
