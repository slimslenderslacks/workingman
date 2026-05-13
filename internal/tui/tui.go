// Package tui hosts the orch terminal UI. The layout is a two-pane skeleton:
// a left column listing agent sessions and a main panel showing a gallery of
// projects. Both panes render placeholder data; real wiring lives in later
// tasks.
package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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

type projectItem struct {
	description string
	branch      string
	status      string
	repos       []string
}

type model struct {
	width    int
	height   int
	focus    pane
	sessions []sessionItem
	projects []projectItem
}

func newModel() model {
	return model{
		focus: paneSessions,
		sessions: []sessionItem{
			{name: "orch-project-a1b2c3", kind: "project"},
			{name: "orch-planning-tui", kind: "planning"},
			{name: "orch-task-layout-skeleton", kind: "task"},
		},
		projects: []projectItem{
			{
				description: "Add a /healthz endpoint to the gateway",
				branch:      "feat/healthz-probe",
				status:      "working",
				repos:       []string{"docker/gateway", "docker/deploy-manifests"},
			},
			{
				description: "Build the TUI for orch",
				branch:      "tui",
				status:      "working",
				repos:       []string{"slimslenderslacks/workingman"},
			},
			{
				description: "Investigate flaky integration tests",
				branch:      "fix/flaky-tests",
				status:      "blocked",
				repos:       []string{"docker/ci"},
			},
		},
	}
}

func (m model) Init() tea.Cmd { return nil }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
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
	if len(m.projects) == 0 {
		b.WriteString(dimStyle.Render("(none)"))
	} else {
		for i, p := range m.projects {
			if i > 0 {
				b.WriteString("\n")
			}
			header := fmt.Sprintf("%s  %s", renderStatus(p.status), p.branch)
			b.WriteString(truncate(header, innerWidth))
			b.WriteString("\n")
			b.WriteString(truncate(p.description, innerWidth))
			b.WriteString("\n")
			repos := dimStyle.Render(strings.Join(p.repos, ", "))
			b.WriteString(truncate(repos, innerWidth))
			b.WriteString("\n")
		}
	}
	return style.Render(b.String())
}

// truncate clips s to width display columns. Width 0 returns empty.
func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	// Conservative byte-based clip; bubbletea handles styled-rune width on
	// render. The layout always supplies generous widths so this only fires
	// on very narrow terminals.
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

// Run launches the TUI. It blocks until the user quits or ctx is cancelled.
func Run(ctx context.Context) error {
	p := tea.NewProgram(newModel(), tea.WithAltScreen(), tea.WithContext(ctx))
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("tui: %w", err)
	}
	return nil
}
