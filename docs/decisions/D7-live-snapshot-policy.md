# D7: live snapshots are opt-in, restore-in-place, and host-pinned

status: accepted (2026-08-20)
sibling to: D4 (snapshot/fork refusal for gpu passthrough), D6 (MIG slices and
the snapshot/fork guardrail)
related: issue #63 (snapshot artifacts in object storage)

## context

until this change every fuse snapshot was a rootfs copy. `snapshot_create`
quiesced the guest filesystem over ssh and ran `cp --reflink=auto`, and
`snapshot_restore` swapped that rootfs back under a cold boot. the host-agent
wire had an `include_ram` field that nothing read.

that is enough for a rollback point and not enough for anything whose value is
in memory: a warmed cache, a loaded model, a jit that has finished warming up, a
debugger sitting on a breakpoint. firecracker can checkpoint all of it
(`PUT /snapshot/create` with `snapshot_type: Full`, `PUT /snapshot/load` with
`resume_vm: true`), so the capability was there for the taking.

the question was not whether to implement it. it was what shape it should have,
because the naive shape ("snapshots now include memory") is wrong in at least
four different directions: cost, blast radius on a running customer vm, what
happens when the artifact is copied somewhere else, and what happens when two
vms are resumed from one memory image.

## decision

live snapshots ship as an **opt-in flag on the existing snapshot verb**:
`live: true` on the wire, `--live` on the cli, `kind: "disk" | "live"` recorded
on every snapshot. specifically:

1. **disk stays the default and the fallback.** a caller who does not know about
   live, or a host that cannot take one, keeps getting a disk snapshot.
2. **restore branches on the recorded kind, not on a caller argument.** restore
   takes no `live` flag.
3. **live snapshots can only be restored in place.** fork and cross-host
   artifact pull read a snapshot's rootfs and nothing else, so they see the disk
   half of a live snapshot and cold-boot it.
4. **gpu environments are excluded from both kinds**, and the exclusion stays
   structural (the qemu env does not implement the snapshot interfaces) rather
   than becoming a runtime check.

## rationale

### why live is opt-in rather than the default

three costs, each of which a caller who did not ask for live should not pay.

**size.** firecracker writes the full memory image on every create. there is no
sparseness, no incremental encoding, and no dedup between snapshots of the same
vm: an 8 GiB environment produces an 8 GiB memory file per snapshot, and those
bytes are dense and compress badly. the per-tenant byte quotas in the control
plane were calibrated when a snapshot was a rootfs copy. flipping the default
would silently multiply every tenant's snapshot footprint by roughly its memory
size and start returning quota conflicts on workflows that had been fine for
months. an opt-in flag makes that a thing a user chose.

**a pause window on a running customer vm.** both halves of a live snapshot are
taken with the guest stopped, and they have to be: the memory image contains the
guest's own page cache and in-flight writes, so a rootfs copied after the resume
would have drifted from the memory about to be restored on top of it. the guest
would wake up believing things about a filesystem that no longer says them. so
the pause is not an implementation detail to be optimised away later, it is the
correctness property. its duration depends on the filesystem: with reflink (xfs
`reflink=1`) the rootfs copy inside the window is O(1) metadata and the pause is
just the memory write, without it the pause covers a full multi-gigabyte byte
copy. see `docs/runbooks/xfs-reflink-migration.md`. defaulting to live would mean
every existing `fuse snapshot create` against an ext4 host started hard-freezing
a running vm for seconds.

**portability, which is the subject of its own section below.**

there is a fourth, smaller reason: a live snapshot needs a live firecracker
process to ask for the memory image. `snapshot_create` refuses `live=True` on a
vm with no socket (409) rather than quietly downgrading. a silent downgrade would
be the worst of both worlds, a caller who asked to freeze a guest and got a cold
boot back has no way to find out except by restoring and looking.

`kind` is written into the snapshot record at create time and read back from
there, never inferred from which files happen to be present. a create that ran
out of disk leaves a directory with a rootfs and no memory image, which is
byte-for-byte indistinguishable from a deliberate disk snapshot. inferring would
turn a failed live snapshot into a silent cold boot at restore time, which is
exactly the failure the explicit flag was meant to prevent.

### why restore-in-place only

restore-in-place is the case where the answer to "what network does the resumed
guest expect to find" is free. it is a restore onto the same vm, so `setup_tap`
derives the same tap name, host ip, guest ip and mac from the same
`meta["index"]` the frozen guest was using. the device the memory image expects
is the device that now exists, and nothing has to be told anything.

fork is not that case, and making it work would need two things that do not
exist yet.

**network remapping.** a forked vm gets its own index, and therefore its own tap,
ips, mac and host port. a memory image resumed into that vm is still holding the
source's network configuration in guest memory: its interface, its addresses, its
arp state, its open sockets. firecracker's `/snapshot/load` accepts a
`network_overrides` block precisely for this, remapping the snapshotted guest
network interface onto a different host device. wiring fork to live snapshots
means constructing that block from the new vm's allocation, and it means the
guest-visible ip either changes under a running kernel or has to be preserved,
neither of which is a decision to make in passing.

**vmgenid, and this one is a security property, not a nicety.** two vms resumed
from the same memory image start with byte-identical kernel state, which includes
the state of every csprng seeded before the snapshot was taken. they will
generate the same "random" values: the same session keys, the same uuids, the
same nonces, the same tokens. that is not a cosmetic collision, it is two
environments that a third party can confuse for each other and, worse, two tls
sessions whose keys an attacker who holds one can derive for the other. the
virtual machine generation id device exists to tell the guest kernel "you have
been resumed from a checkpoint, reseed", and linux reseeds `random.c` on the
notification. supporting fork from a live snapshot means every resumed vm gets a
fresh vmgenid and it means confirming the guest kernels in the baked images
actually honour it. shipping fork-from-live without that would be shipping a
cryptographic footgun with a convenient interface.

restore-in-place dodges both: there is only ever one vm resumed from a given
memory image, and it is the same vm, on the same host, with the same network. so
the shipped scope is the subset that is correct without either mechanism, and
fork keeps reading the rootfs only. a fork of a live snapshot cold-boots, which
is the same thing a fork did before this change and therefore not a regression.

### why gpu environments are excluded, and how

nothing about live snapshots changes D4 or D6. a vfio-passed-through gpu holds
device state on the host, outside the guest's memory image, so a checkpoint of
the guest is incomplete by construction. adding a memory image makes this worse
rather than better: a disk-only snapshot of a gpu vm would at least be an honest
disk copy, whereas a memory image that omits the device state describes a guest
that believes it holds hardware in a state the host no longer has.

the enforcement mechanism matters as much as the rule. the qemu provider's
`remoteEnv` **does not implement `orchestrator.SnapshotCapable`**, and the
orchestrator reaches the snapshot path through a type assertion on that
interface. the refusal is therefore a property of the type system, not a runtime
predicate: there is no `if env.HasGPU() { return err }` for someone to forget to
update when a new snapshot kind is added, because the kind is reached through an
assertion that a gpu environment already fails.

live capability is expressed the same way. it is a **second, narrower interface**
(`LiveSnapshotCapable`, one method, `CheckpointLive`) rather than a `Live` field
on the existing one or a `SupportsLive() bool` on the provider. the reasons are:

- a boolean can be true on an environment whose provider cannot honour it; an
  unimplemented interface cannot be. the compiler is doing the work.
- a backend can support disk snapshots and not live ones, and that is a real
  configuration rather than a hypothetical. keeping the capabilities separate
  lets that backend give a useful answer to `--live` ("retry without it and you
  get a disk snapshot", 501) instead of the blunt "snapshots unsupported".
- widening `SnapshotCapable` would have forced every existing implementor to
  grow a live path, including ones that cannot have one.

there is deliberately no `RestoreLive` to match. the host agent records the kind
and restore reads it, so `Restore` already restores a live snapshot correctly. a
second restore verb would let a caller name a kind that disagrees with what was
actually written, which is the one thing the recorded kind exists to prevent.

`internal/qemu/qemu_test.go` asserts that both the stub env and `remoteEnv` fail
the `SnapshotCapable` assertion, so a refactor that accidentally satisfies it
fails a test rather than shipping a snapshot endpoint for gpus. neither
implements `LiveSnapshotCapable` either, and could not usefully: it is a strict
superset of a capability they are asserted not to have.

### the portability tension with issue #63

issue #63 wants snapshot artifacts in object storage: push a snapshot, pull it
onto a different host, boot from it there. that works for a rootfs. it does not
work for a memory image, and no amount of implementation effort will make it.

a memory image is host-pinned in at least two ways. it encodes the cpu the guest
was running on, including the exact cpuid feature set the guest has already
probed and is now assuming; resuming it on a host whose cpu lacks one of those
features gets an illegal instruction somewhere unpredictable rather than a clean
refusal. and it is coupled to the firecracker version that wrote it, whose
snapshot format carries no cross-version compatibility guarantee outside a
documented support window.

so the two halves of a live snapshot have genuinely different mobility, and the
design admits it rather than papering over it:

- the recorded `digest` covers the rootfs and only the rootfs, on both kinds. the
  artifact transfer path moves one file, so the digest describes exactly what
  travels.
- a live snapshot pulled to another host arrives as its disk half and cold-boots
  there. it degrades to the thing that is portable instead of failing, and it
  degrades to the behaviour that host would have had anyway.

the alternative was to refuse to export live snapshots at all. that was rejected
because the rootfs in a live snapshot is a perfectly good rootfs, taken under a
*better* consistency guarantee than a disk snapshot's (the guest was stopped, not
merely synced), and there is no reason to withhold it.

what this does mean is that #63 cannot describe itself as "snapshot portability".
it is rootfs portability. any ui or api built on it has to be able to say that a
given live snapshot will not resume anywhere but where it was taken, and the
`kind` field is what lets it.

## consequences

- `snapshot_create` grows a `live` parameter defaulting to false; the wire field
  is `live`, the cli flag is `--live`, and every snapshot record carries
  `kind`. the stored `kind` is what the provider reports it wrote, not what was
  requested: a host agent too old to know about live snapshots answers a live
  request with a disk snapshot, and filing that as `live` would record a
  cold-booting artifact as a resumable one. the schema column defaults to
  `'disk'` so every pre-existing row is correct without a backfill, and it is
  deliberately not an enum or a `CHECK`, so an orchestrator meeting a newer
  agent that names a third kind records the unknown value rather than refusing
  the row and losing the snapshot's metadata.
- `snapshot_restore` reads `snapshot_kind` and branches. anything unreadable,
  unrecognised, or predating this change is `disk`: restoring a live snapshot as
  a disk one costs a cold boot, restoring a disk one as live would hand
  firecracker a memory file that does not exist.
- the firecracker api timeout for snapshot calls is separated from the general
  one (`FC_AGENT_SNAPSHOT_API_TIMEOUT`, default 600s). the general 5s budget
  would turn a slow memory write into a soft 599 that reads like a socket
  problem rather than a snapshot still in progress.
- a live restore resyncs the guest clock from the host. a resumed guest's clock
  is frozen at snapshot time and the kernel never re-reads the rtc because it
  never reboots, so without this a resumed guest fails tls handshakes against
  certificates it decides are not yet valid. best effort, warn on failure: a
  guest with a wrong clock is degraded, a restore that already put the guest
  back is not worth failing over it.
- tenant byte quotas are now much easier to hit, and the docs say so in the
  places a user will be standing when it happens.
- the xfs migration runbook stops being an optimisation and becomes a
  prerequisite for using live snapshots at any scale on a host with running
  customer vms.

## what this decision does not block

fork from a live snapshot is deferred, not refused on principle. the path is:
construct `network_overrides` from the forked vm's allocation, confirm the guest
kernels honour vmgenid and wire a fresh one per resume, then let `fork_vm` pass
the memory image through instead of dropping it. that work needs the vmgenid
half validated on a real guest before any of the rest is worth writing.

cross-host live restore is a harder version of the same thing and additionally
needs a host-compatibility check (cpu model, firecracker version) that nothing
currently records. it is not on the roadmap.

nothing here reopens D4 or D6. gpu environments remain refused for both kinds.
