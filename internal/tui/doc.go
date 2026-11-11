// Package tui is the orch terminal UI: a bubbletea program that shows live
// agent sessions, the work-stream (project) gallery and its tasks, and lets
// the user steer work streams by writing their project.yaml files.
//
// Role: tui
//
// [Run] is the entry point. cmd/orch calls it in integrated mode (`orch
// --root=...`, daemon running in the same process) and in standalone mode
// (`orch tui`, no daemon: no sessions feed, no ACP tabs, no interactive
// launcher). Run blocks until the user quits or ctx is cancelled.
//
// # Screens
//
// The body has three columns, each made of bordered panes that up/down and
// alt+hjkl move focus between:
//
//   - Sessions (left, toggled by "tl") — live agent sessions from the
//     daemon's feed. Enter or a click attaches: an ACP session opens its tab
//     in the ACP view, any other session focuses its terminal tab.
//   - Work streams and Tasks (center) — the project gallery as a grid of
//     cards (border colour shows archive-ready, live cron, PR watch, load
//     errors; hidden streams appear only with "tp"), and the selected
//     project's tasks, most recent first.
//   - Project detail (right, toggled by "tr", hidden by default) — a
//     dashboard of the selected project: status, badges, repos and task graph.
//
// Overlays sit on top of the columns: "z" zooms the focused pane, ":" opens
// the command picker (dir, session, wolf, orch, workspace, review, cleanup, archive, stop,
// start, hide, show), :archive asks for confirmation in a modal, and "a"
// opens the full-window ACP tab view, one tab per live ACP session with its
// transcript (prompts, streamed text, tool calls, plans). Conversational
// sessions such as the wolf accept typed messages there.
//
// # Files
//
//   - tui.go — the model, Update loop, key/mouse handling, layout and
//     rendering of the columns, and [Run].
//   - projects.go — [ProjectView] and [TaskView], [ScanProjects] and the
//     polling [WatchProjects], plus selection helpers.
//   - sessions.go — [SessionView] and sessions-pane selection.
//   - project_detail.go — the right-column dashboard.
//   - command_picker.go — the ":" menu and dispatch of each command.
//   - wolf.go, review.go, cleanup.go, stop.go, hide.go — the project-file
//     writes behind :wolf, :review, :cleanup, :stop/:start and :hide/:show.
//   - archive.go — :archive: removes the wsp workspace, then moves the
//     project directory to the sibling <root>.backup directory.
//   - interactive.go — :dir, :session, :orch and :workspace through [InteractiveLauncher].
//   - tab_attach.go — attaching to non-ACP sessions through [TabFocuser].
//   - acpwatch.go — [WatchACPSessions]: discovers ACP sessions on disk,
//     dials their sockets, sends the opening prompt and streams events.
//   - acptabs.go — the ACP tab view's pure state and rendering.
//   - filetime_*.go — file birth time per platform, for task ages.
//
// # Talking to the daemon
//
// The TUI holds no handle on the daemon. It uses four channels:
//
//   - Project files, read. [WatchProjects] re-scans the --root directories
//     for project.yaml and tasks/*.yaml every second and sends a snapshot
//     when something changed. Files that fail to parse are shown as error
//     cards rather than dropped.
//   - Project files, written. Every command except :archive, :dir,
//     :session, :orch and :workspace works by editing the selected project.yaml through
//     internal/project and letting the daemon's fsnotify watch react: :wolf
//     sets status blocked, :cleanup sets cleanup, :review sets review or
//     review_now, :stop/:start set status stopped and back. These are saved
//     as `agent`, because the daemon ignores its own `daemon` writes.
//     :hide/:show are saved as `daemon` on purpose, so that a display toggle
//     doesn't trigger dispatch.
//   - Sessions feed. The sessions pane reads a channel of [SessionView]
//     snapshots that cmd/orch adapts from the daemon's WatchSessions; nil in
//     standalone mode.
//   - ACP session directories and sockets. When given a sessions root
//     (default ~/.workingman/sessions), [WatchACPSessions] polls
//     <root>/<id>/session.json through internal/session and dials each
//     running session's <root>/<id>/agent.sock with internal/acpclient. The
//     TUI drives these sessions itself: it sends the opening prompt (once,
//     recorded in session.json's PromptCount, so a restarted TUI reconnects
//     and replays the transcript instead of prompting again), and it gives up
//     on hung handshakes and silent turns. Before trusting a socket it checks the
//     sandbox still exists (internal/sbx), and it removes the directories of
//     dead sessions.
//
// # Key types
//
//   - [Run] — starts the program. Takes the roots, the sessions feed, the ACP
//     sessions root, an [InteractiveLauncher] and a [TabFocuser].
//   - [ProjectView] — read-only snapshot of one project.yaml and its tasks;
//     what the gallery, Tasks pane and detail pane render. Built only by
//     [ScanProjects].
//   - [TaskView] — one task's snapshot; [TaskView.Recency] is the Tasks pane's
//     sort key.
//   - [SessionView] — one live agent session, adapted from the daemon's
//     SessionInfo by cmd/orch so this package doesn't import internal/daemon.
//   - [InteractiveLauncher] — interface for :dir, :session, :orch and :workspace; implemented in
//     cmd/orch over the daemon's OpenInteractiveShell/OpenInteractiveSession.
//   - [TabFocuser] — interface to focus a session's terminal tab; implemented
//     by terminal.TabLauncher, shared with the daemon by cmd/orch.
//   - [ScanProjects], [WatchProjects], [WatchACPSessions] — the data sources
//     behind the panes.
//
// # Collaborators
//
// Uses internal/project and internal/task to read and write work-stream
// files, internal/policy for task policy rules, internal/session,
// internal/acpclient and internal/sbx for ACP sessions, internal/terminal
// for tab focus, and internal/workspace (wsp) for :archive. Used only by
// cmd/orch. See docs/terminal-tabs.md and docs/state-snapshot.md;
// docs/tui-survey.md is the original design survey and is now partly out of
// date.
package tui
