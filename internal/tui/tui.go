// Package tui hosts the orch terminal UI. The layout is a two-pane skeleton:
// a left column listing agent sessions and a main panel showing a gallery of
// project cards. Both panes reflect live state — projects from the
// .project.yaml files scanned by WatchProjects, sessions from a channel the
// daemon feeds in via its WatchSessions adapter.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/slimslenderslacks/work/internal/task"
)

type pane int

const (
	paneSessions pane = iota
	paneProjects
)

type projectsMsg struct {
	views []ProjectView
}

type sessionsMsg struct {
	views []SessionView
}

type model struct {
	width         int
	height        int
	focus         pane
	sessions      []SessionView
	sessCh        <-chan []SessionView
	sessLoaded    bool
	sessSel       string
	sessionsWidth int
	projects      []ProjectView
	projCh        <-chan []ProjectView
	loaded        bool
	attacher      tmuxAttacher
	statusMsg     string
}

func newModel(projCh <-chan []ProjectView, sessCh <-chan []SessionView, attacher tmuxAttacher) model {
	// When no sessions source is wired in (standalone tui mode), short-circuit
	// to the empty state so the pane shows "(none)" instead of an endless
	// "(loading…)".
	return model{
		focus:      paneSessions,
		projCh:     projCh,
		sessCh:     sessCh,
		sessLoaded: sessCh == nil,
		attacher:   attacher,
	}
}

func (m model) Init() tea.Cmd {
	var cmds []tea.Cmd
	if m.projCh != nil {
		cmds = append(cmds, waitForProjects(m.projCh))
	}
	if m.sessCh != nil {
		cmds = append(cmds, waitForSessions(m.sessCh))
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

func waitForProjects(ch <-chan []ProjectView) tea.Cmd {
	return func() tea.Msg {
		views, ok := <-ch
		if !ok {
			return nil
		}
		return projectsMsg{views: views}
	}
}

func waitForSessions(ch <-chan []SessionView) tea.Cmd {
	return func() tea.Msg {
		views, ok := <-ch
		if !ok {
			return nil
		}
		return sessionsMsg{views: views}
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.sessionsWidth, _ = paneWidths(m.width)
	case projectsMsg:
		m.projects = msg.views
		m.loaded = true
		return m, waitForProjects(m.projCh)
	case sessionsMsg:
		m.sessions = msg.views
		m.sessLoaded = true
		m.sessSel = reconcileSelection(m.sessions, m.sessSel)
		return m, waitForSessions(m.sessCh)
	case attachResultMsg:
		if msg.err != nil {
			m.statusMsg = fmt.Sprintf("attach %s: %v", msg.target, msg.err)
		} else {
			m.statusMsg = ""
		}
		return m, nil
	case tea.MouseMsg:
		return m.handleMouse(msg)
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "tab", "shift+tab":
			m.focus = togglePane(m.focus)
			m.statusMsg = ""
		case "right", "l":
			m.focus = paneProjects
			m.statusMsg = ""
		case "left", "h":
			m.focus = paneSessions
			m.statusMsg = ""
		case "up":
			if m.focus == paneSessions {
				m.sessSel = moveSelection(m.sessions, m.sessSel, -1)
				m.statusMsg = ""
			}
		case "down":
			if m.focus == paneSessions {
				m.sessSel = moveSelection(m.sessions, m.sessSel, 1)
				m.statusMsg = ""
			}
		case "enter":
			if m.focus == paneSessions {
				return m.attachSelected()
			}
		}
	}
	return m, nil
}

// handleMouse routes a mouse event to its pane. A left-button press inside
// the sessions pane selects the row under the cursor and immediately attaches
// to it — the task's "click to attach" UX trumps click-to-select-only.
func (m model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return m, nil
	}
	if m.sessionsWidth <= 0 || msg.X < 0 || msg.X >= m.sessionsWidth {
		return m, nil
	}
	idx := sessionRowAtY(msg.Y, len(m.sessions))
	if idx < 0 {
		m.focus = paneSessions
		return m, nil
	}
	m.focus = paneSessions
	m.sessSel = m.sessions[idx].ID
	m.statusMsg = ""
	return m.attachSelected()
}

// attachSelected dispatches the tmux-attach command for the currently
// selected session row. The actual suspend/resume is owned by bubbletea via
// tea.ExecProcess; this model just returns the Cmd.
func (m model) attachSelected() (tea.Model, tea.Cmd) {
	if m.attacher == nil {
		m.statusMsg = "tmux attach disabled (no attacher wired)"
		return m, nil
	}
	target, ok := selectedTmuxTarget(m.sessions, m.sessSel)
	if !ok {
		m.statusMsg = "no session selected"
		return m, nil
	}
	m.statusMsg = ""
	return m, m.attacher.Attach(target)
}

func selectedTmuxTarget(views []SessionView, id string) (string, bool) {
	for _, v := range views {
		if v.ID == id {
			return v.TmuxTarget, true
		}
	}
	return "", false
}

// sessionRowAtY maps an absolute y row to a session index using the same
// layout constants View() renders with: header(1) + pane top border(1) +
// "Sessions" title + blank(2) + per-session triplet of (head, status,
// separator). The separator row is intentionally a dead zone so clicking
// between rows doesn't pick the wrong session.
func sessionRowAtY(y, count int) int {
	const (
		headerLines    = 1
		paneTopBorder  = 1
		paneTitleLines = 2
		rowsPerSession = 3
	)
	rel := y - (headerLines + paneTopBorder + paneTitleLines)
	if rel < 0 {
		return -1
	}
	idx := rel / rowsPerSession
	if rel%rowsPerSession == 2 {
		return -1
	}
	if idx < 0 || idx >= count {
		return -1
	}
	return idx
}

// paneWidths reproduces View()'s sessions/projects split so handleMouse can
// decide whether a click landed in the sessions pane without re-rendering.
// Keep this in lockstep with the width clamps used at the top of View().
func paneWidths(width int) (sessionsWidth, projectsWidth int) {
	if width <= 0 {
		return 0, 0
	}
	sessionsWidth = width / 3
	if sessionsWidth < 20 {
		sessionsWidth = 20
	}
	if sessionsWidth > 40 {
		sessionsWidth = 40
	}
	if sessionsWidth > width-10 {
		sessionsWidth = width - 10
		if sessionsWidth < 10 {
			sessionsWidth = width
		}
	}
	projectsWidth = width - sessionsWidth
	if projectsWidth < 1 {
		projectsWidth = 1
	}
	return sessionsWidth, projectsWidth
}

func togglePane(p pane) pane {
	if p == paneSessions {
		return paneProjects
	}
	return paneSessions
}

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("212"))
	hintStyle = lipgloss.NewStyle().
			Faint(true)
	paneTitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("110"))
	focusedBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("212")).
			Padding(0, 1)
	unfocusedBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("240")).
			Padding(0, 1)
	statusReady   = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	statusWorking = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	statusBlocked = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	statusDone    = lipgloss.NewStyle().Foreground(lipgloss.Color("46"))
	statusRunning = lipgloss.NewStyle().Foreground(lipgloss.Color("82"))
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	cardNameStyle = lipgloss.NewStyle().Bold(true)
	cardBorder    = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("240")).
			Padding(0, 1)
	sessionRowSelectedStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("212"))
	statusErrStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196"))
)

// Marker glyphs for the sessions pane. The selected row gets a filled marker;
// other rows get whitespace of the same width so the agent-name column stays
// aligned regardless of which row is selected.
const (
	sessionMarkerSelected = "▶ "
	sessionMarkerIdle     = "  "
)

// Card sizing. Width is a target; the layout falls back to a single-column
// stack when the projects pane is too narrow to fit a card at this size.
const (
	cardTargetWidth = 30
	cardMinWidth    = 20
	cardGap         = 1
)

func (m model) borderStyle(p pane) lipgloss.Style {
	if m.focus == p {
		return focusedBorder
	}
	return unfocusedBorder
}

func renderStatus(s string) string {
	switch s {
	case "ready":
		return statusReady.Render(s)
	case "working":
		return statusWorking.Render(s)
	case "blocked":
		return statusBlocked.Render(s)
	case "done":
		return statusDone.Render(s)
	default:
		return s
	}
}

func (m model) renderSessions(width, height int) string {
	style := m.borderStyle(paneSessions).Width(width).Height(height)
	innerWidth := width - style.GetHorizontalFrameSize()
	if innerWidth < 0 {
		innerWidth = 0
	}

	var b strings.Builder
	b.WriteString(paneTitleStyle.Render("Sessions"))
	b.WriteString("\n\n")
	if !m.sessLoaded {
		b.WriteString(dimStyle.Render("(loading…)"))
		return style.Render(b.String())
	}
	if len(m.sessions) == 0 {
		b.WriteString(dimStyle.Render("(none)"))
		return style.Render(b.String())
	}
	for i, s := range m.sessions {
		b.WriteString(renderSessionRow(s, s.ID == m.sessSel, innerWidth))
		if i < len(m.sessions)-1 {
			b.WriteString("\n")
		}
	}
	return style.Render(b.String())
}

// renderSessionRow draws one session as a two-line block: the headline carries
// the agent kind and project; the second line carries a colored status
// indicator. Truncation happens on the raw text before any style is applied so
// the byte-slice in truncate never tears an ANSI escape.
func renderSessionRow(s SessionView, selected bool, width int) string {
	marker := sessionMarkerIdle
	if selected {
		marker = sessionMarkerSelected
	}
	head := marker + s.AgentName
	if s.Project != "" {
		head += "  " + s.Project
	}
	head = truncate(head, width)
	if selected {
		head = sessionRowSelectedStyle.Render(head)
	}

	statusText := truncate("  "+sessionStatusGlyph(s.Status)+" "+s.Status, width)
	statusStyled := sessionStatusStyle(s.Status).Render(statusText)
	return head + "\n" + statusStyled
}

// sessionStatusGlyph returns a compact symbol that pairs with the status
// label. It mirrors the colored dots used by terminal status lines so the
// pane reads at a glance.
func sessionStatusGlyph(status string) string {
	switch status {
	case "running":
		return "●"
	default:
		return "○"
	}
}

func sessionStatusStyle(status string) lipgloss.Style {
	switch status {
	case "running":
		return statusRunning
	default:
		return dimStyle
	}
}

func (m model) renderProjects(width, height int) string {
	style := m.borderStyle(paneProjects).Width(width).Height(height)
	innerWidth := width - style.GetHorizontalFrameSize()
	if innerWidth < 0 {
		innerWidth = 0
	}

	var b strings.Builder
	b.WriteString(paneTitleStyle.Render("Projects"))
	b.WriteString("\n\n")
	if !m.loaded {
		b.WriteString(dimStyle.Render("(loading…)"))
		return style.Render(b.String())
	}
	if len(m.projects) == 0 {
		b.WriteString(dimStyle.Render("(none)"))
		return style.Render(b.String())
	}

	b.WriteString(renderProjectGrid(m.projects, innerWidth))
	return style.Render(b.String())
}

// renderProjectGrid lays project cards out left-to-right, wrapping to a new
// row whenever the next card wouldn't fit in innerWidth. Cards shrink to the
// available width when the pane can't hold one at the target width.
func renderProjectGrid(views []ProjectView, innerWidth int) string {
	if innerWidth <= 0 || len(views) == 0 {
		return ""
	}

	cardWidth := cardTargetWidth
	if cardWidth > innerWidth {
		cardWidth = innerWidth
	}
	if cardWidth < cardMinWidth {
		cardWidth = innerWidth
	}

	perRow := (innerWidth + cardGap) / (cardWidth + cardGap)
	if perRow < 1 {
		perRow = 1
	}

	var rows []string
	for i := 0; i < len(views); i += perRow {
		end := i + perRow
		if end > len(views) {
			end = len(views)
		}
		cards := make([]string, 0, end-i)
		for j := i; j < end; j++ {
			cards = append(cards, renderProjectCard(views[j], cardWidth))
		}
		if cardGap > 0 && len(cards) > 1 {
			joined := make([]string, 0, len(cards)*2-1)
			gap := strings.Repeat(" ", cardGap)
			for k, c := range cards {
				if k > 0 {
					joined = append(joined, gap)
				}
				joined = append(joined, c)
			}
			rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, joined...))
		} else {
			rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, cards...))
		}
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

func renderProjectCard(v ProjectView, width int) string {
	style := cardBorder.Width(width)
	inner := width - style.GetHorizontalFrameSize()
	if inner < 1 {
		inner = 1
	}

	name := cardNameStyle.Render(truncate(v.Name, inner))
	statusLine := renderStatus(string(v.Status))
	if v.Branch != "" {
		branchTxt := dimStyle.Render(v.Branch)
		statusLine = truncate(statusLine+"  "+branchTxt, inner)
	}
	breakdown := renderTaskBreakdown(v.TaskCounts, inner)

	body := name + "\n" + statusLine + "\n" + breakdown
	return style.Render(body)
}

// renderTaskBreakdown renders the per-state task counts in a compact form.
// Done counts both committed and success tasks — from the user's vantage the
// task is "done" once it has produced a successful run, even if the commit
// agent hasn't flipped it to committed yet.
func renderTaskBreakdown(counts map[task.Status]int, width int) string {
	if len(counts) == 0 {
		return dimStyle.Render(truncate("no tasks", width))
	}
	ready := counts[task.StatusReady]
	running := counts[task.StatusRunning]
	blocked := counts[task.StatusBlocked]
	done := counts[task.StatusSuccess] + counts[task.StatusCommitted]
	failed := counts[task.StatusFailed]
	line := fmt.Sprintf("R:%d W:%d B:%d D:%d F:%d",
		ready, running, blocked, done, failed)
	return dimStyle.Render(truncate(line, width))
}

// truncate clips s to width display columns. Width 0 returns empty.
func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	if width <= 1 {
		return "…"
	}
	for i := len(s); i > 0; i-- {
		candidate := s[:i] + "…"
		if lipgloss.Width(candidate) <= width {
			return candidate
		}
	}
	return "…"
}

func (m model) View() string {
	if m.width == 0 || m.height == 0 {
		return "starting orch tui…"
	}

	header := titleStyle.Render("orch")
	footer := m.renderFooter()

	bodyHeight := m.height - lipgloss.Height(header) - lipgloss.Height(footer) - 1
	if bodyHeight < 3 {
		bodyHeight = 3
	}

	sessionsWidth, projectsWidth := paneWidths(m.width)

	left := m.renderSessions(sessionsWidth, bodyHeight)
	right := m.renderProjects(projectsWidth, bodyHeight)
	body := lipgloss.JoinHorizontal(lipgloss.Top, left, right)

	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

// renderFooter draws the bottom hint line. When statusMsg is set (typically a
// tmux-attach failure), it replaces the keybinding hint so the error is
// front-and-centre instead of buried.
func (m model) renderFooter() string {
	if m.statusMsg != "" {
		return statusErrStyle.Render(m.statusMsg)
	}
	return hintStyle.Render("tab: switch pane  •  ↑/↓: select session  •  enter/click: attach  •  q: quit  •  focus: " + paneName(m.focus))
}

func paneName(p pane) string {
	if p == paneSessions {
		return "sessions"
	}
	return "projects"
}

// Run launches the TUI. It scans the given roots for projects and live-updates
// the gallery as files change; the sessions pane is fed by sessCh, which the
// caller wires up to a real source (the daemon's WatchSessions) or leaves nil
// for standalone/demo mode. Run blocks until the user quits or ctx is
// cancelled.
//
// Mouse cell-motion is enabled so the sessions pane responds to clicks; the
// tmux-attach plumbing uses tea.ExecProcess, which suspends and restores the
// alt-screen automatically.
func Run(ctx context.Context, roots []string, sessCh <-chan []SessionView) error {
	tuiCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var ch <-chan []ProjectView
	if len(roots) > 0 {
		ch = WatchProjects(tuiCtx, roots, time.Second)
	}

	p := tea.NewProgram(
		newModel(ch, sessCh, newTmuxAttacher()),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
		tea.WithContext(tuiCtx),
	)
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("tui: %w", err)
	}
	return nil
}
