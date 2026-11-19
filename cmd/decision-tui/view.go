package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

var (
	titleStyle    = lipgloss.NewStyle().Bold(true)
	dimStyle      = lipgloss.NewStyle().Faint(true)
	selectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	errorStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	okStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	headingStyle  = lipgloss.NewStyle().Bold(true).Underline(true)
)

const timeLayout = "2006-01-02 15:04"

func (m model) View() string {
	if m.screen == screenDetail && m.currentPending() != nil {
		return m.detailView()
	}
	return m.listView()
}

func (m model) listView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(fmt.Sprintf("Pending decisions (%d)", len(m.pending))))
	b.WriteString("\n" + dimStyle.Render(m.truncate("inbox: "+m.cfg.Inbox)) + "\n\n")
	if len(m.pending) == 0 {
		b.WriteString(dimStyle.Render("  No pending decisions. Waiting for requests…") + "\n")
	}
	for i, p := range m.pending {
		var meta []string
		if p.Request.Source != "" {
			meta = append(meta, p.Request.Source)
		}
		if p.Request.CreatedAt != nil {
			meta = append(meta, p.Request.CreatedAt.Local().Format(timeLayout))
		}
		line := p.Request.Title
		if len(meta) > 0 {
			line += "  " + dimStyle.Render(strings.Join(meta, " · "))
		}
		if i == m.cursor {
			line = selectedStyle.Render("> "+p.Request.Title) + strings.TrimPrefix(line, p.Request.Title)
		} else {
			line = "  " + line
		}
		b.WriteString(m.truncate(line) + "\n")
	}
	if len(m.errors) > 0 {
		b.WriteString("\n" + errorStyle.Render(fmt.Sprintf("Errors (%d)", len(m.errors))) + "\n")
		for _, e := range m.errors {
			s := e.Err
			if e.Filename != "" {
				s = e.Filename + ": " + s
			}
			b.WriteString(m.truncate(errorStyle.Render("  "+s)) + "\n")
		}
	}
	b.WriteString("\n" + m.statusLine() + "\n")
	b.WriteString(dimStyle.Render("↑/↓ j/k move · enter open · q quit"))
	return b.String()
}

func (m model) detailView() string {
	p := m.currentPending()
	var b strings.Builder
	b.WriteString(m.truncate(titleStyle.Render(p.Request.Title)) + "\n")
	meta := []string{"id: " + p.Request.ID}
	if p.Request.Source != "" {
		meta = append(meta, "source: "+p.Request.Source)
	}
	if p.Request.CreatedAt != nil {
		meta = append(meta, "created: "+p.Request.CreatedAt.Local().Format(timeLayout))
	}
	b.WriteString(m.truncate(dimStyle.Render(strings.Join(meta, " · "))) + "\n\n")
	b.WriteString(m.viewport.View() + "\n")
	scroll := ""
	if !(m.viewport.AtTop() && m.viewport.AtBottom()) {
		scroll = fmt.Sprintf(" (%3.0f%%)", m.viewport.ScrollPercent()*100)
	}
	b.WriteString(dimStyle.Render(strings.Repeat("─", max(m.width-len(scroll)-1, 1))+scroll) + "\n")
	b.WriteString(headingStyle.Render("Choices") + "\n")
	for i, c := range p.Request.Choices {
		if i == m.choiceCursor {
			b.WriteString(m.truncate(selectedStyle.Render("> "+c.Label)) + "\n")
		} else {
			b.WriteString(m.truncate("  "+c.Label) + "\n")
		}
	}
	b.WriteString("\n" + m.statusLine() + "\n")
	b.WriteString(dimStyle.Render("↑/↓ j/k choose · enter answer · pgup/pgdn scroll · esc back · q quit"))
	return b.String()
}

func (m model) statusLine() string {
	if m.status == "" {
		return ""
	}
	if m.statusErr {
		return m.truncate(errorStyle.Render("✗ " + m.status))
	}
	return m.truncate(okStyle.Render("✓ " + m.status))
}

func (m model) truncate(s string) string {
	if m.width <= 0 {
		return s
	}
	return ansi.Truncate(s, m.width, "…")
}
