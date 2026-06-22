package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	fuse "github.com/andrewn6/fuse/sdks/go"
)

// streamEnvironment subscribes to an environment's SSE event stream and renders
// transitions until a terminal state. it uses an interactive bubbletea view on
// a tty, and plain (or ndjson) output otherwise.
func streamEnvironment(ctx context.Context, cl *fuse.Client, vmID string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	ch, err := cl.Environments.Events(ctx, vmID)
	if err != nil {
		return friendly(err)
	}
	if app.isJSON() || !isInteractive() {
		return streamPlain(ch)
	}
	return streamTUI(ch, vmID)
}

// streamPlain prints events as they arrive (ndjson in json mode, one line each
// otherwise) and returns when the stream reaches a terminal state or closes.
func streamPlain(ch <-chan fuse.Event) error {
	for ev := range ch {
		if ev.Err != nil {
			return friendly(ev.Err)
		}
		if app.isJSON() {
			if err := printJSON(ev); err != nil {
				return err
			}
		} else {
			detail := ev.URL
			if ev.Error != "" {
				detail = ev.Error
			}
			fmt.Fprintf(os.Stdout, "%s  %-12s %s\n", shortTime(ev.UpdatedAt), ev.State, detail)
		}
		if fuse.IsTerminalState(ev.State) {
			return nil
		}
	}
	return nil
}

// --- bubbletea view ---

type eventMsg fuse.Event
type streamClosedMsg struct{}

type watchModel struct {
	vmID    string
	ch      <-chan fuse.Event
	spinner spinner.Model
	events  []fuse.Event
	done    bool
	err     error
}

func newWatchModel(vmID string, ch <-chan fuse.Event) watchModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	return watchModel{vmID: vmID, ch: ch, spinner: sp}
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
		m.events = append(m.events, ev)
		if fuse.IsTerminalState(ev.State) {
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
	}
	return m, nil
}

func (m watchModel) View() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", styleHeader.Render("watching "+m.vmID))
	for _, ev := range m.events {
		line := fmt.Sprintf("  %s  %s", shortTime(ev.UpdatedAt), stateStyle(ev.State))
		if ev.URL != "" {
			line += "  " + styleFaint.Render(ev.URL)
		}
		if ev.Error != "" {
			line += "  " + styleBad.Render(ev.Error)
		}
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

func streamTUI(ch <-chan fuse.Event, vmID string) error {
	final, err := tea.NewProgram(newWatchModel(vmID, ch)).Run()
	if err != nil {
		return err
	}
	if m, ok := final.(watchModel); ok && m.err != nil {
		return friendly(m.err)
	}
	return nil
}
