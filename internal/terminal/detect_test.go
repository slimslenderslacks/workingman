package terminal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envOf(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func TestDetect(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"ghostty TERM_PROGRAM", map[string]string{"TERM_PROGRAM": "ghostty"}, "ghostty"},
		{"ghostty resources dir only", map[string]string{"GHOSTTY_RESOURCES_DIR": "/Applications/Ghostty.app/Contents/Resources/ghostty"}, "ghostty"},
		{"ghostty TERM only", map[string]string{"TERM": "xterm-ghostty"}, "ghostty"},
		{"wezterm TERM_PROGRAM", map[string]string{"TERM_PROGRAM": "WezTerm"}, "wezterm"},
		{"wezterm pane only", map[string]string{"WEZTERM_PANE": "3"}, "wezterm"},
		{"kitty window id", map[string]string{"KITTY_WINDOW_ID": "1"}, "kitty"},
		{"kitty TERM", map[string]string{"TERM": "xterm-kitty"}, "kitty"},
		// TERM_PROGRAM wins over a leftover variable from another terminal.
		{"TERM_PROGRAM wins", map[string]string{"TERM_PROGRAM": "ghostty", "KITTY_WINDOW_ID": "1"}, "ghostty"},
		{"Apple Terminal unsupported", map[string]string{"TERM_PROGRAM": "Apple_Terminal"}, ""},
		{"iTerm unsupported", map[string]string{"TERM_PROGRAM": "iTerm.app"}, ""},
		{"nothing", nil, ""},
	}
	for _, c := range cases {
		if got := Detect(envOf(c.env)); got != c.want {
			t.Errorf("%s: Detect = %q, want %q", c.name, got, c.want)
		}
	}
}

func writeSettings(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "settings.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadSettings(t *testing.T) {
	s, err := LoadSettings(writeSettings(t, "terminal: Ghostty\n"))
	if err != nil || s.Terminal != "ghostty" {
		t.Errorf("got %+v, %v; want terminal ghostty (normalized)", s, err)
	}

	s, err = LoadSettings(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil || s.Terminal != "" {
		t.Errorf("missing file: got %+v, %v; want zero settings, no error", s, err)
	}

	s, err = LoadSettings(writeSettings(t, "# nothing set\n"))
	if err != nil || s.Terminal != "" {
		t.Errorf("empty file: got %+v, %v", s, err)
	}

	if _, err := LoadSettings(writeSettings(t, "terminal: alacritty\n")); err == nil || !strings.Contains(err.Error(), "unsupported terminal") {
		t.Errorf("unsupported terminal: err = %v", err)
	}
	if _, err := LoadSettings(writeSettings(t, "terminal: [oops\n")); err == nil {
		t.Error("malformed yaml should error")
	}
}

func TestDefaultSettingsPath(t *testing.T) {
	got, err := DefaultSettingsPath(envOf(map[string]string{"HOME": "/home/u"}))
	if err != nil || got != "/home/u/.config/workingman/settings.yaml" {
		t.Errorf("got %q, %v", got, err)
	}
	got, _ = DefaultSettingsPath(envOf(map[string]string{"HOME": "/home/u", "XDG_CONFIG_HOME": "/xdg"}))
	if got != "/xdg/workingman/settings.yaml" {
		t.Errorf("XDG: got %q", got)
	}
}

func TestResolvePrefersDetectedTerminal(t *testing.T) {
	// The setting says ghostty, but we are running in kitty: use kitty.
	l, err := Resolve(envOf(map[string]string{"TERM_PROGRAM": "kitty"}), writeSettings(t, "terminal: ghostty\n"))
	if err != nil || l.Name() != "kitty" {
		t.Fatalf("got %v, %v; want kitty", l, err)
	}
}

func TestResolveFallsBackToSetting(t *testing.T) {
	l, err := Resolve(envOf(map[string]string{"TERM_PROGRAM": "Apple_Terminal"}), writeSettings(t, "terminal: ghostty\n"))
	if err != nil || l.Name() != "ghostty" {
		t.Fatalf("got %v, %v; want ghostty from settings", l, err)
	}
}

func TestResolveFailsClearlyWithoutDetectionOrSetting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.yaml")
	_, err := Resolve(envOf(map[string]string{"TERM_PROGRAM": "Apple_Terminal"}), path)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"Apple_Terminal", "terminal: ghostty", path} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestResolveSurfacesBadSettings(t *testing.T) {
	_, err := Resolve(envOf(nil), writeSettings(t, "terminal: xterm\n"))
	if err == nil || !strings.Contains(err.Error(), "unsupported terminal") {
		t.Errorf("err = %v", err)
	}
}
