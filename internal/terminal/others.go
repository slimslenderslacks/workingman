package terminal

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// WezTerm opens tabs with `wezterm cli`, which talks to the running WezTerm GUI
// (WEZTERM_UNIX_SOCKET, set in every WezTerm pane). A tab is referenced by the
// pane id `spawn` prints.
type WezTerm struct {
	// Binary is the wezterm binary. Defaults to "wezterm".
	Binary string
	// Run executes commands. Defaults to ExecRunner.
	Run Runner
}

func (w *WezTerm) Name() string { return "wezterm" }

func (w *WezTerm) binary() string {
	if w.Binary != "" {
		return w.Binary
	}
	return "wezterm"
}

// spawnArgs builds `wezterm cli spawn [--cwd DIR] -- <command>`.
func (w *WezTerm) spawnArgs(tab Tab) []string {
	args := []string{"cli", "spawn"}
	if tab.Dir != "" {
		args = append(args, "--cwd", tab.Dir)
	}
	args = append(args, "--")
	return append(args, tab.Command...)
}

func (w *WezTerm) OpenTab(ctx context.Context, tab Tab) (string, error) {
	if len(tab.Command) == 0 {
		return "", fmt.Errorf("wezterm: tab %q has no command", tab.Title)
	}
	out, err := run(ctx, w.Run, w.binary(), w.spawnArgs(tab)...)
	if err != nil {
		return "", fmt.Errorf("wezterm: open tab %q: %w", tab.Title, err)
	}
	pane := strings.TrimSpace(string(out))
	if tab.Title != "" && pane != "" {
		if _, err := run(ctx, w.Run, w.binary(), "cli", "set-tab-title", "--pane-id", pane, tab.Title); err != nil {
			return pane, fmt.Errorf("wezterm: title tab %q: %w", tab.Title, err)
		}
	}
	return pane, nil
}

func (w *WezTerm) FocusTab(ctx context.Context, ref TabRef) error {
	if ref.ID == "" {
		return fmt.Errorf("wezterm: no pane id for tab %q: %w", ref.Title, ErrTabNotFound)
	}
	if _, err := run(ctx, w.Run, w.binary(), "cli", "activate-pane", "--pane-id", ref.ID); err != nil {
		return fmt.Errorf("wezterm: focus tab %q: %w", ref.Title, err)
	}
	return nil
}

// Kitty opens tabs with kitty's remote-control protocol (`kitty @`), which
// needs `allow_remote_control` (or a `listen_on` socket) in kitty.conf. A tab is
// referenced by the window id `launch` prints.
type Kitty struct {
	// Binary is the kitty binary. Defaults to "kitty".
	Binary string
	// Run executes commands. Defaults to ExecRunner.
	Run Runner
}

func (k *Kitty) Name() string { return "kitty" }

func (k *Kitty) binary() string {
	if k.Binary != "" {
		return k.Binary
	}
	return "kitty"
}

// launchArgs builds `kitty @ launch --type=tab [--tab-title T] [--cwd DIR] <command>`.
func (k *Kitty) launchArgs(tab Tab) []string {
	args := []string{"@", "launch", "--type=tab"}
	if tab.Title != "" {
		args = append(args, "--tab-title", tab.Title)
	}
	if tab.Dir != "" {
		args = append(args, "--cwd", tab.Dir)
	}
	return append(args, tab.Command...)
}

func (k *Kitty) OpenTab(ctx context.Context, tab Tab) (string, error) {
	if len(tab.Command) == 0 {
		return "", fmt.Errorf("kitty: tab %q has no command", tab.Title)
	}
	out, err := run(ctx, k.Run, k.binary(), k.launchArgs(tab)...)
	if err != nil {
		return "", fmt.Errorf("kitty: open tab %q (is allow_remote_control on?): %w", tab.Title, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (k *Kitty) FocusTab(ctx context.Context, ref TabRef) error {
	match := "id:" + ref.ID
	if ref.ID == "" {
		match = "title:^" + regexp.QuoteMeta(ref.Title) + "$"
	}
	if _, err := run(ctx, k.Run, k.binary(), "@", "focus-window", "--match", match); err != nil {
		return fmt.Errorf("kitty: focus tab %q: %w", ref.Title, err)
	}
	return nil
}
