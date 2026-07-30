package main

import (
	"fmt"
	"strings"
	"time"

	fuse "github.com/folsomintel/fuse/sdks/go"
)

// verdicts printed at the head of a step line. the word carries the meaning,
// so the output still reads correctly with color stripped.
const (
	verdictCached = "CACHED"
	verdictRun    = "RUN"
	verdictFailed = "FAILED"
)

// stepLabelWidth is the column the duration is right-aligned after. longer
// commands are truncated so the durations stay in one column.
const stepLabelWidth = 48

// stepEntry is one setup step's reported outcome.
type stepEntry struct {
	index      int
	total      int
	label      string
	cached     bool
	missReason string
	missDetail []string
	durationMS int64
	exitCode   int
}

// stepTracker accumulates step events for one provisioning run and renders
// them. labels are the setup commands the CLI compiled locally, since a step
// event carries only an index: the orchestrator never sees a Fusefile.
type stepTracker struct {
	labels []string
	steps  []stepEntry
}

func newStepTracker(labels []string) *stepTracker {
	return &stepTracker{labels: labels}
}

// add records a step event and returns the entry it produced.
func (t *stepTracker) add(ev fuse.Event) stepEntry {
	e := stepEntry{
		index:      ev.Index,
		total:      ev.Total,
		cached:     ev.Cached,
		missReason: ev.MissReason,
		missDetail: ev.MissDetail,
		durationMS: ev.DurationMS,
		exitCode:   ev.ExitCode,
	}
	// a server that does not number its steps still gets ordered lines.
	if e.index == 0 {
		e.index = len(t.steps) + 1
	}
	if e.total == 0 {
		e.total = len(t.labels)
	}
	e.label = t.label(e.index)
	t.steps = append(t.steps, e)
	return e
}

// label returns the locally known command for a 1-based step index, or an
// empty string when the CLI has no line for it (a server reporting more steps
// than the Fusefile declares).
func (t *stepTracker) label(index int) string {
	if index < 1 || index > len(t.labels) {
		return ""
	}
	// a setup command can span lines; collapse it so it stays one row.
	return strings.Join(strings.Fields(t.labels[index-1]), " ")
}

// count reports how many step events arrived.
func (t *stepTracker) count() int { return len(t.steps) }

// cacheReported reports whether any step carried cache information. it stays
// false against a server that reports boundaries and timing only, which is why
// the summary hides the layer counts in that case.
func (t *stepTracker) cacheReported() bool {
	for _, e := range t.steps {
		if e.cached || e.missReason != "" {
			return true
		}
	}
	return false
}

// setupDuration sums the reported step durations.
func (t *stepTracker) setupDuration() time.Duration {
	var ms int64
	for _, e := range t.steps {
		ms += e.durationMS
	}
	return time.Duration(ms) * time.Millisecond
}

// summary renders the trailing one-line recap, or "" when no step arrived.
// total is the wall-clock time the CLI measured for the whole up.
func (t *stepTracker) summary(total time.Duration) string {
	if len(t.steps) == 0 {
		return ""
	}
	var parts []string
	if t.cacheReported() {
		cached := 0
		for _, e := range t.steps {
			if e.cached {
				cached++
			}
		}
		parts = append(parts, fmt.Sprintf("%d cached, %d built", cached, len(t.steps)-cached))
	} else {
		parts = append(parts, fmt.Sprintf("%d steps", len(t.steps)))
	}
	if d := t.setupDuration(); d > 0 {
		parts = append(parts, "setup "+formatStepDuration(d.Milliseconds()))
	}
	if total.Milliseconds() > 0 {
		parts = append(parts, "total "+formatStepDuration(total.Milliseconds()))
	}
	return "  layers  " + strings.Join(parts, "   ")
}

// stepVerdict returns the word printed at the head of a step line.
func stepVerdict(e stepEntry) string {
	switch {
	case e.exitCode != 0:
		return verdictFailed
	case e.cached:
		return verdictCached
	default:
		return verdictRun
	}
}

// formatStepDuration renders a millisecond duration compactly, or "-" when the
// server did not report one.
func formatStepDuration(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	d := time.Duration(ms) * time.Millisecond
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// formatStepLine renders the primary line for a step: verdict, position,
// command, and duration. plain text with no escape codes, so the layout is
// measurable and identical on a pipe and on a tty.
func formatStepLine(e stepEntry) string {
	label := e.label
	if label == "" {
		label = "(step " + fmt.Sprint(e.index) + ")"
	}
	position := fmt.Sprintf("[%d/%d]", e.index, e.total)
	if e.total <= 0 {
		position = fmt.Sprintf("[%d]", e.index)
	}
	return fmt.Sprintf("  %-6s %-7s %s %s",
		stepVerdict(e),
		position,
		padTruncate(label, stepLabelWidth),
		fmt.Sprintf("%7s", formatStepDuration(e.durationMS)),
	)
}

// formatStepNotes renders the indented follow-up lines for a step: the cache
// miss reason, and the exit code of a step that failed. it returns nil when
// there is nothing to add.
func formatStepNotes(e stepEntry) []string {
	var out []string
	if !e.cached && e.missReason != "" {
		line := "         miss: " + e.missReason
		if len(e.missDetail) > 0 {
			line += " (" + strings.Join(e.missDetail, ", ") + ")"
		}
		out = append(out, line)
	}
	if e.exitCode != 0 {
		out = append(out, fmt.Sprintf("         exit code %d", e.exitCode))
	}
	return out
}

// renderStepEntry returns the display lines for a step, styled for a terminal.
// styling wraps whole lines so the column layout in formatStepLine is
// preserved, and never carries meaning on its own.
func renderStepEntry(e stepEntry) []string {
	lines := []string{stepLineStyle(e).Render(formatStepLine(e))}
	for _, n := range formatStepNotes(e) {
		lines = append(lines, styleFaint.Render(n))
	}
	return lines
}

// padTruncate pads s to width, or truncates it with a trailing ellipsis when
// it is longer.
func padTruncate(s string, width int) string {
	if len(s) > width {
		if width <= 3 {
			return s[:width]
		}
		return s[:width-3] + "..."
	}
	return s + strings.Repeat(" ", width-len(s))
}
