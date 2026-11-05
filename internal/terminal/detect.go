package terminal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Supported lists the terminals orch can open tabs in, by settings name.
var Supported = []string{"ghostty", "wezterm", "kitty"}

// Settings is ~/.config/workingman/settings.yaml.
type Settings struct {
	// Terminal selects the terminal orch opens session tabs in when it cannot
	// detect the one it's running in: one of Supported.
	Terminal string `yaml:"terminal"`
}

// DefaultSettingsPath is ~/.config/workingman/settings.yaml, honouring
// XDG_CONFIG_HOME when set.
func DefaultSettingsPath(getenv func(string) string) (string, error) {
	if xdg := getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "workingman", "settings.yaml"), nil
	}
	home := getenv("HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("terminal: home dir: %w", err)
		}
		home = h
	}
	return filepath.Join(home, ".config", "workingman", "settings.yaml"), nil
}

// LoadSettings reads the settings file at path. A missing file is not an error:
// it yields zero Settings.
func LoadSettings(path string) (Settings, error) {
	var s Settings
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("parse %s: %w", path, err)
	}
	s.Terminal = strings.ToLower(strings.TrimSpace(s.Terminal))
	if s.Terminal != "" && ForName(s.Terminal) == nil {
		return s, fmt.Errorf("%s: unsupported terminal %q (supported: %s)", path, s.Terminal, strings.Join(Supported, ", "))
	}
	return s, nil
}

// Detect identifies the terminal emulator orch is running in from the
// environment it inherited, returning its settings name, or "" when it is not
// one orch knows. TERM_PROGRAM is the primary signal; terminal-specific
// variables back it up for when TERM_PROGRAM was not exported or was
// overwritten (e.g. by a multiplexer).
func Detect(getenv func(string) string) string {
	switch strings.ToLower(getenv("TERM_PROGRAM")) {
	case "ghostty":
		return "ghostty"
	case "wezterm":
		return "wezterm"
	case "kitty":
		return "kitty"
	}
	switch {
	case getenv("GHOSTTY_RESOURCES_DIR") != "", getenv("GHOSTTY_BIN_DIR") != "":
		return "ghostty"
	case getenv("WEZTERM_PANE") != "", getenv("WEZTERM_UNIX_SOCKET") != "":
		return "wezterm"
	case getenv("KITTY_WINDOW_ID") != "", getenv("KITTY_PID") != "":
		return "kitty"
	case getenv("TERM") == "xterm-ghostty":
		return "ghostty"
	case getenv("TERM") == "xterm-kitty":
		return "kitty"
	}
	return ""
}

// ForName returns the Launcher for a settings name, or nil if unsupported.
func ForName(name string) Launcher {
	switch name {
	case "ghostty":
		return &Ghostty{}
	case "wezterm":
		return &WezTerm{}
	case "kitty":
		return &Kitty{}
	}
	return nil
}

// Resolve picks the Launcher for session tabs: the detected terminal when Detect
// recognizes it, else the `terminal:` setting in the settings file (settingsPath,
// or DefaultSettingsPath when empty). With neither it returns an error telling
// the user how to configure one.
func Resolve(getenv func(string) string, settingsPath string) (Launcher, error) {
	if name := Detect(getenv); name != "" {
		return ForName(name), nil
	}
	if settingsPath == "" {
		p, err := DefaultSettingsPath(getenv)
		if err != nil {
			return nil, err
		}
		settingsPath = p
	}
	s, err := LoadSettings(settingsPath)
	if err != nil {
		return nil, fmt.Errorf("terminal settings: %w", err)
	}
	if s.Terminal != "" {
		return ForName(s.Terminal), nil
	}
	detected := getenv("TERM_PROGRAM")
	if detected == "" {
		detected = "unknown"
	}
	return nil, fmt.Errorf("don't know how to open a terminal tab here (TERM_PROGRAM=%s): run orch from %s, or set `terminal: ghostty` in %s",
		detected, strings.Join(Supported, "/"), settingsPath)
}
