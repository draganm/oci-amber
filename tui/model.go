package tui

import (
	"time"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/draganm/oci-amber/importer"
)

// tickInterval is how often the view is refreshed from the tracker.
const tickInterval = 250 * time.Millisecond

type tickMsg time.Time

// doneMsg says the import returned; the program quits with an empty view.
type doneMsg struct{}

// model is the Bubble Tea model: it owns no import state, it renders the
// tracker's snapshots.
type model struct {
	tr     *importer.Tracker
	title  string
	cancel func()
	width  int
	bar    progress.Model
	spin   spinner.Model
	done   bool
}

func newModel(tr *importer.Tracker, title string, cancel func()) model {
	bar := progress.New(progress.WithDefaultGradient(), progress.WithoutPercentage())
	bar.Width = 30
	sp := spinner.New(spinner.WithSpinner(spinner.Dot))
	return model{tr: tr, title: title, cancel: cancel, width: 80, bar: bar, spin: sp}
}

func tick() tea.Cmd {
	return tea.Tick(tickInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m model) Init() tea.Cmd { return tea.Batch(tick(), m.spin.Tick) }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.bar.Width = max(10, min(40, msg.Width/3))
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.cancel()
		}
	case tickMsg:
		return m, tick()
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	case doneMsg:
		m.done = true
		return m, tea.Quit
	}
	return m, nil
}

func (m model) View() string {
	if m.done {
		return ""
	}
	return RenderView(m.tr.Snapshot(), m.title, m.width, m.bar.ViewAs, m.spin.View())
}

// Run drives run under the TUI: run starts on a goroutine, the program
// renders the tracker until run returns, and run's result is returned. cancel
// is called on q or ctrl-c; it should cancel run's context. Signals are left
// to the caller (the program does not install a handler), so a SIGINT from
// outside cancels through the caller's context the same way.
func Run(tr *importer.Tracker, title string, cancel func(), run func() (*importer.Report, error)) (*importer.Report, error) {
	p := tea.NewProgram(newModel(tr, title, cancel), tea.WithoutSignalHandler())
	type result struct {
		rep *importer.Report
		err error
	}
	results := make(chan result, 1)
	go func() {
		rep, err := run()
		results <- result{rep, err}
		p.Send(doneMsg{})
	}()
	if _, err := p.Run(); err != nil {
		// The terminal failed under us; the import keeps going and its
		// result still decides the exit status.
		cancel()
	}
	r := <-results
	return r.rep, r.err
}
