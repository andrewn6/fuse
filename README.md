# Fuse

[![CI](https://img.shields.io/github/actions/workflow/status/folsomintel/fuse/ci.yml?branch=main&label=CI)](https://github.com/folsomintel/fuse/actions/workflows/ci.yml) [![Release](https://img.shields.io/github/v/release/folsomintel/fuse)](https://github.com/folsomintel/fuse/releases/latest) [![Go Reference](https://pkg.go.dev/badge/github.com/folsomintel/fuse/sdks/go.svg)](https://pkg.go.dev/github.com/folsomintel/fuse/sdks/go) [![npm](https://img.shields.io/npm/v/%40folsom%2Ffuse)](https://www.npmjs.com/package/@folsom/fuse) [![PyPI](https://img.shields.io/pypi/v/folsom-fuse)](https://pypi.org/project/folsom-fuse/) [![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](https://github.com/folsomintel/fuse/blob/main/LICENSE)

**Sandboxed microVMs on your own hosts, driven by an API.**

Fuse is a control plane for Firecracker and QEMU microVMs. Register your hosts, describe a
workload in a `Fusefile`, and Fuse schedules it, boots the VM, and tracks the whole lifecycle
(provision, running, drain, destroy) with snapshots, fork, exec, live event streams, and
Prometheus metrics. CPU workloads run on Firecracker; GPU workloads run on QEMU hosts with
whole cards or MIG slices.

You bring the hosts. State lives in Postgres and on your disks, nowhere else.

## Install

```bash
brew install --cask folsomintel/fuse/fuse
```

Or without Homebrew, on macOS and Linux:

```bash
curl -fsSL https://raw.githubusercontent.com/folsomintel/fuse/main/ops/install-fuse.sh | bash
```

The script verifies the release archive against the release's `checksums.txt` before
installing, and puts the binary in `/usr/local/bin` (or `~/.local/bin` if that is not
writable). Set `VERSION=vX.Y.Z` to pin a release, `FUSE_INSTALL_DIR` to choose where.

Upgrade with `brew upgrade --cask fuse`, check with `fuse --version`. You can also build
from a checkout with `go build -o bin/fuse ./cli`. Mind the naming: the `fuse_*` archives
are the orchestrator, `fuse-cli_*` is the CLI.

## First run

The CLI is only a client. You need an orchestrator reachable over HTTP and at least one host
agent running before it can do anything. `sudo ./fc-agent.sh bootstrap` in
[`host-agent/`](host-agent/) brings both up on a single machine.

```bash
fuse quickstart   # connect an orchestrator, register a host
fuse init         # scaffold a Fusefile
fuse up --secret pg_password=devpassword

fuse environment list
fuse environment exec <id> -- echo hello
fuse environment fork <id>
```

`quickstart` needs a tty. Headless, run the two commands it wraps:

```bash
fuse connect https://orch.example.com --token "$ORCH_AUTH_TOKEN"
fuse host register prod-east-1 --url http://10.0.0.5:8090 --token "$FC_AGENT_TOKEN" --max-vms 20
```

## Docs

- [Quickstart](docs/content/docs/learn/quickstart.mdx), the steps above with the detail
- [Fusefile](docs/content/docs/concepts/fusefile.mdx), the YAML `fuse up` compiles
- [Guides](docs/content/docs/guides/), agent sandboxes, CI runners, GPU workloads, interactive debugging
- [Self-hosting](docs/content/docs/cloud/index.mdx), orchestrator deploy plus Firecracker and GPU host setup
- [CLI reference](docs/content/reference/cli/index.mdx), every command, flag, and context
- [REST API](internal/api/openapi.yaml), the OpenAPI spec the docs site renders
- [SDKs](docs/content/reference/index.mdx), Go (`sdks/go`), Python (`folsom-fuse`), TypeScript (`@folsom/fuse`)

## Contributing

Go 1.26 monorepo, no Makefile. Most tests run against in-memory stubs, so you need neither
VMs nor Postgres to work on it.

```bash
gofmt -l .                     # must print nothing
go vet ./...
go build ./...
go test ./... -race -count=1
golangci-lint run ./...        # pinned to v2.12.2 in ci
```

## License

MIT, see [LICENSE](LICENSE).
