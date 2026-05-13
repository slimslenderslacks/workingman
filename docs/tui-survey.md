# TUI Survey: Orch Daemon

Survey of the existing orch daemon to identify where a TUI subsystem can be
wired in. The planned TUI has a left-hand column listing active agent sessions
(clicking launches an external tmux session attached to that agent) and a main
panel showing a gallery of active projects with their state.

Module path: `github.com/slimslenderslacks/work` (`go.mod:1`)
Binary: `orch` (built via `go build -o orch ./cmd/orch`)

## 1. Daemon Entry Point

- Entry: `func main()` in `cmd/orch/main.go:35`.
- Signal handling: `signal.NotifyContext` for `SIGINT`/`SIGTERM` (`cmd/orch/main.go:80`).
- Main run loop: `d.Run(ctx)` (`cmd/orch/main.go:88`) drops into a `select` in
  `internal/daemon/daemon.go:83` that blocks on:
  - `ctx.Done()` — clean exit on signal.
  - `watcher.Events` — fsnotify events routed to dispatch handlers.
  - `watcher.Errors` — logged to audit; loop continues.
- Shutdown: `d.shutdown()` closes the fsnotify watcher, stops the scheduler
  (5s timeout), and closes all live sessions. Audit-logs `daemon_stop`.
- The daemon writes nothing to stdout/stderr — every meaningful event goes to
  the audit log (`--audit-log`, default `logs/audit.log`). The terminal is free
  for a TUI.

## 2. Agent Sessions

- Registry: `Daemon.sessions map[string]agent.Session` guarded by
  `Daemon.sessionsMu` (`internal/daemon/daemon.go:33-34`). Key is the absolute
  path of the `.project.yaml` driving the session.
- Interface: `agent.Session` (`internal/agent/agent.go:51-55`):
  - `Name() string`
  - `Wait(ctx context.Context) error`
  - `Close() error`
- Lifecycle:
  - Created and registered via `trackSession()` (`internal/daemon/daemon.go:138`).
  - One goroutine per session waits for exit, removes the entry, and invokes an
    optional `onEnd` callback (`internal/daemon/daemon.go:147`).
  - `hasSession(key)` (`internal/daemon/daemon.go:160`) deduplicates so the
    same project never has two concurrent agents.
- Agent kinds: `ProjectAgent`, `PlanningAgent`, `TaskAgent`, `WolfAgent`,
  `CommitAgent` (`internal/agent/agent.go:11-35`).
- **tmux is already integrated.** `agent.TmuxLauncher`
  (`internal/agent/tmux.go:15`) shells out to `tmux new-session -d -s <name>`
  (`internal/agent/tmux.go:53`). `TmuxSession.Wait()` polls `has-session` every
  200ms to detect exit (`internal/agent/tmux.go:39-44`). Session names come
  from `sessionName()` in `internal/agent/runner.go:152`, format
  `orch-<kind>-<branch-or-hash>`. The TUI can launch `tmux attach -t <name>`
  in an external terminal using the same `Name()`.

## 3. Projects & Project State

- Struct: `project.Project` (`internal/project/project.go:26-33`):
  - `Description string`
  - `Repos []Repo` (`{Org, Name}`)
  - `Branch string`
  - `Status Status`
  - `Cron string`
  - `UpdatedBy Writer` (`agent` | `daemon`; daemon ignores its own writes)
- Status values (`internal/project/status.go:5-12`):
  - `StatusReady` — needs planning.
  - `StatusWorking` — tasks in flight.
  - `StatusBlocked` — task/project failure; wolf agent kicked in.
  - `StatusDone` — terminal.
- There is **no in-memory project registry**. Projects are discovered on disk:
  the daemon watches each `--root` directory recursively with fsnotify
  (`internal/daemon/watcher.go:13`) and any `.project.yaml` found is loaded and
  routed through dispatch.
- Dispatch routing (`internal/daemon/dispatch.go:48-84`):
  - Empty project → `ProjectAgent`.
  - `ready` → `PlanningAgent`.
  - `working` → next task agent (`dispatchNextTask`).
  - `blocked` → `WolfAgent`.
  - `done` → no-op.

## 4. Proposed Integration Seam

A TUI naturally splits into two read-only views over daemon state. Suggested
package: `internal/tui/`.

**Sessions pane (left column).** Source of truth is `Daemon.sessions`.

- Add a mutex-safe accessor on `Daemon`, e.g.
  `ListSessions() []SessionInfo` returning `{Key, Name, Kind, StartedAt}` so
  the TUI never touches the map directly.
- Clicking an entry: shell out to `tmux attach -t <Name>` in an external
  terminal emulator. The daemon already owns the tmux session lifecycle, so
  the TUI is a pure viewer.

**Projects gallery (main panel).** Re-walk the watched roots (same logic as
`Daemon.addTree()` in `internal/daemon/watcher.go:13`) to discover
`.project.yaml` files, then `project.Load()` each one. Display `Description`,
`Branch`, `Status`, repo list, and (optionally) task counts from
`tasks/*.yaml`.

**Update strategy.** Two viable options:

- *Snapshot/poll* — TUI polls `Daemon.ListSessions()` and re-walks project
  files every 100–200ms. Simplest, no daemon changes beyond the accessor.
- *Event channel* — add an `events chan StateChange` to `Daemon`, emit on
  session start/end and on project file writes (the dispatch path already
  observes both). Decoupled, but requires touching every mutation site.

Recommendation: start with polling. Promote to events only if latency or CPU
becomes an issue.

**Conflicts.** None at the terminal: the daemon does not write to stdout or
stderr. The audit log is independent of the TUI and can remain as the durable
record. No existing logger needs to be redirected.

**Where the TUI runs.** The daemon currently runs in the foreground waiting on
`d.Run(ctx)`. Two placements are reasonable:

- *In-process* — start the TUI in a goroutine inside `cmd/orch/main.go` after
  `daemon.New(...)` and before `d.Run(ctx)`. The TUI gets a `*Daemon` for
  read-only queries. Daemon and TUI share lifetime and signal handling.
- *Separate binary* — a `cmd/orch-tui/` that opens the audit log and walks the
  roots itself. No daemon API surface needed, but loses the live session map.

In-process is the more direct fit for the described feature.

## 5. Build & Run

- Build: `go build -o orch ./cmd/orch`.
- Run:

  ```
  ./orch --root <dir> [--root <dir> ...] --audit-log <path> \
    [--workspace-manager {wsp|stub}]
  ```

- Flags:
  - `--root` (required, repeatable) — directories watched for `.project.yaml`.
  - `--audit-log` (default `logs/audit.log`).
  - `--workspace-manager` (default `wsp`): `wsp` real repos or `stub` for tests.
  - `--stub-workspace-root` (default `$TMPDIR/orch-workspaces`).
- Go toolchain: 1.26.2 (`go.mod:3`).
- Dependencies: `fsnotify`, `robfig/cron/v3`, `gopkg.in/yaml.v3`.
- External tools: `tmux`, `claude` (claude-code CLI), `wsp` (optional).
- No Makefile or justfile.

## 6. Library Choice: bubbletea

Picked **bubbletea** (`github.com/charmbracelet/bubbletea`), paired with
**lipgloss** (`github.com/charmbracelet/lipgloss`) for styling.

- Native Go — matches the daemon's runtime (`go.mod:1`), so the TUI can be
  embedded in-process without an FFI boundary or sidecar process.
- Elm-style `Model / Update / View` fits the read-only views described in §4:
  the sessions pane and projects gallery are both pure projections of daemon
  state, which maps cleanly onto `tea.Msg` ticks and re-renders.
- `tea.WithContext` integrates with the existing `signal.NotifyContext`-driven
  shutdown in `cmd/orch/main.go`, so the TUI shares the daemon's lifetime.
- Mature ecosystem (`bubbles` widgets, `lipgloss` styling) for the gallery and
  list rendering work that follows this task.

Alternatives considered: `tview` (immediate-mode, less idiomatic for the
projection-style updates we want), `gocui` (lower-level, more wiring per pane).

### Wiring

- `internal/tui/tui.go` hosts the bubbletea program. Current model renders a
  centered "hello, TUI" screen and quits on `q` / `esc` / `ctrl+c`.
- `cmd/orch/main.go` now dispatches on the first argument: `orch tui` runs the
  TUI, anything else falls through to the existing daemon flag parsing. The
  daemon invocation is unchanged.

### Run

```
go build -o orch ./cmd/orch
./orch tui
```

## 7. Notes from README.md / design.md

Both confirm the state machine `ready → working → {done | blocked}` and the
agent roster (project / planning / task / wolf / commit). `design.md` notes
that the daemon re-evaluates a project on every `.project.yaml` write (the
fsnotify path), task graphs live under `tasks/*.yaml`, and the per-task retry
budget is 3 before falling through to the wolf agent. None of these documents
mention a TUI today — this is greenfield.
