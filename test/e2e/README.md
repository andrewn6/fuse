# Fusefile end-to-end tests

The Go tests under `internal/fusefile` stop at the compiler: they check that a
Fusefile turns into the right spec, manifest and startup script. Nothing there
boots a VM, so a field can compile perfectly and still never reach the guest.

These tests close that gap. Every case creates a real environment on a real
host from a real Fusefile, then reads the guest back to check that what the
Fusefile asked for is what the guest got.

## Running

The CLI has to be pointed at an orchestrator with a host selected:

```sh
fuse connect http://<orchestrator>:8080
fuse host <host-id>

./test/e2e/run.sh                  # every case
./test/e2e/run.sh env files        # only cases matching these substrings
FUSE_BIN=/tmp/fuse-main ./test/e2e/run.sh   # test a specific build
```

`FUSE_BIN` matters more than it looks: several fields here (`env`, `build`,
`copy`, `healthcheck`) postdate the last release, so a CLI installed from a
release tarball rejects these Fusefiles at parse time. Build from the checkout:

```sh
go build -buildvcs=false -o /tmp/fuse-main ./cli
```

The orchestrator has to be current too. The `env` block is resolved
orchestrator-side, so a stale orchestrator silently drops it while the CLI
happily compiles it.

## Cleanup

Every environment is created with a task id under `E2E_PREFIX` (default `e2e`),
giving ids of the form `fuse-e2e-<case>`. The exit trap destroys everything
matching that prefix, on interrupt as well as on normal exit. Nothing outside
the prefix is touched, so a run cannot disturb environments it did not create.

## The cases

| Case      | Fusefile               | What it proves                                                                                                               |
| --------- | ---------------------- | ---------------------------------------------------------------------------------------------------------------------------- |
| baseline  | `Fusefile.baseline`    | scheduling, boot, build steps, run, workspace, per-step `workdir` scoping                                                    |
| env       | `Fusefile.env-secrets` | top-level `env` literals and secret refs reach build and run; secrets stay out of the startup script                         |
| cache-env | `Fusefile.cache-env`   | whether `env` reaches build steps on the layer-cache path as well as the startup-script path                                 |
| files     | `Fusefile.files-copy`  | `files` content and modes, `copy` directory walking, relative paths resolving against a non-default `workspace`              |
| cache     | `Fusefile.cache`       | `--plan`, layer reuse across two builds, `fuse build` artifacts, `fuse up --from-build`                                      |
| health    | `Fusefile.healthcheck` | the probe verdict reaching the orchestrator API, and `expose` reachable from outside the host                                |
| services  | `Fusefile.services`    | compose services and service-env secrets                                                                                     |
| negative  | `negative/*`           | the refusals: bad manifests, missing secrets, over-provisioning, GPU on a GPU-less fleet, unknown host pin, a blocking `run` |

Negative cases are as load-bearing as positive ones. A scheduler that accepts a
512-CPU request, or a `placement.host` gate that degrades into a preference,
fails quietly and in production rather than loudly here.

## Known failures

`services` fails against a firecracker host today, and the failure is real. The
guest has no docker: the baked rootfs ships podman with docker's compose v2
binary as the compose provider. `podman.socket` is enabled for exactly this
reason, but nothing sets `DOCKER_HOST`, so compose dials `/var/run/docker.sock`,
finds nothing, and the create fails with a 500. The whole environment fails, not
just the service. Setting `DOCKER_HOST=unix:///run/podman/podman.sock` in the
`compose_up` command in `fc-agent.py` makes the same compose file work.

## Writing a new case

Two rules the guest enforces and the compiler does not:

- `run` executes synchronously during create. Anything long-lived has to be
  backgrounded or the create hangs until the startup timeout and fails.
- The base rootfs is sparse. `/opt` does not exist, and a build step gets no
  directory made for it beyond the workspace, so a step writing outside the
  workspace has to `mkdir -p` first.
