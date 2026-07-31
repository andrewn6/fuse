package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// initScaffold is the commented example Fusefile written by `fuse init`. it
// documents the full v1 contract; every field here is parsed, validated, and
// compiled today.
//
// the first line is the yaml-language-server modeline, which points editors at
// the published json schema (schema/fusefile-v1.json) for completion and inline
// validation. it is a yaml comment, so it changes nothing about how the file
// parses. the url is the schema's $id and is pinned by a test.
const initScaffold = `# yaml-language-server: $schema=https://raw.githubusercontent.com/folsomintel/fuse/main/schema/fusefile-v1.json
version: 1

# base rootfs to boot, by name. this is NOT an oci image ref: the host agent
# resolves it against the rootfs images baked on the host, so the name must
# already exist there. omit it to boot the host's default base rootfs.
# (services[].image below IS an oci ref -- those run in containers in the vm.)
# image: cuda-12

# files materialized in the guest before setup runs. for config and small code,
# not weights or datasets: entries are carried inside the create request, so
# their combined size is capped at 64KiB. fetch large artifacts from setup.
# commented out because 'source' would have to point at a file that exists.
# files:
#   - path: config/app.yaml   # relative paths resolve against the workspace
#     source: ./app.yaml      # read relative to this Fusefile, not the cwd
#   - path: entrypoint.sh
#     content: |              # or inline it
#       #!/bin/sh
#       echo hello
#     mode: "0755"            # optional octal mode

resources:
  cpus: 2
  memory: 2GB # accepts MB/GB suffixes, compiles to ram_mb
  storage: 10GB # compiles to storage_gb
  max_runtime: 1h # accepts go duration, compiles to max_runtime_seconds
  # idle_timeout: 15m # uncomment to destroy after this long with no exec or attach
  # region: us-east # uncomment to schedule only onto a host registered in this region

# opt in to the setup layer cache. layers are host-local and firecracker-only;
# a gpu environment gets no caching. see 'fuse build --plan'.
cache:
  enabled: true

# bound on setup + run, which the orchestrator runs synchronously during
# create. the default is 30s and the ceiling is an operator setting (55s out of
# the box), so this is headroom for a slow setup, not a budget for a long one:
# bake genuinely long work into an image with 'fuse build' instead.
startup_timeout: 55s

# convenience layer run once at boot, before run. compiles into startup_script.
setup:
  # bare string form: keyed on its bytes plus the step before it.
  - apt-get update -qq && apt-get install -y --no-install-recommends ripgrep

  # mapping form: inputs are hashed into the step's cache key, so editing one
  # of these files re-runs this step (and every step after it).
  # - run: npm ci
  #   inputs:
  #     - package.json
  #     - package-lock.json

  # a step that reads /fuse/secrets.json or has effects outside the rootfs must
  # opt out, or its layer would be reused with stale secret material.
  # - run: ./scripts/register.sh
  #   cache: false

# services brought up inside the vm. compiles to manifest.services then a compose project.
services:
  postgres:
    image: postgres:16
    ports: [5432]
    env:
      POSTGRES_PASSWORD: { secret: pg_password }
  redis:
    image: redis:7
    ports: [6379]

# the main task entrypoint. compiles into startup_script (after setup).
run: ./start.sh

workspace: /workspace

# ports published to the outside world (ingress).
expose:
  - port: 8080
    as: http

# secret names this environment requires. values are supplied out-of-band
# (cli flag / env / secret store), never written in the Fusefile.
secrets:
  - pg_password
`

func newInitCmd() *cobra.Command {
	var (
		file  string
		force bool
	)
	cmd := &cobra.Command{
		Use:   "init [path]",
		Short: "Scaffold a commented example Fusefile",
		Long: "init writes a commented example Fusefile to the target path so a user\n" +
			"can edit it. it refuses to overwrite an existing file unless --force is set.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := resolveFusefilePath(file, args)

			if !force {
				if _, err := os.Stat(path); err == nil {
					return fmt.Errorf("a Fusefile already exists at %s (use --force to overwrite)", path)
				} else if !os.IsNotExist(err) {
					return fmt.Errorf("stat %s: %w", path, err)
				}
			}

			if err := os.WriteFile(path, []byte(initScaffold), 0o644); err != nil {
				return fmt.Errorf("write %s: %w", path, err)
			}
			successf("wrote %s", path)
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "path to write the Fusefile (default: ./Fusefile, or the positional path)")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing Fusefile")
	return cmd
}
