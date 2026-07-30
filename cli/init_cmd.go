package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// initScaffold is the commented example Fusefile written by `fuse init`. it
// documents the full v1 contract; image and expose are parsed and validated
// today but not yet compiled (that lands in a later pr).
const initScaffold = `version: 1

# base environment. an oci image ref, or omitted to use the baked base rootfs.
image: ghcr.io/acme/worker:latest

resources:
  cpus: 2
  memory: 2GB # accepts MB/GB suffixes, compiles to ram_mb
  storage: 10GB # compiles to storage_gb
  max_runtime: 1h # accepts go duration, compiles to max_runtime_seconds
  # idle_timeout: 15m # uncomment to destroy after this long with no exec or attach

# convenience layer run once at boot, before run. compiles into startup_script.
setup:
  - apt-get update -qq
  - apt-get install -y --no-install-recommends ripgrep

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
