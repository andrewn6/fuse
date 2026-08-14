package main

import (
	"encoding/base64"
	"fmt"
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
	if len(entries) == 0 {
		return nil, nil
	}

	c := &copyCollector{files: make(map[string][]byte), owner: make(map[string]int)}
	for i, entry := range entries {
		src := entry.From
		if !filepath.IsAbs(src) {
			src = filepath.Join(baseDir, src)
		}

		// Lstat, not Stat: a symlink has to be reported as itself to be
		// refused rather than resolved behind the author's back.
		info, err := os.Lstat(src)
		if err != nil {
			return nil, fmt.Errorf("copy[%d].from: %w", i, err)
		}

		switch {
		case info.Mode()&os.ModeSymlink != 0:
			return nil, fmt.Errorf(
				"copy[%d].from: %s is a symlink, which copy does not follow; name what it points at instead", i, entry.From)
		case info.IsDir():
			if err := walkCopyDir(c, i, src, entry, filter); err != nil {
				return nil, err
			}
		case info.Mode().IsRegular():
			if err := c.add(i, src, entry.To, info.Size()); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf(
				"copy[%d].from: %s is neither a regular file nor a directory (mode %s)", i, entry.From, info.Mode())
		}
	}
	return c.files, nil
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
func walkCopyDir(c *copyCollector, i int, root string, entry fusefile.CopySpec, filter copyFilter) error {
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

		if filter != nil && !filter(rel, d.IsDir()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
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

// copyCollector accumulates the walked files and enforces, as it goes, the two
// rules that are only knowable across entries: the total size cap, and that no
// two entries write the same guest path.
type copyCollector struct {
	files map[string][]byte
	owner map[string]int // guest path -> the copy entry that claimed it
	total int64
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
	return nil
}
