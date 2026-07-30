package fusefile

import (
	"encoding/base64"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// modePattern matches an octal permission string with an optional leading
// zero, e.g. "755" or "0644". Setuid/setgid/sticky (a fourth digit) is
// deliberately not accepted: nothing in the authoring format needs it, and a
// typo that silently produced a setuid binary in the guest is a worse outcome
// than an error.
var modePattern = regexp.MustCompile(`^0?[0-7]{3}$`)

// ResolveFiles reads every Files entry that names a Source into its Content,
// resolving Source relative to baseDir (the Fusefile's directory).
//
// It is separate from Compile so Compile stays a pure function of the
// Fusefile: only this step touches the filesystem, and a caller that already
// holds the content (a test, or a future non-file authoring path) can skip it.
// Callers that never set Source can skip it too; Compile validates either way.
func ResolveFiles(f *Fusefile, baseDir string) error {
	for i := range f.Files {
		src := f.Files[i].Source
		if src == "" {
			continue
		}
		full := src
		if !filepath.IsAbs(full) {
			full = filepath.Join(baseDir, full)
		}
		data, err := os.ReadFile(full)
		if err != nil {
			return fmt.Errorf("files[%d].source: %w", i, err)
		}
		f.Files[i].Content = string(data)
		// Clearing Source keeps the "exactly one of source/content" rule
		// satisfied for the validator, which runs against the resolved
		// Fusefile and cannot tell a read-in body from a literal one.
		f.Files[i].Source = ""
	}
	return nil
}

// validateFiles checks the Files block at compile time: each entry names a
// usable guest path and a well-formed mode, and the block as a whole fits
// MaxFilesBytes. Every violation is collected so one `fuse up` reports them
// all.
//
// Which body an entry carries is checked in validate() at Parse time instead,
// because ResolveFiles runs in between and folds Source into Content. By the
// time compilation sees an entry, an empty Content is indistinguishable from
// an empty source file, which is legitimate.
func validateFiles(files []File) []error {
	var errs []error
	var total int
	seen := make(map[string]int, len(files))

	for i, file := range files {
		if file.Mode != "" && !modePattern.MatchString(file.Mode) {
			errs = append(errs, fmt.Errorf("files[%d].mode: invalid octal mode %q (expected e.g. \"0644\")", i, file.Mode))
		}

		if err := validateGuestPath(i, file.Path); err != nil {
			errs = append(errs, err)
		} else if prev, dup := seen[path.Clean(file.Path)]; dup {
			errs = append(errs, fmt.Errorf("files[%d].path: %q is already written by files[%d]", i, file.Path, prev))
		} else {
			seen[path.Clean(file.Path)] = i
		}

		total += len(file.Content)
	}

	if total > MaxFilesBytes {
		errs = append(errs, fmt.Errorf(
			"files: total content is %d bytes, over the %d byte limit; fetch large artifacts from `setup` instead",
			total, MaxFilesBytes))
	}
	return errs
}

// validateGuestPath rejects paths that would not land where the author meant.
// A relative path is resolved against the workspace by the generated script,
// so ".." is refused rather than quietly escaping it; the same check keeps an
// absolute path from carrying a traversal segment.
func validateGuestPath(i int, p string) error {
	if p == "" {
		return fmt.Errorf("files[%d].path: is required", i)
	}
	if strings.ContainsAny(p, "\x00\n") {
		return fmt.Errorf("files[%d].path: must not contain newlines or NUL bytes", i)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("files[%d].path: must not contain %q segments", i, "..")
		}
	}
	if strings.HasSuffix(p, "/") {
		return fmt.Errorf("files[%d].path: %q is a directory, not a file", i, p)
	}
	return nil
}

// renderFiles emits the shell that materializes every file in the guest.
//
// Content goes through base64 so an arbitrary body (binary, quotes, embedded
// heredoc terminators, no trailing newline) survives being carried inside a
// generated shell script. The heredoc terminator is quoted, so the shell does
// no expansion on the payload, and the payload is base64 so it cannot contain
// the terminator.
func renderFiles(files []File) string {
	if len(files) == 0 {
		return ""
	}
	var b strings.Builder
	for i, file := range files {
		if dir := path.Dir(file.Path); dir != "." && dir != "/" {
			fmt.Fprintf(&b, "mkdir -p %s\n", shellQuote(dir))
		}
		term := fmt.Sprintf("FUSE_FILE_EOF_%d", i)
		fmt.Fprintf(&b, "base64 -d > %s <<'%s'\n", shellQuote(file.Path), term)
		// Wrapped so a large file does not become one enormous line, which
		// some shells and intermediate transports handle poorly.
		enc := base64.StdEncoding.EncodeToString([]byte(file.Content))
		for start := 0; start < len(enc); start += 76 {
			end := min(start+76, len(enc))
			b.WriteString(enc[start:end])
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s\n", term)
		if file.Mode != "" {
			fmt.Fprintf(&b, "chmod %s %s\n", file.Mode, shellQuote(file.Path))
		}
	}
	return b.String()
}
