package tui

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// renderProjectYAML draws the optional right column ("tr"): a scrollable
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

	path := m.projSel
	if m.yamlSrc == yamlSourceTask {
		path = m.taskSel
	}

	var b strings.Builder

	contentRows := innerHeight
	if contentRows < 0 {
		contentRows = 0
	}

	body, isErr := projectYAMLBody(path)
	if !isErr {
		body = collapseYAMLBlocks(body, m.yamlExpanded)
	}
	lines := wrapDisplayWidth(body, innerWidth)

	// Derive the cursor line and scroll offset together so the highlighted
	// line stays within the viewport. An error/placeholder body has nothing to
	// navigate, so no cursor is shown; the cursor is only drawn while this pane
	// holds focus, so it reads as "the active line here".
	cursor, scroll := reconcileYAMLView(m.yamlCursor, m.yamlScroll, len(lines), contentRows)
	showCursor := !isErr && len(lines) > 0 && m.focus == paneProjectYAML

	end := scroll + contentRows
	if end > len(lines) {
		end = len(lines)
	}
	for i := scroll; i < end; i++ {
		line := lines[i]
		switch {
		case isErr:
			line = statusErrStyle.Render(line)
		case showCursor && i == cursor:
			// Width pads the highlight to a full-width bar so the cursor line
			// reads clearly even on short/blank YAML lines.
			line = yamlCursorStyle.Width(innerWidth).Render(line)
		}
		b.WriteString(line)
		if i < end-1 {
			b.WriteString("\n")
		}
	}

	return style.Render(clampLines(b.String(), innerHeight))
}

// yamlCursorStyle highlights the current line in the YAML viewer as a reversed
// bar. Applied only while the pane is focused (see renderProjectYAML).
var yamlCursorStyle = lipgloss.NewStyle().Reverse(true)

// yamlLines returns the wrapped display lines for the currently-selected YAML
// source (project when yamlSrc is project, else the selected task) at the given
// inner width, plus whether the body is an error/placeholder (in which case
// there's nothing to put a cursor on). Shared by the renderer and the key
// handler so cursor math matches exactly what's on screen.
func (m model) yamlLines(innerWidth int) ([]string, bool) {
	path := m.projSel
	if m.yamlSrc == yamlSourceTask {
		path = m.taskSel
	}
	body, isErr := projectYAMLBody(path)
	if !isErr {
		body = collapseYAMLBlocks(body, m.yamlExpanded)
	}
	return wrapDisplayWidth(body, innerWidth), isErr
}

// collapsibleYAMLKeys are the long free-text fields the metadata viewer folds
// down by default so the shorter, scannable fields around them stay on screen.
var collapsibleYAMLKeys = map[string]bool{"description": true, "summary": true}

// yamlCollapseLines is how many body lines of a collapsed block scalar stay
// visible before the "… +N more" marker.
const yamlCollapseLines = 3

// blockScalarRE matches the opening line of a YAML block scalar —
// `<key>: |`, `<key>: >`, and their chomping/indent variants (`|-`, `>+`, …) —
// capturing the leading indentation and the key name.
var blockScalarRE = regexp.MustCompile(`^(\s*)([A-Za-z0-9_]+):\s*[|>][-+0-9]*\s*$`)

// collapseYAMLBlocks shortens the multi-line block scalars of the collapsible
// keys (description, summary) to yamlCollapseLines lines, replacing the hidden
// remainder with a "… +N more (tt to expand)" marker. With expanded=true it
// returns body unchanged.
//
// Only block scalars (`key: |` / `key: >` and variants) are folded; a
// single-line value is already short and left alone. A block's extent is the
// run of following lines indented deeper than the key (blank lines included);
// trailing blank lines are treated as separators, kept, and not counted toward
// the limit. The marker is plain text on purpose: it flows through the same
// width-wrap and cursor math as every other line, and injecting ANSI styling
// here would corrupt that rune-based wrap.
func collapseYAMLBlocks(body string, expanded bool) string {
	if expanded {
		return body
	}
	lines := strings.Split(body, "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		m := blockScalarRE.FindStringSubmatch(lines[i])
		if m == nil || !collapsibleYAMLKeys[m[2]] {
			out = append(out, lines[i])
			continue
		}
		keyIndent := len(m[1])
		out = append(out, lines[i]) // the `key: |` line itself

		// Gather the block body: following lines indented deeper than the key.
		j := i + 1
		var block []string
		for ; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) != "" && lineIndent(lines[j]) <= keyIndent {
				break
			}
			block = append(block, lines[j])
		}
		i = j - 1 // resume after the block

		// Split off trailing blank separator lines so they don't count as content.
		end := len(block)
		for end > 0 && strings.TrimSpace(block[end-1]) == "" {
			end--
		}
		content, trailing := block[:end], block[end:]

		if len(content) > yamlCollapseLines {
			out = append(out, content[:yamlCollapseLines]...)
			indent := leadingWhitespace(content[0])
			out = append(out, fmt.Sprintf("%s… +%d more (tt to expand)", indent, len(content)-yamlCollapseLines))
		} else {
			out = append(out, content...)
		}
		out = append(out, trailing...)
	}
	return strings.Join(out, "\n")
}

// lineIndent counts the leading spaces of s (YAML indents with spaces).
func lineIndent(s string) int { return len(s) - len(strings.TrimLeft(s, " ")) }

// leadingWhitespace returns the leading-space prefix of s, so a marker can line
// up under the block body it stands in for.
func leadingWhitespace(s string) string { return s[:len(s)-len(strings.TrimLeft(s, " "))] }

// reconcileYAMLView clamps a cursor line index into [0, n-1] and derives the
// scroll offset (index of the first visible line) so the cursor sits within a
// contentRows-tall viewport. n is the total number of display lines. Returns
// (0, 0) for an empty body.
func reconcileYAMLView(cursor, scroll, n, contentRows int) (int, int) {
	if n == 0 {
		return 0, 0
	}
	if contentRows < 1 {
		contentRows = 1
	}
	if cursor < 0 {
		cursor = 0
	}
	if cursor > n-1 {
		cursor = n - 1
	}
	if cursor < scroll {
		scroll = cursor
	}
	if cursor >= scroll+contentRows {
		scroll = cursor - contentRows + 1
	}
	if maxScroll := n - contentRows; scroll > maxScroll {
		scroll = maxScroll
	}
	if scroll < 0 {
		scroll = 0
	}
	return cursor, scroll
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
