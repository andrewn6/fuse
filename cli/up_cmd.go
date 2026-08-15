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

func newUpCmd() *cobra.Command {
	var (
		file              string
		secrets           []string
		secretsFile       string
		allowEmptySecrets bool
		taskID            string
		gatewayURL        string
		gatewayToken      string
		noWait            bool
		dryRun            bool
		fromBuild         string
		plan              bool
		noCache           bool
		waitHealthy       bool
		healthTimeout     time.Duration
		showCopy          bool
	)
	cmd := &cobra.Command{
		Use:   "up [path]",
		Short: "Compile a Fusefile and create an environment from it",
		Long: "up reads a Fusefile, compiles it into a resource spec, manifest, and\n" +
			"startup script, then creates an environment from the result. it streams\n" +
			"provisioning events until a terminal state unless --no-wait is set.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// both print and exit, and they print different things, so asking
			// for both is a mistake rather than a combination to resolve.
			if dryRun && plan {
				return fmt.Errorf("--dry-run and --plan are mutually exclusive: --dry-run prints the create request, --plan prints the setup layer cache plan")
			}
			// waiting for a verdict requires waiting at all, and --no-wait
			// returns before the environment is even running.
			if waitHealthy && noWait {
				return fmt.Errorf("--wait-healthy and --no-wait are mutually exclusive: --no-wait returns before the environment is running, so there is no verdict to wait for")
			}

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
			// `files` entries name sources relative to the Fusefile, not the
			// working directory, so `fuse up -f ../other/Fusefile` reads the
			// same files it would from that directory.
			if err := fusefile.ResolveFiles(f, filepath.Dir(path)); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			// --from-build boots a rootfs that already has the setup phase
			// baked in, so this boot runs no setup steps at all. the layer
			// cache is a property of running them, so both flags that govern
			// it would be describing work that does not happen here.
			if fromBuild != "" {
				if plan {
					return fmt.Errorf("--plan and --from-build are mutually exclusive: a --from-build boot runs no setup steps to plan")
				}
				if noCache {
					return fmt.Errorf("--no-cache and --from-build are mutually exclusive: a --from-build boot runs no setup steps to cache")
				}
			}
			if noCache {
				f.Cache.Enabled = false
			}
			c, err := fusefile.Compile(f)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			// waiting for a verdict nothing will ever produce would just hang
			// until the timeout, so say what is missing instead.
			if waitHealthy && c.Healthcheck == nil {
				return fmt.Errorf("--wait-healthy needs a `healthcheck:` block in %s: without one the environment reports no verdict to wait for", path)
			}

			// `copy` entries name sources relative to the Fusefile too, and
			// the walk happens here rather than at compile time because it is
			// the only part of the pipeline that reads the filesystem. It runs
			// before the --dry-run branch so a missing source or an oversized
			// tree fails the dry run as well, which is the point of one.
			//
			// The walk is filtered through the .fuseignore next to the
			// Fusefile, which exists whether or not the author wrote one: the
			// built-in defaults are what keep `from: .` from walking .git into
			// a 512KiB request body. Ignored paths are dropped before they are
			// sized, so what a pattern removes does not count against the cap.
			ign, err := loadFuseignore(filepath.Dir(path))
			if err != nil {
				return err
			}
			copyFiles, copyStats, err := collectCopy(c.Copy, filepath.Dir(path), ign.decide)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			// printed before anything else can fail and long before the
			// create, since the answer to "what am I about to upload" is only
			// worth having while it can still change something.
			if showCopy {
				renderCopyReport(os.Stderr, copyStats)
			}

			var lp *layerPlan
			if fromBuild == "" {
				// derive layer keys when caching is on (so a bad `inputs` entry
				// fails here rather than mid-provision) or when the user asked
				// to see the plan.
				lp, err = planSetupLayers(path, f, plan)
				if err != nil {
					return err
				}
				if plan {
					return renderLayerPlan(lp)
				}
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
			// --secret flags override --secrets-file entries on key collision.
			for k, v := range flagSecrets {
				secretMap[k] = v
			}
			if missing := missingSecrets(c.RequiredSecrets, secretMap, allowEmptySecrets); len(missing) > 0 {
				return fmt.Errorf("missing required secrets: %s (pass --allow-empty-secrets to accept empty values)", strings.Join(missing, ", "))
			}

			// --task-id wins, then the Fusefile's name:, then the parent
			// directory. only the last one is a guess, so it is the only one
			// announced: a declared name is what the author asked for and
			// does not need narrating back at them.
			if taskID == "" && f.Name != "" {
				taskID = f.Name
			}
			if taskID == "" {
				taskID = defaultTaskID(path)
				// nothing declared a name, so the cli invents one from the
				// parent directory. say so before the create, since two
				// checkouts of the same repo derive the same id and the
				// second one collides.
				infof("no --task-id and no `name:` in the Fusefile: using %q, derived from the Fusefile's directory", taskID)
			}

			// a build artifact already carries the setup phase's result baked
			// into its rootfs, so replaying setup on boot would redo the exact
			// work --from-build exists to skip. only the run phase is sent.
			startupScript := c.StartupScript
			if fromBuild != "" {
				if c.Spec.Image != "" {
					return fmt.Errorf("--from-build and the Fusefile's `image` are mutually exclusive: both name the rootfs to boot")
				}
				startupScript = c.RunScript
			}

			if dryRun {
				// everything above this point is what `up` does before it
				// touches the network, so a dry run still applies the secret
				// gate and still reports the derived task id. the rendering
				// belongs to `fuse compile`, which owns the format.
				format, err := resolveCompileFormat("")
				if err != nil {
					return err
				}
				req := newCompiledRequest(taskID, fromBuild, c)
				// the walk already happened, so a dry run can show the copied
				// files exactly as the create would carry them.
				req.Files = encodeCopyFiles(copyFiles)
				return writeCompiled(cmd.OutOrStdout(), format, c, req)
			}

			manifestInline := base64.StdEncoding.EncodeToString(c.ManifestJSON)

			cl, cur, err := app.client()
			if err != nil {
				return err
			}

			// `up` consumes the layer cache but never produces it: snapshotting
			// mid-boot would mean pausing an environment somebody asked to have
			// running. a hit here just means booting from an artifact `fuse
			// build` already made and skipping the setup steps it covers.
			seedID := fromBuild
			image := c.Spec.Image
			if lp != nil && lp.CacheEnabled {
				if arch := targetArch(cmd.Context(), cl, cur.ActiveHost); arch != "" {
					lp.Arch = arch
					hit, err := resolveDeepestLayer(cmd.Context(), cl, lp, arch)
					if err != nil {
						warnf("layer cache lookup failed, booting cold: %v", err)
					} else if hit != nil {
						lp.markHit(hit.Index)
						seedID = hit.Snapshot.ID
						// seed and image both name the rootfs to boot, and the
						// api rejects being given two answers.
						image = ""
						startupScript = fusefile.StartupScriptFrom(f, hit.Index+1)
						successf("reusing %d cached layer(s) from %s", lp.hitCount(), hit.Snapshot.ID)
					}
				}
			}

			e, err := cl.Environments.Create(cmd.Context(), fuse.CreateRequest{
				TaskID: taskID,
				Spec: fuse.Spec{
					CPUs:               c.Spec.CPUs,
					RamMB:              c.Spec.RamMB,
					StorageGB:          c.Spec.StorageGB,
					Region:             c.Spec.Region,
					Arch:               c.Spec.Arch,
					MaxRuntimeSeconds:  c.Spec.MaxRuntimeSeconds,
					IdleTimeoutSeconds: c.Spec.IdleTimeoutSeconds,
					Image:              image,
					GPUs:               c.Spec.GPUs,
					GPUKind:            c.Spec.GPUKind,
					GPUProfile:         c.Spec.GPUProfile,
					HostID:             c.Spec.HostID,
					Labels:             c.Spec.Labels,
				},
				ManifestInline:              manifestInline,
				Files:                       encodeCopyFiles(copyFiles),
				Secrets:                     secretMap,
				StartupScript:               startupScript,
				StartupScriptTimeoutSeconds: c.StartupTimeoutSeconds,
				Expose:                      toSDKExpose(c.Expose),
				Healthcheck:                 toSDKHealthcheck(c.Healthcheck),
				Desktop:                     toSDKDesktop(c.Desktop),
				SeedSnapshotID:              seedID,
				// the gateway carries a credential, so it is a flag rather
				// than a Fusefile field. matching `fuse environment create`
				// keeps the Fusefile path from being the less capable one.
				GatewayURL:   gatewayURL,
				GatewayToken: gatewayToken,
			})
			if err != nil {
				return friendly(err)
			}
			successf("creating environment %s (task %s)", e.ID, e.TaskID)
			if !noWait {
				// the step events carry only an index, so the cli supplies the
				// build commands it just compiled as the labels.
				buildSteps := f.BuildSteps()
				labels := make([]string, 0, len(buildSteps))
				for _, step := range buildSteps {
					labels = append(labels, step.Run)
				}
				steps := newStepTracker(labels)
				started := time.Now()
				if err := waitForEnvironmentReady(cmd.Context(), cl, e.ID, steps); err != nil {
					return err
				}
				if s := steps.summary(time.Since(started)); s != "" && !app.isJSON() {
					_, _ = fmt.Fprintln(os.Stdout, s)
				}
				// running only means the environment booted and its startup
				// script returned. --wait-healthy keeps waiting until the
				// probe the author declared actually passes.
				if waitHealthy {
					if !app.isJSON() {
						infof("waiting for the healthcheck to pass (up to %s)", healthTimeout)
					}
					if err := waitForEnvironmentHealthy(cmd.Context(), cl, e.ID, healthTimeout); err != nil {
						return err
					}
				}
				return reportReady(cmd.Context(), cl, e.ID)
			}
			if app.isJSON() {
				return printJSON(e)
			}
			renderEnvDetail(e, nil)
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "path to the Fusefile (default: ./Fusefile, or the positional path)")
	cmd.Flags().StringArrayVar(&secrets, "secret", nil, "secret as key=value (repeatable, overrides --secrets-file)")
	cmd.Flags().StringVar(&secretsFile, "secrets-file", "", "path to a file of KEY=VALUE secret lines")
	cmd.Flags().BoolVar(&allowEmptySecrets, "allow-empty-secrets", false, "treat an empty value as satisfying a required secret")
	cmd.Flags().StringVar(&taskID, "task-id", "", "environment task id (default: the Fusefile's parent directory name)")
	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "gateway url")
	cmd.Flags().StringVar(&gatewayToken, "gateway-token", "", "gateway token")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the create request this Fusefile would post and exit without connecting to the orchestrator")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "create the environment without streaming provisioning events")
	cmd.Flags().StringVar(&fromBuild, "from-build", "", "boot from a `fuse build` artifact instead of a base image (skips the setup phase)")
	cmd.Flags().BoolVar(&plan, "plan", false, "print the derived setup layer cache plan and exit without creating anything")
	cmd.Flags().BoolVar(&noCache, "no-cache", false, "ignore the Fusefile's cache block and run every setup step")
	cmd.Flags().BoolVar(&waitHealthy, "wait-healthy", false, "after the environment is running, keep waiting until its `healthcheck` reports passing")
	cmd.Flags().DurationVar(&healthTimeout, "health-timeout", 2*time.Minute, "how long --wait-healthy waits for a passing verdict before giving up")
	cmd.Flags().BoolVar(&showCopy, "show-copy", false, "print what the copy block ships, and what .fuseignore dropped, before creating anything")
	return cmd
}

// reportReady prints the summary that ends a waiting `fuse up`. The event
// stream carries a state and nothing else, so the addresses the author needs
// have to be re-fetched once the environment settles.
//
// The environment is already up by the time this runs, so a failed fetch is
// reported as a warning and not as a failed `up`: the command did its job and
// the author can still run `fuse environment get`.
func reportReady(ctx context.Context, cl *fuse.Client, vmID string) error {
	e, err := cl.Environments.Get(ctx, vmID)
	if err != nil {
		warnf("environment %s is up, but reading its details failed: %v", vmID, friendly(err))
		return nil
	}
	if app.isJSON() {
		return printJSON(e)
	}
	renderUpSummary(e)
	return nil
}

// renderUpSummary prints what the author can act on: where the environment is,
// what it published, and how to get inside it. It is deliberately shorter than
// renderEnvDetail, which repeats the spec the author just wrote.
func renderUpSummary(e *fuse.EnvironmentInfo) {
	pairs := [][2]string{
		{"environment", fmt.Sprintf("%s  %s", e.ID, stateStyle(e.State))},
		{"task", dash(e.TaskID)},
	}
	if e.HostID != "" {
		pairs = append(pairs, [2]string{"host", e.HostID})
	}
	if e.URL != "" {
		pairs = append(pairs, [2]string{"agent", e.URL})
	}
	// only shown when the environment declared a probe, so a Fusefile without
	// one prints exactly what it printed before.
	if e.Health != nil {
		health := e.Health.State
		if e.Health.Message != "" {
			health += "  " + e.Health.Message
		}
		pairs = append(pairs, [2]string{"health", health})
	}
	pairs = append(pairs, endpointPairs(e.Endpoints)...)
	pairs = append(pairs, [2]string{"shell", "fuse environment shell " + e.ID})
	renderDetail(pairs)
}

// fusefileNames are the filenames a command that reads a Fusefile looks for
// in the working directory when neither -f/--file nor a positional path names
// one, in priority order. The first that exists wins, so a directory holding
// two of them is resolved deterministically.
var fusefileNames = []string{"Fusefile", "Fusefile.yaml", "Fusefile.yml", "fusefile.yaml", "fusefile.yml"}

// resolveFusefilePath picks the Fusefile path from -f/--file, a positional
// argument, or the default "Fusefile", in that priority order. It never
// touches the filesystem, so it names a path that need not exist yet: `fuse
// init` uses it to pick a write target. Commands that read a Fusefile want
// findFusefilePath instead.
func resolveFusefilePath(file string, args []string) string {
	if file != "" {
		return file
	}
	if len(args) > 0 {
		return args[0]
	}
	return fusefileNames[0]
}

// findFusefilePath resolves the Fusefile to read. An explicit -f/--file or
// positional path is returned as given, so a typo is reported against the name
// the caller typed rather than against a default. With neither, every name in
// fusefileNames is tried in the working directory and the not-found error
// names all of them, since "no such file or directory: Fusefile" does not say
// that Fusefile.yaml would also have worked.
func findFusefilePath(file string, args []string) (string, error) {
	if file != "" {
		return file, nil
	}
	if len(args) > 0 {
		return args[0], nil
	}
	for _, name := range fusefileNames {
		if st, err := os.Stat(name); err == nil && !st.IsDir() {
			return name, nil
		}
	}
	return "", fmt.Errorf("no Fusefile in the current directory (looked for %s): run `fuse init` to scaffold one, or pass -f <path>",
		strings.Join(fusefileNames, ", "))
}

// defaultTaskID derives a task id from the resolved Fusefile's parent
// directory name, falling back to "fuse-env" for degenerate paths (the
// Fusefile itself has no task id field, so the CLI must pick one).
func defaultTaskID(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		// filepath.Abs only fails if the cwd is unreadable; fall back to a safe default rather than erroring.
		return "fuse-env"
	}
	base := filepath.Base(filepath.Dir(abs))
	if base == "" || base == "." || base == "/" {
		return "fuse-env"
	}
	return base
}

// toSDKExpose converts the compiler's expose entries into the SDK wire type.
func toSDKExpose(in []fusefile.ExposeSpec) []fuse.ExposeSpec {
	if len(in) == 0 {
		return nil
	}
	out := make([]fuse.ExposeSpec, len(in))
	for i, e := range in {
		out[i] = fuse.ExposeSpec{Port: e.Port, As: e.As}
	}
	return out
}

// toSDKHealthcheck converts the compiler's environment-level probe into the
// SDK wire type. Nil in, nil out: a Fusefile with no `healthcheck:` block must
// send no healthcheck, which is what tells the server to ship no probe into
// the guest.
func toSDKHealthcheck(in *fusefile.HealthcheckSpec) *fuse.HealthcheckSpec {
	if in == nil {
		return nil
	}
	out := &fuse.HealthcheckSpec{
		Exec:               in.Exec,
		IntervalSeconds:    in.IntervalSeconds,
		TimeoutSeconds:     in.TimeoutSeconds,
		Retries:            in.Retries,
		StartPeriodSeconds: in.StartPeriodSeconds,
	}
	if in.HTTP != nil {
		out.HTTP = &fuse.HealthcheckHTTP{Port: in.HTTP.Port, Path: in.HTTP.Path}
	}
	return out
}

// toSDKDesktop converts the compiler's desktop block into the SDK wire type.
// Nil in, nil out: a Fusefile with no `desktop:` block must send no desktop,
// which is what tells the server to ship no geometry file into the guest.
func toSDKDesktop(in *fusefile.DesktopSpec) *fuse.DesktopSpec {
	if in == nil {
		return nil
	}
	return &fuse.DesktopSpec{Width: in.Width, Height: in.Height}
}

// missingSecrets returns the entries of required that have does not supply a
// value for. An empty value counts as missing: `--secret PG_PASSWORD=` is a
// typo far more often than it is a deliberate empty credential, and an
// environment that boots with one fails later and further from the cause.
// --allow-empty-secrets is the opt-out.
func missingSecrets(required []string, have map[string]string, allowEmpty bool) []string {
	var missing []string
	for _, name := range required {
		v, ok := have[name]
		if !ok || (v == "" && !allowEmpty) {
			missing = append(missing, name)
		}
	}
	return missing
}

// loadSecretsFile parses KEY=VALUE lines from path, ignoring blank lines and
// lines starting with '#'. an empty path returns a nil map.
func loadSecretsFile(path string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read secrets file: %w", err)
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lines = append(lines, line)
	}
	return parseKeyVals(lines)
}
