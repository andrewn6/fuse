# Computer-use desktop image

**Status: proposed. None of this is implemented.** This document is a reference for a
possible default image, not a description of the current tree. The base rootfs today is
minimal (see [`host-agent/firecracker/fc-bake-rootfs.sh`](../host-agent/firecracker/fc-bake-rootfs.sh))
and contains no graphical stack of any kind.

## Why it would exist

Two audiences want the same image. An agent driving a computer needs a screen to
screenshot and a pointer to move. A human watching that agent needs the same session
rendered somewhere they can open. Both reduce to "run a headless graphical session inside
the guest and expose two interfaces onto it".

Fuse is a good host for this specifically because a desktop is expensive to build and
cheap to copy. Baking it as a build artifact means cold start is an artifact seed rather
than an `apt-get`, and `fork` yields N identical desktops with the browser profile already
warm.

## What the image contains

| Layer | Contents | Why |
| --- | --- | --- |
| Display | Xvfb at one fixed resolution, a lightweight WM (labwc, i3, or mutter), DejaVu + Noto fonts including CJK and emoji, a clipboard manager | Without a WM, apps stack unmanaged on the X root window. Missing fonts render as boxes and the model reads the screenshot wrong. |
| Input/output | `xdotool`, `scrot` or `grim`, `xclip`, optionally `wmctrl` | These are the primitives every computer-use action compiles down to. |
| Apps | Chromium with a pre-seeded profile, terminal emulator, file manager, text editor | Most computer-use work is browser work. The rest is filler that makes the desktop feel real. |

## The tool surface

The computer-use tool schema (`screenshot`, `left_click`, `type`, `key`, `scroll`,
`cursor_position`) maps close to 1:1 onto `xdotool` and `scrot` invocations. The
translation layer is a dispatcher, not a subsystem.

Two places it could live:

1. **Over the existing exec path.** `fc-agent.py` already drives guests over ssh, and
   `SSH_BASE` sets `ControlMaster=auto` with `ControlPersist=60s`
   ([`fc-agent.py:368`](../host-agent/firecracker/fc-agent.py)), so calls after the first
   reuse a multiplexed master. Per-action overhead is tens of milliseconds, not a fresh
   handshake. Costs nothing to build and is enough to prove the loop.
2. **In-guest HTTP.** A small surface in `fused` alongside the existing `/health` and
   `/v1/info` ([`fused/main.go:173`](../fused/main.go)). One round trip per action, and
   screenshots can be encoded and downscaled in-guest rather than shipped as full PNGs.

Option 1 first. Option 2 is the upgrade if the screenshot loop turns out to be the
bottleneck, which it probably will: the cost is image bytes and base64, not connection
setup.

## What Fuse already provides

- **Publishing the viewer.** The Fusefile `expose` block is already validated with named
  ports ([`internal/fusefile/parse.go:252`](../internal/fusefile/parse.go)) and resolves to
  published `Endpoint`s ([`internal/orchestrator/provider.go:119`](../internal/orchestrator/provider.go)).
  Putting x11vnc + noVNC behind a URL is a Fusefile line, not orchestrator work.
- **Baking the image.** `fuse build` produces a `SnapshotModeBuild` artifact that later
  environments seed from, and the layer cache keys on the setup steps.
- **Copies.** `fork` boots from the source's rootfs, so a logged-in, profile-warm desktop
  can be duplicated instead of rebuilt.

## What is actually hard

Everything above is packaging. These three are not:

1. **Resolution is not a free parameter.** Model click accuracy degrades above roughly
   WXGA. The image should bake one blessed resolution. Exposing it as a knob produces
   silent coordinate drift that presents as model failure rather than as a config error.
2. **First-launch Chromium in a microVM.** Needs `/dev/shm` sized correctly, software
   rendering, and a pre-seeded profile. Otherwise every fresh session opens on a first-run
   dialog and the agent spends its opening actions dismissing it. The default `RamMB` in a
   Fusefile should be checked against a real Chromium session before this is called done.
3. **"Good" is taste, not a package list.** Window placement, clipboard behaviour, fonts,
   sensible default apps. This is an iteration loop against real agent runs. It is the
   difference between a demo and something usable, and it is most of the work.

## Cost this imposes elsewhere

A desktop rootfs runs roughly 2 GB against the current few hundred megabytes. That is fine
on a single host, but it is 2 GB crossing the wire the first time a task lands somewhere
new, which pushes directly on the artifact-move and migration paths. Worth measuring before
it becomes a surprise.

## Sketch

Not validated, illustrative only:

```yaml
name: desktop

spec:
  cpus: 2
  ram_mb: 4096
  storage_gb: 20

setup:
  - run: apt-get update && apt-get install -y --no-install-recommends
      xvfb x11vnc novnc websockify labwc
      xdotool scrot xclip
      fonts-dejavu fonts-noto-core fonts-noto-color-emoji fonts-noto-cjk
      chromium
  - run: /opt/seed-chromium-profile.sh

expose:
  - port: 6080
    as: desktop
```

## Open questions

- Does the dispatcher belong in `fused` (in-guest, versioned with the image) or in the
  host agent (out-of-guest, versioned with the fleet)?
- Screenshot encoding and downscale policy: what does the model actually need, and what
  is the smallest payload that preserves click accuracy?
- Is the pre-seeded browser profile part of the image, or a separate layer so credentials
  never enter a shared artifact?
- X11 now versus Wayland later. Wayland is the better long-term target but `xdotool` and
  the surrounding tooling are X11-shaped.
