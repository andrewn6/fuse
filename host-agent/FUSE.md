# Fuse + fc-agent (host toolchain)

This directory is the **Firecracker host toolchain** bundled with Fuse. `fc-agent.py` is the
per-host agent that drives one Firecracker microVM per VM; Fuse's `firecracker` provider
talks to its HTTP API (`POST /v1/vm`, `GET /v1/vm`, `DELETE /v1/vm/{id}`,
`POST /v1/vm/{id}/start-agent`-class endpoints). See `README.md` for full setup.

## Requirement

Firecracker needs hardware virtualization — the host must expose `/dev/kvm` (bare-metal
Linux or a nested-virt-enabled instance). See [`README.md`](README.md) → _Requirements_ for
the full prerequisites and a quick `/dev/kvm` check.

## Bring up a host

```bash
cd firecracker
./fc-install.sh        # fetch firecracker binary + CI kernel/rootfs + SSH key
./fc-bake-rootfs.sh    # bake the guest rootfs (agent binary is baked in)
./fc-agent.sh start    # prints FIRECRACKER_BASE_URL + FIRECRACKER_TOKEN for Fuse
./fc-agent-test.sh     # smoke test the contract
```

Point Fuse at the printed `FIRECRACKER_BASE_URL` / `FIRECRACKER_TOKEN`. Full setup,
the HTTP contract, and the networking model are in [`README.md`](README.md).

## The in-guest agent (fused vs. your own)

Fuse does not hardcode a specific in-guest daemon — it uploads a set of files into the
guest and launches a configurable command (`AgentSpec`; see `../docs/DECOUPLING.md`). The
**reference** in-guest agent is `fused`, a small Go daemon in [`../fused`](../fused):
it reads the uploaded `/fuse/manifest.json` + `/fuse/secrets.json`, binds `--listen`
(`:9550`), serves `/health` + `/v1/info`, and quiesces cleanly on SIGTERM (the drain path).

`fc-bake-rootfs.sh` bakes two inputs from `firecracker/`:

- `fused` — the agent binary. Build it with `../shared/fc-build-agent.sh` (static
  `linux/amd64`), or drop your own here to run a different agent.
- `fused.service` — the systemd unit (committed in `firecracker/`; the host fc-agent
  overrides its `ExecStart` via a drop-in on start-agent).

**To run your own in-guest agent instead of fused:** replace `fused` (+ `fused.service`) and
have your agent consume the files Fuse uploads (manifest/secrets/credentials) and accept the
same start/stop entry points. The agent is baked into the image, so re-bake whenever the
binary changes. (Fuse's `AgentSpec.DownloadURL` / the `/start-agent` `download_url` field can
alternatively fetch the binary from a URL at boot — e.g. a GitHub release of `fused` — but
the default model here is bake-every-time.)
