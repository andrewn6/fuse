package main

import (
	"encoding/base64"
	"fmt"
	"os"
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
		file        string
		name        string
		secrets     []string
		secretsFile string
		keep        bool
	)
	cmd := &cobra.Command{
		Use:   "build [path]",
		Short: "Run a Fusefile's setup phase once and snapshot the result",
		Long: "build boots a throwaway environment, runs the Fusefile's setup phase in\n" +
			"it, snapshots the resulting disk, and destroys the environment. it prints\n" +
			"the snapshot id, which `fuse up --from-build <id>` boots directly so the\n" +
			"setup work does not rerun on every boot.\n\n" +
			"the artifact is host-local: there is no object storage, so it can only be\n" +
			"booted on the host that produced it. firecracker hosts only, since qemu\n" +
			"hosts cannot snapshot.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := resolveFusefilePath(file, args)

			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("read %s: %w", path, err)
			}
			f, err := fusefile.Parse(data)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			c, err := fusefile.Compile(f)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if c.BuildScript == "" {
				return fmt.Errorf("%s: no setup phase to build", path)
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
			if missing := missingSecrets(c.RequiredSecrets, secretMap); len(missing) > 0 {
				return fmt.Errorf("missing required secrets: %s", strings.Join(missing, ", "))
			}

			if name == "" {
				name = defaultTaskID(path)
			}

			cl, _, err := app.client()
			if err != nil {
				return err
			}

			// no startup script: the build phase runs through exec instead, for
			// the 600s ceiling and so its output can be shown on failure.
			e, err := cl.Environments.Create(cmd.Context(), fuse.CreateRequest{
				TaskID: buildTaskID(name),
				Spec: fuse.Spec{
					CPUs:      c.Spec.CPUs,
					RamMB:     c.Spec.RamMB,
					StorageGB: c.Spec.StorageGB,
					Region:    c.Spec.Region,
					Image:     c.Spec.Image,
				},
				ManifestInline: base64.StdEncoding.EncodeToString(c.ManifestJSON),
				Secrets:        secretMap,
			})
			if err != nil {
				return friendly(err)
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

			if err := waitForEnvironmentReady(cmd.Context(), cl, e.ID); err != nil {
				destroy()
				return err
			}

			res, err := cl.Environments.Exec(cmd.Context(), e.ID, fuse.ExecRequest{
				Shell:     c.BuildScript,
				TimeoutMS: int(buildExecTimeout / time.Millisecond),
			})
			if err != nil {
				destroy()
				return friendly(err)
			}
			// unlike the startup-script path, build output is surfaced. a build
			// that fails silently is not debuggable.
			if res.Stdout != "" {
				fmt.Fprint(cmd.OutOrStdout(), res.Stdout)
			}
			if res.Stderr != "" {
				fmt.Fprint(cmd.ErrOrStderr(), res.Stderr)
			}
			if res.ExitCode != 0 {
				destroy()
				return fmt.Errorf("build phase exited %d", res.ExitCode)
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
			fmt.Fprintln(cmd.OutOrStdout(), snap.ID)
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "path to the Fusefile (default: ./Fusefile, or the positional path)")
	cmd.Flags().StringVar(&name, "name", "", "name for the build artifact (default: the Fusefile's parent directory name)")
	cmd.Flags().StringArrayVar(&secrets, "secret", nil, "secret as key=value (repeatable, overrides --secrets-file)")
	cmd.Flags().StringVar(&secretsFile, "secrets-file", "", "path to a file of KEY=VALUE secret lines")
	cmd.Flags().BoolVar(&keep, "keep", false, "leave the builder environment running instead of destroying it")
	return cmd
}

// buildTaskID derives the builder's task id. it is deliberately distinct from
// the task id `fuse up` derives from the same Fusefile, since a task id may
// only be assigned to one vm at a time and a build must not collide with the
// environment it is building for.
func buildTaskID(name string) string {
	return fmt.Sprintf("build-%s-%d", name, time.Now().UnixNano())
}
