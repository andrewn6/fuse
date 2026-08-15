package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/folsomintel/fuse/internal/fusefile"
)

// copyFilter reports whether something found under a directory source should
// be shipped into the guest. rel is slash-separated and relative to that
// entry's `from` directory, and isDir says which kind of entry it is: returning
// false for a directory prunes the whole subtree rather than just that one
// path, which is what makes a filter cheap on a big tree.
//
// A nil filter ships everything, which is what `fuse up` passes today. It is a
// parameter rather than a hardcoded rule so an ignore-file matcher can be
// dropped in without the walk itself changing.
type copyFilter func(rel string, isDir bool) bool

// copyDecision is what the walk does with one path a filter rejected. The two
// drops are kept apart because --show-copy reports them separately: a file
// missing because of a pattern the author wrote is a different surprise from
// one missing because of a built-in default they never saw.
type copyDecision int

const (
	copyKeep copyDecision = iota
	copySkipPattern
	copySkipDefault
)

// copyDecider is the reporting form of copyFilter, and what the walk actually
// consults. .fuseignore supplies one (fuseignore.decide); a plain copyFilter
// is adapted into one.
type copyDecider func(rel string, isDir bool) copyDecision

// copyEntryStat is one copy entry's contribution to the create request, which
// is what --show-copy prints. A skipped directory counts once rather than once
// per file beneath it: the point of pruning is not to walk what was ignored,
// so nothing here knows how big node_modules was.
type copyEntryStat struct {
	Spec           fusefile.CopySpec
	Files          int
	Bytes          int64
	SkippedPattern int
	SkippedDefault int
}

// collectCopyFiles resolves each compiled copy entry against baseDir (the
// Fusefile's directory) and reads it into the guest-path-to-bytes map the
// create request carries as `files`.
//
// It is the only part of `copy` that touches the filesystem: the compiler
// resolves guest paths and nothing else, so everything that needs to know what
// a source actually is happens here. Three rules it enforces that the compiler
// cannot:
//
//   - `from` resolves against the Fusefile's directory, never the process
//     working directory, so `fuse up ./repro/Fusefile` copies the same files it
//     would from inside that directory.
//   - a symlink is an error rather than something followed. Following one
//     silently ships whatever it points at, which for an absolute or escaping
//     link is a file the author never meant to send.
//   - the whole block is capped at fusefile.MaxCopyBytes, checked from the
//     stat size as the walk goes, so `copy: {from: .}` on a repo full of build
//     output fails naming the limit instead of reading gigabytes into memory
//     and posting a body the orchestrator was always going to refuse.
func collectCopyFiles(entries []fusefile.CopySpec, baseDir string, filter copyFilter) (map[string][]byte, error) {
	var decide copyDecider
	if filter != nil {
		// a bare filter says no more than "drop it", so everything it drops
		// is reported as a pattern skip.
		decide = func(rel string, isDir bool) copyDecision {
			if filter(rel, isDir) {
				return copyKeep
			}
			return copySkipPattern
		}
	}
	files, _, err := collectCopy(entries, baseDir, decide)
	return files, err
}

// collectCopy is collectCopyFiles with the per-entry counts --show-copy needs.
func collectCopy(entries []fusefile.CopySpec, baseDir string, decide copyDecider) (map[string][]byte, []copyEntryStat, error) {
	if len(entries) == 0 {
		return nil, nil, nil
	}

	c := &copyCollector{files: make(map[string][]byte), owner: make(map[string]int)}
	for i, entry := range entries {
		// one stat per entry, in entry order, so stats[i] is entry i.
		c.stats = append(c.stats, copyEntryStat{Spec: entry})

		src := entry.From
		if !filepath.IsAbs(src) {
			src = filepath.Join(baseDir, src)
		}

		// Lstat, not Stat: a symlink has to be reported as itself to be
		// refused rather than resolved behind the author's back.
		info, err := os.Lstat(src)
		if err != nil {
			return nil, nil, fmt.Errorf("copy[%d].from: %w", i, err)
		}

		switch {
		case info.Mode()&os.ModeSymlink != 0:
			return nil, nil, fmt.Errorf(
				"copy[%d].from: %s is a symlink, which copy does not follow; name what it points at instead", i, entry.From)
		case info.IsDir():
			if err := walkCopyDir(c, i, src, entry, decide); err != nil {
				return nil, nil, err
			}
		case info.Mode().IsRegular():
			// an entry that names one file is copied as written: ignores only
			// bound the expansion of a directory, so `from: ./.env` still
			// ships the file the author explicitly asked for.
			if err := c.add(i, src, entry.To, info.Size()); err != nil {
				return nil, nil, err
			}
		default:
			return nil, nil, fmt.Errorf(
				"copy[%d].from: %s is neither a regular file nor a directory (mode %s)", i, entry.From, info.Mode())
		}
	}
	return c.files, c.stats, nil
}

// walkCopyDir expands a directory source into one entry per regular file
// beneath it, landing each at entry.To plus its path relative to the source.
//
// Directories themselves are never entries: the host agent creates a file's
// parents on upload, so an empty directory simply does not travel. Anything
// that is neither a directory nor a regular file (a symlink, a socket, a
// device node) fails the walk rather than being skipped, because silently
// dropping part of what an author asked to copy is how a guest ends up missing
// one file and nobody knows why.
func walkCopyDir(c *copyCollector, i int, root string, entry fusefile.CopySpec, decide copyDecider) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("copy[%d].from: %w", i, err)
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return fmt.Errorf("copy[%d].from: %w", i, err)
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)

		// checked before anything reads or sizes the path, so an ignored
		// file counts against neither the size cap nor the request body.
		if decide != nil {
			if got := decide(rel, d.IsDir()); got != copyKeep {
				if got == copySkipDefault {
					c.stats[i].SkippedDefault++
				} else {
					c.stats[i].SkippedPattern++
				}
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			kind := "is not a regular file"
			if d.Type()&fs.ModeSymlink != 0 {
				kind = "is a symlink, which copy does not follow"
			}
			return fmt.Errorf("copy[%d].from: %s %s", i, path.Join(entry.From, rel), kind)
		}

		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("copy[%d].from: %w", i, err)
		}
		return c.add(i, p, path.Join(entry.To, rel), info.Size())
	})
}

// encodeCopyFiles renders the walked files as the create request's `files`
// map: guest path to base64 body, the same encoding manifest_inline uses,
// since json cannot carry arbitrary bytes. Nil in, nil out, so a Fusefile with
// no copy block sends no field at all.
func encodeCopyFiles(files map[string][]byte) map[string]string {
	if len(files) == 0 {
		return nil
	}
	out := make(map[string]string, len(files))
	for path, data := range files {
		out[path] = base64.StdEncoding.EncodeToString(data)
	}
	return out
}

// renderCopyReport prints what --show-copy exists for: what each entry
// actually ships, and what was taken out of it, while the answer can still
// change something.
//
// Skips are counted per path rather than per file, so an ignored directory
// counts once no matter how much was under it. That is the point of pruning:
// nothing here walked node_modules, so nothing here can say how big it was.
func renderCopyReport(w io.Writer, stats []copyEntryStat) {
	if len(stats) == 0 {
		_, _ = fmt.Fprintln(w, "copy: nothing, this Fusefile has no copy block")
		return
	}
	var files int
	var total int64
	for _, s := range stats {
		_, _ = fmt.Fprintf(w, "copy %s -> %s\n", s.Spec.From, s.Spec.To)
		_, _ = fmt.Fprintf(w, "  %s, %s%s\n", copyFileCount(s.Files), copySize(s.Bytes), copySkipNote(s))
		files += s.Files
		total += s.Bytes
	}
	_, _ = fmt.Fprintf(w, "total: %s, %s (limit %s)\n", copyFileCount(files), copySize(total), copySize(fusefile.MaxCopyBytes))
}

func copyFileCount(n int) string {
	if n == 1 {
		return "1 file"
	}
	return fmt.Sprintf("%d files", n)
}

// copySize is humanBytes with a real zero, since an entry that shipped nothing
// shipped 0 B rather than an unknown size.
func copySize(n int64) string {
	if n == 0 {
		return "0 B"
	}
	return humanBytes(n)
}

// copySkipNote reports the two kinds of skip apart, because "my file is
// missing" has a different answer for each: edit your .fuseignore, or add a
// `!` line to override a default you never wrote.
func copySkipNote(s copyEntryStat) string {
	switch {
	case s.SkippedPattern > 0 && s.SkippedDefault > 0:
		return fmt.Sprintf(" (%d skipped by %s, %d by the defaults)", s.SkippedPattern, fuseignoreName, s.SkippedDefault)
	case s.SkippedPattern > 0:
		return fmt.Sprintf(" (%d skipped by %s)", s.SkippedPattern, fuseignoreName)
	case s.SkippedDefault > 0:
		return fmt.Sprintf(" (%d skipped by the defaults)", s.SkippedDefault)
	}
	return ""
}

// copyCollector accumulates the walked files and enforces, as it goes, the two
// rules that are only knowable across entries: the total size cap, and that no
// two entries write the same guest path.
type copyCollector struct {
	files map[string][]byte
	owner map[string]int // guest path -> the copy entry that claimed it
	total int64
	stats []copyEntryStat // one per entry, in entry order
}

// add records one local file at its guest path, refusing to read it at all
// once the block is over budget.
func (c *copyCollector) add(i int, src, dst string, size int64) error {
	if prev, taken := c.owner[dst]; taken {
		return fmt.Errorf("copy[%d]: %s would overwrite the file copy[%d] already puts there (from %s)", i, dst, prev, src)
	}

	// Checked before the read, from the stat size, so an oversized source is
	// never pulled into memory just to be rejected.
	c.total += size
	if c.total > fusefile.MaxCopyBytes {
		return fmt.Errorf(
			"copy[%d]: the copy block is over its %d byte limit (reached %d bytes at %s); "+
				"copy a narrower directory, or fetch large files from `setup` instead",
			i, fusefile.MaxCopyBytes, c.total, src)
	}

	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("copy[%d].from: %w", i, err)
	}
	c.files[dst] = data
	c.owner[dst] = i
	c.stats[i].Files++
	c.stats[i].Bytes += size
	return nil
}
