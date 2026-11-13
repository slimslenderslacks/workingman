// Package daemon is the orch control loop: it watches the orch roots, drives
// each project and task through its state machine, and launches the agent
// sessions that do the work.
//
// Role: daemon
//
// A [Daemon] watches every root recursively with fsnotify and reacts to four
// kinds of file: project.yaml (route on the project's status), project.md (a
// seed for a new project), intake/*.md (a request for the planner) and
// tasks/*.yaml (audit only). Task lifecycle is driven off session-end
// callbacks rather than task-file events, so an agent that writes
// status:success and exits at once cannot race the session tracker. Events
// run on their own goroutines, chained per directory so one project's events
// stay in order while unrelated projects proceed in parallel.
//
// The transitions themselves are documented in state-machine.md and the
// eight agent kinds (project, planning, task, commit, wolf, archive, review,
// workingman) in agents.md; this comment is a map of where each lives.
//
// Each project has one shared session slot, keyed by its project.yaml path,
// used in turn by its project, task, commit and review agents. Planning,
// wolf and archive agents run in slots of their own ("<path>#planning",
// "#wolf", "#archive"), and the workingman agent, which belongs to no
// project, has a fixed key. The daemon writes project.yaml and task files
// itself (status transitions) and ignores the fsnotify events its own writes
// cause.
//
// Run as a launchd service (see `orch service install`) the daemon lives in the
// background and gates itself on AC power; see the README.
//
// Without [WithRunner] the daemon only observes: it audit-logs what it would
// dispatch but launches nothing.
//
// # Files
//
//   - daemon.go: [Daemon], [New], the [Option]s, [Daemon.Run], the per-directory
//     event chains and the session tracker that runs each session's end
//     callback.
//   - watcher.go: recursive watch setup, routing by file name, and the
//     startup scan that re-evaluates every project.yaml already on disk.
//   - dispatch.go: handleProject and dispatchProject, the per-status router
//     (unpopulated → project agent, ready → planning, working → next ready
//     task, cleanup → archive), plus the planning and project agents, their
//     circuit breakers and backoff, and cron registration.
//   - dispatch_lifecycle.go: what happens when a session ends. Task results
//     lead to commit, retry or block; commit results lead to the next task or
//     project completion. Also the idle/blocked transitions, the wolf agent
//     and the archive (cleanup) agent.
//   - cron_missed.go: sleep-aware cron. Persists when each schedule last
//     fired ([WithCronState]), counts ticks lost to daemon downtime or host
//     sleep into one [MissedSchedule] per project, and never fires them itself.
//   - sleepy_head.go: when schedules were missed, opens the interactive
//     sleepy-head agent (an ACP session in its own sandbox) once at startup to
//     report them; `orch catchup` drops a
//     marker that [RequestCatchup] writes and the daemon turns into exactly one
//     cron firing per approved project.
//   - power_gate.go: with `--require-ac`, pauses dispatch while on battery
//     (running sessions are left alone) and records cron firings as missed.
//   - intake.go: intake/*.md requests, which get a planner onto new work
//     whatever the project is doing.
//   - repo_sync.go: before each planning run, clones repos added since the
//     last plan into the existing workspace and summarises what changed for
//     the planning prompt.
//   - review.go: the PR-resolution loop. A finished project that has a PR
//     becomes idle and watched; an adaptive #review poll compares PR
//     fingerprints and runs the review agent on change, with fix-cycle and
//     crash limits; merged PRs move the project to merged.
//   - review_gateway.go: the daemon's own github-only MCP gateway
//     ([WithReviewGateway]) for the PR fingerprint check: minted at Run (a
//     stale one a crashed run recorded is deleted first), retried with
//     backoff on failure, deleted at shutdown.
//   - review_mcp.go: the PR fingerprint probe — github MCP pull_request_read
//     calls on one lazily opened MCP session over that gateway, recreated
//     with backoff when lost and closed at shutdown before the gateway.
//   - stop.go: status:stopped kills every session of the project and drops
//     its schedules.
//   - reaper.go: the stranded-session reaper, which closes sessions whose
//     agent finished or went idle but whose acp-wrapper never exited.
//   - reconcile.go: at startup, adopts ACP sessions left on disk by a
//     previous orch process so they are not dispatched twice.
//   - interactive.go: [Daemon.OpenInteractiveShell] and
//     [Daemon.OpenInteractiveSession], which back the TUI's :dir and :session
//     commands.
//   - workingman_agent.go: supervises the always-on workingman agent
//     ([WithWorkingmanAgent]), restarting it with backoff.
//   - channels_notify.go: wolf start and end messages on the `wolf` channel
//     topic ([WithChannels]), with message IDs recorded so replies can find
//     the wolf again.
//   - router_support.go: the read-only views the inbound-message router
//     needs: [Daemon.WolfSessions], [Daemon.StatusSummary].
//   - sessions.go: [Daemon.ListSessions] and [Daemon.WatchSessions], the live
//     session feed for the TUI's sessions pane.
//   - snapshot.go, snapshot_live.go, snapshot_text.go: the runtime-state
//     snapshot. snapshot.go has the JSON model, the project/task collector
//     and the offline builder; snapshot_live.go publishes it to --state-file;
//     snapshot_text.go renders it and reads it back. See
//     docs/state-snapshot.md.
//
// # Key types
//
//   - [Daemon] — the control loop. Built by [New] from the roots, an audit
//     logger and [Option]s, then run with [Daemon.Run]. cmd/orch owns the
//     only instance.
//   - [Option] — functional options for [New]: [WithRunner], [WithScheduler],
//     [WithNotifier], [WithChannels], [WithStateFile], [WithRuntimeInfo],
//     [WithWorkingmanAgent], [WithSessionIdleTimeout].
//   - [ChannelSender] — interface for sending to a channel topic;
//     *channels.Registry implements it. Given to [WithChannels].
//   - [SessionInfo] — one live session as the TUI's sessions pane shows it;
//     cmd/orch turns [Daemon.WatchSessions] output into tui.SessionView.
//   - [WolfSession], [WorkingmanAgentSession], [SleepyHeadSession] — live
//     sessions the router can
//     attach to; cmd/orch adapts them to router.Session.
//   - [Snapshot] — the JSON state document ([ProjectSnapshot],
//     [SessionSnapshot], [AuditEvent], ...) behind --state-file and
//     `orch status`. Built live by [Daemon.Snapshot], from disk alone by
//     [BuildOfflineSnapshot], and read back by [ReadSnapshot].
//
// # Collaborators
//
// Imported only by cmd/orch, which builds the Daemon, runs it next to the
// TUI (or headless), feeds [Daemon.WatchSessions] to internal/tui, adapts
// the daemon to router.Source for internal/router, and reads snapshots for
// `orch status`. The TUI does not import this package; the two meet only in
// cmd/orch.
//
// It builds on [github.com/slimslenderslacks/work/internal/runner] to launch
// sessions and [github.com/slimslenderslacks/work/internal/agent] for agent
// kinds. It reads and writes project and task files through
// [github.com/slimslenderslacks/work/internal/project] and
// [github.com/slimslenderslacks/work/internal/task], and orders tasks with
// [github.com/slimslenderslacks/work/internal/taskgraph]. Cron and review
// polls go through [github.com/slimslenderslacks/work/internal/scheduler],
// on-disk ACP session records through
// [github.com/slimslenderslacks/work/internal/session], and workspaces
// through [github.com/slimslenderslacks/work/internal/workspace]. It also
// uses internal/audit (event log), internal/notify and internal/channels
// (wolf alerts), internal/policy (sandbox rules), internal/sbx (sandbox
// liveness), internal/gitsign (push identity) and internal/terminal (host
// tabs).
package daemon
