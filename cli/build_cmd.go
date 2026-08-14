package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/folsomintel/fuse/internal/fusefile"
	fuse "github.com/folsomintel/fuse/sdks/go"
)

// buildExecTimeout bounds the build phase inside the guest. the exec path allows
// 600s (the host agent's own EXEC_TIMEOUT_MAX); running the same work as a
// startup script would cap it at 30s, which the init scaffold's apt-get
// routinely exceeds. closing that gap is why this command exists.
const buildExecTimeout = 600 * time.Second

func newBuildCmd() *cobra.Command {
	var (
		file              string
		name              string
		secrets           []string
		secretsFile       string
		allowEmptySecrets bool
		keep              bool
		plan              bool
		noCache           bool
	)
	cmd := &cobra.Command{
		Use:   "build [path]",
		Short: "Run a Fusefile's setup phase once and snapshot the result",
		Long: "build boots a throwaway environment, runs the Fusefile's setup phase in\n" +
			"it, snapshots the resulting disk, and destroys the environment. it prints\n" +
			"the snapshot id, which `fuse up --from-build <id>` boots directly so the\n" +
			"setup work does not rerun on every boot.\n\n" +
			"the artifact is stored on the host that produced it. there is no object\n" +
			"storage, but it is not stuck there either: a host that needs it fetches\n" +
			"it from a host that has it. firecracker hosts only, since qemu hosts\n" +
			"cannot snapshot.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := findFusefilePath(file, args)
			if err != nil {
				return err
			}

			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("read %s: %w", path, err)
			}
			f, err := fusefile.Parse(data)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if err := fusefile.ResolveFiles(f, filepath.Dir(path)); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if noCache {
				f.Cache.Enabled = false
			}
			c, err := fusefile.Compile(f)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if c.BuildScript == "" {
				return fmt.Errorf("%s: no setup phase to build", path)
			}

			// build is where the setup phase actually runs, so the layer plan
			// describes exactly this command's work. derive it before booting
			// the builder: a bad `inputs` entry should fail here rather than
			// after a vm is up and holding host capacity.
			lp, err := planSetupLayers(path, f, plan)
			if err != nil {
				return err
			}
			if plan {
				return renderLayerPlan(lp)
			}

			secretMap, err := loadSecretsFile(secretsFile)
			if err != nil {
				return err
			}
			if secretMap == nil {
				secretMap = map[string]string{}
			}
			flagSecrets, err := parseKeyVals(secrets)
			if err != nil {
				return err
			}
			// --secret flags override --secrets-file entries on key collision,
			// matching `fuse up`.
			for k, v := range flagSecrets {
				secretMap[k] = v
			}
			if missing := missingSecrets(c.RequiredSecrets, secretMap, allowEmptySecrets); len(missing) > 0 {
				return fmt.Errorf("missing required secrets: %s (pass --allow-empty-secrets to accept empty values)", strings.Join(missing, ", "))
			}

			if name == "" {
				name = defaultTaskID(path)
			}

			cl, cur, err := app.client()
			if err != nil {
				return err
			}

			// resolve the cache before anything is provisioned. a hit changes
			// what the builder boots from, so it has to be known at create time.
			var hit *layerHit
			if lp != nil && lp.CacheEnabled {
				arch := targetArch(cmd.Context(), cl, cur.ActiveHost)
				if arch == "" {
					// no safe answer, so no lookup. filing a layer under an arch
					// it was not built on would serve a later build a rootfs it
					// cannot boot, which is far worse than rebuilding.
					warnf("cannot determine the target host architecture; building without the layer cache")
					lp.CacheEnabled = false
				} else {
					lp.Arch = arch
					hit, err = resolveDeepestLayer(cmd.Context(), cl, lp, arch)
					if err != nil {
						// the cache is an optimization. an orchestrator that
						// cannot answer should cost time, never the build.
						warnf("layer cache lookup failed, building cold: %v", err)
						hit = nil
					}
				}
			}

			spec := fuse.Spec{
				CPUs:      c.Spec.CPUs,
				RamMB:     c.Spec.RamMB,
				StorageGB: c.Spec.StorageGB,
				Region:    c.Spec.Region,
				Image:     c.Spec.Image,
			}
			req := fuse.CreateRequest{
				TaskID:         buildTaskID(name),
				ManifestInline: base64.StdEncoding.EncodeToString(c.ManifestJSON),
				Secrets:        secretMap,
			}
			if hit != nil {
				// seed and image are mutually exclusive: the artifact already
				// is the rootfs, and the image it descended from is folded into
				// the layer key, so the seed is the stronger statement.
				spec.Image = ""
				req.SeedSnapshotID = hit.Snapshot.ID
				lp.markHit(hit.Index)
			}
			req.Spec = spec

			// no startup script: the build phase runs through exec instead, for
			// the 600s ceiling and so its output can be shown on failure.
			e, err := cl.Environments.Create(cmd.Context(), req)
			if err != nil {
				return friendly(err)
			}
			if hit != nil {
				successf("reusing %d cached layer(s) from %s", lp.hitCount(), hit.Snapshot.ID)
			}
			successf("building in environment %s", e.ID)

			// the builder is torn down on every exit path except --keep, so a
			// failed build does not leave a vm holding host capacity.
			destroy := func() {
				if keep {
					warnf("keeping builder environment %s", e.ID)
					return
				}
				if err := cl.Environments.Destroy(cmd.Context(), e.ID); err != nil {
					warnf("destroy builder %s: %v", e.ID, err)
				}
			}

			// no step tracker: this only waits for the builder to boot, and the
			// builder is created without a startup script, so no setup step runs
			// in this window. the setup phase runs below as one exec of
			// BuildScript, which reports a single exit code rather than per-step
			// events, so there is nothing for a tracker to record.
			if err := waitForEnvironmentReady(cmd.Context(), cl, e.ID, nil); err != nil {
				destroy()
				return err
			}

			// runScript execs one shell script in the builder and surfaces its
			// output. unlike the startup-script path, build output is shown: a
			// build that fails silently is not debuggable. guest stdout/stderr
			// keep their own streams so a caller can separate them.
			runScript := func(script, label string) error {
				res, err := cl.Environments.Exec(cmd.Context(), e.ID, fuse.ExecRequest{
					Shell:     script,
					TimeoutMS: int(buildExecTimeout / time.Millisecond),
				})
				if err != nil {
					return friendly(err)
				}
				if res.Stdout != "" {
					fmt.Print(res.Stdout)
				}
				if res.Stderr != "" {
					fmt.Fprint(os.Stderr, res.Stderr)
				}
				if res.ExitCode != 0 {
					return fmt.Errorf("%s exited %d", label, res.ExitCode)
				}
				return nil
			}

			if lp == nil || !lp.CacheEnabled {
				// caching off: one exec of the whole build script, byte for byte
				// what this command has always done.
				if err := runScript(c.BuildScript, "build phase"); err != nil {
					destroy()
					return err
				}
			} else if err := runCachedSetup(cmd.Context(), cl, e.ID, f, lp, hit, name, runScript); err != nil {
				// layers already snapshotted stay in the store on purpose, so a
				// retry after a fixed setup line resumes instead of restarting.
				destroy()
				return err
			}

			snap, err := cl.Snapshots.Create(cmd.Context(), e.ID, fuse.SnapshotRequest{
				Comment:  "fuse build: " + name,
				Mode:     "build",
				Metadata: map[string]string{"name": name},
			})
			if err != nil {
				destroy()
				return friendly(err)
			}
			// the snapshot is durable and independent of the builder before the
			// builder goes away, so this order is what makes the artifact
			// outlive it.
			destroy()

			if app.isJSON() {
				return printJSON(snap)
			}
			successf("built %s", snap.ID)
			// the id goes to stdout so `fuse up --from-build "$(fuse build)"`
			// works; all other chatter is on stderr.
			fmt.Println(snap.ID)
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "path to the Fusefile (default: ./Fusefile, or the positional path)")
	cmd.Flags().StringVar(&name, "name", "", "name for the build artifact (default: the Fusefile's parent directory name)")
	cmd.Flags().StringArrayVar(&secrets, "secret", nil, "secret as key=value (repeatable, overrides --secrets-file)")
	cmd.Flags().StringVar(&secretsFile, "secrets-file", "", "path to a file of KEY=VALUE secret lines")
	cmd.Flags().BoolVar(&allowEmptySecrets, "allow-empty-secrets", false, "treat an empty value as satisfying a required secret")
	cmd.Flags().BoolVar(&keep, "keep", false, "leave the builder environment running instead of destroying it")
	cmd.Flags().BoolVar(&plan, "plan", false, "print the derived setup layer cache plan and exit without building anything")
	cmd.Flags().BoolVar(&noCache, "no-cache", false, "ignore the Fusefile's cache block and run every setup step")
	return cmd
}

// buildTaskID derives the builder's task id. it is deliberately distinct from
// the task id `fuse up` derives from the same Fusefile, since a task id may
// only be assigned to one vm at a time and a build must not collide with the
// environment it is building for.
func buildTaskID(name string) string {
	return fmt.Sprintf("build-%s-%d", name, time.Now().UnixNano())
}

// runCachedSetup runs the setup phase one cacheable step at a time, snapshotting
// after each, then runs everything left as a single exec.
//
// The split is not a heuristic. Cacheable steps are always a leading run of the
// setup list, because the chain breaks permanently at the first step that opts
// out, so there is exactly one boundary: before it, every step can produce an
// artifact worth keeping; after it, none can, and running them one at a time
// would buy nothing but round trips.
//
// Each step is its own exec, and each exec is its own shell. That is why every
// script here carries the prelude: strict mode does not survive across execs,
// and neither does the `cd` into the workspace, so a step rendered without it
// would run in whatever directory the shell started in. The file block is
// written only on the first exec of a cold build, since a seed artifact already
// contains it.
//
// A failure leaves every layer snapshotted so far in the store. That is the
// point of snapshotting per step: fixing the setup line that broke and running
// again resumes from the last good layer instead of starting over.
func runCachedSetup(
	ctx context.Context,
	cl *fuse.Client,
	envID string,
	f *fusefile.Fusefile,
	lp *layerPlan,
	hit *layerHit,
	name string,
	runScript func(script, label string) error,
) error {
	start := 0
	if hit != nil {
		start = hit.Index + 1
	}
	// a seed artifact was built from the same base key, which folds the file
	// block in, so the files are already on that disk.
	writeFiles := hit == nil
	prefix := cacheablePrefixLen(lp)

	for i := start; i < prefix; i++ {
		script := fusefile.SetupScriptRange(f, i, i+1, writeFiles && i == start)
		if err := runScript(script, fmt.Sprintf("setup[%d]", i)); err != nil {
			return err
		}
		if _, err := cl.Snapshots.Create(ctx, envID, fuse.SnapshotRequest{
			Comment:  fmt.Sprintf("fuse build layer %d: %s", i, name),
			Mode:     "build",
			LayerKey: lp.Steps[i].Key,
		}); err != nil {
			return friendly(err)
		}
		lp.Statuses[i] = layerStatusMiss
	}

	// everything from the first uncacheable step on, as one exec. nothing here
	// can be snapshotted, so there is no reason to pay for a round trip per
	// step.
	tail := prefix
	if start > tail {
		tail = start
	}
	if tail < len(lp.Steps) {
		script := fusefile.SetupScriptRange(f, tail, len(lp.Steps), writeFiles && tail == start)
		if err := runScript(script, "setup tail"); err != nil {
			return err
		}
	}
	return nil
}
