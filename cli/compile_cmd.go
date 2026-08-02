package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/folsomintel/fuse/internal/fusefile"
)

// compile output formats. text is the default human rendering; json and yaml
// emit the wire request `fuse up` would post.
const (
	compileFormatText = "text"
	compileFormatJSON = "json"
	compileFormatYAML = "yaml"
)

// compileParts are the valid --only selectors, in the order they render in
// text mode.
var compileParts = []string{"spec", "startup-script", "manifest", "expose", "secrets"}

// compiledSpec mirrors fuse.Spec field-for-field with yaml tags added, so yaml
// output keys match the json wire names instead of yaml.v3's lowercased
// defaults. TestCompiledRequestMirrorsWireTypes guards against drift.
type compiledSpec struct {
	CPUs       int32  `json:"cpus,omitempty" yaml:"cpus,omitempty"`
	RamMB      int32  `json:"ram_mb,omitempty" yaml:"ram_mb,omitempty"`
	StorageGB  int32  `json:"storage_gb,omitempty" yaml:"storage_gb,omitempty"`
	GPUs       int32  `json:"gpus,omitempty" yaml:"gpus,omitempty"`
	GPUKind    string `json:"gpu_kind,omitempty" yaml:"gpu_kind,omitempty"`
	GPUProfile string `json:"gpu_profile,omitempty" yaml:"gpu_profile,omitempty"`
	Region     string `json:"region,omitempty" yaml:"region,omitempty"`
	// Arch restricts scheduling to hosts of this CPU architecture ("amd64",
	// "arm64"). Not yet settable from a Fusefile; carried for wire parity.
	Arch              string `json:"arch,omitempty" yaml:"arch,omitempty"`
	MaxRuntimeSeconds int64  `json:"max_runtime_seconds,omitempty" yaml:"max_runtime_seconds,omitempty"`
	// IdleTimeoutSeconds is the no-exec/no-attach window after which the
	// environment is destroyed. Zero means no idle expiry.
	IdleTimeoutSeconds int64  `json:"idle_timeout_seconds,omitempty" yaml:"idle_timeout_seconds,omitempty"`
	Image              string `json:"image,omitempty" yaml:"image,omitempty"`
	// HostID and Labels are the placement constraints; both are hard gates
	// the scheduler applies, so they belong in what the Fusefile compiles to.
	HostID string            `json:"host_id,omitempty" yaml:"host_id,omitempty"`
	Labels map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
}

// compiledExpose mirrors fuse.ExposeSpec with yaml tags added.
type compiledExpose struct {
	Port int    `json:"port" yaml:"port"`
	As   string `json:"as,omitempty" yaml:"as,omitempty"`
}

// compiledRequest is the create-environment body a Fusefile compiles into. it
// mirrors fuse.CreateRequest so the output is directly comparable to what the
// orchestrator receives, with two deliberate differences:
//
//   - secrets is always an empty object. `fuse compile` never accepts secret
//     values, so there is nothing to put in it; values are supplied at
//     `fuse up`.
//   - required_secrets is extra, not part of the wire body: it is the list of
//     secret names (never values) the environment needs.
//
// the gateway fields of fuse.CreateRequest are omitted on purpose: they carry
// a credential and are not set from a Fusefile.
type compiledRequest struct {
	TaskID         string            `json:"task_id" yaml:"task_id"`
	Spec           compiledSpec      `json:"spec" yaml:"spec"`
	ManifestInline string            `json:"manifest_inline,omitempty" yaml:"manifest_inline,omitempty"`
	Secrets        map[string]string `json:"secrets" yaml:"secrets"`
	StartupScript  string            `json:"startup_script,omitempty" yaml:"startup_script,omitempty"`
	Expose         []compiledExpose  `json:"expose,omitempty" yaml:"expose,omitempty"`
	SeedSnapshotID string            `json:"seed_snapshot_id,omitempty" yaml:"seed_snapshot_id,omitempty"`
	// StartupScriptTimeoutSeconds bounds setup + run. Zero means the author
	// asked for no bound and the orchestrator's default applies.
	StartupScriptTimeoutSeconds int64    `json:"startup_script_timeout_seconds,omitempty" yaml:"startup_script_timeout_seconds,omitempty"`
	RequiredSecrets             []string `json:"required_secrets,omitempty" yaml:"required_secrets,omitempty"`
}

func newCompileCmd() *cobra.Command {
	var (
		file      string
		taskID    string
		format    string
		only      string
		fromBuild string
	)
	cmd := &cobra.Command{
		Use:   "compile [path]",
		Short: "Compile a Fusefile and print the result without creating anything",
		Long: "compile reads a Fusefile, compiles it into a resource spec, manifest,\n" +
			"startup script, exposed ports, and required secret names, then prints the\n" +
			"result. it is entirely client-side: no orchestrator connection is made and\n" +
			"no context or host needs to be configured.\n\n" +
			"secret values are never accepted or printed; only the names a `fuse up` of\n" +
			"the same Fusefile would require.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := resolveFusefilePath(file, args)

			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("read %s: %w", path, err)
			}
			// parse before compile: Compile accepts an unvalidated Fusefile,
			// so going through Parse is what applies the structural rules.
			f, err := fusefile.Parse(data)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			c, err := fusefile.Compile(f)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}

			// same rule as `fuse up`: a build artifact and `image` both name
			// the rootfs to boot, so compiling both would preview a request
			// the orchestrator would reject.
			if fromBuild != "" && c.Spec.Image != "" {
				return fmt.Errorf("--from-build and the Fusefile's `image` are mutually exclusive: both name the rootfs to boot")
			}

			if taskID == "" {
				taskID = defaultTaskID(path)
			}
			req := newCompiledRequest(taskID, fromBuild, c)

			out := cmd.OutOrStdout()
			if only != "" {
				if cmd.Flags().Changed("format") {
					return fmt.Errorf("--only and --format are mutually exclusive")
				}
				return writeCompiledPart(out, only, c, req)
			}

			resolved, err := resolveCompileFormat(format)
			if err != nil {
				return err
			}
			return writeCompiled(out, resolved, c, req)
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "path to the Fusefile (default: ./Fusefile, or the positional path)")
	cmd.Flags().StringVar(&taskID, "task-id", "", "environment task id (default: the Fusefile's parent directory name)")
	cmd.Flags().StringVar(&format, "format", "", "output format: text | json | yaml (overrides -o/--output)")
	cmd.Flags().StringVar(&only, "only", "", "print one part undecorated: "+strings.Join(compileParts, " | "))
	cmd.Flags().StringVar(&fromBuild, "from-build", "", "compile as `fuse up --from-build` would: boot from a build artifact instead of a base image (skips the setup phase)")
	return cmd
}

// resolveCompileFormat picks the output format. an explicit --format wins;
// otherwise the persistent -o/--output is mapped onto it (table -> text).
func resolveCompileFormat(format string) (string, error) {
	if format == "" {
		if app.isJSON() {
			return compileFormatJSON, nil
		}
		return compileFormatText, nil
	}
	switch format {
	case compileFormatText, compileFormatJSON, compileFormatYAML:
		return format, nil
	default:
		return "", fmt.Errorf("invalid --format %q: want text, json, or yaml", format)
	}
}

// newCompiledRequest turns a compiled Fusefile into the wire request shape. a
// non-empty seedSnapshotID mirrors `fuse up --from-build`, which sends the run
// phase alone because the artifact already carries the setup phase's result.
func newCompiledRequest(taskID, seedSnapshotID string, c *fusefile.Compiled) compiledRequest {
	startupScript := c.StartupScript
	if seedSnapshotID != "" {
		startupScript = c.RunScript
	}
	req := compiledRequest{
		TaskID: taskID,
		Spec: compiledSpec{
			CPUs:               c.Spec.CPUs,
			RamMB:              c.Spec.RamMB,
			StorageGB:          c.Spec.StorageGB,
			GPUs:               c.Spec.GPUs,
			GPUKind:            c.Spec.GPUKind,
			GPUProfile:         c.Spec.GPUProfile,
			Region:             c.Spec.Region,
			Arch:               c.Spec.Arch,
			MaxRuntimeSeconds:  c.Spec.MaxRuntimeSeconds,
			IdleTimeoutSeconds: c.Spec.IdleTimeoutSeconds,
			HostID:             c.Spec.HostID,
			Labels:             c.Spec.Labels,
			Image:              c.Spec.Image,
		},
		// values are supplied at `fuse up`, so the object is always empty.
		Secrets:                     map[string]string{},
		StartupScript:               startupScript,
		SeedSnapshotID:              seedSnapshotID,
		StartupScriptTimeoutSeconds: c.StartupTimeoutSeconds,
		RequiredSecrets:             c.RequiredSecrets,
	}
	if len(c.ManifestJSON) > 0 {
		req.ManifestInline = base64.StdEncoding.EncodeToString(c.ManifestJSON)
	}
	for _, e := range c.Expose {
		req.Expose = append(req.Expose, compiledExpose{Port: e.Port, As: e.As})
	}
	return req
}

// writeCompiled renders a compiled Fusefile in the given format. it is the
// single owner of compiled-artifact output so `fuse compile` and any future
// `--dry-run` cannot drift.
func writeCompiled(w io.Writer, format string, c *fusefile.Compiled, req compiledRequest) error {
	switch format {
	case compileFormatJSON:
		return writeJSON(w, req)
	case compileFormatYAML:
		enc := yaml.NewEncoder(w)
		enc.SetIndent(2)
		if err := enc.Encode(req); err != nil {
			return err
		}
		return enc.Close()
	default:
		return writeCompiledText(w, c, req)
	}
}

// writeCompiledText renders the human view: sections in a fixed order, empty
// ones omitted, so two runs over the same Fusefile diff cleanly.
func writeCompiledText(w io.Writer, c *fusefile.Compiled, req compiledRequest) error {
	_, _ = fmt.Fprintf(w, "task id  %s\n", req.TaskID)
	if req.SeedSnapshotID != "" {
		_, _ = fmt.Fprintf(w, "seed snapshot  %s (setup phase skipped)\n", req.SeedSnapshotID)
	}
	if req.StartupScriptTimeoutSeconds > 0 {
		_, _ = fmt.Fprintf(w, "startup timeout  %ds\n", req.StartupScriptTimeoutSeconds)
	}

	_, _ = fmt.Fprintf(w, "\nspec\n")
	rows := [][2]string{
		{"cpus", fmt.Sprint(req.Spec.CPUs)},
		{"ram mb", fmt.Sprint(req.Spec.RamMB)},
		{"storage gb", fmt.Sprint(req.Spec.StorageGB)},
		{"max runtime s", fmt.Sprint(req.Spec.MaxRuntimeSeconds)},
		{"idle timeout s", fmt.Sprint(req.Spec.IdleTimeoutSeconds)},
	}
	if req.Spec.Image != "" {
		rows = append(rows, [2]string{"image", req.Spec.Image})
	}
	if req.Spec.Region != "" {
		rows = append(rows, [2]string{"region", req.Spec.Region})
	}
	if req.Spec.GPUs > 0 {
		rows = append(rows, [2]string{"gpus", fmt.Sprint(req.Spec.GPUs)})
	}
	if req.Spec.GPUKind != "" {
		rows = append(rows, [2]string{"gpu kind", req.Spec.GPUKind})
	}
	if req.Spec.GPUProfile != "" {
		rows = append(rows, [2]string{"gpu profile", req.Spec.GPUProfile})
	}
	if req.Spec.HostID != "" {
		rows = append(rows, [2]string{"host", req.Spec.HostID})
	}
	// label keys are sorted so two runs over the same Fusefile diff cleanly.
	labelKeys := make([]string, 0, len(req.Spec.Labels))
	for key := range req.Spec.Labels {
		labelKeys = append(labelKeys, key)
	}
	sort.Strings(labelKeys)
	for _, key := range labelKeys {
		rows = append(rows, [2]string{"label " + key, req.Spec.Labels[key]})
	}
	for _, r := range rows {
		_, _ = fmt.Fprintf(w, "  %-15s %s\n", r[0], r[1])
	}

	if req.StartupScript != "" {
		_, _ = fmt.Fprintf(w, "\nstartup script\n")
		writeIndented(w, req.StartupScript)
	}

	if len(c.ManifestJSON) > 0 {
		_, _ = fmt.Fprintf(w, "\nmanifest (decoded; sent base64 as manifest_inline)\n")
		writeIndented(w, string(c.ManifestJSON))
	}

	if len(c.Expose) > 0 {
		_, _ = fmt.Fprintf(w, "\nexpose\n")
		for _, e := range c.Expose {
			if e.As != "" {
				_, _ = fmt.Fprintf(w, "  %d as %s\n", e.Port, e.As)
				continue
			}
			_, _ = fmt.Fprintf(w, "  %d\n", e.Port)
		}
	}

	if len(c.RequiredSecrets) > 0 {
		_, _ = fmt.Fprintf(w, "\nrequired secrets (values supplied at `fuse up`)\n")
		names := append([]string(nil), c.RequiredSecrets...)
		sort.Strings(names)
		for _, name := range names {
			_, _ = fmt.Fprintf(w, "  %s\n", name)
		}
	}
	return nil
}

// writeCompiledPart writes a single part with no decoration, for piping.
func writeCompiledPart(w io.Writer, part string, c *fusefile.Compiled, req compiledRequest) error {
	switch part {
	case "spec":
		return writeJSON(w, req.Spec)
	case "manifest":
		// the manifest bytes exactly as compiled, so `| jq` sees what the
		// guest sees.
		_, err := fmt.Fprintln(w, string(c.ManifestJSON))
		return err
	case "startup-script":
		if req.StartupScript == "" {
			return nil
		}
		_, err := io.WriteString(w, req.StartupScript)
		return err
	case "expose":
		for _, e := range c.Expose {
			if _, err := fmt.Fprintf(w, "%d %s\n", e.Port, e.As); err != nil {
				return err
			}
		}
		return nil
	case "secrets":
		// names only, one per line. never values.
		names := append([]string(nil), c.RequiredSecrets...)
		sort.Strings(names)
		for _, name := range names {
			if _, err := fmt.Fprintln(w, name); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("invalid --only %q: want %s", part, strings.Join(compileParts, ", "))
	}
}

// writeIndented writes s indented by two spaces per line, dropping the
// trailing blank line a script's final newline would otherwise produce.
func writeIndented(w io.Writer, s string) {
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		_, _ = fmt.Fprintf(w, "  %s\n", line)
	}
}
