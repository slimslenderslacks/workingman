# Terminal tabs for host sessions

orch doesn't use tmux. Most agents run as ACP sessions (`--acp-kit`). You
watch those in the TUI's `a` tab view and never need a terminal of their own.
The sessions that do need a human at a real TTY each open in a **new tab of
the terminal orch runs in**:

| session | opened by | session name | tab title |
|---|---|---|---|
| host wolf (`--wolf-host`) | the daemon, when the project blocks | `wolf-<branch>` | project name |
| `:dir` shell (bash in the workspace, on the host) | the TUI command | `shell-<work-stream>` | project name |
| `:session` claude (in the `session-<work-stream>` sandbox) | the TUI command | `session-<work-stream>` | project name |
| `:orch` shell (bash in the work stream's orch directory) | the TUI command | `orch-<work-stream>` | project name |
| `:workspace` shell (bash at the root of the wsp workspace) | the TUI command | `workspace-<work-stream>` | project name |
| any agent, when orch runs **without** `--acp-kit` | the daemon | `<kind>-<task-or-branch>` | project name |

The tab title is always the project's (work stream's) name. The session name
is the key orch uses to track the tab.

## One tab per session

If a session's tab is already open, orch doesn't open a second one. Running
`:dir`, `:session`, `:orch` or `:workspace` again for the same work stream:

1. leaves the existing tab running,
2. switches to it (brings its window and the terminal to the front), and
3. shows a warning in the TUI footer:
   `⚠ session tab for <project> is already open — switched to it`.

   If the terminal can't switch tabs from the outside (Ghostty on Linux), the
   warning says to switch to it yourself instead.

The daemon side behaves the same way. If orch restarts while the host wolf's
tab is still open, the relaunched wolf adopts that tab instead of opening a
duplicate.

**Attaching from the sessions pane.** Enter or click on a session row:

- **ACP session:** opens the TUI's ACP tab view on that session, as before.
- **Session in a terminal tab:** switches to that tab. If the switch fails, the
  footer shows `attach <name>: …`. Typical reasons: the tab was closed, or the
  terminal can't switch tabs.

### How open tabs are tracked

`terminal.TabLauncher` (`internal/terminal/tabs.go`) keeps its tracking files
in `~/.workingman/tabs/`, so tracking survives an orch restart:

- `<session>.json` holds the title, the terminal, the terminal's tab id, and
  when the tab was opened. It's written as soon as the terminal accepts the
  tab.
- `<session>.pid` holds the pid of the command running in the tab. The command
  is wrapped in `sh -c 'echo $$ > <session>.pid; exec "$@"'`, so the recorded
  pid belongs to the command itself.

A session counts as open while that pid is alive. During the first minute
after a tab opens, before its command has recorded a pid, the `.json` alone
counts. Stale files are removed when they're noticed.

Tracking by pid rather than by asking the terminal works the same way on every
terminal, including ones whose scripting can't list tabs. Closing a tab sends
the command SIGHUP, so tracking follows the tab. The daemon stopping a session
(for example `:stop` on a host-wolf project) sends that same SIGHUP.

The tab's process is a child of the terminal, not of orch, so it wouldn't
inherit orch's environment. `PATH` (augmented at startup) and `SSH_AUTH_SOCK`
(the private signing agent from `start.sh`) are passed through explicitly
(`terminal.ForwardedEnv`).

## Which terminal

The terminal is resolved when a tab is first needed, not at startup.

1. **Detected.** orch detects the terminal it's running in from its
   environment. `TERM_PROGRAM` is checked first (`ghostty`, `WezTerm`,
   `kitty`). After that come terminal-specific variables:
   `GHOSTTY_RESOURCES_DIR`/`GHOSTTY_BIN_DIR`,
   `WEZTERM_PANE`/`WEZTERM_UNIX_SOCKET`, `KITTY_WINDOW_ID`/`KITTY_PID`, and
   `TERM=xterm-ghostty` / `xterm-kitty`. If orch is running in one of the
   supported terminals, that terminal is used, even if the settings file
   names a different one.
2. **Settings.** Otherwise orch reads `~/.config/workingman/settings.yaml`
   (or `$XDG_CONFIG_HOME/workingman/settings.yaml` when that's set):

   ```yaml
   # The terminal orch opens session tabs in when it can't tell which
   # terminal it's running in: ghostty, wezterm or kitty.
   terminal: ghostty
   ```

   A missing file is fine. An unsupported value or malformed YAML is an error.
3. **Neither.** The launch that needed the tab fails with an error, shown in
   the TUI footer for `:dir`/`:session`, or as `session_start_error` in the
   audit log for the wolf:

   ```
   don't know how to open a terminal tab here (TERM_PROGRAM=Apple_Terminal):
   run orch from ghostty/wezterm/kitty, or set `terminal: ghostty` in
   ~/.config/workingman/settings.yaml
   ```

The `daemon_start` audit line records the detected terminal as `terminal=…`.

## Per-terminal mechanics

### Ghostty on macOS

orch drives Ghostty through its AppleScript dictionary with `osascript -e …`.

To open a tab:

```applescript
tell application "Ghostty"
	set cfg to new surface configuration
	set initial working directory of cfg to "/path/to/workspace"
	set command of cfg to "sh -c '…' … env PATH=… claude --dangerously-skip-permissions …"
	set t to new tab with configuration cfg
	set name override of t to "my-project"
	activate
	return id of t
end tell
```

- Ghostty runs the `command` through the user's shell, so orch shell-quotes
  the argv first.
- The title is set as a *name override*, so the program in the tab can't
  retitle it with an escape sequence.
- orch keeps the returned stable tab id.

To focus a tab, orch walks `windows` → `tabs`, matching on `id` (or on `name`
when no id was recorded). Then it runs `select tab`, `activate window`, and
`activate`.

AppleScript needs to be enabled. It is by default; check that
`macos-applescript = false` isn't in your Ghostty config. macOS also asks once
to let your terminal control Ghostty (Privacy & Security → Automation).

### Ghostty on Linux and other platforms

orch runs the CLI action:

```
ghostty +new-tab --working-directory=<dir> --title=<project> -e <command…>
```

This needs a GTK build of Ghostty with D-Bus activation set up (see Ghostty's
`+new-tab --help`). The CLI has no way to select an existing tab, so focusing
is unsupported. The duplicate-tab warning still fires, telling you to switch
to the open tab yourself.

### WezTerm

- Open: `wezterm cli spawn --cwd <dir> -- <command…>`, then
  `wezterm cli set-tab-title --pane-id <id> <project>`.
- Focus: `wezterm cli activate-pane --pane-id <id>`.

### kitty

- Open: `kitty @ launch --type=tab --tab-title <project> --cwd <dir> <command…>`.
- Focus: `kitty @ focus-window --match id:<id>`.

This needs remote control enabled (`allow_remote_control yes`, or a
`listen_on` socket, in `kitty.conf`).

Adding another terminal means implementing `terminal.Launcher`
(`Name`/`OpenTab`/`FocusTab`) and registering it in `terminal.ForName` and
`terminal.Detect`.

## Code map

- `internal/terminal/terminal.go`: the `Launcher` interface, `Tab`/`TabRef`,
  the `ErrFocusUnsupported`/`ErrTabNotFound` sentinels, and quoting helpers.
- `internal/terminal/ghostty.go`, `others.go`: the per-terminal launchers.
- `internal/terminal/detect.go`: `Detect`, `Settings`/`LoadSettings`, and
  `Resolve`.
- `internal/terminal/tabs.go`: `TabLauncher` (the `agent.Launcher` the
  runner uses), `TabSession`, and `AlreadyOpenError`.
- `internal/runner/runner.go`: `Start` sets `agent.Spec.Title` to the project
  name. `LaunchInteractive` calls `TabLauncher.Open` and passes an
  `*AlreadyOpenError` back up.
- `internal/tui/interactive.go`: `handleInteractiveLaunched` shows the
  duplicate-tab warning.
- `internal/tui/tab_attach.go`: enter or click on a session row calls
  `TabLauncher.Focus`.

## Manual test plan

Build with `go build -o orch ./cmd/orch` and start orch from Ghostty
(`./start.sh`, or `./orch --root … --acp-kit …`).

1. **New tab.** Select a work stream and run `:dir`. Expect a new Ghostty tab
   titled with the work stream's name, running bash in its workspace. The
   footer reads `opened shell in a new tab → shell-<work-stream>`.
2. **Duplicate.** Switch back to orch's tab and run `:dir` again. Expect no new
   tab: Ghostty switches to the existing one, and orch's footer shows the
   `⚠ … already open — switched to it` warning.
3. **Reopen after close.** Close the shell tab (or `exit` in it), then run
   `:dir` again. Expect a fresh tab.
4. **`:session`.** Same as steps 1–3, with a claude session in the
   `session-<work-stream>` sandbox.
5. **Host wolf.** Run `./orch --root … --acp-kit … --wolf-host` and block a
   project (`status: blocked`). Expect a tab titled with the project name
   running claude. Enter on the wolf's row in the sessions pane switches to
   that tab. Close the tab, and the row disappears within a second.
6. **No terminal.** From Terminal.app with no settings file, `:dir` fails with
   the `don't know how to open a terminal tab here …` error in the footer.
   Add `terminal: ghostty` to `~/.config/workingman/settings.yaml` and retry:
   the tab opens in Ghostty.
