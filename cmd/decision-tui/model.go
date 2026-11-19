package main

import (
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"

	"github.com/slimslenderslacks/work/internal/decision"
)

// config is everything the model needs from main.
type config struct {
	Inbox  string
	Outbox string
	// Events is the channel from decision.Watch; nil disables live updates.
	Events <-chan decision.Event
	// GlamourStyle is a glamour standard style name ("dark", "light",
	// "notty", ...).
	GlamourStyle string
	// InitialErrors are the per-file errors from the startup decision.Scan.
	InitialErrors []error
	// Now returns the current time; defaults to time.Now.
	Now func() time.Time
}

type screen int

const (
	screenList screen = iota
	screenDetail
)

// eventMsg carries one decision.Watch event into Update.
type eventMsg decision.Event

// watchClosedMsg reports that the watcher channel was closed.
type watchClosedMsg struct{}

// waitForEvent returns a command that blocks for the next watcher event.
// Update re-issues it after every event so the stream is consumed for the
// lifetime of the program.
func waitForEvent(ch <-chan decision.Event) tea.Cmd {
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return watchClosedMsg{}
		}
		return eventMsg(ev)
	}
}

// fileError is a request file that could not be read or parsed.
type fileError struct {
	Filename string
	Err      string
}

type model struct {
	cfg config

	pending []decision.Pending
	errors  []fileError
	cursor  int

	screen screen
	// current is the file name of the request open in the detail screen.
	current      string
	choiceCursor int
	viewport     viewport.Model
	// renderedFor and renderedWidth cache the glamour output so the summary
	// is only re-rendered when the request or the width changes.
	renderedFor   string
	renderedWidth int

	width, height int

	status    string
	statusErr bool
}

func newModel(cfg config, pending []decision.Pending) model {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.GlamourStyle == "" {
		cfg.GlamourStyle = "dark"
	}
	m := model{
		cfg:      cfg,
		pending:  pending,
		width:    80,
		height:   24,
		viewport: viewport.New(80, 10),
	}
	for _, err := range cfg.InitialErrors {
		m.addError(err)
	}
	return m
}

func (m model) Init() tea.Cmd { return waitForEvent(m.cfg.Events) }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil
	case eventMsg:
		m.handleEvent(decision.Event(msg))
		return m, waitForEvent(m.cfg.Events)
	case watchClosedMsg:
		m.setError("inbox watcher stopped; new requests will not appear until restart")
		return m, nil
	case tea.KeyMsg:
		if s := msg.String(); s == "ctrl+c" || s == "q" {
			return m, tea.Quit
		}
		if m.screen == screenDetail {
			return m.updateDetail(msg)
		}
		return m.updateList(msg)
	case tea.MouseMsg:
		if m.screen == screenDetail {
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		}
	}
	return m, nil
}

func (m model) updateList(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.pending)-1 {
			m.cursor++
		}
	case "home", "g":
		m.cursor = 0
	case "end", "G":
		m.cursor = max(len(m.pending)-1, 0)
	case "enter":
		if m.cursor < len(m.pending) {
			m.open(m.pending[m.cursor].Filename)
		}
	}
	return m, nil
}

func (m model) updateDetail(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := m.currentPending()
	if p == nil {
		m.screen = screenList
		return m, nil
	}
	switch msg.String() {
	case "esc":
		m.screen = screenList
		m.current = ""
	case "up", "k":
		if m.choiceCursor > 0 {
			m.choiceCursor--
		}
	case "down", "j":
		if m.choiceCursor < len(p.Request.Choices)-1 {
			m.choiceCursor++
		}
	case "enter":
		m.answer(*p, p.Request.Choices[m.choiceCursor])
	default:
		// pgup/pgdown, ctrl+u/ctrl+d etc. scroll the summary.
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd
	}
	return m, nil
}

// open switches to the detail screen for the request in filename.
func (m *model) open(filename string) {
	m.screen = screenDetail
	m.current = filename
	m.choiceCursor = 0
	m.renderedFor = ""
	m.layout()
	m.viewport.GotoTop()
}

// answer records c as the answer to p: the answer is written to the
// outbox, then the request is archived and dropped from the list.
func (m *model) answer(p decision.Pending, c decision.Choice) {
	a := decision.Answer{
		ID:          p.Request.ID,
		ChoiceID:    c.ID,
		ChoiceLabel: c.Label,
		AnsweredAt:  m.cfg.Now().UTC(),
	}
	if err := decision.WriteAnswer(m.cfg.Outbox, a); err != nil {
		m.setError(fmt.Sprintf("answer not saved: %v", err))
		return
	}
	m.removePending(p.Filename)
	m.screen = screenList
	m.current = ""
	if err := decision.ArchiveRequest(m.cfg.Inbox, p.Filename); err != nil {
		m.setError(fmt.Sprintf("answered %q with %q, but the request could not be archived: %v", p.Request.Title, c.Label, err))
		return
	}
	m.setStatus(fmt.Sprintf("answered %q: %s", p.Request.Title, c.Label))
}

func (m *model) handleEvent(ev decision.Event) {
	switch ev.Kind {
	case decision.EventAdded, decision.EventUpdated:
		if ev.Pending == nil {
			return
		}
		m.clearError(ev.Filename)
		m.upsertPending(*ev.Pending)
		if m.screen == screenDetail && m.current == ev.Filename {
			// Re-render the changed summary and keep the cursor on a
			// valid choice.
			m.renderedFor = ""
			m.choiceCursor = min(m.choiceCursor, len(ev.Pending.Request.Choices)-1)
			m.layout()
		}
		if ev.Kind == decision.EventAdded {
			m.setStatus(fmt.Sprintf("new request: %s", ev.Pending.Request.Title))
		}
	case decision.EventRemoved:
		m.clearError(ev.Filename)
		if !m.removePending(ev.Filename) {
			return
		}
		if m.screen == screenDetail && m.current == ev.Filename {
			m.screen = screenList
			m.current = ""
			m.setError(fmt.Sprintf("%s was removed from the inbox", ev.Filename))
		}
	case decision.EventError:
		if ev.Filename == "" {
			m.setError(fmt.Sprint(ev.Err))
			return
		}
		// A file that became invalid is no longer answerable.
		m.removePending(ev.Filename)
		if m.screen == screenDetail && m.current == ev.Filename {
			m.screen = screenList
			m.current = ""
		}
		m.addError(&decision.FileError{Filename: ev.Filename, Path: ev.Path, Err: ev.Err})
		m.setError(fmt.Sprintf("malformed request %s", ev.Filename))
	}
}

// upsertPending adds p, or replaces the entry with the same file name,
// keeping the list sorted and the list cursor on the same request.
func (m *model) upsertPending(p decision.Pending) {
	selected := m.selectedFilename()
	replaced := false
	for i := range m.pending {
		if m.pending[i].Filename == p.Filename {
			m.pending[i] = p
			replaced = true
			break
		}
	}
	if !replaced {
		m.pending = append(m.pending, p)
	}
	decision.SortPending(m.pending)
	m.selectFilename(selected)
}

// removePending drops the request with filename and reports whether it was
// present.
func (m *model) removePending(filename string) bool {
	selected := m.selectedFilename()
	for i := range m.pending {
		if m.pending[i].Filename == filename {
			m.pending = append(m.pending[:i], m.pending[i+1:]...)
			if selected != filename {
				m.selectFilename(selected)
			}
			m.cursor = min(m.cursor, max(len(m.pending)-1, 0))
			return true
		}
	}
	return false
}

func (m *model) selectedFilename() string {
	if m.cursor < len(m.pending) {
		return m.pending[m.cursor].Filename
	}
	return ""
}

func (m *model) selectFilename(filename string) {
	for i := range m.pending {
		if m.pending[i].Filename == filename {
			m.cursor = i
			return
		}
	}
}

func (m *model) currentPending() *decision.Pending {
	for i := range m.pending {
		if m.pending[i].Filename == m.current {
			return &m.pending[i]
		}
	}
	return nil
}

func (m *model) addError(err error) {
	fe := fileError{Err: err.Error()}
	if f, ok := err.(*decision.FileError); ok {
		fe = fileError{Filename: f.Filename, Err: f.Err.Error()}
	}
	if fe.Filename != "" {
		for i := range m.errors {
			if m.errors[i].Filename == fe.Filename {
				m.errors[i] = fe
				return
			}
		}
	}
	m.errors = append(m.errors, fe)
}

func (m *model) clearError(filename string) {
	for i := range m.errors {
		if m.errors[i].Filename == filename {
			m.errors = append(m.errors[:i], m.errors[i+1:]...)
			return
		}
	}
}

func (m *model) setStatus(s string) { m.status, m.statusErr = s, false }
func (m *model) setError(s string)  { m.status, m.statusErr = s, true }

// layout sizes the detail viewport for the current window and request and
// (re-)renders the summary when needed.
func (m *model) layout() {
	p := m.currentPending()
	if p == nil {
		return
	}
	w := max(m.width, 20)
	// Title + meta, blank, [viewport], blank, "Choices", choices, blank,
	// status, help.
	fixed := 2 + 1 + 1 + 1 + len(p.Request.Choices) + 1 + 2
	m.viewport.Width = w
	m.viewport.Height = max(m.height-fixed, 3)
	if m.renderedFor != p.Filename || m.renderedWidth != w {
		m.viewport.SetContent(renderMarkdown(p.Request.Summary, m.cfg.GlamourStyle, w))
		m.renderedFor, m.renderedWidth = p.Filename, w
	}
}

// renderMarkdown renders md with glamour wrapped to width, falling back to
// the raw markdown if rendering fails.
func renderMarkdown(md, style string, width int) string {
	r, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle(style),
		glamour.WithWordWrap(max(width-4, 10)),
	)
	if err != nil {
		return md
	}
	out, err := r.Render(md)
	if err != nil {
		return md
	}
	return out
}
