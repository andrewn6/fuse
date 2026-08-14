package fusefile

import (
	"fmt"
	"path"
	"strings"
)

// MaxCopyBytes caps the total size of everything the `copy` block ships.
//
// It is a different number from MaxFilesBytes because it bounds a different
// transport. `files` is embedded in the startup script, which reaches the guest
// as one shell argument and is therefore bounded by MAX_ARG_STRLEN. A copy
// entry instead rides the create request's own files map and is uploaded per
// file, so what bounds it is the orchestrator's 1 MiB request body
// (api.MaxEnvironmentBodyBytes): 512 KiB of sources inflate to roughly 683 KiB
// of base64, which still leaves room in the same body for the manifest, the
// startup script, the secrets map, and the guest paths themselves.
//
// The client enforces it while it walks (cli/copy.go), so `copy: {from: .}` on
// a repo fails naming the size and the limit instead of posting a body that was
// never going to be accepted. The orchestrator re-enforces it, because a client
// it did not compile is not one it can trust.
const MaxCopyBytes = 512 << 10

// reservedGuestDir is the directory the guest agent keeps its own files in:
// the manifest, the resolved secrets, and the per-VM TLS credentials. A copy
// entry that lands there would either clobber them or be clobbered by them, so
// it is refused.
//
// The literal is duplicated from the orchestrator's fused profile
// (internal/orchestrator/agent_profile.go) rather than imported: the compiler
// is the client half of a wire contract and must not depend on the server. The
// orchestrator applies the same rule at the API boundary, which is the check
// that actually protects it; this one exists so an author hears about the
// mistake at `fuse up` time.
const reservedGuestDir = "/fuse"

// CopySpec is one compiled copy entry: an authoring-side source and the
// absolute guest path it lands at.
type CopySpec struct {
	// From is the authored source path, carried through unchanged. It is
	// still relative to the Fusefile's directory, because only the client
	// that reads the Fusefile knows where that is.
	From string

	// To is the resolved absolute guest path. A file source lands exactly
	// here; a directory source lands its tree under here.
	To string
}

// compileCopy resolves every copy entry's guest path against the workspace and
// reports the entries that cannot land where they claim to.
//
// It stays filesystem-free, like the rest of the compiler: whether `from`
// exists, whether it is a directory, and what walking it yields are all the
// CLI's business (cli/copy.go). What is decided here is only what needs the
// workspace, which the CLI does not have to re-derive.
func compileCopy(f *Fusefile) ([]CopySpec, []error) {
	if len(f.Copy) == 0 {
		return nil, nil
	}

	var errs []error
	out := make([]CopySpec, 0, len(f.Copy))
	// guest path -> the entry that claimed it, so a collision names both.
	seen := make(map[string]int, len(f.Copy))
	workspace := workspaceOf(f)

	for i, entry := range f.Copy {
		if strings.TrimSpace(entry.To) == "" {
			// validate() already reported this; resolving it would invent a
			// path the author never wrote.
			continue
		}
		// a relative `to` resolves against the workspace for the same reason
		// setup and run start there: it is the one directory an author can
		// name without knowing anything about the base image.
		to := entry.To
		if !path.IsAbs(to) {
			to = path.Join(workspace, to)
		}
		to = path.Clean(to)

		if to == reservedGuestDir || strings.HasPrefix(to, reservedGuestDir+"/") {
			errs = append(errs, fmt.Errorf(
				"copy[%d].to: %q resolves to %s, which is reserved for the guest agent's manifest, secrets and credentials",
				i, entry.To, to))
			continue
		}
		// checked on the resolved path rather than as written, so `to: ./app`
		// and `to: /workspace/app` are caught as the one collision they are.
		if prev, dup := seen[to]; dup {
			errs = append(errs, fmt.Errorf(
				"copy[%d].to: %q resolves to %s, which copy[%d] already writes", i, entry.To, to, prev))
			continue
		}
		seen[to] = i

		out = append(out, CopySpec{From: entry.From, To: to})
	}
	return out, errs
}
