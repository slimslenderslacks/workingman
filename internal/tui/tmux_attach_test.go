package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// runCmd invokes a tea.Cmd to retrieve the message it produces, mirroring
// what bubbletea's event loop does. Returns nil if the Cmd is nil.
func runCmd(t *testing.T, c tea.Cmd) tea.Msg {
	t.Helper()
	if c == nil {
		return nil
	}
	return c()
}

func TestAttachReturnsErrorWhenTargetEmpty(t *testing.T) {
	a := newTmuxAttacher()
	msg := runCmd(t, a.Attach(""))
	res, ok := msg.(attachResultMsg)
	if !ok {
		t.Fatalf("got %T, want attachResultMsg", msg)
	}
	if res.err == nil {
		t.Fatal("expected error for empty target")
	}
}

func TestAttachReturnsErrorWhenBinaryMissing(t *testing.T) {
	a := &defaultTmuxAttacher{
		binary:       "tmux",
		lookPath:     func(string) (string, error) { return "", errors.New("not found") },
		exists:       func(string, string) bool { return true },
		switchClient: func(string, string) bool { return false },
		openInTerm:   func(string, string) error { return nil },
	}
	msg := runCmd(t, a.Attach("orch-task-alpha"))
	res := msg.(attachResultMsg)
	if res.err == nil || !strings.Contains(res.err.Error(), "tmux not found") {
		t.Errorf("err = %v, want 'tmux not found'", res.err)
	}
}

func TestAttachReturnsErrorWhenSessionDead(t *testing.T) {
	a := &defaultTmuxAttacher{
		binary:       "tmux",
		lookPath:     func(string) (string, error) { return "/usr/bin/tmux", nil },
		exists:       func(string, string) bool { return false },
		switchClient: func(string, string) bool { return false },
		openInTerm:   func(string, string) error { return nil },
	}
	msg := runCmd(t, a.Attach("orch-task-ghost"))
	res := msg.(attachResultMsg)
	if res.err == nil || !strings.Contains(res.err.Error(), "not alive") {
		t.Errorf("err = %v, want 'not alive' error", res.err)
	}
	if res.target != "orch-task-ghost" {
		t.Errorf("target = %q", res.target)
	}
}

func TestAttachPrefersExistingClientOverNewWindow(t *testing.T) {
	var switched, opened []string
	a := &defaultTmuxAttacher{
		binary:   "tmux",
		lookPath: func(string) (string, error) { return "/usr/bin/tmux", nil },
		exists:   func(string, string) bool { return true },
		switchClient: func(_, target string) bool {
			switched = append(switched, target)
			return true
		},
		openInTerm: func(_, target string) error {
			opened = append(opened, target)
			return nil
		},
	}
	msg := runCmd(t, a.Attach("orch-task-alpha"))
	res := msg.(attachResultMsg)
	if res.err != nil {
		t.Errorf("unexpected err: %v", res.err)
	}
	if len(switched) != 1 || switched[0] != "orch-task-alpha" {
		t.Errorf("switchClient called with %v, want [orch-task-alpha]", switched)
	}
	if len(opened) != 0 {
		t.Errorf("openInTerm should not run when an existing client switched; got %v", opened)
	}
}

func TestAttachFallsBackToOpenTerminalWhenNoClient(t *testing.T) {
	var opened []string
	a := &defaultTmuxAttacher{
		binary:       "tmux",
		lookPath:     func(string) (string, error) { return "/usr/bin/tmux", nil },
		exists:       func(string, string) bool { return true },
		switchClient: func(string, string) bool { return false },
		openInTerm: func(_, target string) error {
			opened = append(opened, target)
			return nil
		},
	}
	msg := runCmd(t, a.Attach("orch-task-alpha"))
	res := msg.(attachResultMsg)
	if res.err != nil {
		t.Errorf("unexpected err: %v", res.err)
	}
	if len(opened) != 1 || opened[0] != "orch-task-alpha" {
		t.Errorf("openInTerm called with %v, want [orch-task-alpha]", opened)
	}
}

func TestAttachSurfaceErrorFromOpenInTerm(t *testing.T) {
	a := &defaultTmuxAttacher{
		binary:       "tmux",
		lookPath:     func(string) (string, error) { return "/usr/bin/tmux", nil },
		exists:       func(string, string) bool { return true },
		switchClient: func(string, string) bool { return false },
		openInTerm:   func(string, string) error { return errors.New("osascript exploded") },
	}
	msg := runCmd(t, a.Attach("orch-task-alpha"))
	res := msg.(attachResultMsg)
	if res.err == nil || !strings.Contains(res.err.Error(), "exploded") {
		t.Errorf("err = %v, want it to mention 'exploded'", res.err)
	}
}

func TestAppleScriptEscape(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"orch-task-tui", "orch-task-tui"},
		{`weird"name`, `weird\"name`},
		{`back\slash`, `back\\slash`},
	}
	for _, c := range cases {
		if got := applescriptEscape(c.in); got != c.want {
			t.Errorf("escape(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// fakeAttacher records the targets it was asked to attach to and returns
// pre-canned messages. It lets us drive the model through an attach flow
// without spawning real processes or touching bubbletea's exec layer.
type fakeAttacher struct {
	targets []string
	result  attachResultMsg
}

func (f *fakeAttacher) Attach(target string) tea.Cmd {
	f.targets = append(f.targets, target)
	res := f.result
	res.target = target
	return func() tea.Msg { return res }
}

func TestEnterOnSessionsPaneAttachesSelected(t *testing.T) {
	att := &fakeAttacher{}
	m := newModel(nil, make(<-chan []SessionView), nil, att)
	step, _ := m.Update(sessionsMsg{views: []SessionView{
		{ID: "a", AgentName: "task", Project: "alpha", TmuxTarget: "orch-task-alpha"},
		{ID: "b", AgentName: "task", Project: "bravo", TmuxTarget: "orch-task-bravo"},
	}})
	m = step.(model)
	// Move selection to b, then press Enter.
	step2, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = step2.(model)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	_ = runCmd(t, cmd)
	if len(att.targets) != 1 || att.targets[0] != "orch-task-bravo" {
		t.Errorf("targets = %v, want [orch-task-bravo]", att.targets)
	}
}

func TestEnterIgnoredWhenProjectsFocused(t *testing.T) {
	att := &fakeAttacher{}
	m := newModel(nil, make(<-chan []SessionView), nil, att)
	step, _ := m.Update(sessionsMsg{views: []SessionView{
		{ID: "a", TmuxTarget: "orch-task-alpha"},
	}})
	m = step.(model)
	step2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m = step2.(model)
	if m.focus != paneProjects {
		t.Fatalf("focus = %v, want projects", m.focus)
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	_ = runCmd(t, cmd)
	if len(att.targets) != 0 {
		t.Errorf("targets = %v, want none (projects focused)", att.targets)
	}
}

func TestAttachResultErrorSetsStatusMsg(t *testing.T) {
	att := &fakeAttacher{result: attachResultMsg{err: errors.New("boom")}}
	m := newModel(nil, make(<-chan []SessionView), nil, att)
	// Width/height are needed so View() renders the footer.
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	m = sized.(model)
	step, _ := m.Update(sessionsMsg{views: []SessionView{
		{ID: "a", TmuxTarget: "orch-task-alpha"},
	}})
	m = step.(model)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	resMsg := runCmd(t, cmd)
	final, _ := m.Update(resMsg)
	m = final.(model)
	if !strings.Contains(m.statusMsg, "boom") {
		t.Errorf("statusMsg = %q, want it to mention 'boom'", m.statusMsg)
	}
	if !strings.Contains(m.View(), "boom") {
		t.Errorf("View missing attach error; got:\n%s", m.View())
	}
}

func TestAttachResultSuccessClearsStatusMsg(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	m.statusMsg = "leftover"
	final, _ := m.Update(attachResultMsg{target: "x"})
	m = final.(model)
	if m.statusMsg != "" {
		t.Errorf("statusMsg = %q, want cleared", m.statusMsg)
	}
}

func TestMouseClickAttachesSessionRow(t *testing.T) {
	att := &fakeAttacher{}
	m := newModel(nil, make(<-chan []SessionView), nil, att)
	// Set window size first so paneWidths populates sessionsWidth.
	step, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step.(model)
	step2, _ := m.Update(sessionsMsg{views: []SessionView{
		{ID: "a", TmuxTarget: "orch-task-alpha"},
		{ID: "b", TmuxTarget: "orch-task-bravo"},
	}})
	m = step2.(model)
	// Click on row 1 (second session). sessionRowAtY: row 1 head is at y=7.
	final, cmd := m.Update(tea.MouseMsg{
		X:      5,
		Y:      7,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})
	m = final.(model)
	_ = runCmd(t, cmd)
	if len(att.targets) != 1 || att.targets[0] != "orch-task-bravo" {
		t.Errorf("targets = %v, want [orch-task-bravo]", att.targets)
	}
	if m.sessSel != "b" {
		t.Errorf("sessSel = %q, want %q", m.sessSel, "b")
	}
}

func TestMouseClickOutsideSessionsPaneIgnored(t *testing.T) {
	att := &fakeAttacher{}
	m := newModel(nil, make(<-chan []SessionView), nil, att)
	step, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step.(model)
	step2, _ := m.Update(sessionsMsg{views: []SessionView{
		{ID: "a", TmuxTarget: "orch-task-alpha"},
	}})
	m = step2.(model)
	// Click well to the right of the sessions pane.
	_, cmd := m.Update(tea.MouseMsg{
		X:      80,
		Y:      4,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})
	_ = runCmd(t, cmd)
	if len(att.targets) != 0 {
		t.Errorf("targets = %v, want none (click outside sessions pane)", att.targets)
	}
}

func TestSessionRowAtYHandlesSeparatorsAndOOB(t *testing.T) {
	// Header(1) + top border(1) + title+blank(2) = 4. Per-session: head=4+3*i,
	// status=4+3*i+1, separator=4+3*i+2.
	tests := []struct {
		name  string
		y     int
		count int
		want  int
	}{
		{"row 0 head", 4, 3, 0},
		{"row 0 status", 5, 3, 0},
		{"separator", 6, 3, -1},
		{"row 1 head", 7, 3, 1},
		{"row 2 head", 10, 3, 2},
		{"above pane", 0, 3, -1},
		{"past end", 13, 3, -1},
	}
	for _, tc := range tests {
		if got := sessionRowAtY(tc.y, tc.count); got != tc.want {
			t.Errorf("sessionRowAtY(%d, %d) = %d, want %d (%s)",
				tc.y, tc.count, got, tc.want, tc.name)
		}
	}
}

func TestPaneWidthsAtCommonSizes(t *testing.T) {
	if s, _ := paneWidths(0); s != 0 {
		t.Errorf("zero width: sessions = %d, want 0", s)
	}
	// Wide terminal: sessions clamped to 40.
	if s, _ := paneWidths(200); s != 40 {
		t.Errorf("paneWidths(200) sessions = %d, want 40", s)
	}
	// Narrow terminal: floor to 20 then trimmed by width-10.
	s, p := paneWidths(50)
	if s+p != 50 {
		t.Errorf("paneWidths(50) sessions+projects = %d, want 50", s+p)
	}
}
