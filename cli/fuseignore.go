package main

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// fuseignoreName is the file `copy` filters its directory sources through. It
// sits next to the Fusefile, and only that one is read: a nested ignore file
// would have to anchor against something other than the directory `from`
// itself resolves against, which is a rule worth deferring rather than
// guessing at.
const fuseignoreName = ".fuseignore"

// defaultIgnorePatterns are matched before anything in the file, so a `!` line
// in a .fuseignore overrides one (matching is last-match-wins).
//
// The first group is size: `copy: {from: .}` on a checkout otherwise walks
// .git and node_modules into a create request capped at 512KiB, which fails on
// something the author never meant to send. The last four are secrecy: a
// Fusefile deliberately names secrets without carrying their values, so a copy
// step that quietly uploaded .env or a private key would undo that separation
// on its first use.
var defaultIgnorePatterns = []string{
	".git/",
	"node_modules/",
	"__pycache__/",
	".venv/",
	"venv/",
	"target/",
	"dist/",
	"build/",
	".DS_Store",
	"*.pyc",
	".env",
	".env.*",
	"*.pem",
	"*.key",
}

// ignoreRule is one compiled pattern. The syntax is gitignore's, minus the
// parts that only make sense for a repository (nested files, and the
// re-inclusion of something under an already-excluded directory, which cannot
// happen here because an ignored directory is pruned rather than walked).
type ignoreRule struct {
	glob     string // the pattern, with '!', the anchoring '/' and the trailing '/' removed
	negate   bool   // a leading '!': re-includes what an earlier rule dropped
	dirOnly  bool   // a trailing '/': matches directories only
	anchored bool   // a leading or interior '/': matches from the source root rather than at any depth
	builtin  bool   // from defaultIgnorePatterns rather than from the file
}

// fuseignore is an ordered rule list: the built-in defaults, then the file's
// lines in the order they were written. Nothing is sorted or deduplicated,
// because last-match-wins is the whole semantic.
type fuseignore struct {
	rules []ignoreRule
}

// loadFuseignore reads the .fuseignore in dir, which is the Fusefile's
// directory.
//
// A missing file is not an error, it just leaves the defaults in place, so a
// Fusefile that never mentions ignoring anything still does not ship .git.
func loadFuseignore(dir string) (*fuseignore, error) {
	ig := &fuseignore{rules: compileIgnoreRules(defaultIgnorePatterns, true)}

	p := filepath.Join(dir, fuseignoreName)
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return ig, nil
		}
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	ig.rules = append(ig.rules, compileIgnoreRules(strings.Split(string(data), "\n"), false)...)
	return ig, nil
}

// compileIgnoreRules turns pattern lines into rules, dropping blanks and
// '#' comments.
func compileIgnoreRules(lines []string, builtin bool) []ignoreRule {
	rules := make([]ignoreRule, 0, len(lines))
	for _, line := range lines {
		// trailing whitespace is not part of a pattern, and a stray '\r'
		// would otherwise make every pattern in a crlf file match nothing.
		p := strings.TrimRight(line, " \t\r")
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}

		r := ignoreRule{builtin: builtin}
		if rest, ok := strings.CutPrefix(p, "!"); ok {
			r.negate = true
			p = rest
		}
		if rest, ok := strings.CutSuffix(p, "/"); ok {
			r.dirOnly = true
			p = rest
		}
		// a leading slash anchors explicitly; an interior one anchors
		// implicitly, which is why `src/main.go` matches only at the top
		// while `main.go` matches at any depth.
		if rest, ok := strings.CutPrefix(p, "/"); ok {
			r.anchored = true
			p = rest
		}
		if strings.Contains(p, "/") {
			r.anchored = true
		}
		if p == "" {
			continue
		}
		r.glob = p
		rules = append(rules, r)
	}
	return rules
}

// Match reports whether rel is ignored. rel is slash-separated and relative to
// the root of the copy entry being walked, which for the usual `from: .` is
// the Fusefile's directory.
//
// It answers for that one path only. Everything under an ignored directory is
// ignored because the walk prunes at the directory, not because this reports
// true for its children.
func (ig *fuseignore) Match(rel string, isDir bool) bool {
	r, ok := ig.matched(rel, isDir)
	return ok && !r.negate
}

// matched returns the last rule that matched rel, which is the one that
// decides: a later `!keep.log` beats an earlier `*.log`, and a `!` in the file
// beats a built-in default because the defaults are compiled in first.
func (ig *fuseignore) matched(rel string, isDir bool) (ignoreRule, bool) {
	var winner ignoreRule
	found := false
	for _, r := range ig.rules {
		if r.match(rel, isDir) {
			winner, found = r, true
		}
	}
	return winner, found
}

// match reports whether this one rule covers rel.
func (r ignoreRule) match(rel string, isDir bool) bool {
	if r.dirOnly && !isDir {
		return false
	}
	if !r.anchored {
		// an unanchored pattern matches a name at any depth, so `*.log`
		// covers both ./app.log and ./logs/app.log.
		ok, err := path.Match(r.glob, path.Base(rel))
		return err == nil && ok
	}
	return matchSegments(strings.Split(r.glob, "/"), strings.Split(rel, "/"))
}

// matchSegments matches an anchored pattern against a path, segment by
// segment. "**" spans any number of segments; every other segment is matched
// with path.Match, whose '*' and '?' stop at a separator.
func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(name); i++ {
				if matchSegments(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		ok, err := path.Match(pat[0], name[0])
		if err != nil || !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	// a pattern that ran out with path left over named an ancestor, and the
	// walk prunes at that ancestor, so it does not need to match here too.
	return len(name) == 0
}

// decide is the copy walk's view of the matcher: keep, or drop and say which
// half of the rule list dropped it, so --show-copy can report a file the
// author's own pattern removed separately from one a built-in default did.
func (ig *fuseignore) decide(rel string, isDir bool) copyDecision {
	r, ok := ig.matched(rel, isDir)
	switch {
	case !ok || r.negate:
		return copyKeep
	case r.builtin:
		return copySkipDefault
	default:
		return copySkipPattern
	}
}
