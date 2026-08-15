# fc-agent

The **Fuse host agent for Firecracker**. Runs on each Firecracker host, drives one
microVM per VM, and speaks the HTTP contract Fuse's `firecracker` provider expects
(`POST /v1/vm`, upload/exec, `start-agent`, snapshots). Fuse is the control plane; `fc-agent`
is the per-host worker.

## Requirements (read first)

Firecracker needs **hardware virtualization (KVM)**. You must run `fc-agent` on a host that
exposes `/dev/kvm`:

- **Bare-metal Linux**, or a cloud instance type with **nested virtualization** enabled
  (e.g. GCP `*-metal` / nested-virt images, AWS `*.metal`, bare-metal providers like Equinix
  / Hetzner dedicated). It will **not** run inside an ordinary container or a VM without
  nested virt.
- Confirm before you start: `ls -l /dev/kvm` and `[ -r /dev/kvm ] && [ -w /dev/kvm ] && echo ok`.
  If `/dev/kvm` is missing, the host can't run Firecracker.
- Linux only (the agent shells out to `ip`, `iptables`, `firecracker`, SSH). `x86_64` today
  — the baked rootfs pulls `amd64` podman/iptables.
- Needs `sudo` (TAP devices, iptables, mounting the rootfs to bake), plus `curl`, `tar`,
  `ssh`, and `iptables` on the host.

## Layout

```
firecracker/        # everything to run a Firecracker host — see below
qemu/                # everything to run a QEMU/GPU host — see below
shared/              # fc-build-agent.sh, the guest-agent build step used by both backends
```

```
firecracker/
  fc-agent.py         # the agent — one firecracker process per VM, SSH for guest ops
  fc-agent.sh         # start/stop/restart/log/env
  fc-agent-test.sh    # end-to-end smoke test against the contract

  fc-install.sh       # fetch firecracker binary, kernel, base rootfs, SSH key
  fc-up.sh / fc-down.sh / fc-ssh.sh / fc-status.sh / fc-test.sh / fc-expose.sh
                      # manual helpers for a single VM (pre-agent; still useful for debugging)
```

Runtime-only (ignored in git), written into whichever of `firecracker/` or `qemu/` you're
working in:

```
vmlinux.bin         # guest kernel
rootfs.ext4         # base Firecracker CI rootfs
rootfs-fused.ext4   # baked rootfs with fused + systemd unit
ubuntu.id_rsa       # SSH key for root@<guest>
fused               # binary (used only for baking the rootfs)
agent-state/        # per-VM metadata, rootfs copies, snapshots
.fc-agent.env       # bearer token (generated on first start)
```

## Setup (one command)

On a host that meets the requirements above, `bootstrap` does everything: host
deps, firecracker, the agent service, a local Postgres, the orchestrator (control
plane), the weekly auto-update timer, the guest agent + rootfs bake, and it
self-registers the host. Then it prints the token and the exact connect line.

```bash
git clone <this repo> ~/fc && cd ~/fc/host-agent/firecracker
sudo ./fc-agent.sh bootstrap
```

It is idempotent - safe to re-run. Flags: `--no-updater` (skip the auto-update
timer), `--no-register` (don't self-register). After it finishes, drive the host
from your laptop with the printed `fuse connect http://<host>:8080 --token <token>`.

Everything below is the manual, step-by-step equivalent - use it when you want to
run or skip individual stages.

## Setup (manual, step by step)

On a host that meets the requirements above:

```bash
git clone <this repo> ~/fc && cd ~/fc/host-agent/firecracker

# 1. Fetch firecracker binary + CI kernel + base rootfs + SSH key.
./fc-install.sh

# 2. Build the reference in-guest agent (produces ./fused). Needs Go.
#    To run your own agent instead, drop your binary here as `fused` and skip this.
../shared/fc-build-agent.sh

# 3. Bake the guest rootfs (rootfs-fused.ext4). Bakes in `fused` + fused.service.
#    Re-run whenever the agent binary changes — the agent is baked into the image.
./fc-bake-rootfs.sh

# 4. Start the agent. Prints FIRECRACKER_BASE_URL + FIRECRACKER_TOKEN.
./fc-agent.sh start

# 5. Smoke-test the contract end to end.
./fc-agent-test.sh
```

Point Fuse at the printed values:

```
FIRECRACKER_BASE_URL=http://<host>:8090
FIRECRACKER_TOKEN=<generated>
```

## QEMU GPU host setup

GPU environments use QEMU/KVM with whole IOMMU groups passed through via VFIO.
This requires bare-metal Linux, enabled IOMMU, `/dev/kvm`, and a GPU that can be
detached from its host driver. GPU environments do not support snapshots or forks.

```bash
cd host-agent/qemu

# install qemu, ovmf, a base cloud image, and the ssh keypair
./qemu-install.sh

# build the in-guest agent and bake a cuda image with an explicit driver branch
../shared/fc-build-agent.sh
./qemu-bake-cuda-rootfs.sh 550

# inspect groups, then bind every member of each gpu group to vfio-pci
./qemu-vfio-bind.sh --list
sudo ./qemu-vfio-bind.sh

# install and start the host agent
sudo cp qemu-agent.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now qemu-agent
```

The bind step writes `vfio-inventory.txt`, which `qemu-agent.py` consumes. Each
line represents an indivisible IOMMU group:

```text
<gpu_count> <gpu_kind> <pci_slot> [<pci_slot> ...]
1 a100 0000:17:00.0 0000:17:00.1
```

The count includes GPU display functions only. The PCI list includes every
function in the group, such as a GPU's companion audio function, because QEMU
must attach the complete group.

Set `QEMU_AGENT_TOKEN` in the service environment, then register the host using
the inventory's count and exact kind:

```bash
fuse host register gpu-1 \
  --url http://gpu-host:8091 \
  --token "$QEMU_AGENT_TOKEN" \
  --backend qemu \
  --gpus 1 \
  --gpu-kind a100 \
  --cpus 16 \
  --ram-mb 65536 \
  --storage-gb 500 \
  --max-vms 4
```

Request the device without naming a virtualization backend in the Fusefile:

```yaml
resources:
  gpu: 1
  gpu_kind: a100
```

The normal image path is a CUDA-capable qcow2 supplied by the operator. The
reference bake creates `rootfs-cuda.qcow2`, installs the generated SSH public
key as root's `authorized_keys`, and extracts `vmlinuz.bin` for QEMU.

Hardware validation is explicit:

```bash
./qemu-agent-test.sh
FUSE_GPU_E2E=1 FUSE_GPU_KIND=a100 ./qemu-e2e.sh
```

The e2e registers a QEMU host, creates a GPU environment, runs `nvidia-smi` in
the guest, verifies snapshots are refused, and confirms destroy removes the
QEMU VM. Without `FUSE_GPU_E2E=1` and a reachable GPU agent, it reports `SKIP`.

Open these at your cloud / external firewall:

- `8090/tcp` — the agent's HTTP API
- `19551–19799/tcp` — the per-VM guest-agent DNAT range

## Contract

All routes under `/v1/vm`, bearer auth (`Authorization: Bearer $TOKEN`), JSON in/out.

| Method | Path                      | Purpose                                                                                                                                                                                                                                                    |
| ------ | ------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| POST   | `/v1/vm`                  | Create a microVM. Body: `{name,cpus,memory_mb,storage_gb,region}`. Returns `{vm_id,url}`.                                                                                                                                                                  |
| GET    | `/v1/vm/{id}`             | `{vm_id,url}`                                                                                                                                                                                                                                              |
| GET    | `/v1/vm?prefix=`          | `{vms:[{vm_id,url}]}` — prefix match on `name`                                                                                                                                                                                                             |
| DELETE | `/v1/vm/{id}`             | Tear down, free TAP + DNAT.                                                                                                                                                                                                                                |
| POST   | `/v1/vm/{id}/upload`      | `{path, content_b64}` — writes into the guest (mkdir -p).                                                                                                                                                                                                  |
| POST   | `/v1/vm/{id}/exec`        | `{cmd:[...]}` — returns `{exit_code, stdout (b64), stderr (b64)}`.                                                                                                                                                                                         |
| POST   | `/v1/vm/{id}/start-agent` | Preferred. `{manifest_path, secrets_path, gateway?, extra_args?, tls_cert_path?, tls_key_path?, auth_token?, download_url?, binary_path?, listen?}` — optionally fetches the agent binary via `download_url`, then writes a systemd drop-in and starts it. |
| POST   | `/v1/vm/{id}/start-surfd` | Frozen legacy wire. Same as `start-agent` with fused defaults and no `download_url`. Fuse falls back to this on a 404 from `start-agent`.                                                                                                                  |
| POST   | `/v1/vm/{id}/snapshot`    | `{comment, include_ram}` — disk-only (`include_ram` ignored).                                                                                                                                                                                              |
| GET    | `/v1/vm/{id}/snapshots`   | `{snapshots:[...]}`                                                                                                                                                                                                                                        |
| POST   | `/v1/vm/{id}/restore`     | `{snapshot_id, include_ram}` — stops fc, swaps rootfs, reboots VM.                                                                                                                                                                                         |
| GET    | `/v1/capacity`            | `{cpus, ram_mb, storage_gb}` — real cpu count, total ram, and free disk on this host. `fuse host register` probes this instead of trusting operator-declared flags.                                                                                        |

`url` is `<public_host>:<host_port>`, DNAT'd to the guest's `9550`. Host port =
`19550 + vm_index`. Public host is auto-detected, preferring IPv4 on a
dual-stack host and falling back to IPv6; an IPv6 host is bracketed
(`[2607:5300:203:535a::]:19554`) so the authority stays well-formed. Override
with `PUBLIC_HOST=...` in the env — an IP or a hostname. Anything else fails at
agent startup rather than emitting a URL nobody can reach.

## Networking model

- One TAP per VM (`fcv<N>`), /30 subnet `10.200.<N>.0/30`, host `.1`, guest `.2`.
- Host iptables: MASQUERADE for egress, FORWARD accept for the TAP, per-VM
  PREROUTING DNAT `<public_host>:<port> -> <guest>:9550` to the guest agent.
- Cloud SG / external firewall must allow inbound TCP to the agent port (8090)
  and the DNAT range (19551–19799 at current defaults).

## Baking the rootfs (`rootfs-fused.ext4`)

`./fc-bake-rootfs.sh` builds the guest image. **The in-guest agent is baked into the
image**, so you must (re-)bake before first start and whenever the agent binary changes.
The reference agent is `fused`, built from this repo's [`fused`](../fused) by
`../shared/fc-build-agent.sh` (output: `host-agent/firecracker/fused`); its systemd unit
`fused.service` ships alongside it in `host-agent/firecracker/`. To bake your own agent
instead, drop your binary here as `fused` (and replace `fused.service`) — see
[`FUSE.md`](FUSE.md).

Built on top of the Firecracker CI Ubuntu 22.04 rootfs. Contents injected:

- `/usr/local/bin/fused` — static Go binary, linux/amd64 (the reference agent)
- `/usr/local/bin/podman` + crun/runc/conmon/netavark/pasta/fuse-overlayfs —
  [`mgoltzsche/podman-static`](https://github.com/mgoltzsche/podman-static) v5.8.1
- `iptables` + libxtables + `/usr/lib/x86_64-linux-gnu/xtables/*` extracted from
  Ubuntu 22.04 (`iptables` deb). `/usr/sbin/iptables` etc. re-symlinked to
  `xtables-legacy-multi` because the kernel has no nftables.
- `/etc/ssl/certs/ca-certificates.crt` (copied from host)
- `/etc/systemd/system/fused.service` with drop-in slot; `start-surfd` writes
  a drop-in with `--manifest/--secrets/--gateway/--vm-id` and `systemctl start`.
- `/etc/containers/storage.conf` — native kernel overlay (kernel has
  `CONFIG_OVERLAY_FS=y` but no `CONFIG_FUSE_FS`, so fuse-overlayfs is unused).
- `/etc/containers/containers.conf`:
  ```toml
  [containers]
  netns = "host"
  [network]
  firewall_driver = "none"
  ```
  **Why host netns**: the Firecracker CI kernel (`vmlinux-5.10.223`) ships
  without `CONFIG_NETFILTER_XT_MATCH_COMMENT`, `CONFIG_FUSE_FS`, or
  `CONFIG_NF_TABLES`. Netavark unconditionally emits `-m comment` rules and
  fails. Forcing new containers into the host's network namespace sidesteps
  netavark entirely. Isolation is provided by the microVM itself. If you need
  per-container network isolation inside one VM, build a custom kernel that
  enables the missing netfilter matches.
- `/var/tmp`, `/var/lib/containers`, `/run/containers` — pre-created (the CI
  rootfs was missing `/var/tmp`, which breaks image pulls).

**Known limitations of this rootfs**:

- `apt` is unusable (the CI rootfs has an empty `/var/lib/dpkg/status`).
  Customize via the mounted ext4 from the host, not from inside the guest.
- `podman run` without `--network=host` falls back to host netns anyway because
  of the config. Bridged per-container networks are not supported.
- Kernel lacks fuse and nftables (see above).

## Baking the desktop rootfs (`rootfs-desktop.ext4`)

`./fc-bake-desktop-rootfs.sh` layers a graphical session onto `rootfs-fused.ext4`
(bake that first). It exists so a Fuse environment can be driven by a computer-use
agent: something has to render pixels, take clicks, and answer screenshots.

Contents added on top of the base image:

- Xvfb on display `:1` (software framebuffer — no GPU, no extra devices), default
  geometry `1024x768x24`, override at bake time with `FC_DESKTOP_GEOMETRY`
- mutter (window manager) and tint2 (panel), matching Anthropic's computer-use
  reference image so behaviour is comparable
- `xdotool`, `scrot`, `xclip` — the input/capture/clipboard primitives
- Firefox ESR (from the mozillateam PPA; the archive `firefox` on 22.04 is a
  snap shim and snaps cannot run in the guest), pcmanfm, xterm
- systemd units `fuse-display` / `fuse-wm` / `fuse-panel`, enabled at boot, with
  a `fused.service` drop-in ordering the agent after the desktop

The guest cannot `apt-get` (see the known limitations above), so the desktop
stack is resolved inside an `ubuntu:22.04` container on the host and the
resulting files are extracted into the image — the same trick the base bake uses
for iptables, just bigger. Expect the image to land in the 1.5–2 GB range
against the base's few hundred MB, and budget it accordingly on first
distribution to a new host.

The bake ends by booting Xvfb inside a chroot of the image and capturing a real
screenshot, so a bundle with a missing library fails at bake time instead of at
first VM boot (`FC_DESKTOP_SKIP_DISPLAY_CHECK=1` skips this on hosts where
chroot X refuses to start).

To use it, place it in the images dir and name it from a Fusefile:

```bash
mkdir -p images && cp rootfs-desktop.ext4 images/desktop.ext4
```

```yaml
image: desktop
resources:
  memory: 4GB # a browser session does not fit the 1GB default
```

## Re-baking

Re-run `./fc-bake-rootfs.sh` — it rebuilds `rootfs-fused.ext4` idempotently from
`rootfs.ext4` + your `fused` binary + podman-static + the iptables bundle. New VMs pick up
the new image on their next `create`; existing VMs keep their per-VM copy until recreated.

Under the hood it does roughly this (kept here as a reference for adapting the bake to your
own agent):

```bash
cp rootfs.ext4 rootfs-fused.ext4
sudo truncate -s 4G rootfs-fused.ext4
sudo e2fsck -f -y rootfs-fused.ext4 && sudo resize2fs rootfs-fused.ext4
sudo mount -o loop rootfs-fused.ext4 /tmp/fcroot

# fused + systemd unit + /fuse + CA bundle + container dirs
sudo cp fused /tmp/fcroot/usr/local/bin/fused && sudo chmod 755 $_
sudo cp fused.service /tmp/fcroot/etc/systemd/system/
sudo ln -sf /etc/systemd/system/fused.service \
  /tmp/fcroot/etc/systemd/system/multi-user.target.wants/fused.service
sudo mkdir -p /tmp/fcroot/fuse /tmp/fcroot/var/tmp \
  /tmp/fcroot/var/lib/containers /tmp/fcroot/run/containers
sudo chmod 1777 /tmp/fcroot/var/tmp
sudo cp /etc/ssl/certs/ca-certificates.crt /tmp/fcroot/etc/ssl/certs/

# podman-static
curl -fsSL -o /tmp/podman.tgz \
  https://github.com/mgoltzsche/podman-static/releases/download/v5.8.1/podman-linux-amd64.tar.gz
sudo tar -xzf /tmp/podman.tgz -C /tmp/fcroot --strip-components=1

# iptables bundle — extracted from ubuntu:22.04 via podman on host
# (see history in this README / fc-agent session for the exact tar recipe)
sudo tar -xf iptables-full.tar -C /tmp/fcroot
for n in iptables iptables-save iptables-restore ip6tables ip6tables-save ip6tables-restore; do
  sudo ln -sf xtables-legacy-multi /tmp/fcroot/usr/sbin/$n
done

# containers.conf + storage.conf (see above)
sudo tee /tmp/fcroot/etc/containers/containers.conf ...
sudo tee /tmp/fcroot/etc/containers/storage.conf ...

sudo umount /tmp/fcroot
```

## Operating

The commands below run from `host-agent/firecracker/`.

```bash
./fc-agent.sh start               # launch agent on :8090, print env
./fc-agent.sh stop                 # stop
./fc-agent.sh restart              # stop+start; re-attaches to running VMs
./fc-agent.sh log                  # tail agent log
./fc-agent.sh env                  # print env keys for an already-running agent
./fc-agent-test.sh                 # contract smoke test

# systemd integration (optional, for long-lived hosts)
./fc-agent.sh install-service      # enable fc-agent.service (survives reboot)
./fc-agent.sh uninstall-service
./fc-agent.sh install-orchestrator # co-locate the orchestrator here (see below)
```

To pick up a new agent binary, re-run `./fc-bake-rootfs.sh` and `./fc-agent.sh restart`.

## Co-locating the orchestrator (control plane)

The orchestrator (`bin/fuse`, built from [`orchestrator/`](../orchestrator)) is a plain HTTP client to
this agent, so the simplest production setup runs it on the same host and talks to the agent
over loopback. One command installs it as a systemd service next to the agent:

```bash
./fc-agent.sh install-orchestrator
```

It resolves the `orchestrator` binary (from `ORCH_BIN_SRC=/path`, a local `./orchestrator`,
an existing `/usr/local/bin/orchestrator`, or the latest GitHub release), writes
`/etc/default/orchestrator` prefilled with `FIRECRACKER_BASE_URL=http://127.0.0.1:8090`, this
host's `FIRECRACKER_TOKEN`, and freshly generated `ORCH_AUTH_TOKEN` + `TOKEN_ENCRYPTION_KEY`
(it never overwrites an existing file), then installs and enables `orchestrator.service`
(ordered after `fc-agent.service`).

Auth is on by default, so the orchestrator refuses to boot until you provide a Postgres
`DATABASE_URL`. The installer leaves that as a placeholder and does not start the service
until you fill it in:

```bash
sudoedit /etc/default/orchestrator   # set DATABASE_URL=postgres://...
sudo systemctl start orchestrator    # the schema is created on first boot
```

It then prints the `FUSE_BASE_URL` + `FUSE_TOKEN` for the dashboard
([`fuse-frontend`](../../fuse-frontend)); the dashboard's `FUSE_TOKEN` must match the
orchestrator's `ORCH_AUTH_TOKEN`. Open `8080/tcp` (or terminate TLS at a proxy and point the
dashboard there). Remove it with `./fc-agent.sh uninstall-orchestrator`.

To run the orchestrator somewhere else instead (one control plane scheduling across many
hosts), skip this and point its `FIRECRACKER_BASE_URL` at each host's agent URL from
`./fc-agent.sh env`.

## Re-attach on restart

On startup the agent walks `agent-state/vms/` and for each VM:

- pid alive + socket present → reuse (no-op)
- pid dead / socket gone → recreate the TAP, re-add the DNAT rule, relaunch
  firecracker with the same config (same vm_id, guest IP, URL)

That means you can `systemctl restart fc-agent` without losing VMs, and
host reboots transparently bring everything back (as long as the systemd unit
is installed).

## Auto-update (self-host)

`fc-update.sh` keeps a self-hosted box on the latest GitHub release. It compares the baked
`fused --version` to the newest `folsomintel/fuse` tag and, when there's a newer one, pulls the
repo, downloads the new `fused`, re-bakes the rootfs, and restarts the agent:

```bash
./fc-update.sh                 # one-shot: update now if a newer release exists (no-op if current)
./fc-agent.sh install-updater  # weekly systemd timer (Mon 04:00 UTC ±30m)
./fc-agent.sh uninstall-updater
```

Public repo — no token needed. Optional `host-agent/firecracker/.fc-updater.env` is sourced if present, e.g.
`GH_TOKEN=...` (dodge API rate limits) or `FUSE_ORCH_SERVICE=orchestrator.service FUSE_ORCH_BIN=/usr/local/bin/orchestrator`
to also update a co-located orchestrator. Override the source repo with `FUSE_REPO=owner/name`.

## End-to-end test

`fc-e2e.sh` boots the real `./bin/fuse` binary and drives a complete environment
lifecycle over HTTP (create → get → list → snapshot → restore → drain →
destroy), asserting each step:

```bash
# hermetic (no host needed) — uses the in-memory stub provider
./fc-e2e.sh

# against a real Firecracker host (network + a Fuse-compatible host required).
# reads FIRECRACKER_BASE_URL / FIRECRACKER_TOKEN from ../../.env
FUSE_E2E_REMOTE=1 ./fc-e2e.sh
```

Exit 0 means everything works; any failed assertion exits non-zero. For GPU
environments, see `qemu-e2e.sh` (registers a QEMU host, deploys a GPU environment,
verifies the device is visible in the guest).

## State

State lives under `agent-state/vms/<vm_id>/` — safe to `rm -rf` if the agent is
stopped and you want a clean slate.
