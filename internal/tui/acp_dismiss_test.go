package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/slimslenderslacks/work/internal/acpclient"
)

// TestACPDismissDeadTab: `x` in the ACP view drops a dead (failed-to-start)
// tab and removes its session directory, but refuses a live or still-starting
// one.
func TestACPDismissDeadTab(t *testing.T) {
	m := newModel(nil, nil, nil)
	m.acpCh = make(chan acpTabEvent) // non-nil so the view can open
	m.width, m.height = 100, 30
	var removed []string
	m.acpRemove = func(id string) error {
		removed = append(removed, id)
		return nil
	}
	step, _ := m.Update(acpTabEvent{kind: acpTabFailed, id: "commit-a", title: "commit-a", text: "push preflight FAILED"})
	m = step.(model)
	step, _ = m.Update(acpTabEvent{kind: acpTabAdded, id: "task-b", title: "task-b"})
	m = step.(model)
	step, _ = m.Update(acpTabEvent{kind: acpTabStarting, id: "task-c", title: "task-c"})
	m = step.(model)
	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if !m.showACP {
		t.Fatal("`a` did not open the ACP view")
	}
	x := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}

	// The live tab (connecting) is refused, with a visible reason.
	m.acp.selectID("task-b")
	m = key(m, x)
	if len(m.acp.tabs) != 3 || len(removed) != 0 {
		t.Fatalf("x removed a live tab: tabs=%d removed=%v", len(m.acp.tabs), removed)
	}
	if !strings.Contains(m.renderACPView(), "only dismisses dead sessions") {
		t.Error("refused x shows no reason")
	}

	// A still-starting placeholder is refused too.
	m.acp.selectID("task-c")
	m = key(m, x)
	if len(m.acp.tabs) != 3 {
		t.Fatalf("x removed a starting placeholder")
	}

	// The failed tab offers and accepts dismissal.
	m.acp.selectID("commit-a")
	m.acpInputErr = "" // a real tab switch is a keypress, which clears it
	if !strings.Contains(m.renderACPView(), "x: dismiss") {
		t.Error("dead tab's hint does not offer x: dismiss")
	}
	m = key(m, x)
	if m.acp.indexOf("commit-a") >= 0 || len(removed) != 1 || removed[0] != "commit-a" {
		t.Fatalf("dead tab not dismissed: tabs=%v removed=%v", m.acp.tabs, removed)
	}

	// A tab whose transport disconnected is dead as well.
	m.acp.apply("task-b", acpclient.Event{State: acpclient.StateDisconnected})
	m.acp.selectID("task-b")
	m = key(m, x)
	if m.acp.indexOf("task-b") >= 0 {
		t.Error("disconnected tab not dismissed")
	}
}
