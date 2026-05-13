// Package tui hosts the orch terminal UI. The current entry point renders a
// minimal "hello" screen so the rest of the daemon can be wired up against
// a known-good bubbletea program.
package tui

import (
	"context"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type model struct {
	width  int
	height int
}

func (m model) Init() tea.Cmd { return nil }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
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
)

func (m model) View() string {
	body := lipgloss.JoinVertical(lipgloss.Left,
		titleStyle.Render("orch TUI"),
		"",
		"hello, TUI",
		"",
		hintStyle.Render("press q to quit"),
	)
	if m.width == 0 || m.height == 0 {
		return body
	}
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, body)
}

// Run launches the TUI. It blocks until the user quits or ctx is cancelled.
func Run(ctx context.Context) error {
	p := tea.NewProgram(model{}, tea.WithAltScreen(), tea.WithContext(ctx))
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("tui: %w", err)
	}
	return nil
}
