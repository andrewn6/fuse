package main

import (
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
		noWait            bool
		fromBuild         string
		plan              bool
		noCache           bool
	)
	cmd := &cobra.Command{
		Use:   "up [path]",
		Short: "Compile a Fusefile and create an environment from it",
		Long: "up reads a Fusefile, compiles it into a resource spec, manifest, and\n" +
			"startup script, then creates an environment from the result. it streams\n" +
			"provisioning events until a terminal state unless --no-wait is set.",
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

			if fromBuild == "" {
				// derive layer keys when caching is on (so a bad `inputs` entry
				// fails here rather than mid-provision) or when the user asked
				// to see the plan.
				if lp, err := planSetupLayers(path, f, plan); err != nil {
					return err
				} else if plan {
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

			if taskID == "" {
				taskID = defaultTaskID(path)
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

			manifestInline := base64.StdEncoding.EncodeToString(c.ManifestJSON)

			cl, _, err := app.client()
			if err != nil {
				return err
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
					Image:              c.Spec.Image,
					GPUs:               c.Spec.GPUs,
					GPUKind:            c.Spec.GPUKind,
					GPUProfile:         c.Spec.GPUProfile,
					HostID:             c.Spec.HostID,
					Labels:             c.Spec.Labels,
				},
				ManifestInline:              manifestInline,
				Secrets:                     secretMap,
				StartupScript:               startupScript,
				StartupScriptTimeoutSeconds: c.StartupTimeoutSeconds,
				Expose:                      toSDKExpose(c.Expose),
				SeedSnapshotID:              fromBuild,
			})
			if err != nil {
				return friendly(err)
			}
			successf("creating environment %s (task %s)", e.ID, e.TaskID)
			if !noWait {
				// the step events carry only an index, so the cli supplies the
				// setup commands it just compiled as the labels.
				labels := make([]string, 0, len(f.Setup))
				for _, step := range f.Setup {
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
				return nil
			}
			if app.isJSON() {
				return printJSON(e)
			}
			renderEnvDetail(e)
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "path to the Fusefile (default: ./Fusefile, or the positional path)")
	cmd.Flags().StringArrayVar(&secrets, "secret", nil, "secret as key=value (repeatable, overrides --secrets-file)")
	cmd.Flags().StringVar(&secretsFile, "secrets-file", "", "path to a file of KEY=VALUE secret lines")
	cmd.Flags().BoolVar(&allowEmptySecrets, "allow-empty-secrets", false, "treat an empty value as satisfying a required secret")
	cmd.Flags().StringVar(&taskID, "task-id", "", "environment task id (default: the Fusefile's parent directory name)")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "create the environment without streaming provisioning events")
	cmd.Flags().StringVar(&fromBuild, "from-build", "", "boot from a `fuse build` artifact instead of a base image (skips the setup phase)")
	cmd.Flags().BoolVar(&plan, "plan", false, "print the derived setup layer cache plan and exit without creating anything")
	cmd.Flags().BoolVar(&noCache, "no-cache", false, "ignore the Fusefile's cache block and run every setup step")
	return cmd
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
