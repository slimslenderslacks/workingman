package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// wolfModel returns a model with the ACP view open on a single interactive
// (persistent) tab, plus the messages the model "sent" through acpSend.
func wolfModel(t *testing.T) (model, *[]string) {
	t.Helper()
	m := newModel(nil, nil, nil, nil)
	m.acpCh = make(chan acpTabEvent) // non-nil so the view can open
	m.width, m.height = 100, 30
	var sent []string
	m.acpSend = func(id, text string) error {
		sent = append(sent, id+":"+text)
		return nil
	}
	step, _ := m.Update(acpTabEvent{kind: acpTabAdded, id: "wolf-a", title: "wolf-a", interactive: true})
	m = step.(model)
	step, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	m = step.(model)
	if !m.showACP {
		t.Fatal("`a` did not open the ACP view")
	}
	return m, &sent
}

func key(m model, msg tea.KeyMsg) model {
	step, _ := m.Update(msg)
	return step.(model)
}

func typeText(m model, s string) model {
	for _, r := range s {
		if r == ' ' {
			m = key(m, tea.KeyMsg{Type: tea.KeySpace})
			continue
		}
		m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return m
}

func TestWolfTabComposeAndSend(t *testing.T) {
	m, sent := wolfModel(t)

	if out := m.View(); !strings.Contains(out, "enter to message this session") {
		t.Errorf("interactive tab should invite a message; got:\n%s", out)
	}

	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.acpComposing {
		t.Fatal("enter on an interactive tab should start composing")
	}
	// While composing, command letters are text, not commands: q must not quit and
	// z must not toggle tool expansion.
	m = typeText(m, "quiz me")
	if m.acpToolsExpanded {
		t.Error("`z` toggled tool expansion while composing")
	}
	if got, _ := m.acp.selected(); got.draft != "quiz me" {
		t.Fatalf("draft = %q, want %q", got.draft, "quiz me")
	}
	if out := m.View(); !strings.Contains(out, "› quiz me") {
		t.Errorf("draft not rendered; got:\n%s", out)
	}

	// Backspace edits; enter sends and clears.
	m = key(m, tea.KeyMsg{Type: tea.KeyBackspace})
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(*sent) != 1 || (*sent)[0] != "wolf-a:quiz m" {
		t.Fatalf("sent = %v, want [wolf-a:quiz m]", *sent)
	}
	if got, _ := m.acp.selected(); got.draft != "" {
		t.Errorf("draft after send = %q, want empty", got.draft)
	}
	if !m.acpComposing {
		t.Error("composing should continue after a send so a chat can flow")
	}

	// An empty draft sends nothing.
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(*sent) != 1 {
		t.Errorf("empty enter sent a message: %v", *sent)
	}

	// esc stops composing without leaving the view; esc again leaves it.
	m = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.acpComposing || !m.showACP {
		t.Fatalf("first esc: composing=%v showACP=%v, want false/true", m.acpComposing, m.showACP)
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.showACP {
		t.Error("second esc should leave the ACP view")
	}
}

func TestWolfTabSendFailureKeepsDraftAndShowsError(t *testing.T) {
	m, _ := wolfModel(t)
	m.acpSend = func(string, string) error { return errors.New("session wolf-a is not accepting input") }
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m = typeText(m, "hello")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})

	if got, _ := m.acp.selected(); got.draft != "hello" {
		t.Errorf("a failed send must not lose the draft; got %q", got.draft)
	}
	if out := m.View(); !strings.Contains(out, "not accepting input") {
		t.Errorf("send error not shown; got:\n%s", out)
	}
	// The error clears on the next keystroke.
	m = typeText(m, "!")
	if m.acpInputErr != "" {
		t.Errorf("acpInputErr = %q after a keystroke, want cleared", m.acpInputErr)
	}
}

func TestWolfTabCtrlUClearsAndPasteIsSingleLine(t *testing.T) {
	m, _ := wolfModel(t)
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("line one\nline two"), Paste: true})
	if got, _ := m.acp.selected(); got.draft != "line one line two" {
		t.Errorf("pasted draft = %q, want newlines folded to spaces", got.draft)
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyCtrlU})
	if got, _ := m.acp.selected(); got.draft != "" {
		t.Errorf("ctrl+u left draft %q", got.draft)
	}
}

// A one-shot (non-interactive) tab is still read-only: enter does nothing and
// its single-letter commands keep working.
func TestOneShotTabIsNotTypeable(t *testing.T) {
	m := newModel(nil, nil, nil, nil)
	m.acpCh = make(chan acpTabEvent)
	m.width, m.height = 100, 30
	step, _ := m.Update(acpTabEvent{kind: acpTabAdded, id: "task-x", title: "task-x"})
	m = step.(model)
	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.acpComposing {
		t.Error("enter started composing on a one-shot tab")
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("z")})
	if !m.acpToolsExpanded {
		t.Error("z should still toggle tools on a one-shot tab")
	}
	if out := m.View(); strings.Contains(out, "message this session") {
		t.Errorf("one-shot tab shows a message line:\n%s", out)
	}
}

func TestWolfTabSwitchingStopsComposingAndKeepsDrafts(t *testing.T) {
	m, _ := wolfModel(t)
	step, _ := m.Update(acpTabEvent{kind: acpTabAdded, id: "wolf-b", title: "wolf-b", interactive: true})
	m = step.(model)

	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m = typeText(m, "for a")
	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}, Alt: true})
	if m.acpComposing {
		t.Error("switching tabs should stop composing")
	}
	if cur, _ := m.acp.selected(); cur.id != "wolf-b" {
		t.Fatalf("selected = %q, want wolf-b", cur.id)
	}
	if got := m.acp.tabs[0].draft; got != "for a" {
		t.Errorf("tab a's draft = %q, want it kept", got)
	}
	if got := m.acp.tabs[1].draft; got != "" {
		t.Errorf("tab b's draft = %q, want empty (drafts are per tab)", got)
	}
}

func TestWolfTabViewNeverExceedsHeight(t *testing.T) {
	m, _ := wolfModel(t)
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m = typeText(m, strings.Repeat("a very long draft ", 20))
	for _, h := range []int{6, 8, 10, 20, 34} {
		m.width, m.height = 60, h
		lines := strings.Split(m.renderACPView(), "\n")
		if len(lines) > h {
			t.Errorf("height=%d: %d lines, want <= %d", h, len(lines), h)
		}
		// Past the tiny-terminal clamp, keep the reserved slack row (see
		// renderACPView) even with the extra message line.
		if h >= 10 && len(lines) >= h {
			t.Errorf("height=%d: %d lines, want < %d (slack row)", h, len(lines), h)
		}
	}
}

// The wolf has no tmux window under ACP: selecting its row in the sessions pane
// opens the ACP view on its tab (where it can be watched and typed into) rather
// than trying `tmux attach` on an ACP session id.
func TestEnterOnACPSessionOpensItsTab(t *testing.T) {
	att := &fakeAttacher{}
	m := newModel(nil, make(<-chan []SessionView), nil, att)
	m.acpCh = make(chan acpTabEvent)
	m.width, m.height = 120, 30
	for _, id := range []string{"task-first", "wolf-alpha"} {
		step, _ := m.Update(acpTabEvent{kind: acpTabAdded, id: id, title: id, interactive: id == "wolf-alpha"})
		m = step.(model)
	}
	step, _ := m.Update(sessionsMsg{views: []SessionView{
		{ID: "/orch/alpha/.project.yaml#wolf", AgentName: "wolf", Project: "alpha", TmuxTarget: "wolf-alpha", Interactive: true, SandboxName: "alpha-wolf"},
	}})
	m = step.(model)
	m.focus = paneSessions
	m.sessSel = "/orch/alpha/.project.yaml#wolf"

	step, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = step.(model)
	if len(att.targets) != 0 {
		t.Errorf("ACP session was tmux-attached: %v", att.targets)
	}
	if !m.showACP {
		t.Fatal("enter on an ACP session should open the ACP view")
	}
	if cur, _ := m.acp.selected(); cur.id != "wolf-alpha" {
		t.Errorf("selected tab = %q, want wolf-alpha", cur.id)
	}
}

// The host/tmux wolf (--wolf-host) has no sandbox name, so it still attaches
// through tmux exactly as before.
func TestEnterOnHostWolfStillAttachesViaTmux(t *testing.T) {
	att := &fakeAttacher{}
	m := newModel(nil, make(<-chan []SessionView), nil, att)
	m.acpCh = make(chan acpTabEvent)
	step, _ := m.Update(sessionsMsg{views: []SessionView{
		{ID: "/orch/alpha/.project.yaml#wolf", AgentName: "wolf", Project: "alpha", TmuxTarget: "orch:wolf-alpha", Interactive: true},
	}})
	m = step.(model)
	m.focus = paneSessions
	m.sessSel = "/orch/alpha/.project.yaml#wolf"

	step, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = step.(model)
	if cmd == nil || len(att.targets) != 1 || att.targets[0] != "orch:wolf-alpha" {
		t.Errorf("host wolf should tmux-attach; targets=%v", att.targets)
	}
	if m.showACP {
		t.Error("host wolf must not open the ACP view")
	}
}
