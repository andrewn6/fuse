package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/folsomintel/fuse/internal/fusefile"
)

// layerPlan is the derived cache plan for a Fusefile's setup steps. It is the
// single place layer keys are computed for the CLI, so `--plan` and per-step
// cache reporting cannot disagree about what a step's key is.
type layerPlan struct {
	BaseKey string `json:"base_key"`
	// Arch is the architecture the artifacts would be built on. It is not part
	// of a layer key, only a serving constraint, and it is not knowable until
	// the build is scheduled onto a host, so it is empty today. The field is a
	// shipped `--plan --json` surface, so it stays; a later phase populates it
	// from the host the builder lands on.
	Arch         string                `json:"arch"`
	CacheEnabled bool                  `json:"cache_enabled"`
	Steps        []fusefile.LayerKey   `json:"steps"`
	Statuses     []layerPlanStepStatus `json:"-"`
}

// layerPlanStepStatus is the per-step outcome.
type layerPlanStepStatus string

const (
	// layerStatusHit means a stored artifact already covers this step, so it
	// does not run. Every step at or below the deepest hit is a hit: one
	// artifact satisfies the whole chain leading to it.
	layerStatusHit = layerPlanStepStatus("hit")
	// layerStatusMiss means the step is cacheable but has no stored artifact,
	// so it runs and is snapshotted.
	layerStatusMiss = layerPlanStepStatus("miss")
	// layerStatusSkip means the step cannot be cached at all; LayerKey.Reason
	// says why. It runs as part of the uncacheable tail.
	layerStatusSkip = layerPlanStepStatus("skip")
)

// markHit records that a resolved artifact covers every step up to and
// including index, so `--plan`-style reporting and the build loop agree about
// what was reused.
func (p *layerPlan) markHit(index int) {
	for i := 0; i <= index && i < len(p.Statuses); i++ {
		if p.Steps[i].Cacheable {
			p.Statuses[i] = layerStatusHit
		}
	}
}

// hitCount reports how many steps a resolved artifact covered.
func (p *layerPlan) hitCount() int {
	n := 0
	for _, s := range p.Statuses {
		if s == layerStatusHit {
			n++
		}
	}
	return n
}

// planSetupLayers derives the plan for the commands that run the setup phase
// (`fuse up` and `fuse build`), or returns (nil, nil) when there is no reason
// to. show is the caller's --plan: it forces derivation so the plan can be
// printed. otherwise the plan is only derived when caching is enabled, where
// its value is failing on a bad `inputs` entry before anything is provisioned
// rather than mid-build.
func planSetupLayers(fusefilePath string, f *fusefile.Fusefile, show bool) (*layerPlan, error) {
	if !show && !f.Cache.Enabled {
		return nil, nil
	}
	lp, err := buildLayerPlan(fusefilePath, f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", fusefilePath, err)
	}
	return lp, nil
}

// buildLayerPlan derives the layer key of every setup step. fusefilePath is
// the Fusefile's path; declared `inputs` are resolved relative to its
// directory, which is the only directory the CLI will read project files from.
func buildLayerPlan(fusefilePath string, f *fusefile.Fusefile) (*layerPlan, error) {
	dir := filepath.Dir(fusefilePath)

	keys, err := fusefile.LayerKeys(f, inputDigester(dir), fusefile.LayerOptions{})
	if err != nil {
		return nil, err
	}

	plan := &layerPlan{
		BaseKey: fusefile.BaseKey(f.Image, f.Files),
		// arch is not a key component and is not knowable client-side: the
		// build has not been scheduled onto a host yet, and the client's own
		// arch says nothing about the host's. left empty so the plan does not
		// assert something it cannot know; a later phase fills it in from the
		// host the builder actually lands on.
		Arch:         "",
		CacheEnabled: f.Cache.Enabled,
		Steps:        keys,
		Statuses:     make([]layerPlanStepStatus, len(keys)),
	}
	for i, k := range keys {
		if k.Cacheable {
			plan.Statuses[i] = layerStatusMiss
		} else {
			plan.Statuses[i] = layerStatusSkip
		}
	}
	return plan, nil
}

// renderLayerPlan prints the derived plan.
//
// `--plan` deliberately does not resolve anything, so every cacheable step
// reports as a miss here. Resolution needs a target architecture, which needs
// the fleet, and a command whose entire purpose is "show me what would happen"
// should not depend on the orchestrator being reachable. The real hit/miss
// breakdown is reported by the build itself.
func renderLayerPlan(p *layerPlan) error {
	if app.isJSON() {
		return printJSON(p.jsonView())
	}

	detail := [][2]string{{"base key", p.BaseKey}}
	// the arch row is omitted rather than shown blank: until the build is
	// placed on a host there is no answer, and an empty value reads as "amd64
	// probably" to anyone skimming.
	if p.Arch != "" {
		detail = append(detail, [2]string{"arch", p.Arch})
	}
	detail = append(detail, [2]string{"cache", map[bool]string{true: "enabled", false: "disabled"}[p.CacheEnabled]})
	renderDetail(detail)
	if len(p.Steps) == 0 {
		fmt.Fprintln(os.Stderr, styleFaint.Render("no setup steps"))
		return nil
	}

	rows := make([][]string, 0, len(p.Steps))
	for i, k := range p.Steps {
		// only an uncacheable step needs a note: the reason it cannot be
		// cached is not derivable from anything else on the row. for a
		// cacheable step the status column already says everything.
		note := k.Reason
		rows = append(rows, []string{
			fmt.Sprintf("%d", k.Index),
			string(p.Statuses[i]),
			dash(shortKey(k.Key)),
			truncateScript(k.Script),
			note,
		})
	}
	renderTable([]string{"#", "STATUS", "KEY", "STEP", "NOTE"}, rows)
	return nil
}

// layerPlanJSON is the machine-readable plan; the status is flattened onto each
// step so a consumer does not have to zip two slices.
type layerPlanJSON struct {
	BaseKey      string              `json:"base_key"`
	Arch         string              `json:"arch"`
	CacheEnabled bool                `json:"cache_enabled"`
	Steps        []layerPlanStepJSON `json:"steps"`
}

type layerPlanStepJSON struct {
	Index        int    `json:"index"`
	Script       string `json:"script"`
	Key          string `json:"key,omitempty"`
	ParentKey    string `json:"parent_key,omitempty"`
	InputsDigest string `json:"inputs_digest,omitempty"`
	Cacheable    bool   `json:"cacheable"`
	Status       string `json:"status"`
	Reason       string `json:"reason,omitempty"`
}

func (p *layerPlan) jsonView() layerPlanJSON {
	out := layerPlanJSON{
		BaseKey:      p.BaseKey,
		Arch:         p.Arch,
		CacheEnabled: p.CacheEnabled,
		Steps:        make([]layerPlanStepJSON, 0, len(p.Steps)),
	}
	for i, k := range p.Steps {
		out.Steps = append(out.Steps, layerPlanStepJSON{
			Index:        k.Index,
			Script:       k.Script,
			Key:          k.Key,
			ParentKey:    k.ParentKey,
			InputsDigest: k.InputsDigest,
			Cacheable:    k.Cacheable,
			Status:       string(p.Statuses[i]),
			Reason:       k.Reason,
		})
	}
	return out
}

// shortKey trims a hex layer key to a readable prefix.
func shortKey(key string) string {
	if len(key) <= 12 {
		return key
	}
	return key[:12]
}

// truncateScript renders a step's script on one line for table display.
func truncateScript(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= 48 {
		return s
	}
	return s[:45] + "..."
}

// inputDigester returns the digester LayerKeys calls for a step's declared
// inputs. Patterns are globbed relative to root and every match must stay
// inside it: this is the first time the CLI reads project files beyond the
// Fusefile and the secrets file, so containment is checked on the resolved
// path rather than assumed from the pattern.
//
// The digest is sha256 over the sorted list of
// (relative path, mode & 0o111, sha256(content)), so it is stable across runs
// and across machines, and changes when a file's contents or its executable
// bit change.
func inputDigester(root string) fusefile.InputDigester {
	return func(patterns []string) (string, error) {
		absRoot, err := filepath.Abs(root)
		if err != nil {
			return "", fmt.Errorf("resolve %s: %w", root, err)
		}

		// path -> digest line, so patterns that overlap contribute once.
		lines := map[string]string{}

		for _, pattern := range patterns {
			matches, err := filepath.Glob(filepath.Join(absRoot, filepath.FromSlash(pattern)))
			if err != nil {
				return "", fmt.Errorf("bad pattern %q: %w", pattern, err)
			}
			if len(matches) == 0 {
				// a pattern that matches nothing would silently drop out of
				// the key, so it is an error rather than an empty contribution.
				return "", fmt.Errorf("no files matched %q", pattern)
			}
			for _, match := range matches {
				if err := digestPath(absRoot, match, lines); err != nil {
					return "", err
				}
			}
		}

		rels := make([]string, 0, len(lines))
		for rel := range lines {
			rels = append(rels, rel)
		}
		sort.Strings(rels)

		h := sha256.New()
		for _, rel := range rels {
			h.Write([]byte(lines[rel]))
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	}
}

// digestPath adds path to lines, walking it if it is a directory.
func digestPath(root, path string, lines map[string]string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat input: %w", err)
	}
	if info.IsDir() {
		return filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			return digestFile(root, p, d.Type(), lines)
		})
	}
	return digestFile(root, path, info.Mode().Type(), lines)
}

// digestFile hashes one regular file into lines. Anything that is not a
// regular file (a symlink, a device, a socket) is rejected: a symlink's target
// can point outside root, and hashing what it resolves to would silently widen
// what the key covers.
func digestFile(root, path string, typ fs.FileMode, lines map[string]string) error {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return fmt.Errorf("resolve input %s: %w", path, err)
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return fmt.Errorf("input %s is outside the Fusefile's directory", rel)
	}
	if typ != 0 {
		return fmt.Errorf("input %s is not a regular file", rel)
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("read input %s: %w", rel, err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat input %s: %w", rel, err)
	}

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("read input %s: %w", rel, err)
	}

	// only the executable bits are folded in: the rest of the mode is noise
	// that varies with umask and checkout tooling.
	lines[rel] = fmt.Sprintf("%s\x00%o\x00%s\n", rel, info.Mode().Perm()&0o111, hex.EncodeToString(h.Sum(nil)))
	return nil
}
