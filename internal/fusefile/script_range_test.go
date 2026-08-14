package fusefile

import (
	"strings"
	"testing"
)

func rangeFusefile(t *testing.T) *Fusefile {
	t.Helper()
	f, err := Parse([]byte(`version: 1
image: base-ubuntu-24
workspace: /srv/app
cache:
  enabled: true
files:
  - path: hello.txt
    content: hi
setup:
  - apt-get update -qq
  - run: npm ci
    workdir: web
  - make build
run: ./start.sh
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return f
}

// The stepped build path and the single-blob path must render the same bytes
// when they cover the same work, or a cached build and an uncached one would
// silently run different scripts.
func TestSetupScriptRangeMatchesCompileBuildScript(t *testing.T) {
	f := rangeFusefile(t)
	if got, want := SetupScriptRange(f, 0, len(f.Setup), true), compileBuildScript(f); got != want {
		t.Errorf("full range != compileBuildScript\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestStartupScriptFromZeroMatchesCompileStartupScript(t *testing.T) {
	f := rangeFusefile(t)
	if got, want := StartupScriptFrom(f, 0), compileStartupScript(f); got != want {
		t.Errorf("from 0 != compileStartupScript\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// Every exec is a fresh shell, so a range that omits the prelude would run
// without strict mode and outside the workspace. That failure is silent, which
// is exactly why it is pinned.
func TestSetupScriptRangeAlwaysCarriesThePrelude(t *testing.T) {
	f := rangeFusefile(t)
	for i := range f.Setup {
		script := SetupScriptRange(f, i, i+1, false)
		if !strings.HasPrefix(script, "set -eu\n") {
			t.Errorf("step %d does not start with strict mode:\n%s", i, script)
		}
		if !strings.Contains(script, "cd '/srv/app'") {
			t.Errorf("step %d does not cd into the workspace:\n%s", i, script)
		}
	}
}

// A per-step workdir is rendered as a subshell around the step, so it must
// survive being sliced out on its own.
func TestSetupScriptRangeKeepsPerStepWorkdir(t *testing.T) {
	f := rangeFusefile(t)
	script := SetupScriptRange(f, 1, 2, false)
	if !strings.Contains(script, "(cd 'web'; npm ci)") {
		t.Errorf("step 1 lost its workdir:\n%s", script)
	}
}

// The file block belongs only on the first exec of a cold build: a seed
// artifact was built from the same base key, which folds the files in, so
// rewriting them on a resumed build is work with no effect.
func TestSetupScriptRangeFileBlockIsOptional(t *testing.T) {
	f := rangeFusefile(t)
	if with := SetupScriptRange(f, 0, 1, true); !strings.Contains(with, "hello.txt") {
		t.Errorf("includeFiles=true dropped the file block:\n%s", with)
	}
	if without := SetupScriptRange(f, 0, 1, false); strings.Contains(without, "hello.txt") {
		t.Errorf("includeFiles=false emitted the file block:\n%s", without)
	}
}

// A hit covering every setup step leaves only the run phase, and must not
// re-emit any setup command.
func TestStartupScriptFromSkipsCoveredSteps(t *testing.T) {
	f := rangeFusefile(t)
	script := StartupScriptFrom(f, 2)
	for _, gone := range []string{"apt-get update", "npm ci"} {
		if strings.Contains(script, gone) {
			t.Errorf("step %q should have been covered by the seed:\n%s", gone, script)
		}
	}
	if !strings.Contains(script, "make build") {
		t.Errorf("remaining setup step missing:\n%s", script)
	}
	if !strings.Contains(script, "./start.sh") {
		t.Errorf("run phase missing:\n%s", script)
	}
}
