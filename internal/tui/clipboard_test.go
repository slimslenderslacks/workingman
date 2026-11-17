package tui

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// captureClipboard redirects copyToClipboard's OSC 52 output into a buffer
// for the test's duration, with tmux/screen wrapping disabled.
func captureClipboard(t *testing.T) *bytes.Buffer {
	t.Helper()
	t.Setenv("TMUX", "")
	t.Setenv("STY", "")
	var buf bytes.Buffer
	prev := clipboardOut
	clipboardOut = &buf
	t.Cleanup(func() { clipboardOut = prev })
	return &buf
}

// clipboardText decodes the payload of the OSC 52 sequence in buf.
func clipboardText(t *testing.T, buf *bytes.Buffer) string {
	t.Helper()
	s := buf.String()
	const prefix = "\x1b]52;c;"
	if !strings.HasPrefix(s, prefix) {
		t.Fatalf("clipboard output %q is not an OSC 52 sequence", s)
	}
	s = strings.TrimSuffix(strings.TrimPrefix(s, prefix), "\x07")
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decode OSC 52 payload: %v", err)
	}
	return string(b)
}

// pressKeyCmd is pressKey that also returns the command the key produced.
func pressKeyCmd(t *testing.T, m model, key rune) (model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{key}})
	return next.(model), cmd
}

// TestMouseCaptureOffByDefault: the TUI starts in select mode so the
// terminal's native click-drag selection works without any modifier.
func TestMouseCaptureOffByDefault(t *testing.T) {
	m := newModel(nil, nil, &fakeAttacher{})
	if m.mouseCapture {
		t.Fatal("mouseCapture = true by default, want select mode")
	}
	if !strings.Contains(m.renderFooter(), "select mode") {
		t.Errorf("footer should show select mode; got: %s", m.renderFooter())
	}
}

func TestMKeyTogglesMouseCapture(t *testing.T) {
	m := newModel(nil, nil, &fakeAttacher{})
	m, cmd := pressKeyCmd(t, m, 'm')
	if !m.mouseCapture {
		t.Fatal("after m, mouseCapture = false, want true")
	}
	if cmd == nil {
		t.Fatal("enabling mouse mode should return a command")
	}
	if got, want := cmd(), tea.EnableMouseCellMotion(); got != want {
		t.Errorf("cmd() = %T, want %T", got, want)
	}
	m.statusMsg = ""
	if !strings.Contains(m.renderFooter(), "mouse mode") {
		t.Errorf("footer should show mouse mode; got: %s", m.renderFooter())
	}

	m, cmd = pressKeyCmd(t, m, 'm')
	if m.mouseCapture {
		t.Fatal("second m should turn mouse capture back off")
	}
	if got, want := cmd(), tea.DisableMouse(); got != want {
		t.Errorf("cmd() = %T, want %T", got, want)
	}
}

func TestMKeyTogglesMouseCaptureInACPView(t *testing.T) {
	m := newModel(nil, nil, &fakeAttacher{})
	m.showACP = true
	m, cmd := pressKeyCmd(t, m, 'm')
	if !m.mouseCapture || cmd == nil {
		t.Fatalf("m in the ACP view: mouseCapture = %v, cmd nil = %v", m.mouseCapture, cmd == nil)
	}
	if !m.showACP {
		t.Error("m should not leave the ACP view")
	}
}

func TestYCopiesSelectedWorkStreamDir(t *testing.T) {
	buf := captureClipboard(t)
	m, path := selectProject(t, t.TempDir(), "widget")
	m = pressKey(m, "y")
	want := strings.TrimSuffix(path, "/project.yaml")
	if got := clipboardText(t, buf); got != want {
		t.Errorf("copied %q, want %q", got, want)
	}
	if !strings.Contains(m.statusMsg, "copied") {
		t.Errorf("statusMsg = %q, want a copied confirmation", m.statusMsg)
	}
}

func TestYCopiesSelectedTaskPath(t *testing.T) {
	buf := captureClipboard(t)
	m := newModel(nil, nil, &fakeAttacher{})
	m.focus = paneTasks
	m.taskSel = "/orch/widget/tasks/build.yaml"
	pressKey(m, "y")
	if got := clipboardText(t, buf); got != m.taskSel {
		t.Errorf("copied %q, want %q", got, m.taskSel)
	}
}

func TestYWithNothingSelectedReports(t *testing.T) {
	buf := captureClipboard(t)
	m := newModel(nil, nil, &fakeAttacher{})
	m.focus = paneTasks
	m = pressKey(m, "y")
	if buf.Len() != 0 {
		t.Errorf("nothing selected should write nothing; got %q", buf.String())
	}
	if m.statusMsg != "nothing selected to copy" {
		t.Errorf("statusMsg = %q", m.statusMsg)
	}
}

func TestYInACPViewCopiesPlainTranscript(t *testing.T) {
	buf := captureClipboard(t)
	m := newModel(nil, nil, &fakeAttacher{})
	m.showACP = true
	m.acp.upsert("s1", "task-alpha")
	m.acp.addPrompt("s1", "build the widget")
	pressKey(m, "y")
	got := clipboardText(t, buf)
	if !strings.Contains(got, "build the widget") {
		t.Errorf("copied transcript %q is missing the prompt", got)
	}
	if strings.Contains(got, "\x1b[") {
		t.Errorf("copied transcript still carries ANSI styling: %q", got)
	}
}
