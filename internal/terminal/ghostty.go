package terminal

import (
	"context"
	"fmt"
	"runtime"
	"strings"
)

// Ghostty opens tabs in Ghostty. On macOS it drives the app's AppleScript
// dictionary through osascript (`new tab with configuration`, `name override`,
// `select tab`); elsewhere it uses the CLI's `ghostty +new-tab` action, which
// talks to the running instance over D-Bus (GTK builds only). The Linux CLI has
// no way to select an existing tab, so FocusTab is unsupported there.
type Ghostty struct {
	// GOOS overrides runtime.GOOS (tests).
	GOOS string
	// Osascript is the osascript binary (macOS). Defaults to "osascript".
	Osascript string
	// Binary is the ghostty binary (non-macOS). Defaults to "ghostty".
	Binary string
	// Run executes commands. Defaults to ExecRunner.
	Run Runner
}

func (g *Ghostty) Name() string { return "ghostty" }

func (g *Ghostty) goos() string {
	if g.GOOS != "" {
		return g.GOOS
	}
	return runtime.GOOS
}

func (g *Ghostty) OpenTab(ctx context.Context, tab Tab) (string, error) {
	name, args, err := g.openCommand(tab)
	if err != nil {
		return "", err
	}
	out, err := run(ctx, g.Run, name, args...)
	if err != nil {
		// osascript errors are multi-line; keep the TUI status line readable.
		return "", fmt.Errorf("ghostty: open tab %q: %s", tab.Title, strings.Join(strings.Fields(err.Error()), " "))
	}
	if g.goos() == "darwin" {
		return strings.TrimSpace(string(out)), nil
	}
	return "", nil
}

// openCommand builds the argv that opens tab: an osascript invocation on
// macOS, `ghostty +new-tab ... -e <command>` elsewhere.
func (g *Ghostty) openCommand(tab Tab) (string, []string, error) {
	if len(tab.Command) == 0 {
		return "", nil, fmt.Errorf("ghostty: tab %q has no command", tab.Title)
	}
	if g.goos() == "darwin" {
		return g.osascript(), []string{"-e", ghosttyOpenScript(tab)}, nil
	}
	bin := g.Binary
	if bin == "" {
		bin = "ghostty"
	}
	args := []string{"+new-tab"}
	if tab.Dir != "" {
		args = append(args, "--working-directory="+tab.Dir)
	}
	if tab.Title != "" {
		args = append(args, "--title="+tab.Title)
	}
	// -e must come last: everything after it is the command, verbatim.
	args = append(args, "-e")
	args = append(args, tab.Command...)
	return bin, args, nil
}

func (g *Ghostty) osascript() string {
	if g.Osascript != "" {
		return g.Osascript
	}
	return "osascript"
}

// ghosttyOpenScript is the AppleScript that opens tab in the frontmost Ghostty
// window and prints the new tab's stable id. The surface configuration's
// command is a shell command line (Ghostty runs it through the user's shell),
// hence the shellJoin. The title is set as a name override so the program
// running in the tab can't retitle it with an escape sequence.
func ghosttyOpenScript(tab Tab) string {
	var b strings.Builder
	b.WriteString("tell application \"Ghostty\"\n")
	b.WriteString("\tset cfg to new surface configuration\n")
	if tab.Dir != "" {
		fmt.Fprintf(&b, "\tset initial working directory of cfg to %s\n", appleScriptString(tab.Dir))
	}
	fmt.Fprintf(&b, "\tset command of cfg to %s\n", appleScriptString(shellJoin(tab.Command)))
	// With no window open there is nothing to add a tab to, so the first
	// "tab" is the new window's own.
	b.WriteString("\tif (count of windows) is 0 then\n")
	b.WriteString("\t\tset w to new window with configuration cfg\n")
	b.WriteString("\t\tset t to selected tab of w\n")
	b.WriteString("\telse\n")
	b.WriteString("\t\tset t to new tab with configuration cfg\n")
	b.WriteString("\tend if\n")
	if tab.Title != "" {
		fmt.Fprintf(&b, "\tset name override of t to %s\n", appleScriptString(tab.Title))
	}
	b.WriteString("\tactivate\n")
	b.WriteString("\treturn id of t\n")
	b.WriteString("end tell")
	return b.String()
}

func (g *Ghostty) FocusTab(ctx context.Context, ref TabRef) error {
	if g.goos() != "darwin" {
		return fmt.Errorf("ghostty on %s: %w", g.goos(), ErrFocusUnsupported)
	}
	out, err := run(ctx, g.Run, g.osascript(), "-e", ghosttyFocusScript(ref))
	if err != nil {
		return fmt.Errorf("ghostty: focus tab: %w", err)
	}
	if strings.TrimSpace(string(out)) != "ok" {
		return fmt.Errorf("ghostty: %w", ErrTabNotFound)
	}
	return nil
}

// ghosttyFocusScript selects the tab matching ref — by id when OpenTab
// reported one, by title otherwise — brings its window and Ghostty to the
// front, and prints "ok", or prints "missing" when no tab matches.
func ghosttyFocusScript(ref TabRef) string {
	match := "id of t is " + appleScriptString(ref.ID)
	if ref.ID == "" {
		match = "name of t is " + appleScriptString(ref.Title)
	}
	return strings.Join([]string{
		`tell application "Ghostty"`,
		"\trepeat with w in windows",
		"\t\trepeat with t in tabs of w",
		"\t\t\tif " + match + " then",
		"\t\t\t\tselect tab t",
		"\t\t\t\tactivate window w",
		"\t\t\t\tactivate",
		`				return "ok"`,
		"\t\t\tend if",
		"\t\tend repeat",
		"\tend repeat",
		`	return "missing"`,
		"end tell",
	}, "\n")
}
