package main

import (
	"strings"
	"testing"
	"time"

	fuse "github.com/folsomintel/fuse/sdks/go"
)

func TestFormatStepDuration(t *testing.T) {
	cases := []struct {
		ms   int64
		want string
	}{
		{0, "-"},
		{-1, "-"},
		{400, "0.4s"},
		{18204, "18.2s"},
		{60000, "1m00s"},
		{95500, "1m35s"},
	}
	for _, c := range cases {
		if got := formatStepDuration(c.ms); got != c.want {
			t.Errorf("formatStepDuration(%d) = %q, want %q", c.ms, got, c.want)
		}
	}
}

func TestFormatStepLineVerdicts(t *testing.T) {
	cached := stepEntry{index: 1, total: 3, label: "apt-get update -qq", cached: true, durationMS: 400}
	run := stepEntry{index: 2, total: 3, label: "npm ci", durationMS: 18204}
	failed := stepEntry{index: 3, total: 3, label: "./setup.sh", durationMS: 1200, exitCode: 2}

	if got := formatStepLine(cached); !strings.HasPrefix(got, "  CACHED [1/3]   apt-get update -qq") {
		t.Errorf("cached line = %q", got)
	}
	if got := formatStepLine(run); !strings.HasPrefix(got, "  RUN    [2/3]   npm ci") {
		t.Errorf("run line = %q", got)
	}
	if got := formatStepLine(failed); !strings.HasPrefix(got, "  FAILED [3/3]   ./setup.sh") {
		t.Errorf("failed line = %q", got)
	}
	// durations line up because the command column is fixed width.
	if !strings.HasSuffix(formatStepLine(cached), "   0.4s") {
		t.Errorf("cached line does not end with the duration: %q", formatStepLine(cached))
	}
	if len(formatStepLine(cached)) != len(formatStepLine(run)) {
		t.Errorf("step lines have different widths:\n%q\n%q", formatStepLine(cached), formatStepLine(run))
	}
}

func TestFormatStepLineTruncatesLongCommands(t *testing.T) {
	long := strings.Repeat("x", 200)
	line := formatStepLine(stepEntry{index: 1, total: 1, label: long, durationMS: 1000})
	if strings.Contains(line, long) {
		t.Error("long command was not truncated")
	}
	if !strings.Contains(line, "...") {
		t.Errorf("truncated command has no ellipsis: %q", line)
	}
	if len(line) != len(formatStepLine(stepEntry{index: 1, total: 1, label: "short", durationMS: 1000})) {
		t.Errorf("truncated line width differs: %q", line)
	}
}

func TestFormatStepLineWithoutTotal(t *testing.T) {
	// a server that reports boundaries but not a step count still renders.
	line := formatStepLine(stepEntry{index: 2, label: "npm ci"})
	if !strings.Contains(line, "[2]") {
		t.Errorf("line = %q, want a bare index", line)
	}
	if !strings.Contains(line, "-") {
		t.Errorf("line = %q, want a dash for the unreported duration", line)
	}
}

func TestFormatStepNotes(t *testing.T) {
	miss := stepEntry{
		index: 2, total: 3, label: "npm ci", durationMS: 18204,
		missReason: fuse.MissReasonInputsChange,
		missDetail: []string{"package-lock.json", "package.json"},
	}
	notes := formatStepNotes(miss)
	if len(notes) != 1 {
		t.Fatalf("notes = %v, want one line", notes)
	}
	if !strings.Contains(notes[0], "miss: inputs-changed (package-lock.json, package.json)") {
		t.Errorf("note = %q", notes[0])
	}

	// a cached step has nothing to explain.
	if notes := formatStepNotes(stepEntry{cached: true, missReason: "no-entry"}); notes != nil {
		t.Errorf("cached step notes = %v, want none", notes)
	}

	// a step with no cache information at all degrades to no note.
	if notes := formatStepNotes(stepEntry{index: 1, durationMS: 100}); notes != nil {
		t.Errorf("timing-only step notes = %v, want none", notes)
	}

	// an unknown reason from a newer server passes through verbatim.
	notes = formatStepNotes(stepEntry{missReason: "brand-new-reason"})
	if len(notes) != 1 || !strings.Contains(notes[0], "brand-new-reason") {
		t.Errorf("notes = %v, want the unknown reason passed through", notes)
	}

	notes = formatStepNotes(stepEntry{exitCode: 7, missReason: "no-entry"})
	if len(notes) != 2 || !strings.Contains(notes[1], "exit code 7") {
		t.Errorf("notes = %v, want a miss line and an exit code line", notes)
	}
}

func TestStepTrackerFillsIndexAndLabel(t *testing.T) {
	tr := newStepTracker([]string{"apt-get update", "npm ci"})
	// no index or total on the wire: the tracker numbers by arrival and takes
	// the total from the locally compiled setup list.
	first := tr.add(fuse.Event{Kind: fuse.EventKindStep, DurationMS: 100})
	second := tr.add(fuse.Event{Kind: fuse.EventKindStep, DurationMS: 200})
	if first.index != 1 || second.index != 2 {
		t.Errorf("indexes = %d, %d, want 1, 2", first.index, second.index)
	}
	if first.total != 2 {
		t.Errorf("total = %d, want 2", first.total)
	}
	if second.label != "npm ci" {
		t.Errorf("label = %q, want %q", second.label, "npm ci")
	}
	// an index past the locally known setup lines has no label to show.
	third := tr.add(fuse.Event{Kind: fuse.EventKindStep, Index: 9, Total: 9})
	if third.label != "" {
		t.Errorf("label = %q, want empty", third.label)
	}
	if line := formatStepLine(third); !strings.Contains(line, "(step 9)") {
		t.Errorf("line = %q, want a placeholder label", line)
	}
}

func TestStepTrackerCollapsesMultilineLabels(t *testing.T) {
	tr := newStepTracker([]string{"apt-get update &&\n  apt-get install -y ripgrep"})
	e := tr.add(fuse.Event{Kind: fuse.EventKindStep, Index: 1, Total: 1})
	if strings.Contains(e.label, "\n") {
		t.Errorf("label = %q, want a single line", e.label)
	}
	if e.label != "apt-get update && apt-get install -y ripgrep" {
		t.Errorf("label = %q", e.label)
	}
}

func TestStepTrackerSummary(t *testing.T) {
	if got := newStepTracker(nil).summary(time.Second); got != "" {
		t.Errorf("summary with no steps = %q, want empty", got)
	}

	tr := newStepTracker([]string{"a", "b", "c"})
	tr.add(fuse.Event{Kind: fuse.EventKindStep, Index: 1, Total: 3, Cached: true, DurationMS: 400})
	tr.add(fuse.Event{Kind: fuse.EventKindStep, Index: 2, Total: 3, DurationMS: 18204, MissReason: fuse.MissReasonInputsChange})
	tr.add(fuse.Event{Kind: fuse.EventKindStep, Index: 3, Total: 3, DurationMS: 9700, MissReason: fuse.MissReasonParentChange})
	got := tr.summary(34400 * time.Millisecond)
	for _, want := range []string{"1 cached, 2 built", "setup 28.3s", "total 34.4s"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary = %q, want it to contain %q", got, want)
		}
	}
}

func TestStepTrackerSummaryWithoutCacheInfo(t *testing.T) {
	// against a server that reports boundaries and timing only, the summary
	// must not claim every step was a cache miss.
	tr := newStepTracker([]string{"a", "b"})
	tr.add(fuse.Event{Kind: fuse.EventKindStep, Index: 1, Total: 2, DurationMS: 1000})
	tr.add(fuse.Event{Kind: fuse.EventKindStep, Index: 2, Total: 2, DurationMS: 2000})
	got := tr.summary(4 * time.Second)
	if strings.Contains(got, "cached") || strings.Contains(got, "built") {
		t.Errorf("summary = %q, want no cache counts", got)
	}
	if !strings.Contains(got, "2 steps") || !strings.Contains(got, "setup 3.0s") {
		t.Errorf("summary = %q", got)
	}
}

func TestStepTrackerCacheReported(t *testing.T) {
	tr := newStepTracker([]string{"a"})
	tr.add(fuse.Event{Kind: fuse.EventKindStep, Index: 1, Total: 1, DurationMS: 500})
	if tr.cacheReported() {
		t.Error("cacheReported = true for a step with no cache fields")
	}
	tr.add(fuse.Event{Kind: fuse.EventKindStep, Index: 1, Total: 1, Cached: true})
	if !tr.cacheReported() {
		t.Error("cacheReported = false for a cached step")
	}
	if tr.count() != 2 {
		t.Errorf("count = %d, want 2", tr.count())
	}
}

func TestPadTruncate(t *testing.T) {
	if got := padTruncate("ab", 5); got != "ab   " {
		t.Errorf("padTruncate = %q", got)
	}
	if got := padTruncate("abcdef", 5); got != "ab..." {
		t.Errorf("padTruncate = %q", got)
	}
	if got := padTruncate("abcdef", 2); got != "ab" {
		t.Errorf("padTruncate = %q", got)
	}
}

func TestStreamPlainRendersStepsWithoutClobberingState(t *testing.T) {
	// a step event carries no state. if it were treated as a transition it
	// would reset the last state to "" and a healthy environment would be
	// reported as one that never produced an event.
	ch := make(chan fuse.Event, 4)
	ch <- fuse.Event{State: fuse.StateProvisioning}
	ch <- fuse.Event{Kind: fuse.EventKindStep, Index: 1, Total: 2, Cached: true, DurationMS: 400}
	ch <- fuse.Event{Kind: fuse.EventKindStep, Index: 2, Total: 2, DurationMS: 1500, MissReason: fuse.MissReasonNoEntry}
	close(ch)

	steps := newStepTracker([]string{"apt-get update", "npm ci"})
	var state string
	out, err := captureWithDeadline(t, 5*time.Second, func() error {
		var err error
		state, err = streamPlain(ch, fuse.IsSettledState, steps)
		return err
	})
	if err != nil {
		t.Fatalf("streamPlain: %v", err)
	}
	if state != fuse.StateProvisioning {
		t.Errorf("state = %q, want %q", state, fuse.StateProvisioning)
	}
	if steps.count() != 2 {
		t.Errorf("tracked %d steps, want 2", steps.count())
	}
	for _, want := range []string{"CACHED [1/2]   apt-get update", "RUN    [2/2]   npm ci", "miss: no-entry"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestStreamPlainIgnoresUnknownKinds(t *testing.T) {
	ch := make(chan fuse.Event, 3)
	ch <- fuse.Event{Kind: fuse.EventKindState, State: fuse.StateProvisioning}
	ch <- fuse.Event{Kind: "log", Error: "should not render"}
	ch <- fuse.Event{Kind: fuse.EventKindState, State: fuse.StateRunning}

	var state string
	out, err := captureWithDeadline(t, 5*time.Second, func() error {
		var err error
		state, err = streamPlain(ch, fuse.IsSettledState, nil)
		return err
	})
	if err != nil {
		t.Fatalf("streamPlain: %v", err)
	}
	if state != fuse.StateRunning {
		t.Errorf("state = %q, want %q", state, fuse.StateRunning)
	}
	if strings.Contains(out, "should not render") {
		t.Errorf("unknown kind leaked into the output:\n%s", out)
	}
}
