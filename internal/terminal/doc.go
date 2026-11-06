// Package terminal opens sessions in new tabs of the user's terminal emulator
// (Ghostty, WezTerm or kitty) and tracks those tabs across orch restarts.
//
// Role: shared (daemon and TUI)
//
// Sessions that need a human at a real TTY each open in their own terminal tab,
// titled after their work stream: the --wolf-host wolf, the TUI's `:dir` shell
// and `:session` claude session, and every agent when orch runs without
// --acp-kit. ACP agents never get a tab.
//
// [Launcher] is the small interface the rest of orch uses ("open a tab titled X
// running Y", "focus tab X"). [Ghostty], [WezTerm] and [Kitty] implement it by
// shelling out (AppleScript, `wezterm cli`, `kitty @`) through a [Runner], so
// tests can capture the argv. [Resolve] picks the implementation: the terminal
// orch is running in, if [Detect] recognizes it; otherwise the `terminal:`
// setting in ~/.config/workingman/settings.yaml ([Settings]); otherwise an
// error that explains how to set it.
//
// [TabLauncher] adds session tracking on top. It adapts a Launcher to
// agent.Launcher and allows only one tab per session name: launching a session
// whose tab is still open focuses that tab instead. Tracking is file-based, in
// ~/.workingman/tabs/<name>.{json,pid}. The tab's command is wrapped so that it
// records its own pid, so tracking survives orch restarts and works the same
// way on every terminal.
//
// # Key types
//
//   - [Launcher] — one terminal emulator: Name, OpenTab, FocusTab.
//   - [Tab] and [TabRef] — what to open, and how to find an open tab again.
//   - [TabLauncher] — the agent.Launcher for tab sessions; [TabLauncher.Open]
//     also reports an already-open tab, and [TabLauncher.Focus] switches to a
//     session's tab by name.
//   - [TabSession] — the agent.Session for a command in a tab. Wait polls the
//     pid; Close sends SIGHUP.
//   - [AlreadyOpenError], [ErrFocusUnsupported], [ErrTabNotFound] — errors
//     the TUI turns into footer warnings.
//
// # Collaborators
//
// Uses internal/agent for Spec, Session and the Launcher interface it
// implements. cmd/orch creates one TabLauncher and gives it to internal/runner
// (as Runner.Launcher, for host-wolf and `:dir`/`:session` tabs) and to
// internal/tui, which uses TabLauncher.Focus to attach to a session's tab and
// turns ErrTabNotFound and ErrFocusUnsupported into footer warnings.
// internal/runner and internal/daemon pass AlreadyOpenError back to the TUI.
// cmd/orch also logs Detect's result at startup. See docs/terminal-tabs.md.
package terminal
