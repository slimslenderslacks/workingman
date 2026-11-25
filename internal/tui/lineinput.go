package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// lineInput is a single-line text field: the ACP view's message line to a
// conversational tab and the inbox's reply line share it. It holds only the
// draft; the caller owns the typing mode, handles esc and its own navigation
// keys before handing the rest to update, and decides what a submit does.
type lineInput struct {
	draft string
}

// update applies one keystroke to the draft and reports whether it was enter,
// which the caller treats as submit (see value). Printable keys are text,
// backspace deletes a rune, ctrl+u clears the draft, and a paste's newlines
// are folded to spaces because the field is a single line. Alt-modified runes
// are ignored so a stray meta chord never lands in the text.
func (l *lineInput) update(msg tea.KeyMsg) (submit bool) {
	if msg.String() == "ctrl+u" {
		l.draft = ""
		return false
	}
	switch msg.Type {
	case tea.KeyEnter:
		return true
	case tea.KeyBackspace:
		if r := []rune(l.draft); len(r) > 0 {
			l.draft = string(r[:len(r)-1])
		}
	case tea.KeySpace:
		l.draft += " "
	case tea.KeyRunes:
		if msg.Alt {
			return false
		}
		l.draft += strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(string(msg.Runes))
	}
	return false
}

// value is the draft as it would be submitted: trimmed, so an all-space draft
// submits nothing.
func (l lineInput) value() string { return strings.TrimSpace(l.draft) }

// reset clears the draft, after a successful submit.
func (l *lineInput) reset() { l.draft = "" }

// view draws the field while typing: a prompt, the draft and a cursor. It is
// truncated to width so a long draft can't wrap onto a phantom row (see the
// altscreen overflow notes in renderACPView); when it overflows, the tail
// (where the cursor is) is what stays visible.
func (l lineInput) view(width int) string {
	line := "› " + l.draft
	if r := []rune(line); width > 2 && len(r) > width-1 {
		line = "…" + string(r[len(r)-(width-2):])
	}
	return line + "▏"
}
