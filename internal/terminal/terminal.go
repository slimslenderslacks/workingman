package terminal

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Tab describes a tab to open.
type Tab struct {
	// Title is the tab's title — the work stream's name.
	Title string
	// Dir is the working directory the command starts in.
	Dir string
	// Command is the argv run in the tab. The tab closes when it exits.
	Command []string
}

// TabRef identifies an open tab for FocusTab: ID is what OpenTab returned
// (empty for terminals that report none); Title is the fallback for them.
type TabRef struct {
	ID    string
	Title string
}

// Launcher opens and focuses tabs in one terminal emulator.
type Launcher interface {
	// Name is the terminal's settings name ("ghostty", "wezterm", "kitty").
	Name() string
	// OpenTab opens a new tab titled tab.Title running tab.Command and returns
	// the terminal's id for it ("" when the terminal reports none).
	OpenTab(ctx context.Context, tab Tab) (string, error)
	// FocusTab brings the referenced tab to the front. It returns an error
	// wrapping ErrFocusUnsupported on terminals that cannot switch tabs from
	// the outside, and ErrTabNotFound when the tab is no longer open.
	FocusTab(ctx context.Context, ref TabRef) error
}

var (
	// ErrFocusUnsupported is returned (wrapped) by FocusTab on terminals that
	// can open tabs but give no way to switch to one (Ghostty on Linux).
	ErrFocusUnsupported = errors.New("this terminal cannot switch tabs programmatically")
	// ErrTabNotFound is returned (wrapped) by FocusTab when no open tab
	// matches the reference.
	ErrTabNotFound = errors.New("tab not found")
)

// Runner runs an external command and returns its combined output. Every
// Launcher shells out through one so tests can capture the argv instead.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner is the production Runner.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func run(ctx context.Context, r Runner, name string, args ...string) ([]byte, error) {
	if r == nil {
		r = ExecRunner
	}
	out, err := r(ctx, name, args...)
	if err != nil {
		return out, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// shellQuote quotes s for a POSIX shell, leaving plainly safe words bare so
// the generated commands stay readable in logs.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_./=:,+@%", c)) {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}

// shellJoin renders argv as one POSIX shell command line.
func shellJoin(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = shellQuote(a)
	}
	return strings.Join(q, " ")
}

// appleScriptString renders s as a double-quoted AppleScript string literal.
func appleScriptString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
