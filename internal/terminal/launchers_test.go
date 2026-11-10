package terminal

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// recorder is a Runner that records each invocation and replies with out.
type recorder struct {
	calls [][]string
	out   []string // reply per call, in order; "" when exhausted
}

func (r *recorder) run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if len(r.out) >= len(r.calls) {
		return []byte(r.out[len(r.calls)-1]), nil
	}
	return nil, nil
}

var sampleTab = Tab{
	Title:   "widget",
	Dir:     "/ws/widget",
	Command: []string{"sbx", "exec", "-it", "session-widget", "bash", "-lc", `echo "it's"`},
}

func TestGhosttyMacOSOpenTab(t *testing.T) {
	rec := &recorder{out: []string{"tab-123\n"}}
	g := &Ghostty{GOOS: "darwin", Run: rec.run}
	id, err := g.OpenTab(context.Background(), sampleTab)
	if err != nil {
		t.Fatal(err)
	}
	if id != "tab-123" {
		t.Errorf("id = %q, want the id osascript printed", id)
	}
	if len(rec.calls) != 1 || rec.calls[0][0] != "osascript" || rec.calls[0][1] != "-e" {
		t.Fatalf("calls = %v, want one osascript -e", rec.calls)
	}
	script := rec.calls[0][2]
	for _, want := range []string{
		`tell application "Ghostty"`,
		`set initial working directory of cfg to "/ws/widget"`,
		// The command is shell-quoted, then AppleScript-escaped.
		`set command of cfg to "sbx exec -it session-widget bash -lc 'echo \"it'\\''s\"'"`,
		`set t to new tab with configuration cfg`,
		// No Ghostty window yet: open a window instead of failing.
		`if (count of windows) is 0 then`,
		`set w to new window with configuration cfg`,
		`set name override of t to "widget"`,
		`return id of t`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
}

func TestGhosttyLinuxOpenTab(t *testing.T) {
	rec := &recorder{}
	g := &Ghostty{GOOS: "linux", Run: rec.run}
	id, err := g.OpenTab(context.Background(), sampleTab)
	if err != nil {
		t.Fatal(err)
	}
	if id != "" {
		t.Errorf("id = %q, want none on linux", id)
	}
	want := []string{"ghostty", "+new-tab", "--working-directory=/ws/widget", "--title=widget", "-e",
		"sbx", "exec", "-it", "session-widget", "bash", "-lc", `echo "it's"`}
	if len(rec.calls) != 1 || !reflect.DeepEqual(rec.calls[0], want) {
		t.Errorf("argv = %q\nwant   %q", rec.calls, want)
	}
}

func TestGhosttyMacOSFocusTab(t *testing.T) {
	rec := &recorder{out: []string{"ok\n"}}
	g := &Ghostty{GOOS: "darwin", Run: rec.run}
	if err := g.FocusTab(context.Background(), TabRef{ID: "tab-123", Title: "widget"}); err != nil {
		t.Fatal(err)
	}
	script := rec.calls[0][2]
	for _, want := range []string{`if id of t is "tab-123" then`, "select tab t", "activate window w"} {
		if !strings.Contains(script, want) {
			t.Errorf("focus script missing %q:\n%s", want, script)
		}
	}

	// Without an id the tab is matched by title.
	rec = &recorder{out: []string{"ok"}}
	g.Run = rec.run
	_ = g.FocusTab(context.Background(), TabRef{Title: "widget"})
	if !strings.Contains(rec.calls[0][2], `if name of t is "widget" then`) {
		t.Errorf("title match missing:\n%s", rec.calls[0][2])
	}

	rec = &recorder{out: []string{"missing"}}
	g.Run = rec.run
	if err := g.FocusTab(context.Background(), TabRef{ID: "gone"}); !errors.Is(err, ErrTabNotFound) {
		t.Errorf("err = %v, want ErrTabNotFound", err)
	}
}

func TestGhosttyLinuxFocusUnsupported(t *testing.T) {
	rec := &recorder{}
	g := &Ghostty{GOOS: "linux", Run: rec.run}
	if err := g.FocusTab(context.Background(), TabRef{Title: "widget"}); !errors.Is(err, ErrFocusUnsupported) {
		t.Errorf("err = %v, want ErrFocusUnsupported", err)
	}
	if len(rec.calls) != 0 {
		t.Errorf("must not shell out: %v", rec.calls)
	}
}

func TestWezTermOpenAndFocus(t *testing.T) {
	rec := &recorder{out: []string{"42\n"}}
	w := &WezTerm{Run: rec.run}
	id, err := w.OpenTab(context.Background(), sampleTab)
	if err != nil || id != "42" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	want := [][]string{
		append([]string{"wezterm", "cli", "spawn", "--cwd", "/ws/widget", "--"}, sampleTab.Command...),
		{"wezterm", "cli", "set-tab-title", "--pane-id", "42", "widget"},
	}
	if !reflect.DeepEqual(rec.calls, want) {
		t.Errorf("calls = %q\nwant    %q", rec.calls, want)
	}

	rec = &recorder{}
	w.Run = rec.run
	if err := w.FocusTab(context.Background(), TabRef{ID: "42"}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"wezterm", "cli", "activate-pane", "--pane-id", "42"}; !reflect.DeepEqual(rec.calls[0], want) {
		t.Errorf("focus argv = %q", rec.calls[0])
	}
}

func TestKittyOpenAndFocus(t *testing.T) {
	rec := &recorder{out: []string{"7\n"}}
	k := &Kitty{Run: rec.run}
	id, err := k.OpenTab(context.Background(), sampleTab)
	if err != nil || id != "7" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	want := append([]string{"kitty", "@", "launch", "--type=tab", "--tab-title", "widget", "--cwd", "/ws/widget"}, sampleTab.Command...)
	if !reflect.DeepEqual(rec.calls[0], want) {
		t.Errorf("argv = %q\nwant   %q", rec.calls[0], want)
	}

	rec = &recorder{}
	k.Run = rec.run
	_ = k.FocusTab(context.Background(), TabRef{ID: "7"})
	_ = k.FocusTab(context.Background(), TabRef{Title: "a.b"})
	if rec.calls[0][4] != "id:7" || rec.calls[1][4] != `title:^a\.b$` {
		t.Errorf("focus matches = %q, %q", rec.calls[0][4], rec.calls[1][4])
	}
}

func TestOpenTabRequiresCommand(t *testing.T) {
	for _, l := range []Launcher{&Ghostty{GOOS: "darwin"}, &Ghostty{GOOS: "linux"}, &WezTerm{}, &Kitty{}} {
		if _, err := l.OpenTab(context.Background(), Tab{Title: "x"}); err == nil {
			t.Errorf("%s: expected an error for an empty command", l.Name())
		}
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"bash":        "bash",
		"/ws/a-b_c.d": "/ws/a-b_c.d",
		"":            "''",
		"two words":   "'two words'",
		"it's":        `'it'\''s'`,
		"$HOME":       "'$HOME'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
