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
		switchClient: func(string, string) bool { return true },
	}
	msg := runCmd(t, a.Attach("orch:task-alpha"))
	res := msg.(attachResultMsg)
	if res.err == nil || !strings.Contains(res.err.Error(), "tmux not found") {
		t.Errorf("err = %v, want 'tmux not found'", res.err)
	}
}

func TestAttachReturnsErrorWhenWindowDead(t *testing.T) {
	a := &defaultTmuxAttacher{
		binary:       "tmux",
		lookPath:     func(string) (string, error) { return "/usr/bin/tmux", nil },
		exists:       func(string, string) bool { return false },
		switchClient: func(string, string) bool { return true },
	}
	msg := runCmd(t, a.Attach("orch:task-ghost"))
	res := msg.(attachResultMsg)
	if res.err == nil || !strings.Contains(res.err.Error(), "not alive") {
		t.Errorf("err = %v, want 'not alive' error", res.err)
	}
	if res.target != "orch:task-ghost" {
		t.Errorf("target = %q", res.target)
	}
}

func TestAttachSwitchesAttachedClient(t *testing.T) {
	var switched []string
	a := &defaultTmuxAttacher{
		binary:   "tmux",
		lookPath: func(string) (string, error) { return "/usr/bin/tmux", nil },
		exists:   func(string, string) bool { return true },
		switchClient: func(_, target string) bool {
			switched = append(switched, target)
			return true
		},
	}
	msg := runCmd(t, a.Attach("orch:task-alpha"))
	res := msg.(attachResultMsg)
	if res.err != nil {
		t.Errorf("unexpected err: %v", res.err)
	}
	if len(switched) != 1 || switched[0] != "orch:task-alpha" {
		t.Errorf("switchClient called with %v, want [orch:task-alpha]", switched)
	}
}

func TestAttachWithoutClientReturnsHelpfulError(t *testing.T) {
	a := &defaultTmuxAttacher{
		binary:       "tmux",
		lookPath:     func(string) (string, error) { return "/usr/bin/tmux", nil },
		exists:       func(string, string) bool { return true },
		switchClient: func(string, string) bool { return false },
	}
	msg := runCmd(t, a.Attach("orch:task-alpha"))
	res := msg.(attachResultMsg)
	if res.err == nil {
		t.Fatalf("expected error explaining how to attach")
	}
	if !strings.Contains(res.err.Error(), "tmux attach -t orch") {
		t.Errorf("error should suggest `tmux attach -t orch`; got %v", res.err)
	}
}

func TestUmbrellaFromTargetExtractsSession(t *testing.T) {
	cases := []struct{ in, want string }{
		{"orch:task-alpha", "orch"},
		{"customumbrella:task-x", "customumbrella"},
		{"legacy-target", "orch"}, // falls back to default
	}
	for _, c := range cases {
		if got := umbrellaFromTarget(c.in); got != c.want {
			t.Errorf("umbrellaFromTarget(%q) = %q, want %q", c.in, got, c.want)
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
		{ID: "a", AgentName: "task", Project: "alpha", TmuxTarget: "orch:task-alpha"},
		{ID: "b", AgentName: "task", Project: "bravo", TmuxTarget: "orch:task-bravo"},
	}})
	m = step.(model)
	// Move selection to b (right moves selection within the sessions pane),
	// then press Enter.
	step2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = step2.(model)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	_ = runCmd(t, cmd)
	if len(att.targets) != 1 || att.targets[0] != "orch:task-bravo" {
		t.Errorf("targets = %v, want [orch:task-bravo]", att.targets)
	}
}

func TestEnterIgnoredWhenProjectsFocused(t *testing.T) {
	att := &fakeAttacher{}
	m := newModel(nil, make(<-chan []SessionView), nil, att)
	step, _ := m.Update(sessionsMsg{views: []SessionView{
		{ID: "a", TmuxTarget: "orch:task-alpha"},
	}})
	m = step.(model)
	m = focusProjectsPane(t, m)
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
		{ID: "a", TmuxTarget: "orch:task-alpha"},
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

func TestMouseClickAttachesSessionBox(t *testing.T) {
	att := &fakeAttacher{}
	m := newModel(nil, make(<-chan []SessionView), nil, att)
	step, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step.(model)
	step2, _ := m.Update(sessionsMsg{views: []SessionView{
		{ID: "a", TmuxTarget: "orch:task-alpha"},
		{ID: "b", TmuxTarget: "orch:task-bravo"},
	}})
	m = step2.(model)
	// Sessions is the left column, starting at x=0/y=0. Box 0 (first session)
	// occupies rows [1, 1+sessionBoxHeight); box 1 starts right after it.
	clickY := 1 + sessionBoxHeight
	final, cmd := m.Update(tea.MouseMsg{
		X:      5,
		Y:      clickY,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})
	m = final.(model)
	_ = runCmd(t, cmd)
	if len(att.targets) != 1 || att.targets[0] != "orch:task-bravo" {
		t.Errorf("targets = %v, want [orch:task-bravo]", att.targets)
	}
	if m.sessSel != "b" {
		t.Errorf("sessSel = %q, want %q", m.sessSel, "b")
	}
}

func TestMouseClickOutsideSessionsColumnDoesNotAttach(t *testing.T) {
	// Sessions is the left column; a click further right lands in the center
	// (projects/YAML) column and must not attach anything.
	att := &fakeAttacher{}
	m := newModel(nil, make(<-chan []SessionView), nil, att)
	step, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step.(model)
	step2, _ := m.Update(sessionsMsg{views: []SessionView{
		{ID: "a", TmuxTarget: "orch:task-alpha"},
	}})
	m = step2.(model)
	l := m.computeLayout()
	_, cmd := m.Update(tea.MouseMsg{
		X:      l.leftW + 5,
		Y:      4,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})
	_ = runCmd(t, cmd)
	if len(att.targets) != 0 {
		t.Errorf("targets = %v, want none (click outside sessions column)", att.targets)
	}
}

func TestSessionBoxAtYHandlesChromeAndOOB(t *testing.T) {
	// Inside the left column: top border(1), then one sessionBoxHeight-row
	// band per session box (each box draws its own border, so there's no
	// separate header chrome to skip, unlike the old table).
	tests := []struct {
		name  string
		y     int
		count int
		want  int
	}{
		{"box 0 start", 1, 3, 0},
		{"box 0 end", sessionBoxHeight, 3, 0},
		{"box 1 start", sessionBoxHeight + 1, 3, 1},
		{"top border", 0, 3, -1},
		{"above pane", -1, 3, -1},
		{"past end", 1 + 3*sessionBoxHeight, 3, -1},
		{"empty pane", 1, 0, -1},
	}
	for _, tc := range tests {
		if got := sessionBoxAtY(tc.y, 0, tc.count); got != tc.want {
			t.Errorf("sessionBoxAtY(%d, 0, %d) = %d, want %d (%s)",
				tc.y, tc.count, got, tc.want, tc.name)
		}
	}
}

func TestSessionBoxAtYRespectsPaneStartY(t *testing.T) {
	// Shifting paneStartY by 20 should shift every band by 20 — i.e. the
	// column's vertical position is fully encoded in the argument.
	if got := sessionBoxAtY(21, 20, 2); got != 0 {
		t.Errorf("sessionBoxAtY(21, 20, 2) = %d, want 0", got)
	}
	if got := sessionBoxAtY(21+sessionBoxHeight, 20, 2); got != 1 {
		t.Errorf("sessionBoxAtY(%d, 20, 2) = %d, want 1", 21+sessionBoxHeight, got)
	}
}
