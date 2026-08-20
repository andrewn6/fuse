# runbook: move the firecracker host state onto xfs with reflink

status: draft, not yet executed
applies to: the firecracker host agent (`host-agent/firecracker/fc-agent.py`)
target host: phantom (`debian@51.79.19.90`), and any future firecracker host

**this document is for human execution after review.** nothing in here has been
run against phantom. every step marked `DESTRUCTIVE` takes a customer VM, a
snapshot artifact, or a partition table with it. read the whole thing, run the
pre-flight, and confirm the assumptions in the appendix before typing anything.

this is not published documentation. it lives under `docs/runbooks/` alongside
`docs/decisions/`, both of which sit outside `docs/content/` and are therefore
not built by blume (`docs/blume.config.ts` sets `content.root = "content"`).

---

## 0. why

`snapshot_create` copies the VM rootfs with `cp --reflink=auto`
(`host-agent/firecracker/fc-agent.py:796`, via `copy_rootfs`). `auto` means "use
copy-on-write if the filesystem supports it, otherwise do a full byte copy, and
do not complain either way". phantom's state is on ext4, which has no reflink,
so today every snapshot is a multi-gigabyte read plus write.

that was tolerable while snapshots were disk-only. the live snapshot path on
this branch (`snapshot_create(..., live=True)`, `fc-agent.py:767`) takes the
rootfs copy and the memory image inside a single pause window, because the
memory image contains the guest's own page cache and a rootfs copied after the
resume would already have drifted from it. on ext4 that pause window includes
the entire rootfs copy: a multi-second hard freeze of a running customer VM, per
snapshot.

on xfs with `reflink=1` the same `cp` is an O(1) metadata operation. the pause
window collapses to the memory write, which is the irreducible part.

the decision to move `FC_DIR` state onto xfs has been made. this runbook is how.

---

## 1. what has to land on the new filesystem

path constants, `fc-agent.py:43-59` and `:87-88`:

| constant | value | env override |
| --- | --- | --- |
| `FC_DIR` | default `/home/ubuntu/fc` | `FC_DIR` |
| `STATE_DIR` | `FC_DIR/agent-state` | none (derived) |
| `VMS_DIR` | `STATE_DIR/vms` | **none** |
| `SNAPSHOTS_DIR` | `STATE_DIR/snapshots` | `SNAPSHOTS_DIR` |
| `ARTIFACT_TMP_DIR` | `STATE_DIR/artifact-pull-tmp` | `FC_AGENT_ARTIFACT_TMP_DIR` |
| `SSH_CONTROL_DIR` | `STATE_DIR/ssh-control` | none (derived) |
| `BASE_ROOTFS` | `FC_DIR/rootfs-fused.ext4` | `BASE_ROOTFS` |
| `IMAGES_DIR` | `FC_DIR/images` | `IMAGES_DIR` |
| `KERNEL` | `FC_DIR/vmlinux.bin` | none |
| `SSH_KEY` | `FC_DIR/ubuntu.id_rsa` | none |

reflink requires **source and destination on the same filesystem**. `cp
--reflink=auto` across two filesystems does not error, it silently full-copies,
so a half-done migration looks exactly like success and performs exactly like
today.

the copy pairs that matter:

- `VMS_DIR/<vm>/rootfs.ext4` -> `SNAPSHOTS_DIR/<snap>/rootfs.ext4` (snapshot
  create, the one inside the pause window). **both must be on xfs. this is the
  whole point of the migration.**
- `SNAPSHOTS_DIR/<snap>/rootfs.ext4` -> `VMS_DIR/<vm>/rootfs.ext4` (restore and
  fork). same filesystem, so it comes along for free, except that
  `snapshot_restore` does not currently pass `--reflink` at all
  (`fc-agent.py:922`). see section 7.
- `BASE_ROOTFS` or `IMAGES_DIR/<name>.ext4` -> `VMS_DIR/<vm>/rootfs.ext4` (VM
  create, `fc-agent.py:613`). nice to have, not required. see the note below.
- `ARTIFACT_TMP_DIR` -> `SNAPSHOTS_DIR` (artifact pull commit, a `sudo mv`).
  keeping these on one filesystem keeps that commit a rename instead of a
  cross-device copy. `ARTIFACT_TMP_DIR` defaults to a sibling of
  `SNAPSHOTS_DIR`, so moving `STATE_DIR` wholesale handles it.

does **not** need to move: `KERNEL`, `SSH_KEY`, the agent source, the logs.

### why the base image is deliberately left behind

the bake scripts write `rootfs-fused.ext4` and `rootfs-desktop.ext4` into their
own directory, by hardcoded relative name (`fc-bake-rootfs.sh:19`,
`fc-bake-desktop-rootfs.sh:31-32`), and `FC_DIR` in every shell script is
derived from where the script sits:

```sh
# fc-agent.sh:4, fc-update.sh:25, fc-install.sh:5, fc-bake-rootfs.sh:15, ...
FC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
```

so `FC_DIR` for the tooling is the git checkout, always. the python agent honours
`$FC_DIR` from the environment; the scripts do not. setting `BASE_ROOTFS=` to
somewhere on xfs without moving the checkout splits them: the weekly updater
(`fc-update.sh`, installed as `fc-update.timer`) rebakes into the checkout while
the agent keeps booting the stale copy on xfs, silently, for as long as it takes
someone to notice guests are missing a toolchain update.

so: leave `BASE_ROOTFS` and `IMAGES_DIR` where they are for this migration. VM
create keeps its current full-copy cost, which is no regression. section 7 lists
the follow-up that fixes it properly.

---

## 2. pre-flight

all read-only. run all of it, paste the output into the migration ticket, and do
not proceed on assumptions.

### 2.1 confirm where the state actually is

```sh
systemctl cat fc-agent.service
# expect Environment=FC_DIR=<path> and ExecStart=/usr/bin/python3 <path>/fc-agent.py
```

everything below writes `$FC_DIR` for that path. **assumption to verify:** this
runbook was written expecting
`/home/debian/fuse/host-agent/firecracker`. if `systemctl cat` disagrees, use
what it says.

### 2.2 confirm the filesystem and free space

```sh
df -hT "$FC_DIR" "$FC_DIR/agent-state"
findmnt -no SOURCE,FSTYPE,OPTIONS -T "$FC_DIR/agent-state"
du -sh "$FC_DIR/agent-state" "$FC_DIR/agent-state"/*
lsblk -o NAME,SIZE,TYPE,FSTYPE,MOUNTPOINT
cat /proc/mdstat
sudo mdadm --detail /dev/md3
```

prove ext4 has no reflink today, so the later positive test means something:

```sh
# expect: cp: failed to clone ...: Operation not supported
sudo touch /tmp/rl-src && sudo cp --reflink=always /tmp/rl-src /tmp/rl-dst
sudo rm -f /tmp/rl-src /tmp/rl-dst
```

### 2.3 find out whether there is anywhere to put a new filesystem

this is the question that decides section 3, and it is the one this runbook
cannot answer for you. "7% used" is free space *inside* the ext4 filesystem. it
is not unallocated space on the disks, and it is not a shrinkable partition
while the filesystem is mounted and is `/`.

```sh
sudo parted -l
sudo pvs; sudo vgs; sudo lvs            # empty output = no lvm, which is the common ovh soft-raid layout
for d in $(lsblk -dno NAME); do echo "== /dev/$d"; sudo sfdisk -F "/dev/$d"; done
```

read the results as:

- `vgs` shows free extents -> **option A2**, the easy one.
- `sfdisk -F` shows unallocated space on every member disk -> **option A1**.
- neither -> **option B**, or option C, or an offline shrink you should not do
  casually.

### 2.4 inventory the VMs

from an operator workstation with the CLI pointed at the orchestrator:

```sh
fuse hosts list
fuse hosts get <phantom-host-id>
fuse environment list --host-id <phantom-host-id> --all-pages
```

note the `active_host` trap: `fuse environment list` scopes to the active host
by default and prints "no results" with exit 0 when the context's active host is
stale. pass `--host-id` explicitly.

cross-check against the agent itself, which is the ground truth:

```sh
# on phantom. the token is in $FC_DIR/.env, mode 600. do not echo it.
set +o history
source "$FC_DIR/.env"
curl -fsS -H "Authorization: Bearer $FC_AGENT_TOKEN" http://127.0.0.1:8090/v1/vm | python3 -m json.tool
curl -fsS -H "Authorization: Bearer $FC_AGENT_TOKEN" http://127.0.0.1:8090/v1/capacity
ls -la "$FC_DIR/agent-state/vms" "$FC_DIR/agent-state/snapshots"
pgrep -a firecracker
```

### 2.5 every VM must be gone before you start

a running firecracker holds its `rootfs.ext4` open. copying it out from under a
live guest gives you a torn image, and moving the mount point out from under it
gives you a VM whose disk has silently become an orphaned inode. there is no
online variant of this.

so, for each environment on phantom:

```sh
# DESTRUCTIVE (customer-visible). drain runs the in-guest drain command; it does
# NOT destroy the vm, and nothing reconciles a stuck-draining vm afterwards.
fuse environment drain <env-id>
fuse environment destroy <env-id> --yes
```

then confirm both sides are empty:

```sh
fuse environment list --host-id <phantom-host-id> --all-pages   # expect none
ls "$FC_DIR/agent-state/vms"                                    # expect empty
pgrep -a firecracker                                            # expect nothing
```

cordon the host first so the scheduler does not place a new VM mid-migration:

```sh
fuse hosts cordon <phantom-host-id>
```

if a VM genuinely cannot be destroyed (someone's long-lived box), the migration
does not happen today. an alternative exists (stop the agent, stop firecracker,
copy the vm dir, restart) but it is a cold reboot of that guest either way, so
it buys nothing over destroy-and-recreate and adds a torn-image failure mode.

### 2.6 what is live vs regenerable

| path | verdict |
| --- | --- |
| `agent-state/snapshots/*` | **live.** the orchestrator's snapshot rows point here. build artifacts outlive the VM that made them. must be copied byte-exact. |
| `agent-state/vms/*` | live while VMs exist. empty after 2.5, which is why 2.5 exists. |
| `$FC_DIR/.env` (`FC_AGENT_TOKEN`) | **live and irreplaceable in place.** regenerating it makes the orchestrator's stored per-host token wrong and every `environment create` returns an opaque `firecracker create vm: http 401`. stays on ext4 with the checkout. do not touch. |
| `$FC_DIR/ubuntu.id_rsa` | **live.** its public half is baked into every rootfs image's authorized_keys. regenerating it locks the agent out of every existing guest and every baked image. |
| `$FC_DIR/rootfs-fused.ext4`, `images/*.ext4` | regenerable by rebake, but slowly and with known gotchas (a stale `/tmp/fcbake-work` bundle ignores package-list changes). treat as live for this migration: do not rebake as part of it. |
| `$FC_DIR/vmlinux.bin` | regenerable (`fc-install.sh`). |
| `agent-state/ssh-control/*` | **do not copy.** ssh mux sockets. stale ones are actively harmful. |
| `agent-state/artifact-pull-tmp/*.tmp` | **do not copy.** partial pulls, delete them. |
| `agent-state/vms/*/fc.sock` | **do not copy.** recreated by `start_firecracker`. |
| `fc-agent.log`, `vms/*/fc.log` | keep for forensics, not required. |

---

## 3. the options

### option A: a real block device (partition, md array, or LV)

**A2, an LV, if `vgs` shows free extents.** fully online, no reboot, no partition
table edit, growable later with `lvextend` plus `xfs_growfs`. if this is
available it is unambiguously the right answer.

```sh
# DESTRUCTIVE-ish: consumes vg free space permanently. size per section 3.4.
sudo lvcreate -n fc-state -L 1T <vg-name>
```

**A1, a new partition plus md array, if `sfdisk -F` shows unallocated space on
every member disk.** matches the existing md topology, gives the state its own
raid device. costs a partition table edit on disks that currently have `/`
mounted from them, plus `mdadm.conf` and initramfs work so the array reassembles
at boot. the kernel usually picks up new gpt partitions with `partx -a`, but
plan a **maintenance window with a reboot** anyway: an array that only exists
until the next boot is worse than no array.

```sh
# DESTRUCTIVE: partition table edit on live disks. take the layout from
# `sfdisk -F` output, do not guess device names or partition numbers.
# per member disk: create one partition in the free space, type fd00.
sudo partx -a /dev/<disk>
sudo mdadm --create /dev/md4 --level=1 --raid-devices=2 /dev/<diskA>pN /dev/<diskB>pN
sudo mdadm --detail --scan | sudo tee -a /etc/mdadm/mdadm.conf
sudo update-initramfs -u
```

**A3, shrinking the ext4 root to make room. do not do this.** ext4 cannot shrink
while mounted, and this filesystem is `/`. it means booting phantom into the
provider's rescue image, shrinking the filesystem, shrinking the md array and
its member partitions, and hoping the box boots. hours of downtime and a real
chance of not coming back, to buy a latency property. if A1 and A2 are both
unavailable, take option B now and option C later.

### option B: a loopback-backed xfs image file on the existing root

a preallocated file on ext4, formatted xfs, mounted via loop.

honest read on the "is this real reflink" question: **yes, it is.** the loop
device carries a genuine xfs with its own allocator and its own metadata, so
`cp --reflink=always` succeeds, shares extents, and the pause window collapses
exactly as it would on a bare device. the reservations are operational, not
semantic:

- **it must be preallocated, not sparse.** a sparse image lets the outer ext4
  run out of blocks while the inner xfs still believes it has space, which
  surfaces as IO errors inside xfs rather than a clean `ENOSPC`. use
  `fallocate`, and accept that the space is gone from `/` the moment you do.
- **double bookkeeping.** two journals, two page cache layers, some write
  amplification. it applies to a much smaller write volume after the migration
  than before it (the rootfs copies stop writing data at all), but the memory
  images still pay it.
- **it is a fixed ceiling** that only grows in one direction (`truncate` plus
  `losetup -c` plus `xfs_growfs`, online).
- **the boot-order failure mode is nasty.** if the loop mount does not come up,
  `agent-state` is an empty directory on ext4, the agent starts happily, and
  `reattach_vms` finds nothing to reattach. section 4.5 pins this shut with
  `RequiresMountsFor=`. do not skip it, and do not use `nofail` in fstab.

no reboot required. reversible in five minutes (section 6).

### option C: a physical disk

cleanest steady state: own device, no contention with the root filesystem, no
indirection. on a rented box it is a support ticket or a reprovision, with lead
time and a reboot. it is the right long-term answer and the wrong answer to
"we want live snapshots this week".

### 3.4 recommendation

**run 2.3 first, then:**

1. if `vgs` shows free extents, take **A2**.
2. else if `sfdisk -F` shows unallocated space on every member disk, take **A1**,
   in a maintenance window that includes a reboot to prove the array reassembles.
3. otherwise take **B**, with a preallocated image and the systemd ordering in
   4.5, and file option C as the follow-up.

the default expectation, and this is an assumption to verify, is that a stock
provider soft-raid install consumed the whole disk into md arrays with no lvm
and no free extents, which lands on **B**. that is fine. the migration is buying
a latency property, not a durability property, and B buys it today with a change
that is reversible by one `umount` and one `mv`. do not let the perfect
partition layout hold up the pause window fix.

**sizing.** xfs cannot be shrunk, so this is a one-way decision.

```
size >= current agent-state usage
      + (max_vms * base rootfs size)              # divergence: reflinked extents
                                                  # stop being shared as guests write
      + (expected concurrent live snapshots * mem_size_mib)
      + 30% headroom
```

on a 3.8T root that is 93% free, 1T is a defensible starting point for B and
leaves the root plenty. write down the number you chose and why.

---

## 4. migration

`$FC_DIR` and `$STATE` below refer to the path from 2.1 and its `agent-state`
child. export them once so nothing gets retyped:

```sh
FC_DIR=/home/debian/fuse/host-agent/firecracker   # VERIFY against `systemctl cat fc-agent`
STATE="$FC_DIR/agent-state"
```

### 4.1 create the filesystem

option B only:

```sh
sudo mkdir -p /var/lib/fuse
# DESTRUCTIVE: permanently claims this much space from /. preallocated, not sparse.
sudo fallocate -l 1T /var/lib/fuse/fc-state.img
sudo chmod 600 /var/lib/fuse/fc-state.img
DEV=/var/lib/fuse/fc-state.img
```

option A: `DEV=/dev/<vg>/fc-state` or `DEV=/dev/md4`.

then, for either:

```sh
# DESTRUCTIVE: mkfs. triple-check $DEV. reflink=1 is the default on
# xfsprogs >= 5.1 but is stated explicitly so the intent is in the shell history.
sudo mkfs.xfs -m reflink=1,crc=1 -L fuse-fc "$DEV"
```

if `mkfs.xfs` rejects `-m reflink=1`, xfsprogs is too old. stop, upgrade
xfsprogs, and start this section again. a filesystem made without reflink cannot
be converted, only remade.

### 4.2 mount it somewhere temporary and prove reflink works

```sh
sudo mkdir -p /mnt/fuse-fc
sudo mount -o noatime "$DEV" /mnt/fuse-fc     # option B: add -o loop
xfs_info /mnt/fuse-fc | grep -o 'reflink=[01]'   # must print reflink=1
```

`reflink=1` in `xfs_info` says the feature bit is set. it does not say a clone
will succeed. do the practical test:

```sh
cd /mnt/fuse-fc
sudo dd if=/dev/urandom of=rl-src.bin bs=1M count=1024 status=none
df -h --output=used /mnt/fuse-fc                 # note the number
time sudo cp --reflink=always rl-src.bin rl-dst.bin
df -h --output=used /mnt/fuse-fc                 # must be ~unchanged, not +1G
sudo filefrag -v rl-dst.bin | grep -c shared     # nonzero: extents are shared
sudo rm -f rl-src.bin rl-dst.bin
cd /
```

pass criteria, all three: `cp` exits 0, it takes milliseconds not seconds, and
used space does not grow by 1G. anything else means stop, because the entire
migration is buying exactly this and nothing else.

### 4.3 stop the agent

preconditions: host cordoned, zero environments on it, `pgrep firecracker`
empty (section 2.5).

```sh
sudo systemctl stop fc-agent.service
pgrep -a firecracker            # must be empty; the agent does not stop guests on shutdown
sudo systemctl stop fc-update.timer   # keep the weekly updater out of the window
```

### 4.4 move the state

```sh
sudo umount /mnt/fuse-fc

# DESTRUCTIVE: renames the live state directory. this is the point of no
# quick return; from here, finish or roll back per section 6.
sudo mv "$STATE" "$STATE.ext4-backup"
sudo mkdir -p "$STATE"
sudo mount -o noatime "$DEV" "$STATE"     # option B: add -o loop
sudo chown root:root "$STATE"
sudo chmod 755 "$STATE"

# copy, preserving mode/owner/xattrs. rootfs images are mode 666 and that is
# load-bearing: firecracker runs as root under sudo, but the agent process
# reads the rootfs unprivileged to digest it, and the foreground dev path runs
# as the login user. -H keeps hardlinks, --numeric-ids avoids id remapping.
# the excludes are the three regenerable-and-harmful classes from 2.6.
sudo rsync -aHAX --numeric-ids --info=progress2 \
  --exclude 'ssh-control/***' \
  --exclude 'artifact-pull-tmp/***' \
  --exclude '*/fc.sock' \
  "$STATE.ext4-backup"/ "$STATE"/
```

verify the copy before trusting it:

```sh
# same set of snapshot ids, same file count
diff <(cd "$STATE.ext4-backup/snapshots" && ls) <(cd "$STATE/snapshots" && ls)

# no rootfs lost its 666
sudo find "$STATE" -name 'rootfs.ext4' ! -perm -0666 -printf '%m %p\n'   # expect no output

# every snapshot still matches the digest recorded when it was taken.
# this is the real integrity check: file_digest() covers the rootfs and the
# meta.json digest was computed on the source host at create time.
for d in "$STATE"/snapshots/*/; do
  want=$(sudo python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("digest",""))' "$d/meta.json" 2>/dev/null)
  [ -n "$want" ] || { echo "skip $(basename "$d") (no digest recorded)"; continue; }
  got=$(sudo sha256sum "$d/rootfs.ext4" | cut -d' ' -f1)
  [ "$want" = "$got" ] && echo "ok   $(basename "$d")" || echo "FAIL $(basename "$d")"
done
```

any `FAIL` line: stop and roll back. do not start the agent.

### 4.5 make the mount survive a reboot, and make the agent depend on it

by UUID, never by device name:

```sh
sudo blkid -s UUID -o value "$DEV"    # option B: blkid on the loop device, or use the file path
```

`/etc/fstab`, option A:

```
UUID=<uuid>  /home/debian/fuse/host-agent/firecracker/agent-state  xfs  noatime  0 0
```

`/etc/fstab`, option B:

```
/var/lib/fuse/fc-state.img  /home/debian/fuse/host-agent/firecracker/agent-state  xfs  loop,noatime  0 0
```

**no `nofail`.** `nofail` is what turns a failed mount into an agent that boots
onto an empty `agent-state` and reports itself healthy with zero VMs.

then pin the ordering. use a drop-in, not the main unit: `fc-agent.sh
install-service` regenerates `/etc/systemd/system/fc-agent.service` wholesale
and would eat an edit made there, but leaves drop-ins alone.

```sh
sudo mkdir -p /etc/systemd/system/fc-agent.service.d
sudo tee /etc/systemd/system/fc-agent.service.d/require-state-mount.conf >/dev/null <<'EOF'
[Unit]
# the agent creates VMS_DIR/SNAPSHOTS_DIR on startup if they are missing, so a
# failed mount would otherwise look like a healthy agent with no state at all.
RequiresMountsFor=/home/debian/fuse/host-agent/firecracker/agent-state
EOF
sudo systemctl daemon-reload
sudo findmnt --verify --verbose
```

### 4.6 note on repointing FC_DIR

this runbook mounts the new filesystem **at** `$FC_DIR/agent-state` rather than
setting `FC_DIR` to a new path. that is deliberate:

- every absolute path already recorded in `agent-state/vms/*/meta.json`
  (`rootfs`, `sock`) stays valid, so no metadata rewrite and no risk of
  `reattach_vms` relaunching firecracker against a path that no longer exists.
- the shell tooling derives its own `FC_DIR` from its location and cannot be
  repointed by an environment variable at all (section 1). keeping the checkout
  as `FC_DIR` keeps the agent and the tooling in agreement.
- `vms/<vm_id>/fc.sock` has to fit in a 107-byte `sockaddr_un`. keeping the path
  identical keeps that budget identical.

if a future change does move `FC_DIR` wholesale onto xfs (which is what fixes
`host_capacity`, see section 7), it must also rewrite the `rootfs` and `sock`
keys in every `meta.json` in the same window, and re-check the socket path
budget against the longest live vm id.

---

## 5. verification

```sh
sudo systemctl start fc-agent.service
sudo systemctl status fc-agent.service --no-pager
```

the unit appends stdout and stderr to `$FC_DIR/fc-agent.log`, so read that file
rather than only journalctl:

```sh
sudo tail -n 100 "$FC_DIR/fc-agent.log"
```

### 5.1 reattach

on startup `main()` calls `reattach_vms()` (`fc-agent.py:2016`) before it binds
the port. it walks `VMS_DIR`, leaves alone any VM whose recorded pid is alive
and whose socket exists, and relaunches every other one: teardown and recreate
the tap, re-add the DNAT forward, `start_firecracker`.

after a migration done per section 2.5, `VMS_DIR` is empty and reattach is a
no-op. that is the expected and desired outcome.

**the failure mode to look for:** a per-VM reattach failure is caught, logged,
and does not stop the agent. an agent that came up "fine" can be hiding several
dead VMs. always grep:

```sh
grep -E 'reattach (FAILED|relaunching|:)' "$FC_DIR/fc-agent.log"
```

### 5.2 agent health

```sh
set +o history
source "$FC_DIR/.env"
# /healthz is bearer-authenticated like everything else; an unauthenticated
# probe returns 401, which is not a health signal.
curl -fsS -H "Authorization: Bearer $FC_AGENT_TOKEN" http://127.0.0.1:8090/healthz
curl -fsS -H "Authorization: Bearer $FC_AGENT_TOKEN" http://127.0.0.1:8090/v1/vm
curl -fsS -H "Authorization: Bearer $FC_AGENT_TOKEN" http://127.0.0.1:8090/v1/capacity
```

`/v1/capacity` reports `storage_gb` from `shutil.disk_usage(FC_DIR).free`
(`fc-agent.py:1689`). `FC_DIR` is still on the ext4 root, so this number still
describes the root filesystem and **not** the filesystem the VM rootfs images
now live on. it is only read at host registration time, so nothing acts on it
today, but do not read it as a statement about xfs free space. `df -h "$STATE"`
is the number that matters.

### 5.3 end to end

```sh
fuse hosts uncordon <phantom-host-id>
fuse environment create ...            # per the usual smoke test
fuse snapshot create <vm-id> --comment "post-xfs smoke"
```

and confirm on the host that the snapshot cost no space, which is the actual
acceptance criterion:

```sh
df -h "$STATE"                                   # before and after the snapshot
sudo filefrag -v "$STATE"/snapshots/<snap>/rootfs.ext4 | grep -c shared   # nonzero
```

then time a live snapshot and compare the pause window against the pre-migration
baseline. if the rootfs copy still shows up as seconds of pause, something is on
the wrong filesystem: re-check with `findmnt -T` on both the vm rootfs and the
snapshot dir.

---

## 6. rollback

the ext4 copy is untouched at `$STATE.ext4-backup` until someone deletes it. do
not delete it for at least a week of normal operation.

**anything created after the cutover exists only on xfs.** rolling back after
real traffic means copying those snapshot dirs back out first, or losing them.

```sh
sudo systemctl stop fc-agent.service
pgrep -a firecracker           # destroy or kill any VM started since the cutover

# recover post-cutover artifacts first, if any, and if you want them
sudo rsync -aHAX --numeric-ids "$STATE"/snapshots/ /var/tmp/post-cutover-snapshots/

sudo umount "$STATE"
# DESTRUCTIVE: removes the (now empty) mount point directory
sudo rmdir "$STATE"
sudo mv "$STATE.ext4-backup" "$STATE"

# undo the persistence
sudo rm -f /etc/systemd/system/fc-agent.service.d/require-state-mount.conf
sudo sed -i '\#/agent-state#d' /etc/fstab      # verify the line first with grep
sudo systemctl daemon-reload
sudo findmnt --verify --verbose

sudo systemctl start fc-agent.service
sudo systemctl start fc-update.timer
```

option B leaves `/var/lib/fuse/fc-state.img` behind holding its full preallocated
size. delete it once rollback is confirmed final.

option A leaves an LV or an md array behind. harmless, but remove the
`mdadm.conf` line and re-run `update-initramfs -u` if the array is torn down, or
the next boot waits on a device that is not there.

---

## 7. what this does not fix

be clear about the scope before anyone reads the ticket title and expects more.

- **the memory image is still a full write, every time.** `snapshot_create`
  passes `snapshot_type: "Full"`, so firecracker writes the entire
  `mem_size_mib` on every live snapshot, and it writes it *inside the pause
  window*. reflink does nothing for this: those bytes have no prior extents to
  share. a 4 GiB guest costs 4 GiB written per live snapshot and a pause window
  of however long that takes on the underlying device. reflink removes the
  rootfs copy from the window and leaves the memory write standing.
- **no dedup between snapshots.** ten live snapshots of a 4 GiB VM cost 40 GiB.
  `track_dirty_pages` is enabled at boot on this branch, which is what a future
  `Diff` snapshot would need, but nothing uses it yet.
- **restore and fork do not get the win yet.** `snapshot_restore` copies with a
  plain `sudo cp` and no `--reflink` flag (`fc-agent.py:922`). that is a
  one-word change but it is not in this migration; until it lands, restore still
  full-copies even on xfs.
- **VM create does not get the win either**, because `BASE_ROOTFS` and
  `IMAGES_DIR` stay on ext4 by design (section 1). fixing it properly means
  moving `FC_DIR` itself, which also fixes the `host_capacity` reporting in 5.2.
  separate change, separate window.
- **reflinked extents stop being shared as guests write.** a filesystem that
  looks nearly empty right after a hundred snapshots can fill up later purely
  from divergence. `ENOSPC` during a live snapshot leaves a half-written memory
  image in the snapshot dir. the guest is resumed regardless (the resume is in a
  `finally`), but the junk directory stays. monitor `df` on `$STATE` and alert
  well before full.
- **guest-side behaviour is unchanged.** the rootfs images are ext4 filesystems
  inside files. reflink is a property of the host filesystem holding those
  files, not of anything the guest sees.
- **xfs cannot shrink.** whatever size you pick in 3.4 is the size, modulo
  growing it.

---

## appendix: assumptions a human must verify before running any of this

nothing here was checked against phantom. these are the specific places where
this runbook guessed:

1. **`FC_DIR = /home/debian/fuse/host-agent/firecracker`.** inferred from the
   deployment being a `/home/debian/fuse` checkout plus the documented
   clone-then-`cd host-agent/firecracker` layout. verify: `systemctl cat
   fc-agent.service`.
2. **the agent runs as a systemd unit named `fc-agent.service`, installed by
   `fc-agent.sh install-service`, `User=root`, port 8090.** if it is running
   from the foreground `nohup` path instead, the drop-in in 4.5 does nothing and
   the mount ordering has to be solved another way. verify: `systemctl
   list-units 'fc-*'` and `pgrep -af fc-agent.py`.
3. **`/` is a single ext4 on `/dev/md3`, no lvm, ~3.8T, 7% used.** given in the
   task, not verified. verify: `findmnt -T /`, `df -hT /`, `cat /proc/mdstat`.
4. **whether any unallocated disk space or lvm free extents exist.** completely
   unknown. this is what picks between options A and B. verify: section 2.3.
5. **member disk device names for option A1.** deliberately left as
   `/dev/<disk>` throughout. never guess these. verify: `sudo mdadm --detail
   /dev/md3` and `lsblk`.
6. **xfsprogs is new enough to default to `reflink=1`.** debian 12 ships 6.1,
   which is fine; an older host is not. verify: `mkfs.xfs -V`.
7. **the 1T sizing for option B.** a placeholder. redo the arithmetic in 3.4
   with the real snapshot retention policy and real guest memory sizes.
8. **the current snapshot store contents and size**, and whether every snapshot
   record carries a `digest` (older or hand-made ones may not, and the check in
   4.4 skips those rather than failing them). verify: section 2.4.
9. **that phantom has zero long-lived VMs nobody is willing to destroy.** if
   that is false, section 2.5 does not apply as written and this needs
   rethinking before the window is booked.
10. **the weekly `fc-update.timer` is installed.** 4.3 stops it. if it is not
    installed that command is a harmless no-op, but confirm nothing else
    (`fc-update.service`, a cron entry) can rebake or restart the agent
    mid-migration. verify: `systemctl list-timers --all`.
