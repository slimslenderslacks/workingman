package tui

import (
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// renderProjectYAML draws the bottom pane of the right column: a scrollable
// viewer that shows either the currently-selected project's .project.yaml
// or the selected task's YAML file. The choice is driven by m.yamlSrc,
// toggled with the p / t keys, so the viewer's content is decoupled from
// pane focus — the user can scroll the YAML pane, click around projects
// and tasks, and the file they asked to see stays put.
//
// Long lines are hard-wrapped at the inner width so a description that
// runs off-screen still reads cleanly. Scroll is line-based and operates
// on the post-wrap line count, so up/down stays predictable regardless of
// how many wraps a given source line produced.
//
// The pane's title flips between "Project YAML" and "Task YAML" to match
// the source. Two empty states:
//   - No selection → "(none)".
//   - File missing / unreadable → the OS error message, styled as an error.
//
// `height` is the total rows the pane should occupy; the title and one blank
// line below it consume two of those rows, the rest hold YAML content.
func (m model) renderProjectYAML(width, height int) string {
	bs := m.borderStyle(paneProjectYAML)
	base := bs.Width(width - bs.GetHorizontalBorderSize())
	innerHeight := height - base.GetVerticalFrameSize()
	if innerHeight < 0 {
		innerHeight = 0
	}
	style := base.Height(innerHeight).MaxWidth(width)
	innerWidth := width - style.GetHorizontalFrameSize()
	if innerWidth < 0 {
		innerWidth = 0
	}

	title := "Project YAML"
	path := m.projSel
	if m.yamlSrc == yamlSourceTask {
		title = "Task YAML"
		path = m.taskSel
	}

	var b strings.Builder
	b.WriteString(paneTitleStyle.Render(title))
	b.WriteString("\n\n")

	contentRows := innerHeight - 2
	if contentRows < 0 {
		contentRows = 0
	}

	body, isErr := projectYAMLBody(path)
	lines := wrapDisplayWidth(body, innerWidth)

	// Scroll clamp: never let scroll push the content entirely out of view,
	// but allow scrolling far enough to reveal the last line.
	maxScroll := len(lines) - contentRows
	if maxScroll < 0 {
		maxScroll = 0
	}
	scroll := m.yamlScroll
	if scroll > maxScroll {
		scroll = maxScroll
	}
	if scroll < 0 {
		scroll = 0
	}
	end := scroll + contentRows
	if end > len(lines) {
		end = len(lines)
	}
	for i := scroll; i < end; i++ {
		line := lines[i]
		if isErr {
			line = statusErrStyle.Render(line)
		}
		b.WriteString(line)
		if i < end-1 {
			b.WriteString("\n")
		}
	}

	return style.Render(clampLines(b.String(), innerHeight))
}

// projectYAMLBody returns the raw content of the selected project's
// .project.yaml file. The returned bool is true for error messages so the
// caller can style them in red; false for normal file content or the
// "(none)" placeholder.
func projectYAMLBody(path string) (string, bool) {
	if path == "" {
		return dimStyle.Render("(none)"), false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err.Error(), true
	}
	return string(data), false
}

// wrapDisplayWidth hard-wraps each newline-separated line of s to width
// display columns. Empty source lines become a single empty output line so
// blank rows in the YAML are preserved as visual spacing. The wrap is
// character-based rather than word-aware — YAML values often contain
// hyphens, slashes, and other characters word-aware wrap would treat as
// breakpoints, and a simple display-width wrap is easier to reason about
// during scroll math.
func wrapDisplayWidth(s string, width int) []string {
	if width <= 0 {
		// No meaningful wrap target; return the input split by newlines so
		// the caller's scroll math still has line boundaries to work with.
		return strings.Split(s, "\n")
	}
	srcLines := strings.Split(s, "\n")
	out := make([]string, 0, len(srcLines))
	for _, line := range srcLines {
		if line == "" {
			out = append(out, "")
			continue
		}
		var cur strings.Builder
		curW := 0
		for _, r := range line {
			rw := lipgloss.Width(string(r))
			if rw == 0 {
				rw = 1
			}
			if curW+rw > width && cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
				curW = 0
			}
			cur.WriteRune(r)
			curW += rw
		}
		if cur.Len() > 0 {
			out = append(out, cur.String())
		}
	}
	return out
}
