package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// initScaffold is the commented example Fusefile written by `fuse init`. it
// documents the full v1 contract: every field it mentions is parsed,
// validated, and compiled today. the blocks that name a path on the authoring
// machine (files, copy) are commented out rather than omitted, because a
// scaffold that shipped them live would fail its first `fuse up` on a source
// that does not exist yet.
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

# files materialized in the guest before the build steps run. for config and
# small code, not weights or datasets: entries are carried inside the create
# request, so their combined size is capped at 64KiB. fetch large artifacts
# from a build step.
# commented out because 'source' would have to point at a file that exists.
# files:
#   - path: config/app.yaml   # relative paths resolve against the workspace
#     source: ./app.yaml      # read relative to this Fusefile, not the cwd
#   - path: entrypoint.sh
#     content: |              # or inline it
#       #!/bin/sh
#       echo hello
#     mode: "0755"            # optional octal mode

# local files and directories copied into the guest, also before setup runs.
# this is the block that takes a directory: 'from' is read relative to this
# Fusefile, 'to' resolves against the workspace when it is relative, symlinks
# are an error, and the combined size is capped at 512KiB. permissions are not
# carried, so chmod in setup. commented out for the same reason as files above.
# copy:
#   - from: ./start.sh
#     to: ./start.sh
#   - from: ./src           # a directory is walked into one upload per file
#     to: /workspace/src

resources:
  cpus: 2 # whole vcpus; 2.0 is accepted, a fraction is not
  memory: 2GB # M/MB/MiB, G/GB/GiB, T/TB/TiB (all binary), compiles to ram_mb
  disk: 10GB # same units, rounded up to whole GB; 'storage' is an alias
  max_runtime: 1h # accepts go duration, compiles to max_runtime_seconds
  # idle_timeout: 15m # uncomment to destroy after this long with no exec or attach
  # region: us-east # uncomment to schedule only onto a host registered in this region

# opt in to the build layer cache. layers are host-local and firecracker-only;
# a gpu environment gets no caching. see 'fuse build --plan'.
cache:
  enabled: true

# bound on build + run, which the orchestrator runs synchronously during
# create. the default is 30s and the ceiling is an operator setting (55s out of
# the box), so this is headroom for a slow build, not a budget for a long one:
# bake genuinely long work into an image with 'fuse build' instead.
startup_timeout: 55s

# work that prepares the environment. runs once at boot, before run, and
# compiles into startup_script ahead of it. 'setup:' is the old name for this
# block: it still works, but setting both is an error.
build:
  # bare string form: keyed on its bytes plus the step before it.
  - apt-get update -qq && apt-get install -y --no-install-recommends ripgrep

  # mapping form: inputs are hashed into the step's cache key, so editing one
  # of these files re-runs this step (and every step after it).
  # - run: npm ci
  #   inputs:
  #     - package.json
  #     - package-lock.json

  # 'workdir' scopes one step to a directory. relative paths resolve against
  # the workspace, and the change does not leak into the next step.
  # - workdir: web
  #   run: npm run build

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

# the main task entrypoint, compiled into startup_script (after build). a plain
# string is interpreted by sh -lc; a list ["python", "app.py"] is an argv whose
# elements are shell-quoted, so spaces, quotes, $, and globs in an argument
# cannot alter the command. use the list form only when an argument would
# otherwise be reinterpreted by the shell.
run: ./start.sh

# where the build steps and run execute. absolute path, created with mkdir -p.
# this is the default, so the line can be deleted. services and 'fuse
# environment exec'/'shell' do not inherit it.
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

// initIgnoreScaffold is the .fuseignore written next to the Fusefile. It is
// almost all comments on purpose: the defaults it lists are applied whether or
// not this file exists, so the only lines worth writing are the ones a
// particular project adds and the `!` that takes a default back. The list is
// repeated here because a default nobody can see is a default nobody can
// override, and a test pins it against defaultIgnorePatterns.
const initIgnoreScaffold = `# what the Fusefile's 'copy:' block leaves behind, in gitignore syntax: '#'
# comments, '!' re-includes, a leading '/' anchors to the copy source's root, a
# trailing '/' matches directories only, '*' and '**' glob. last match wins.
#
# these apply before anything below, whether or not this file exists:
#   .git/  node_modules/  __pycache__/  .venv/  venv/  target/  dist/  build/
#   .DS_Store  *.pyc  .env  .env.*  *.pem  *.key
#
# so what belongs here is what your project adds:
# *.log
# /tmp-scratch
#
# and the one line that takes a default back:
# !.env
`

func newInitCmd() *cobra.Command {
	var (
		file  string
		force bool
	)
	cmd := &cobra.Command{
		Use:   "init [path]",
		Short: "Scaffold a commented example Fusefile and .fuseignore",
		Long: "init writes a commented example Fusefile to the target path, and a\n" +
			".fuseignore next to it, so a user can edit them. it refuses to overwrite\n" +
			"either existing file unless --force is set.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := resolveFusefilePath(file, args)
			// the ignore file goes next to the Fusefile because that is the
			// directory `copy:` sources already resolve against, and it is the
			// only one that is read.
			ignorePath := filepath.Join(filepath.Dir(path), fuseignoreName)

			// both are checked before either is written, so a refusal leaves
			// nothing half-scaffolded.
			if !force {
				if err := refuseExisting(path, "a Fusefile"); err != nil {
					return err
				}
				if err := refuseExisting(ignorePath, "a "+fuseignoreName); err != nil {
					return err
				}
			}

			if err := os.WriteFile(path, []byte(initScaffold), 0o644); err != nil {
				return fmt.Errorf("write %s: %w", path, err)
			}
			successf("wrote %s", path)
			if err := os.WriteFile(ignorePath, []byte(initIgnoreScaffold), 0o644); err != nil {
				return fmt.Errorf("write %s: %w", ignorePath, err)
			}
			successf("wrote %s", ignorePath)
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "path to write the Fusefile (default: ./Fusefile, or the positional path)")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing Fusefile and .fuseignore")
	return cmd
}

// refuseExisting reports an error if p is already there, so init protects
// edits somebody already made. what names the file in the message, since the
// path alone does not say which of the two scaffolded files is in the way.
func refuseExisting(p, what string) error {
	if _, err := os.Stat(p); err == nil {
		return fmt.Errorf("%s already exists at %s (use --force to overwrite)", what, p)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", p, err)
	}
	return nil
}
