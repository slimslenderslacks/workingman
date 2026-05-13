// Package tui hosts the orch terminal UI. The layout is a two-pane skeleton:
// a left column listing agent sessions and a main panel showing a gallery of
// project cards. Project cards reflect live state from one or more root
// directories scanned by WatchProjects; sessions remain placeholder data
// until a later task wires them up.
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

type sessionItem struct {
	name string
	kind string
}

type projectsMsg struct {
	views []ProjectView
}

type model struct {
	width    int
	height   int
	focus    pane
	sessions []sessionItem
	projects []ProjectView
	projCh   <-chan []ProjectView
	loaded   bool
}

func newModel(projCh <-chan []ProjectView) model {
	return model{
		focus:  paneSessions,
		projCh: projCh,
		sessions: []sessionItem{
			{name: "orch-project-a1b2c3", kind: "project"},
			{name: "orch-planning-tui", kind: "planning"},
			{name: "orch-task-layout-skeleton", kind: "task"},
		},
	}
}

func (m model) Init() tea.Cmd {
	if m.projCh == nil {
		return nil
	}
	return waitForProjects(m.projCh)
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

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case projectsMsg:
		m.projects = msg.views
		m.loaded = true
		return m, waitForProjects(m.projCh)
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "tab", "shift+tab", "right", "left", "h", "l":
			if m.focus == paneSessions {
				m.focus = paneProjects
			} else {
				m.focus = paneSessions
			}
		}
	}
	return m, nil
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
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	cardNameStyle = lipgloss.NewStyle().Bold(true)
	cardBorder    = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("240")).
			Padding(0, 1)
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
	if len(m.sessions) == 0 {
		b.WriteString(dimStyle.Render("(none)"))
	} else {
		for _, s := range m.sessions {
			line := fmt.Sprintf("• %s", s.name)
			line = truncate(line, innerWidth)
			kind := dimStyle.Render("  " + s.kind)
			b.WriteString(line)
			b.WriteString("\n")
			b.WriteString(truncate(kind, innerWidth))
			b.WriteString("\n")
		}
	}
	return style.Render(b.String())
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
	footer := hintStyle.Render("tab: switch pane  •  q: quit  •  focus: " + paneName(m.focus))

	bodyHeight := m.height - lipgloss.Height(header) - lipgloss.Height(footer) - 1
	if bodyHeight < 3 {
		bodyHeight = 3
	}

	sessionsWidth := m.width / 3
	if sessionsWidth < 20 {
		sessionsWidth = 20
	}
	if sessionsWidth > 40 {
		sessionsWidth = 40
	}
	if sessionsWidth > m.width-10 {
		sessionsWidth = m.width - 10
		if sessionsWidth < 10 {
			sessionsWidth = m.width
		}
	}
	projectsWidth := m.width - sessionsWidth
	if projectsWidth < 1 {
		projectsWidth = 1
	}

	left := m.renderSessions(sessionsWidth, bodyHeight)
	right := m.renderProjects(projectsWidth, bodyHeight)
	body := lipgloss.JoinHorizontal(lipgloss.Top, left, right)

	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

func paneName(p pane) string {
	if p == paneSessions {
		return "sessions"
	}
	return "projects"
}

// Run launches the TUI. It scans the given roots for projects and live-updates
// the gallery as files change. It blocks until the user quits or ctx is
// cancelled. Roots may be empty, in which case the gallery shows "(none)".
func Run(ctx context.Context, roots []string) error {
	tuiCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var ch <-chan []ProjectView
	if len(roots) > 0 {
		ch = WatchProjects(tuiCtx, roots, time.Second)
	}

	p := tea.NewProgram(newModel(ch), tea.WithAltScreen(), tea.WithContext(tuiCtx))
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("tui: %w", err)
	}
	return nil
}
