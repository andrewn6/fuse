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

## Content-addressed artifacts (host-to-host)

A snapshot taken by `POST /v1/vm/{id}/snapshot` is a rootfs file copy. The agent hashes it
inline at creation and records the hex sha256 as `digest` in the artifact's `meta.json` **and
on the create response**:

```json
{
  "snapshot_id": "snap-1712345678-ab12cd",
  "digest": "<hex sha256>"
}
```

The response is the only place a caller ever sees the digest. It is computed while the
artifact is written and nothing upstream recomputes it, so a response that omits it leaves the
control plane with no integrity value to check a later peer transfer against.

That digest is an **integrity check on those exact bytes only**. It is not a cross-build
identity and can never be a cache key: two builds of the same recipe produce different rootfs
bytes (timestamps, inode ordering, package caches). Dedup is not a goal here. The cache key is
the Fusefile layer key, which lives in the orchestrator.

There is no object storage in Fuse, so an artifact built on host A is moved to host B by the
two hosts talking directly.

### `GET /v1/artifacts/{digest}`

Streams the artifact rootfs (`application/octet-stream`, exact `Content-Length`, plus
`X-Fuse-Artifact-Digest`). Unknown digest is `404`.

This is the **only** endpoint that is not authenticated with `Bearer $FC_AGENT_TOKEN`. It is
authenticated by a capability grant in `X-Fuse-Artifact-Grant`. **A pulling host is never given
the serving host's agent token** — that token authorizes creating VMs and executing in guests,
and handing it over to permit one blob read would over-grant catastrophically.

```
grant = "v1." + digest + "." + expiry_unix + "." + nonce + "." + mac
mac   = HMAC-SHA256(key = <serving host's FC_AGENT_TOKEN>,
                    msg = "fuse-artifact-grant/v1\n" + digest + "\n"
                          + expiry_unix + "\n" + nonce)
```

`digest` and `mac` are lowercase hex; `nonce` is 16 random bytes, hex. The orchestrator mints
the grant, which it can do without any new key distribution because it already stores every
host's agent token. The serving agent verifies with its own token as the HMAC key, so it never
has to call the orchestrator.

Verification, in order: the grant is at most 512 bytes and splits into exactly 5 fields; the
version is `v1`; the digest in the grant equals the digest in the URL path; the expiry has not
passed; the recomputed mac matches under `hmac.compare_digest` (constant time). **Any failure is
`403 forbidden` with no indication of which check failed** — the differences would be a probing
oracle.

So a grant is worth exactly one digest, on one endpoint, on one host, until it expires, and the
holder learns nothing about the token that minted it.

### `POST /v1/artifacts/{digest}/pull`

Fetches an artifact from a peer agent and commits it locally **only if it verifies**.
Authenticated normally with this host's `Bearer $FC_AGENT_TOKEN`, because the caller is the
orchestrator; the grant in the body is a credential for the peer, not for this agent.

```json
{
  "peer_url": "http://<peer-host>:8090",
  "grant": "v1.<digest>.<expiry>.<nonce>.<mac>",
  "snapshot_id": "snap-1712345678-ab12cd"
}
```

`snapshot_id` is optional and is the id the artifact commits under, so that a later
`POST /v1/vm` with `seed_snapshot: <id>` resolves it exactly like a locally created snapshot.
It defaults to `art-<first 16 hex of digest>`. Response is the artifact record:

```json
{
  "snapshot_id": "snap-1712345678-ab12cd",
  "comment": "pulled from http://<peer-host>:8090",
  "created_at": "2026-01-01T00:00:00Z",
  "origin_vm_id": "",
  "digest": "<hex sha256>",
  "bytes": 1234567,
  "source_peer": "http://<peer-host>:8090"
}
```

The agent streams the peer's response into a temp file **outside** the snapshot store, hashing
as it goes, and only a matching digest earns the rename into the store. Digest mismatch, short
read, oversized artifact, timeout, full disk, or any transport error is `422` with the temp file
deleted and **nothing** in the snapshot store. Without a trusted bucket in the middle the peer is
the only thing vouching for the bytes, so that check is the whole safety property: a rootfs that
is subtly not what its digest claims would be seeded into guests forever after.

Bounds (all env-tunable):

| Env                                  | Default                                | Bounds                                 |
| ------------------------------------ | -------------------------------------- | -------------------------------------- |
| `FC_AGENT_MAX_ARTIFACT_BYTES`        | 64 GiB                                 | largest artifact this host will accept |
| `FC_AGENT_ARTIFACT_IO_TIMEOUT`       | 60s                                    | per socket operation to the peer       |
| `FC_AGENT_ARTIFACT_TRANSFER_TIMEOUT` | 1800s                                  | whole transfer, serving and pulling    |
| `FC_AGENT_ARTIFACT_TMP_DIR`          | `<SNAPSHOTS_DIR>/../artifact-pull-tmp` | where partial pulls stage              |

Free space on the staging filesystem is checked against the declared length before the first
byte is written, and `ENOSPC` mid-stream is handled as a failed pull.

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
