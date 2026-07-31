package main

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/folsomintel/fuse/internal/fusefile"
)

// validate exit codes. 0 and 1 are the answer to "is this file valid"; 2 says
// the question could not be asked at all (the file could not be read).
const (
	validateExitInvalid = 1
	validateExitIOError = 2
)

// diagnostic is one validation problem. Path is the Fusefile field it belongs
// to ("resources.memory", "services.db"), Line the 1-based yaml line when the
// underlying error carries one. Neither is always known: fusefile errors are
// plain strings today, so Path is recovered from the message prefix and Line is
// only present for yaml decode failures.
type diagnostic struct {
	Path    string `json:"path,omitempty"`
	Line    int    `json:"line,omitempty"`
	Message string `json:"message"`
}

// validateReport is the -o json payload. Path is the Fusefile that was checked.
type validateReport struct {
	Path            string       `json:"path"`
	Valid           bool         `json:"valid"`
	Errors          []diagnostic `json:"errors"`
	RequiredSecrets []string     `json:"required_secrets,omitempty"`
}

func newValidateCmd() *cobra.Command {
	var (
		file         string
		quiet        bool
		checkSecrets bool
		secrets      []string
		secretsFile  string
	)
	cmd := &cobra.Command{
		Use:   "validate [path]",
		Short: "Check a Fusefile without creating an environment",
		Long: "validate parses and compiles a Fusefile and reports every problem it\n" +
			"finds, then exits 0 if the file is valid and 1 if it is not. it never\n" +
			"talks to an orchestrator, so it works before `fuse connect` and in a CI\n" +
			"container with no control plane reachable.\n\n" +
			"a file that validates can still fail at create time: validate cannot\n" +
			"check whether the requested image is baked on a host, whether a region\n" +
			"matches a registered host, or whether there is capacity.",
		Args: cobra.MaximumNArgs(1),
		// RunE returns nil for an invalid Fusefile and reports the verdict
		// through app.exitCode instead: an invalid file is the answer, not a
		// CLI failure, and fang would otherwise render "Error: ..." above
		// diagnostics the user has already read. Same reason as
		// `environment exec`.
		RunE: func(cmd *cobra.Command, args []string) error {
			path := resolveFusefilePath(file, args)
			app.exitCode = 0

			data, err := os.ReadFile(path)
			if err != nil {
				if !quiet {
					warnf("read %s: %v", path, err)
				}
				app.exitCode = validateExitIOError
				return nil
			}

			diags, compiled := checkFusefile(data)

			// missing secrets are informational by default: validate is meant
			// to run where no secrets are present.
			var required []string
			if compiled != nil {
				required = compiled.RequiredSecrets
			}
			if checkSecrets && len(required) > 0 {
				have, err := resolveValidateSecrets(secrets, secretsFile)
				if err != nil {
					if !quiet {
						warnf("%v", err)
					}
					app.exitCode = validateExitIOError
					return nil
				}
				for _, name := range missingSecrets(required, have) {
					diags = append(diags, diagnostic{
						Path:    "secrets",
						Message: fmt.Sprintf("required secret %q is not set", name),
					})
				}
			}

			if len(diags) > 0 {
				app.exitCode = validateExitInvalid
			}

			if quiet {
				return nil
			}
			if app.isJSON() {
				return printJSON(validateReport{
					Path:            path,
					Valid:           len(diags) == 0,
					Errors:          diags,
					RequiredSecrets: required,
				})
			}
			if len(diags) > 0 {
				// diagnostics go to stdout so a CI job can pipe or capture them.
				for _, d := range diags {
					fmt.Println(formatDiagnostic(path, d))
				}
				return nil
			}
			successf("%s is valid", path)
			if len(required) > 0 {
				infof("required secrets: %s", strings.Join(required, ", "))
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "path to the Fusefile (default: ./Fusefile, or the positional path)")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "suppress all output, report the verdict through the exit code only")
	cmd.Flags().BoolVar(&checkSecrets, "check-secrets", false, "also fail when a required secret is missing from --secret/--secrets-file")
	cmd.Flags().StringArrayVar(&secrets, "secret", nil, "secret as key=value, for --check-secrets (repeatable, overrides --secrets-file)")
	cmd.Flags().StringVar(&secretsFile, "secrets-file", "", "path to a file of KEY=VALUE secret lines, for --check-secrets")
	return cmd
}

// checkFusefile decodes, validates, and compiles data, collecting every
// problem. Decoding is the only hard stop: without a decoded file there is
// nothing to validate or compile. Validation and compilation both run even when
// the first of them fails, so a file with a structural and a resource error
// reports both in one pass (`fuse up` stops after the first batch).
func checkFusefile(data []byte) ([]diagnostic, *fusefile.Compiled) {
	f, err := fusefile.Decode(data)
	if err != nil {
		return decodeDiagnostics(err), nil
	}

	var diags []diagnostic
	diags = append(diags, toDiagnostics(fusefile.Validate(f))...)
	compiled, err := fusefile.Compile(f)
	diags = append(diags, toDiagnostics(err)...)
	return diags, compiled
}

// resolveValidateSecrets merges --secrets-file and --secret into one map,
// with --secret winning on collision, matching `fuse up`.
func resolveValidateSecrets(secrets []string, secretsFile string) (map[string]string, error) {
	have, err := loadSecretsFile(secretsFile)
	if err != nil {
		return nil, err
	}
	if have == nil {
		have = map[string]string{}
	}
	flagSecrets, err := parseKeyVals(secrets)
	if err != nil {
		return nil, err
	}
	for k, v := range flagSecrets {
		have[k] = v
	}
	return have, nil
}

// toDiagnostics flattens a (possibly joined) error into one diagnostic per
// leaf error.
func toDiagnostics(err error) []diagnostic {
	if err == nil {
		return nil
	}
	var out []diagnostic
	for _, leaf := range flattenErrors(err) {
		out = append(out, splitDiagnostic(leaf.Error()))
	}
	return out
}

// flattenErrors walks errors.Join trees, which expose their members through
// Unwrap() []error, and returns the leaves in order.
func flattenErrors(err error) []error {
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return []error{err}
	}
	var out []error
	for _, e := range joined.Unwrap() {
		out = append(out, flattenErrors(e)...)
	}
	return out
}

// fieldPrefixPattern matches the "resources.memory: " style field prefix the
// fusefile package puts in front of its messages. A field path has no spaces,
// which is what keeps it from matching a sentence like "parse fusefile: ...".
var fieldPrefixPattern = regexp.MustCompile(`^([A-Za-z0-9_.\[\]]+): (.*)$`)

// splitDiagnostic recovers the field path from a fusefile error message. The
// package has no typed field error yet, so the path is baked into the string.
func splitDiagnostic(msg string) diagnostic {
	if m := fieldPrefixPattern.FindStringSubmatch(msg); m != nil {
		return diagnostic{Path: m[1], Message: m[2]}
	}
	return diagnostic{Message: msg}
}

// yamlLinePattern matches the position go-yaml reports on a decode failure.
var yamlLinePattern = regexp.MustCompile(`line (\d+): `)

// decodeDiagnostics turns a yaml decode failure into one diagnostic per
// problem. go-yaml reports several at once as an "unmarshal errors:" block with
// one problem per following line (this is how unknown fields surface), so the
// block is split rather than printed as a single multi-line message.
func decodeDiagnostics(err error) []diagnostic {
	lines := strings.Split(strings.TrimSpace(err.Error()), "\n")
	if len(lines) == 1 {
		return []diagnostic{decodeLineDiagnostic(lines[0])}
	}
	var out []diagnostic
	for _, line := range lines[1:] {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, decodeLineDiagnostic(line))
		}
	}
	if len(out) == 0 {
		return []diagnostic{decodeLineDiagnostic(lines[0])}
	}
	return out
}

// decodeLineDiagnostic lifts the yaml position out of one decode error line so
// it lands in Line rather than being repeated in the message. Unknown-field
// errors name the field inside prose, not as a path prefix, so Path stays empty.
func decodeLineDiagnostic(msg string) diagnostic {
	d := diagnostic{Message: msg}
	if m := yamlLinePattern.FindStringSubmatch(msg); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			d.Line = n
			d.Message = strings.TrimSpace(strings.Replace(msg, m[0], "", 1))
		}
	}
	return d
}

// formatDiagnostic renders one problem as "Fusefile:4: field: message", with
// the line and the field omitted when unknown.
func formatDiagnostic(path string, d diagnostic) string {
	var b strings.Builder
	b.WriteString(path)
	if d.Line > 0 {
		fmt.Fprintf(&b, ":%d", d.Line)
	}
	b.WriteString(": ")
	if d.Path != "" {
		b.WriteString(d.Path)
		b.WriteString(": ")
	}
	b.WriteString(d.Message)
	return b.String()
}
