package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	fuse "github.com/folsomintel/fuse/sdks/go"
)

// streamEnvironment subscribes to an environment's SSE event stream and renders
// transitions until until(state) is true. it uses an interactive bubbletea view
// on a tty, and plain (or ndjson) output otherwise. it returns the last state
// observed, which is empty if the stream ended before any event arrived.
//
// steps may be nil. when set, setup step events are recorded there and
// rendered as their own lines.
func streamEnvironment(ctx context.Context, cl *fuse.Client, vmID string, until func(string) bool, steps *stepTracker) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	ch, err := cl.Environments.Events(ctx, vmID)
	if err != nil {
		return "", friendly(err)
	}
	if app.isJSON() || !isInteractive() {
		return streamPlain(ch, until, steps)
	}
	return streamTUI(ch, vmID, until, steps)
}

// waitForEnvironmentReady streams provisioning events until the environment
// settles, and reports a failure if it settled anywhere but running. only
// running is success: the stream can also end without settling at all (the sdk
// closes the channel with no error on a clean eof), and a stream that dropped
// mid-provision must not be reported as a ready environment.
func waitForEnvironmentReady(ctx context.Context, cl *fuse.Client, vmID string, steps *stepTracker) error {
	state, err := streamEnvironment(ctx, cl, vmID, fuse.IsSettledState, steps)
	if err != nil {
		return err
	}
	switch state {
	case fuse.StateRunning:
		return nil
	case fuse.StateFailed:
		return fmt.Errorf("environment %s failed to provision", vmID)
	case fuse.StateDestroyed:
		return fmt.Errorf("environment %s was destroyed before it became ready", vmID)
	case "":
		return fmt.Errorf("environment %s: event stream ended with no events, so the environment never reached %s", vmID, fuse.StateRunning)
	default:
		return fmt.Errorf("environment %s: event stream ended in state %q before the environment reached %s", vmID, state, fuse.StateRunning)
	}
}

// streamPlain prints events as they arrive (ndjson in json mode, one line each
// otherwise) and returns when until(state) is true or the stream closes.
//
// only state events advance last and are tested against until: a step event
// carries no state, so treating it like one would clobber the last state and
// make a healthy environment look like it never reported anything.
func streamPlain(ch <-chan fuse.Event, until func(string) bool, steps *stepTracker) (string, error) {
	if steps == nil {
		steps = newStepTracker(nil)
	}
	last := ""
	for ev := range ch {
		if ev.Err != nil {
			return last, friendly(ev.Err)
		}
		if app.isJSON() {
			// json mode passes the raw event through unchanged, so a new kind
			// needs no client support to be consumable.
			if err := printJSON(ev); err != nil {
				return last, err
			}
		}
		switch {
		case ev.Kind == fuse.EventKindStep:
			entry := steps.add(ev)
			if !app.isJSON() {
				for _, line := range renderStepEntry(entry) {
					_, _ = fmt.Fprintln(os.Stdout, line)
				}
			}
			continue
		case !fuse.IsStateEvent(ev.Kind):
			// an unknown kind from a newer server: nothing to render, and it
			// must not be mistaken for a state transition.
			continue
		}
		if !app.isJSON() {
			detail := ev.URL
			if ev.Error != "" {
				detail = ev.Error
			}
			_, _ = fmt.Fprintf(os.Stdout, "%s  %-12s %s\n", shortTime(ev.UpdatedAt), ev.State, detail)
		}
		last = ev.State
		if until(ev.State) {
			return last, nil
		}
	}
	return last, nil
}

// --- bubbletea view ---

type eventMsg fuse.Event
type streamClosedMsg struct{}

type watchModel struct {
	vmID     string
	ch       <-chan fuse.Event
	until    func(string) bool
	spinner  spinner.Model
	progress progress.Model
	steps    *stepTracker
	lines    []string
	last     string
	done     bool
	err      error
}

func newWatchModel(vmID string, ch <-chan fuse.Event, until func(string) bool, steps *stepTracker) watchModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	if steps == nil {
		steps = newStepTracker(nil)
	}
	prog := progress.New(progress.WithDefaultGradient())
	prog.Width = 40
	return watchModel{vmID: vmID, ch: ch, until: until, spinner: sp, progress: prog, steps: steps}
}

// stepProgress reports the fraction of known setup steps completed so far,
// and whether there are any known steps to show a bar for at all. an
// environment watch with no compiled Fusefile (steps.labels is empty) has
// nothing to show a bar against.
func (m watchModel) stepProgress() (float64, bool) {
	total := len(m.steps.labels)
	if total == 0 {
		return 0, false
	}
	frac := float64(m.steps.count()) / float64(total)
	if frac > 1 {
		frac = 1
	}
	return frac, true
}

func (m watchModel) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, waitForEvent(m.ch))
}

// waitForEvent reads one event from the channel as a tea.Cmd.
func waitForEvent(ch <-chan fuse.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return streamClosedMsg{}
		}
		return eventMsg(ev)
	}
}

func (m watchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.progress.Width = msg.Width - 4
		if m.progress.Width > 60 {
			m.progress.Width = 60
		}
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			return m, tea.Quit
		}
	case eventMsg:
		ev := fuse.Event(msg)
		if ev.Err != nil {
			m.err = ev.Err
			return m, tea.Quit
		}
		switch {
		case ev.Kind == fuse.EventKindStep:
			// a step event has no state, so it must not touch last or until.
			m.lines = append(m.lines, renderStepEntry(m.steps.add(ev))...)
			cmds := []tea.Cmd{waitForEvent(m.ch)}
			if frac, ok := m.stepProgress(); ok {
				cmds = append(cmds, m.progress.SetPercent(frac))
			}
			return m, tea.Batch(cmds...)
		case !fuse.IsStateEvent(ev.Kind):
			return m, waitForEvent(m.ch)
		}
		m.lines = append(m.lines, renderStateLine(ev))
		m.last = ev.State
		if m.until(ev.State) {
			m.done = true
			return m, tea.Quit
		}
		return m, waitForEvent(m.ch)
	case streamClosedMsg:
		m.done = true
		return m, tea.Quit
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case progress.FrameMsg:
		newModel, cmd := m.progress.Update(msg)
		if pm, ok := newModel.(progress.Model); ok {
			m.progress = pm
		}
		return m, cmd
	}
	return m, nil
}

func (m watchModel) View() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", styleHeader.Render("watching "+m.vmID))
	if frac, ok := m.stepProgress(); ok && !m.done {
		b.WriteString(m.progress.ViewAs(frac) + "\n")
	}
	for _, line := range m.lines {
		b.WriteString(line + "\n")
	}
	switch {
	case m.err != nil:
		b.WriteString(styleBad.Render("stream error: "+m.err.Error()) + "\n")
	case m.done:
		b.WriteString(styleFaint.Render("done") + "\n")
	default:
		b.WriteString(m.spinner.View() + lipgloss.NewStyle().Faint(true).Render(" waiting for transitions (q to stop)") + "\n")
	}
	return b.String()
}

// renderStateLine renders one lifecycle transition for the interactive view.
func renderStateLine(ev fuse.Event) string {
	line := fmt.Sprintf("  %s  %s", shortTime(ev.UpdatedAt), stateStyle(ev.State))
	if ev.URL != "" {
		line += "  " + styleFaint.Render(ev.URL)
	}
	if ev.Error != "" {
		line += "  " + styleBad.Render(ev.Error)
	}
	return line
}

func streamTUI(ch <-chan fuse.Event, vmID string, until func(string) bool, steps *stepTracker) (string, error) {
	final, err := tea.NewProgram(newWatchModel(vmID, ch, until, steps)).Run()
	if err != nil {
		return "", err
	}
	m, ok := final.(watchModel)
	if !ok {
		return "", nil
	}
	if m.err != nil {
		return m.last, friendly(m.err)
	}
	return m.last, nil
}
