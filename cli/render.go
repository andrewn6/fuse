package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"

	fuse "github.com/folsomintel/fuse/sdks/go"
)

var (
	styleHeader = lipgloss.NewStyle().Bold(true)
	styleFaint  = lipgloss.NewStyle().Faint(true)
	styleGood   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleWarn   = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleBad    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleKey    = lipgloss.NewStyle().Bold(true).Width(18)
)

// printJSON writes v to stdout as indented json.
func printJSON(v any) error { return writeJSON(os.Stdout, v) }

// writeJSON writes v to w as indented json.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// renderTable prints a bordered table to stdout. an empty row set prints a
// faint "no results" line instead.
func renderTable(headers []string, rows [][]string) {
	if len(rows) == 0 {
		fmt.Fprintln(os.Stderr, styleFaint.Render("no results"))
		return
	}
	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(styleFaint).
		Headers(headers...).
		Rows(rows...).
		StyleFunc(func(row, _ int) lipgloss.Style {
			if row == table.HeaderRow {
				return styleHeader.Padding(0, 1)
			}
			return lipgloss.NewStyle().Padding(0, 1)
		})
	fmt.Println(t)
}

// renderDetail prints a key/value detail block to stdout.
func renderDetail(pairs [][2]string) {
	for _, p := range pairs {
		fmt.Printf("%s %s\n", styleKey.Render(p[0]), p[1])
	}
}

// human-facing chatter goes to stderr so stdout stays pipeable (json mode).
func infof(format string, a ...any) { fmt.Fprintln(os.Stderr, fmt.Sprintf(format, a...)) }
func successf(format string, a ...any) {
	fmt.Fprintln(os.Stderr, styleGood.Render(fmt.Sprintf(format, a...)))
}
func warnf(format string, a ...any) {
	fmt.Fprintln(os.Stderr, styleWarn.Render(fmt.Sprintf(format, a...)))
}

// The orchestrator reports both startup-script timeout sentinels
// (internal/orchestrator/fleet.go) as a plain invalid_argument with no
// distinguishing code, and cli/ must not import internal/, so the message text
// is the only handle friendly has on them. A drift here costs the remediation
// hint, not correctness: the message itself still reaches the user.
const (
	errStartupScriptTimeoutText         = "startup script did not complete in time"
	errStartupScriptTimeoutTooLargeText = "startup script timeout exceeds the configured maximum"
)

// friendly maps an sdk *APIError to an actionable message. non-api errors pass
// through unchanged.
func friendly(err error) error {
	if err == nil {
		return nil
	}
	apiErr, ok := fuse.AsAPIError(err)
	if !ok {
		return err
	}
	msg := apiErr.Message
	if msg == "" {
		msg = http.StatusText(apiErr.Status)
	}
	switch {
	case apiErr.Code == fuse.CodeRouteNotFound:
		return fmt.Errorf("%s (is this the orchestrator? the url may be wrong, or point at a different service)", msg)
	case apiErr.Status == http.StatusForbidden || apiErr.Code == "forbidden":
		return fmt.Errorf("forbidden: %s\n  your token may lack permission (api-key commands need the master token), or the endpoint may be CIDR-restricted", msg)
	case fuse.IsUnauthorized(err):
		return fmt.Errorf("unauthorized: %s\n  re-run `fuse connect <url> --token <token>` with a valid token", msg)
	case fuse.IsNotFound(err):
		return fmt.Errorf("not found: %s", msg)
	case fuse.IsConflict(err):
		return fmt.Errorf("conflict: %s", msg)
	case strings.Contains(msg, errStartupScriptTimeoutText):
		return fmt.Errorf("%s\n"+
			"  the startup script runs synchronously inside create, so its ceiling is bounded by the\n"+
			"  control plane's http write timeout. raise it a little with `startup_timeout` in the\n"+
			"  Fusefile, or move the slow work out of `setup:` and bake it once with `fuse build`,\n"+
			"  then boot it with `fuse up --from-build <id>`", msg)
	case strings.Contains(msg, errStartupScriptTimeoutTooLargeText):
		return fmt.Errorf("%s\n"+
			"  `startup_timeout` cannot exceed the fleet's ceiling. for setup work that needs longer,\n"+
			"  bake it once with `fuse build` and boot it with `fuse up --from-build <id>`", msg)
	case fuse.IsInvalidArgument(err):
		return fmt.Errorf("invalid argument: %s", msg)
	case fuse.IsUnavailable(err):
		return fmt.Errorf("unavailable: %s", msg)
	default:
		if apiErr.RequestID != "" {
			return fmt.Errorf("%s (request id: %s)", msg, apiErr.RequestID)
		}
		return errors.New(msg)
	}
}

// dash returns "-" for empty strings, for table cells.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// shortUUID returns the first 8 chars of a uuid (enough to disambiguate
// instances in a host detail view) or "-" when empty.
func shortUUID(s string) string {
	if s == "" {
		return "-"
	}
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}

// shortTime formats a timestamp compactly, or "-" if zero.
func shortTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format("2006-01-02 15:04")
}

// ago renders a coarse age like "3m", "2h", "5d", or "-" for a zero time.
func ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// humanBytes renders a byte count as a human-readable size.
func humanBytes(n int64) string {
	if n <= 0 {
		return "-"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// stepLineStyle colors a setup step line. the verdict word already carries the
// verdict, so color is decoration and never the only signal.
func stepLineStyle(e stepEntry) lipgloss.Style {
	switch {
	case e.exitCode != 0:
		return styleBad
	case e.cached:
		return styleGood
	default:
		return lipgloss.NewStyle()
	}
}

// stateStyle colors a lifecycle state for detail views.
func stateStyle(state string) string {
	switch state {
	case fuse.StateRunning:
		return styleGood.Render(state)
	case fuse.StateFailed:
		return styleBad.Render(state)
	case fuse.StateProvisioning, fuse.StateDraining, fuse.StateDestroying:
		return styleWarn.Render(state)
	default:
		return state
	}
}
