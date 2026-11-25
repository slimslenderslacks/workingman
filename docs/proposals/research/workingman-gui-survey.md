# workingman today: a grounding survey for the GUI proposal

Status: research / reference. This document describes how workingman (`orch`)
works **today**, so that the GUI proposal (`docs/proposals/workingman-gui.md`)
can cite it. It proposes nothing. Every `path:line` reference was read against
the tree at `1b049b3`. Line numbers drift; the function names next to them are
the stable anchor.

Contents

1. [Process topology at a glance](#1-process-topology-at-a-glance)
2. [The daemon run loop](#2-the-daemon-run-loop)
3. [File-based control: which write triggers which action](#3-file-based-control-which-write-triggers-which-action)
4. [On-disk schemas: project.yaml, tasks/*.yaml, intake/](#4-on-disk-schemas-projectyaml-tasksyaml-intake)
5. [Worktrees, the commit queue and integration](#5-worktrees-the-commit-queue-and-integration)
6. [The state snapshot and how the TUI consumes state](#6-the-state-snapshot-and-how-the-tui-consumes-state)
7. [Agent roles](#7-agent-roles)
8. [The session model: ACP sessions and sandboxes](#8-the-session-model-acp-sessions-and-sandboxes)
9. [How a human joins a live session today](#9-how-a-human-joins-a-live-session-today)
10. [The MCP gateway](#10-the-mcp-gateway)
11. [Post-office messaging and channels](#11-post-office-messaging-and-channels)
12. [The TUI: layout, keys and `:` commands](#12-the-tui-layout-keys-and--commands)
13. [Audit log](#13-audit-log)
14. [PR handling and the review loop](#14-pr-handling-and-the-review-loop)
15. [Network listeners and sockets](#15-network-listeners-and-sockets)
16. [Binaries and `orch` subcommands](#16-binaries-and-orch-subcommands)
17. [Facts that matter most for a GUI](#17-facts-that-matter-most-for-a-gui)
18. [Where the docs and the code disagree](#18-where-the-docs-and-the-code-disagree)

---

## 1. Process topology at a glance

```
                      ┌──────────────── orch (one process) ─────────────────┐
  <root>/**  ──fsnotify──▶ daemon (internal/daemon)    TUI (internal/tui)   │
  project.yaml,        │   ├─ scheduler (cron, #review)  ├─ polls roots 1 s │
  tasks/, intake/      │   ├─ runner ─spawn─┐            ├─ polls sessions  │
                       │   ├─ snapshot publisher         │   dir 500 ms     │
                       │   └─ channels/router            └─ polls PO store  │
                       └────────────────────┼────────────────────┬──────────┘
                                            ▼                    │ dial unix
                       acp-wrapper (one per session) ◀───────────┘ agent.sock
                          └─ sbx sandbox ─ claude-acp-client (ACP over stdio)
                                  └─ MCP_URL ─▶ standalone-codemode sub-gateway (host :3333)
                                                     └─ post-office (127.0.0.1:7077) …
```

- `orch` with no subcommand runs the daemon and, unless `--headless`, the TUI
  **in the same process** (`cmd/orch/main.go:365-411`). `orch tui` runs the
  TUI alone, with no daemon (`cmd/orch/main.go:505-535`).
- The daemon and the TUI share **no RPC API**. The TUI reads the files under
  the roots, gets an in-process sessions channel from the daemon, dials ACP
  sockets directly, and reads the post-office store directly (§6.5).
- Each agent session is a separate host process (`acp-wrapper`) that owns one
  `sbx` sandbox and serves the agent's ACP stdio on a unix socket (§8).

## 2. The daemon run loop

**Construction.** `runDaemon` (`cmd/orch/main.go:110`) parses flags and calls
`daemon.New` (`main.go:342`). It always passes a runner, `scheduler.New()`
(robfig/cron v3), `cron_state.json` in the state dir (`main.go:280-281`) and
the parallel-task cap. `daemon.New` (`internal/daemon/daemon.go:335-412`)
creates one `fsnotify.Watcher` and refuses to start if the snapshot file or the
cron-state file lies inside a watched root (`daemon.go:391-406`;
`validateStateFile`, `snapshot_live.go:98-113`).

**Startup order** (`Daemon.Run`, `daemon.go:416-494`):

1. `gitPreflight` (`worktree.go:470`): refuse an old git when worktrees are on.
2. Power gate: `checkPower`, then `powerWatchLoop` every 30 s (`power_gate.go:31`).
3. `detectMissedAtStartup` (`cron_missed.go:316`), `scheduler.Start()`, then `sleepWatchLoop`.
4. For each root: `project.MigrateLegacy` (`.project.yaml` → `project.yaml`) and `addTree`.
5. `reconcileSessions` (`reconcile.go:47`): **one** pass that adopts still-running
   ACP sessions from `~/.workingman/sessions/*/session.json`. No periodic reconcile loop exists.
6. `beginWorkingmanAgent`, `beginStepUpAgent`, `startReviewGateway` (`daemon.go:458-462`).
7. `startupScan` (`watcher.go:63-100`, 8 projects at a time): `releaseOrphanedTasks`,
   `resumeOrphanedOutcomes`, `sweepWorktrees`, `revisitProject` for every `project.yaml`.
8. `launchSleepyHead`, `go reapLoop`, an immediate snapshot write plus `go snapshotLoop`.
9. The main `select` over `watcher.Events` / `watcher.Errors` (`daemon.go:474-493`).

**What is watched.** fsnotify only (no polling in the daemon). `addTree`
(`watcher.go:18-31`) adds every directory under each root except `.worktrees`
(`skipWatchDir`, `watcher.go:38-40`). A `Create` of a new directory runs
`maybeWatchNewDir` (`watcher.go:102-138`), which adds the subtree and runs
handlers for files already in it (so `mkdir` followed at once by a write is not
missed). Files are routed by name in `handlerFor` (`watcher.go:140-164`):

| Path | Handler | Effect |
|---|---|---|
| `*/project.yaml` | `handleProject` | route the project (§3) |
| `*/project.md` | `handleProjectSeed` | create `project.yaml` from the seed |
| `*/catchup-request` | `handleCatchup` | fire the project's cron once |
| `*/tasks/*.yaml` | `handleTask` | **audit only** (`task_file_updated`, `dispatch.go:803-825`) |
| `*/intake/*.md` | `handleIntakeFile` | queue a planning run |

Only `Write` and `Create` events reach a handler; `Remove` and `Rename` are
ignored (`dispatch.go:37-39`).

**Self-write filter.** `handleProject` drops any event whose file says
`updated_by: daemon` (`dispatch.go:67-69`). `project.Save` stamps `daemon`;
`project.SaveAs(..., WriterAgent)` stamps `agent` (`project/project.go:526-565`,
atomic temp+rename). So any external writer that wants the daemon to react must
write `updated_by: agent`. Internal re-evaluations use `revisitProject`
(`dispatch.go:78-88`), which bypasses the filter (session end, cron, startup).

**Ordering and debounce.** `dispatchEvent` (`daemon.go:505-532`) runs each
event on its own goroutine, chained per parent directory: events in one
directory run in order, different directories run in parallel. There is **no
time-based debounce** for file events; duplicate launches are prevented by state
checks (session map, `planningBusy` `dispatch.go:671-680`, `cleanupInFlight`
`dispatch_lifecycle.go:870-880`, per-project scheduling locks). The only debounce
is the snapshot publisher's (§6.3).

**Task outcomes come from session ends, not task-file events.**
`trackConfiguredSession` (`daemon.go:614-674`) waits on each session, removes it
from the session map and runs its `onEnd` callback (`afterTaskSession`,
`afterCommitSession`, …). ACP sessions are waited on with a background context,
so restarting `orch` detaches rather than kills them (`daemon.go:541-569, 585-590`).

**Other loops.**

| Loop | Where | Interval |
|---|---|---|
| Reaper (stranded sessions) | `reaper.go:54-77`, `reapStrandedSessions` `:85` | 30 s; idle 20 m; stage-complete grace 90 s |
| Worktree sweep | `sweepAllWorktrees`, `reaper.go:270` | 5 m |
| Cron and `#review` polls | `internal/scheduler/scheduler.go` (`Register` `:38`, `Unregister` `:64`, `Rearm` `:90`) | per spec; review ladder 2m→4m→8m→16m→32m→1h (`review.go:27`) |
| Sleep / clock-jump detection | `cron_missed.go:382-407` | 30 s; a gap > 60 s is a wake |
| Power | `power_gate.go:107` | 30 s |
| Adopted-session liveness | `reconcile.go:357-385` | 3 s |
| Step-Up request executor | `step_up_exec.go:285-305` | own fsnotify on `requests/` + 2 s poll |
| Snapshot publisher | `snapshot_live.go:139-163` | 2 s tick, 150 ms debounce, 15 s heartbeat |

## 3. File-based control: which write triggers which action

**Routing order** in `dispatchProject` (`dispatch.go:141-309`); first match wins:

1. On battery (with `--require-ac`) → defer (`:143-146`).
2. `status: stopped` → `enforceStopped` (`:153-156`).
3. Unpopulated (empty `status`) → project agent (`:157-166`).
4. First populated sighting → stamp `created_at` as daemon (`:175-185`).
5. `registerCronIfAny` (`:194`); an unbounded cron blocks the project.
6. `cleanup: true` → drain + archive agent (`:204-211`).
7. `idle`/`merged` with a pending seed task or intake file and no planner → set `ready` (`seed_replan`, `:225-237`).
8. Status switch (`:238-308`): `ready` → planning; `working` → planning too if
   there is un-offered intake, then `schedule`; `blocked` → wolf; `merged` →
   unregister `#review`; `idle` → pending tasks, `maybeTransitionMerged`, PR watch.

**Action → write → reaction.** "as agent" = `updated_by: agent`.

| Action | Who writes | Write | Daemon reaction |
|---|---|---|---|
| New project | human, script, workingman agent | `<root>/<name>/project.md` (non-empty) | `handleProjectSeed` (`dispatch.go:102-134`) writes `project.yaml` with only `description:` as agent (once; ignored if `project.yaml` exists) → unpopulated → **project agent** (`launchProjectRootAgent`, `:323-358`) fills it in and sets `status: ready` → planning. Legacy: an empty `project.yaml` does the same. |
| Queue intake | human, script, workingman agent (no TUI command) | `<project>/intake/<x>.md` | `handleIntakeFile` (`intake.go:37-98`): empty → `intake_empty`; planner in flight → `intake_queued deferred=planning_in_flight` (picked up by a follow-up run); `working` → `revisitProject`, a planner runs **alongside** running tasks; other statuses → `status: ready`, `blocked_reason: ""` as agent. The planner renames each file to `*.processed` (`planning.tmpl:143-147`). The files on disk are the only record. |
| Stop | TUI `:stop` (`tui/stop.go:23-44`) | `stopped_from: <status>`, `status: stopped`, as agent | `enforceStopped` (`daemon/stop.go:22-63`) closes every session keyed `<path>` or `<path>#*`, drops token waits, unregisters cron and `#review`. Running tasks go back to `ready` without spending an attempt (`dispatch_lifecycle.go:50-58`). |
| Start | TUI `:start` (`tui/stop.go:55-80`) | `status: <stopped_from or idle>`, `stopped_from: ""`, as agent | normal routing |
| Wolf | TUI `:wolf` (`tui/wolf.go:27-43`) | `status: blocked` (+ a manual `blocked_reason` if empty), as agent | `launchWolfAgent` (`dispatch_lifecycle.go:637`) under `#wolf`, deduped |
| Daemon block | daemon | `transitionProjectBlocked` (`dispatch_lifecycle.go:572-635`): `status: blocked` + reason (appends `; also: …` if already blocked) | notifies (osascript/channels), unregisters `#review`, launches the wolf |
| Review | TUI `:review` (`tui/review.go:37-60`), only on `idle` | `review: true` (not watching) or `review_now: true` (watching), as agent | `ensureReviewPoll` (`review.go:104`) / `forceReviewNow` (`review.go:215`, clears the flag as daemon) → review agent (§14) |
| Cleanup | TUI `:cleanup` (`tui/cleanup.go:38-55`); refused if `archive: true` | `cleanup: true`, as agent | archive agent once nothing is in flight (`launchArchiveAgent`, `dispatch_lifecycle.go:744`); `afterArchiveSession` (`:824-868`) clears `cleanup` as daemon either way; logs `archive_incomplete` if the agent did not set `archive: true` |
| Archive | TUI `:archive` (`tui/archive.go:37-157`), needs `archive: true` + y/n | none in the roots: the TUI itself removes task worktrees and `wm/` refs, the workspace `.orch`, runs `wsp rm --force`, then renames `<root>/<name>` → `<root>.backup/<name>` | none; the project leaves the watched tree. **Runs inline in the TUI, not in the daemon.** |
| Hide / show | TUI `:hide` / `:show` (`tui/hide.go:21-59`) | `hide: true/false`, **as daemon** on purpose | none (display filter only) |
| Retry a task | daemon, automatic | `resetForRetry` (`dispatch_lifecycle.go:110-151`): `attempts+1`, `status: ready`, `failure_reason: ""` | rescheduled. At `attempts >= 3` (`task.MaxAttempts`, `task.go:34`) the sandbox is kept and the project blocks. **There is no human "retry task" command.** Hand-editing a task to `ready` does nothing until the next project dispatch (a `project.yaml` write as agent, a session end, cron, restart). In practice the wolf does this. |
| Cron | `project.yaml` `cron` + `cron_until` and/or `cron_max_runs` | — | `registerCronIfAny` (`dispatch.go:850-888`); on fire `onCronFired` (`:919-946`) → `runCron` → `requestCronReplan` (`:1044-1072`): `idle`/`ready` → `status: ready, replan: true` (`cron_replan_requested`); `working`/`blocked` → re-evaluate only (`cron_replan_skipped`). |
| Catch-up | `orch catchup <project.yaml>` or sleepy-head | `<project>/catchup-request` (`sleepy_head.go:55-63`) | `handleCatchup` removes the marker (the claim) and fires `runCron` once if the project is recorded as behind (`sleepy_head.go:72-112`) |
| Add repos | edit `repos` / `new_repos` | `project.yaml` | next planning launch diffs against `last-planned.yaml` and runs `wsp repo add` (`repo_sync.go:32`) |

Daemon-owned `project.yaml` writes (all as daemon): `created_at`, `cron_runs`,
`replan` set/clear, `review_now` clear, `cleanup` clear, `status: idle`
(`dispatch_lifecycle.go:536`), `status: merged` (`review.go:683`), `status: blocked`.

## 4. On-disk schemas: project.yaml, tasks/*.yaml, intake/

### 4.1 Project directory layout

Each child directory of a `--root` is one work stream; its name is the project
name (`dispatch.go:94-95`).

```
<root>/<work-stream>/
  project.md               one-shot seed (dispatch.go:102-134)
  project.yaml             project.FileName (project/project.go:16)
  tasks/<name>.yaml        one per task
  intake/<req>.md          queued requests → <req>.processed once planned
  blocked-session.yaml     the wolf's durable diagnosis (project/blocked_session.go:40-42)
  last-planned.yaml        daemon snapshot of project fields at last planning (project/planned_snapshot.go:45-47)
  catchup-request          one-shot cron marker (daemon/sleepy_head.go:41)
  .orch/{context.yaml,instructions.md}   handoff files for control-dir agents
```

| Elsewhere | Path |
|---|---|
| Shared wsp workspace | `~/dev/workspaces/<branch>/` (`workspace/wsp.go:52-100`) |
| Task worktrees | `<shared>/.worktrees/<task>/<repo>` (`worktree/worktree.go:19-39`) |
| Archived streams | `<root>.backup/<name>` |
| Sessions | `~/.workingman/sessions/<id>/` (`session/session.go:170-176`) |
| Snapshot / cron state | `~/.workingman/state/{snapshot.json,cron_state.json}` |
| Audit log | `--audit-log`, default `logs/audit.log` relative to cwd (`cmd/orch/main.go:118`); `orch service install` uses `~/.workingman/logs/audit.log` (`cmd/orch/service.go:90,110`) |
| Gateway / harness records | `~/.workingman/{gateways,sandboxes}/<sandbox>.json` |
| Post-office | `~/.workingman/post-office/` or `$WORKINGMAN_POST_OFFICE_DIR` |
| Terminal tabs | `~/.workingman/tabs/<session>.{json,pid}` |

### 4.2 `project.yaml` (`Project`, `internal/project/project.go:96-301`)

| Key | Type | Meaning / owner |
|---|---|---|
| `description` | string | goal; from `project.md` or the project agent |
| `repos` | `[]Repo` | existing repos |
| `new_repos` | `[]Repo` | repos to `gh repo create` before provisioning (`wsp.go:232-253`) |
| `branch` | string | the project (shared) branch and wsp workspace name |
| `status` | Status | see below; empty = unpopulated |
| `cron`, `cron_until`, `cron_max_runs` | string, time, int | schedule and stop conditions (an unbounded cron blocks) |
| `cron_runs` | int | daemon-owned cycle counter |
| `blocked_reason` | string | set by daemon on block; cleared by planning/wolf |
| `cleanup` | bool | **request** for the archive agent (TUI sets, daemon clears) |
| `archive` | bool | **result**: archive agent succeeded; gates `:archive` |
| `replan` | bool | daemon sets on a cron cycle; planning re-plans instead of adding |
| `created_at` | time | daemon stamp |
| `review` | bool | completion is gated on a PR |
| `review_now` | bool | one-shot "poll the PR now" |
| `pull_requests` | `[]PullRequest` | `{repo, number, url, head_sha, state: open\|merged\|closed, fingerprint}` (`project.go:349-356`; `fingerprint.go`) |
| `hide` | bool | TUI-only display filter |
| `stopped_from` | Status | status before `:stop` |
| `max_parallel_tasks` | int | per-project concurrency, < 1 clamps to 1 (`MaxParallel`, `:313-318`) |
| `verify` | string | build check the commit agent runs after rebase |
| `conflict_resolution` | string | `rerun` (default) or `agent` (reserved) |
| `updated_by` | `agent` \| `daemon` | writer marker (§2) |

`Repo` (`project.go:40-88`): `org`, `name`, `base_branch`, `visibility`
(`private`/`public`, `new_repos` only); shorthand `"org/name[@base]"` accepted.

**Project status** (`project/status.go:11-109`): `""` (unpopulated), `ready`,
`working`, `blocked`, `idle`, `stopped`, `merged`. Legacy `done` and `reviewing`
load as `idle`; anything else fails to load. "Watching a PR" is derived
(`WatchingPR()`, `project.go:444-449`), not a status.

### 4.3 `tasks/*.yaml` (`Task`, `internal/task/task.go:170-321`)

| Key | Type | Written by |
|---|---|---|
| `name` | kebab-case, sandbox-name budgeted (`ValidNameFor`, `:76-88`) | planning |
| `description` | string | planning / human |
| `depends_on` | []string | planning / review |
| `status` | Status | various (below) |
| `attempts` | int (budget 3) | daemon |
| `failure_reason`, `blocked_reason` | string | task / commit agent |
| `summary`, `commits[{repo,hash}]`, `created_files` | | commit agent at `committed` |
| `completed_at`, `succeeded_at` | time | daemon (`succeeded_at` orders the commit queue) |
| `static_mcps` | []string | planning: MCP catalog servers for the sandbox gateway |
| `policies` | []`policy.Rule` | planning / review / Step-Up: `sbx policy` allow/deny rules |
| `save_sandbox` | bool | daemon on terminal failure, or human/planner |
| `model` | string, default `default` (`ModelDefault`, `:41`) | planning |
| `resolved_model` | string | acp-wrapper (`acpwrapper/record.go:145-152`) |
| `harness` | string, default `claude-acp` (`HarnessDefault`, `:47`) | planning |
| `resolved_harness` | string (the template image) | acp-wrapper |
| `source` | `{kind: pr-comment\|pr-check\|pr-push, ref, repo, pr}` | review agent |
| `touches` | *[]string (nil = no hint, `[]` = nothing) | planning / wolf |
| `areas` | []string | planning / wolf |
| `worktree`, `task_branch`, `base[{repo,hash}]` | | daemon (`daemon/worktree.go:128-130`) |
| `integration` | `{result: conflict\|verify_failed\|error\|partial, repo, files, onto, output, published, landed, attempts}` | commit agent + daemon |
| `touches_actual` | []string | daemon |

**Task status** (`task.go:21-28, 144-164`): `ready`, `running`, `success`,
`failed`, `blocked`, `committed`. Note `success` means *queued for
integration*, not done.

| Transition | By | Where |
|---|---|---|
| new → `ready` | planning / review agent | `planning.tmpl` |
| `ready` → `running` | daemon | `markTaskRunning`, `dispatch_lifecycle.go:318-337` |
| `running` → `ready` (no attempt spent) | daemon on launch failure / stop / lost session | `releaseRunningTask`, `:345-366` |
| `running` → `success`\|`failed`\|`blocked` | task agent | `task.tmpl:79-95` |
| `failed` → `ready` | daemon while attempts < 3 | `resetForRetry` |
| `success` → `committed` | commit agent | `commit.tmpl` |
| `committed` written by a task agent | invariant violation → project blocks | `dispatch_lifecycle.go:89-92` |

### 4.4 Task graph (`internal/taskgraph`)

`Load` (`graph.go:69-167`) skips seeds (blank name + description), repairs bad
names (`taskgraph_repair`), and fails on duplicate names, unknown deps or cycles
(`taskgraph_error` → project blocked). `Ready()` (`graph.go:228-239`) = `ready`
tasks whose deps are all `committed`. `Runnable` (`graph.go:414-438`) orders by
re-runs first, then most transitive dependents, then name (`hints.go:160-172`),
and defers candidates whose `touches`/`areas` overlap anything in flight
(`overlap.go:16-38`; a task with no hints is *exclusive*).

### 4.5 Intake

Intake files are free-form markdown, one request per file, dropped by a human,
a script or the workingman agent; the TUI has no command for it
(`command_picker.go:93-97`). Flow is in §3. `pendingIntakeFiles`
(`intake.go:113-132`) is a directory scan of non-blank `*.md`; the planner
renames each to `.processed`.

## 5. Worktrees, the commit queue and integration

- Each task runs in its own worktree `<shared>/.worktrees/<task>/<repo>` on
  branch `wm/<project-branch>/<task>` (`worktree/worktree.go:19-47`). The
  project branch is checked out in the shared wsp workspace.
- `Ensure` (`worktree.go:198-281`) reuses, reattaches or creates per repo; any
  other content at the path is moved to `.worktrees/.stale/` (never deleted).
- `prepareTaskWorktree` (`daemon/worktree.go:97-157`) provisions the shared
  workspace, falls back to the legacy shared tree when needed
  (`worktree_skipped_legacy`), and stamps `worktree`, `task_branch`, `base`.
- **Session slots** (`schedule.go:20-38`): `<path>` (project, review),
  `<path>#task:<name>` (one per task), `<path>#commit` (one per project),
  `<path>#planning`, `<path>#wolf`, `<path>#archive`.
- **Commit queue / integration order** (`Queued`, `taskgraph/queue.go:21-49`):
  every non-push task at `success` plus every push task at `ready` with deps
  committed, ordered by `succeeded_at` (unstamped first, then name). One commit
  agent per project integrates the head (`decidePass`, `schedule.go:349-511`).
- The commit agent rebases the task branch onto the shared tip, optionally runs
  `verify`, and publishes with `git merge --ff-only` (`commit.tmpl:40-80`).
  Failures route through `routeIntegrationFailure` (`integration.go:181-211`):
  `conflict`/`verify_failed` → task back to `ready` for a conflict re-run
  (`conflictReRun`, `:275-339`); `partial` → block; transient → retry up to 2.
- After `committed`, `finishWorktreeTask` (`daemon/worktree.go:319-421`)
  verifies the commits are ancestors of the project branch, then removes the
  worktree and branch.

## 6. The state snapshot and how the TUI consumes state

### 6.1 File and flags

- `--state-file PATH|off` (`cmd/orch/main.go:139`); default
  `<sessions-root>/../state/snapshot.json` = `~/.workingman/state/snapshot.json`
  (`DefaultStateFile`, `snapshot_live.go:87-93`). It must lie outside every root.
- Indented JSON, schema `version: 1` (`snapshot.go:24`); field reference in
  `docs/state-snapshot.md:78-207`. Every string passes through
  `audit.Redact` before writing (`snapshot.go:737`, `snapshot_live.go:324`).

### 6.2 Shape (`internal/daemon/snapshot.go:57-332`)

- Top level: `version`, `generated_at`, `source` (`daemon`/`offline`),
  `live_state_available`, `notes`, `daemon{state, pid, started_at,
  workspace_manager, acp_kit, acp_image, acp_wrapper, sessions_root, audit_log,
  state_file, headless, roots}`, `roots`, `projects[]`, `sessions[]`,
  `audit_events[]` (last 50).
- `projects[]`: `work_stream`, `path`, `status`, `blocked_reason`, `branch`,
  `description` (≤ 400 chars), `load_error`, flags (`review`, `review_now`,
  `cleanup`, `archive`, `replan`, `hidden`, `watching_pr`), `cron`,
  `cron_active`, `pull_requests[]`, `task_counts{every status}`, `task_total`,
  `max_parallel_tasks`, `commit_queue[]`, `verify_checks`, `tasks[]`,
  `updated_at`, and the daemon-only `live` block.
- `projects[].tasks[]`: `name`, `status`, `attempts`, reasons, `depends_on`,
  `touches`, `areas`, `exclusive`, `worktree`, `task_branch`, `base`,
  `succeeded_at`, `completed_at`, `queue_position`, `waiting_on`, `integration`.
  **Not included:** `static_mcps`, `policies`, `model`/`resolved_model`,
  `harness`/`resolved_harness`, `description`, `summary`, `commits`. A
  "sandbox card" would need these from the task file.
- `projects[].live`: `sessions` (kinds), `running_tasks`, `integrating`,
  `commit_reserved`, `wolf_in_flight`, `cleanup_in_flight`,
  `review_agent_in_flight`, `planning_failures`, `project_failures`,
  `review_fix_cycles`, `review_errors`, `review_poll{schedule, backoff_index,
  has_baseline, baseline_source}`, `cron_schedule`, `deferred{overlap, cap, power}`.
- `sessions[]`: `key`, `kind`, `work_stream`, `project_path`, `task`,
  `worktree`, `started_at`, `interactive`, `source`, `sandbox_name`,
  `tab_name`, `acp{session_id, session_dir, socket_path, status}`. Session-key
  conventions: `docs/state-snapshot.md:183-201`.
- `audit_events[]`: `{time, event, fields{}}`.

### 6.3 When it is written

At startup, then on a 2 s tick, and 150 ms after any `markSnapshotDirty` kick
(every audit line `daemon.go:409`, session start/end `daemon.go:628,653`, every
project/task/intake event `dispatch.go:41-43`). A write is skipped when only
`generated_at` would change, except a 15 s heartbeat (`publishLocked`,
`snapshot_live.go:197-222`), so `generated_at` is a liveness signal. Writes are
atomic (temp + `fsync` + `rename`, `WriteFileAtomic`, `snapshot_live.go:246-276`).
On clean shutdown a final snapshot has `daemon.state = "stopped"`
(`snapshot_live.go:227-240`). `BuildOfflineSnapshot` (`snapshot.go:650`) builds
the same document without a daemon.

### 6.4 Who reads it

`daemon.ReadSnapshot` (`snapshot_text.go:15-25`) is a one-shot read used by
`orch status` (`cmd/orch/status.go:80`; stale after 1 minute) and
`orch whatsapp status`. The workingman agent mounts the state dir read-only.
**The TUI does not read `snapshot.json`.**

### 6.5 What the TUI reads instead

| Source | Mechanism | Code |
|---|---|---|
| `project.yaml` + `tasks/*.yaml` | `WatchProjects` polls `ScanProjects` every 1 s, emits on change | `tui/projects.go:217-268, 476-517`; `tui.go:2947` |
| live sessions | in-process channel: `d.WatchSessions` polls the session map every 200 ms | `daemon/sessions.go:133-160`; `cmd/orch/main.go:395, 441-481` |
| ACP sessions | `WatchACPSessions` polls `<sessions>/*/session.json` every 500 ms, dials `agent.sock`, replays `stream.log` | `tui/acpwatch.go:209-366, 598-623` |
| post-office inbox | `WatchInbox` polls the store every 1 s | `tui/inbox.go:79-120` |
| terminal tabs | `~/.workingman/tabs/*.json/.pid` | `terminal/tabs.go:174-182` |
| sandbox liveness | `sbx ls` | `sbx/sbx.go:63-74` |

The TUI does not read the audit log (`orch tui --audit-log` is accepted and
ignored, `cmd/orch/main.go:509-511`).

## 7. Agent roles

### 7.1 Common mechanics

- Ten kinds in `agent.Kind` (`internal/agent/agent.go:10-87`): `project`,
  `planning`, `task`, `wolf`, `commit`, `archive`, `review`, `workingman`,
  `sleepy-head`, `step-up`. `Interactive()` is true only for wolf and
  sleepy-head (`agent.go:136-138`).
- Prompts are embedded templates `internal/prompts/templates/<kind>.tmpl`
  (`prompts.go:14-15, 229-241`). The runner renders the template and writes
  `.orch/instructions.md` + `.orch/context.yaml` into the agent's working dir
  (`runner.go:807-893`; `setup.Apply`, `internal/setup/setup.go:123-150`). The
  opening prompt is always "Read .orch/instructions.md and .orch/context.yaml,
  then follow the instructions." (`runner.go:287`; `tui/acpwatch.go:105`;
  `router/turn.go:106`).
- Every kind runs under ACP when `--acp-kit` is set (`Runner.UsesACP`,
  `runner.go:697-705`); the exception is the wolf with `--wolf-host` (a host
  terminal tab, no sandbox).
- **Tool permissions are not a capability boundary.** ACP sessions are switched
  to `bypassPermissions` right after `session/new` (`acpclient.go:330-338`);
  the host path uses `claude --dangerously-skip-permissions` (`runner.go:305-311`).
  Capability is bounded by sandbox mounts, `sbx` policies and the MCP gateway.
- Model: only task and commit agents pass a task `model` (`dispatch.go:1235`,
  `dispatch_lifecycle.go:454`); `--model` is omitted for `""`/`default`
  (`runner.go:1412-1414`). Harness: only `claude-acp` exists
  (`resolveHarness`, `runner.go:1312-1325`).

### 7.2 Project workflow agents

| Agent | Trigger | Runs in / sandbox | May write | Done when | Capabilities |
|---|---|---|---|---|---|
| **project** | unpopulated `project.yaml` (`dispatch.go:157-164`) | control dir; `<ws>-project` | `project.yaml` | status leaves unpopulated (`afterProjectSession`, `dispatch.go:558`; 3 strikes → block) | default MCP set |
| **planning** | `ready`; intake while `working`; cron replan (`dispatch.go:239-256, 391`) | control dir + wsp worktree mount; sandbox `<ws>` | `tasks/*.yaml`, `project.yaml` (`planning.tmpl:281-285`) | status leaves `ready` (`afterPlanningSession`, `dispatch.go:476`) | **chooses each task's `static_mcps`, `policies`, `model`, `harness`** (`planning.tmpl:215-279`) |
| **task** | ready + deps committed + slot free (`schedule.go:295`; `launchTaskAgent`, `dispatch.go:1217`) | own worktree; `<ws>-<task>` | code + commits on its task branch; task `status`/`failure_reason` | task status (`afterTaskSession`, `dispatch_lifecycle.go:46`) | task's `static_mcps`, `policies`, `model`; dynamic gateway |
| **commit** | commit-queue head (`launchCommitAgent`, `dispatch_lifecycle.go:413`) | shared workspace; `<ws>-<task>-commit` | rebase, ff-merge onto the project branch; push only for push tasks; task `committed` / `integration` | `afterCommitSession` (`:216`) | inherits the task's MCPs/policies/model; ssh push preflight for push tasks |
| **wolf** (interactive) | any block (`transitionProjectBlocked`), `:wolf`, `status: blocked` (`dispatch.go:258-268`) | control dir + wsp worktree + snapshot dir (ro); `<ws>-wolf` | this project's files, `blocked-session.yaml` | wrapper ends it once the project is un-blocked for the 2 m grace (`acpwrapper.go:1288-1290, 1383`) | dynamic gateway; persistent |
| **archive** | `cleanup: true` (`dispatch.go:204-209`) | wsp workspace; `<ws>-archive` | ≤ 1 commit + push per repo; `archive: true` | `afterArchiveSession` (`:824`) | daemon grants network to the push host (`archivePolicies`, `dispatch_lifecycle.go:722-728`) |
| **review** | PR poll fingerprint change, `review_now` (`review.go:155, 234`) | control dir; `<ws>-review` | task files (fix/push tasks), `pull_requests`, `status` | `afterReviewSession` (`review.go:293`) | hard-coded `static_mcps: [github]` + network allow `api.github.com`, `api.githubcopilot.com` (`review.go:245-268, 595-603`); static gateway |

The TUI's `:session` / `:orch` / `:workspace` sessions are **not** agent kinds:
they have no template, no `.orch/` handoff and no daemon session tracking (§9).

### 7.3 workingman, sleepy-head and step-up

All three exist in the code as agent kinds. None belongs to a project.

**workingman agent** (`agent.WorkingmanAgent`; `internal/daemon/workingman_agent.go`,
`cmd/orch/workingman_agent.go`, `workingman.tmpl`). "workingman" is also the
product/state-dir name (`~/.workingman`).
- An always-on assistant that answers questions about orch state, reached
  through messaging channels (WhatsApp/Signal/Slack via the router) or its TUI
  ACP tab. On request, and after confirming with the human, it edits
  `project.yaml`, `tasks/*.yaml`, and creates `project.md` / `intake/*.md`
  (`workingman.tmpl:174-208`; never sets `success`/`committed`).
- Enabled by `--workingman-agent=auto|on|off` (auto = on when `--acp-kit`
  is set). Started in `Daemon.Run` (`daemon.go:458`; `beginWorkingmanAgent`,
  `workingman_agent.go:411-431`), supervised with backoff (`:302-351`), exempt
  from the reaper (`reaper.go:164-166`).
- Mounts (`workingmanPlan`, `workingman_agent.go:195-231`): orch roots
  **writable**; snapshot dir, audit-log dir and sessions root read-only;
  sandbox `workingman-agent`; `--require-okta-gateway`; dynamic MCP gateway.
- It does not choose capabilities for other agents. Its least privilege is
  mount-based for observed state and prompt-based for what it edits in the roots.

**sleepy-head** (`agent.SleepyHeadAgent`, interactive; `internal/daemon/sleepy_head.go`,
`cmd/orch/catchup.go`, `sleepy-head.tmpl`).
- Reports cron projects that missed ticks while the machine slept or the daemon
  was down. With the user's explicit confirmation it triggers exactly one
  catch-up per approved project (by writing `catchup-request`; `orch catchup`
  writes the same marker).
- Launched at startup (`daemon.go:464`) and after a detected wake gap
  (`cron_missed.go:362-370`), only if there are missed schedules, the machine is
  on AC, and none is running (`launchSleepyHead`, `sleepy_head.go:159-194`).
- Sandbox `sleepy-head`; writable mounts limited to the behind projects' orch
  dirs (`sleepyHeadPlan`, `sleepy_head.go:242-270`). Chooses no capabilities
  for others.

**step-up** (`agent.StepUpAgent`; `internal/daemon/step_up.go`,
`step_up_exec.go`, `step_up_rules.go`, `cmd/orch/step_up_agent.go`, `step-up.tmpl`).
This is the component that controls capability **escalation at runtime**.
- Task/commit agents that have the `post-office` MCP send it a request for
  more sandbox capability. Step-up judges the request, asks the operator via
  post-office, and files a typed change request into its `requests/` dir.
- A **daemon-side executor**, not the agent, validates and applies it
  (`process`, `step_up_exec.go:375-492`): kill switch, strict parse of five
  ops (network/filesystem allow/revoke, `list_sandbox_policy`), target sandbox
  derived from the original post-office message (never from the request file),
  protected paths (`~/.ssh`, `~/.aws`, `~/.workingman`, `/etc`, docker
  socket…), operator approval within 24 h with the exact change line, single
  use, rate limits (10/h, 3/sandbox/h), no auto-approve list. It then runs one
  `sbx policy` command and persists the rule into the task's `policies:`
  (`persistPolicy`, `:715`) so it survives sandbox recreation.
- Enabled by `--step-up-agent=auto|on|off` (auto needs `--acp-kit` and
  post-office answering on 127.0.0.1:7077). Mounts: only `requests/` writable;
  `results/`, snapshot, audit and sessions read-only; no orch roots;
  `static_mcps: [post-office]` with a static gateway (no `mcp-add`)
  (`stepUpPlan`, `step_up.go:273-316`; `runner.go:277-280, 750-767`).
- Driven by the daemon, not the TUI: `driveStepUpSession` (`step_up.go:492-604`)
  attaches with `acpchat` and prompts it when there is new work.

### 7.4 Where capabilities are decided today

| Decider | Sets | Where |
|---|---|---|
| planning agent (by prompt) | per-task `static_mcps`, `policies`, `model`, `harness` | `planning.tmpl:215-279` |
| daemon (hard-coded) | review: `github` + 2 network allows; archive: push-host allow; step-up: `post-office`, static | `review.go:245-268`; `dispatch_lifecycle.go:722-728`; `runner.go:277-280` |
| runner | default server set, which kinds get a dynamic gateway (wolf, task, workingman), mounts | `runner.go:1427-1492, 2029` |
| step-up + executor | runtime allow/revoke on a live task/commit sandbox, operator-approved | `step_up_exec.go` |
| post-office | refuses any sandbox whose gateway was not minted with `post-office` | `postoffice/mcpserver/identity.go:36` |

## 8. The session model: ACP sessions and sandboxes

### 8.1 Processes

- With `--acp-kit`, `r.AcpLauncher = &agent.ProcessLauncher{}` (`cmd/orch/main.go:243-255`);
  wrapper stderr goes to `acp-wrapper.log` beside the audit log.
- `Runner.startACP` (`runner.go:1161-1236`) spawns one `acp-wrapper` per
  session (`cmd/acp-wrapper/main.go`). The wrapper ensures the sandbox, runs
  `sbx exec … <sandbox> -- claude-acp-client` (ACP over stdio), and serves that
  stdio on `<sessions>/<id>/agent.sock` (`acpwrapper.go:1065-1331`).
- The socket hub (`acpwrapper/bridge.go:49-209`) fans agent frames out to every
  connected client and serializes client frames into stdin; every frame is
  teed to `stream.log` for replay. Ends: agent exit; `--exit-when-empty`
  (one-shot kinds: last client leaves → stdin closed); `--persistent` (wolf,
  workingman, step-up, sleepy-head) with unblock grace (wolf) and idle timeout.

### 8.2 Session record

`~/.workingman/sessions/<id>/` holds `session.json`, `agent.sock`,
`stream.log`. `session.json` (`session.go:44-140`): `status`
(`starting|running|exited|failed`), `sandbox_name`, `harness`, `template`,
`log_path`, `prompt_count`, `project_path`, `task_path`, `kind`, `persistent`,
`signing_broken`, `push_broken`, `error`. The id is `<kind>-<task|branch|hash>`
(`sessionName`, `runner.go:1659-1671`). **The wrapper deletes the whole
directory when the agent exits** (`acpwrapper.go:1316-1321`), so transcripts of
finished sessions are not kept by workingman.

### 8.3 Sandbox naming (`SandboxNameFor`, `runner.go:1750-1826`; `sbx.SanitizeName`, `sbx/name.go:35-99`)

| Kind | Sandbox |
|---|---|
| project | `<ws>-project` |
| planning | `<ws>` |
| task | `<ws>-<task>` |
| commit | `<ws>-<task>-commit` |
| wolf (ACP) | `<ws>-wolf` |
| archive | `<ws>-archive` |
| review | `<ws>-review` |
| workingman / sleepy-head / step-up | `workingman-agent` / `sleepy-head` / `step-up` |
| `:session` | `session-<ws>[-<task>]` |

Names are capped at 63 chars with a hash suffix. Deterministic names mean a
relaunch reuses the same sandbox.

### 8.4 Sandbox lifecycle (`ensureSandbox`, `acpwrapper.go:755-786`)

Reuse when a sandbox of that name exists with the same mounts (and no harness
drift); re-apply policies, gateway rules and the extra CA. On drift: `sbx rm
--force` + release the gateway, then create. `createSandbox` (`:652-710`): mint
the MCP gateway, `sbx create claude --name <n> --kit <kit> -t <image> --pull
missing -e MCP_URL=… <workspaces…>`, apply task policies then gateway rules,
install the extra CA, save gateway and harness records. On exit
`removeSandboxOnExit` (`:963-997`) removes it unless signalled, `save_sandbox`,
or the task ended `failed`/`blocked`/`running`.

### 8.5 The workload's inputs

| Input | Source → mechanism |
|---|---|
| Harness image | `--acp-image` (start.sh default `slimslenderslacks/claude-code-acp:0.90.0`, `start.sh:199`) → `sbx create -t <image> --pull missing`; acp-kit `--kit` layered on top (required). Recorded as `resolved_harness`. |
| Model | task `model` → `--model` → `ANTHROPIC_MODEL` (`acpwrapper.go:473-475`); concrete id learned from `usage_update._meta["_claude/model"]` and written as `resolved_model` (`record.go:32-161`). LLM traffic goes to `llm.es.atko.services` only when an sbx secret targets it; credentials never enter the sandbox (`sbx/claude.go:14-36`). |
| MCPs | task `static_mcps` (or `--mcp-gateway-servers`, default `github`) → per-sandbox gateway; `MCP_URL` env (§10) |
| Policies | task `policies` → `--policy action:kind:resource` → `sbx policy …` (`policy/policy.go:68-118`) |
| Mounts | runner plan: control dir and/or worktree/workspace, plus kind-specific ro mounts |
| Prompt | rendered template → `.orch/instructions.md`, `.orch/context.yaml` in the cwd mount |
| Skills | `setup.Skill` → `.claude/skills/<name>/` (`setup.go:115-150`). `Plan.Skills` exists (`runner.go:188`) but **nothing sets it today**. |
| Git identity / signing | `GIT_AUTHOR_*`, `GIT_CONFIG_*` env; forwarded ssh agent; signing and push preflights set `signing_broken`/`push_broken` |
| Extra CA | `--extra-ca-certs` installed on every create/reuse (`sbx/extraca.go:177`) |

The output of a task workload is its task branch and the commit(s) the commit
agent lands on the project branch (`commits:` in the task file).

## 9. How a human joins a live session today

1. **TUI ACP tab view** (`a`, or Enter on an ACP session row). `WatchACPSessions`
   dials `agent.sock` (`acpclient.Dial`, `acpclient.go:240-247`), runs
   `initialize` → `session/new` → `session/set_mode bypassPermissions`
   (`acpclient.go:306-344`), and replays `stream.log`. **The TUI sends each
   session's opening prompt** when `prompt_count == 0` and then bumps it
   (`acpwatch.go:494-503, 633-641`). One-shot sessions are disconnected after
   that turn (with `--exit-when-empty` that ends the session). Typing is
   allowed only into persistent sessions (wolf, workingman, sleepy-head)
   (`acpwatch.go:162-204, 575-589`).
2. **A second client on the same conversation.** `acpchat.Attach`
   (`acpchat/acpchat.go:166`) binds to the existing ACP session id recovered
   from `stream.log` (`acpclient.Adopt`, `acpclient.go:360`) and uses a
   distinct request-id range. Used by the channel router and the step-up driver,
   so the TUI, the daemon and a phone can share one conversation.
3. **Terminal tabs.** `:session` launches `sbx run --name session-<ws>[-<task>]
   … -- --session-id|--resume <uuid>` in a new tab (`daemon/interactive.go:121-252`;
   `runner.go:1054-1150`); this is the sandbox's own interactive claude, not
   ACP, and nothing removes these sandboxes. `:orch` / `:workspace` open a host
   `bash`. Tabs are driven by `terminal.TabLauncher` (Ghostty, WezTerm, kitty;
   `docs/terminal-tabs.md`) and tracked in `~/.workingman/tabs/`. Enter on a
   non-ACP row focuses its tab (`tui/tab_attach.go:47-61`).
4. **Manual.** `sbx-create.sh` / `sbx-rm.sh`; `sbx exec -it <sandbox> bash`
   gives a shell next to an ACP agent but does not join the conversation.

## 10. The MCP gateway

- The gateway is an **external** host process, standalone-codemode, at
  `--mcp-gateway-url` (start.sh default `http://localhost:3333`). orch and
  acp-wrapper create one sub-gateway per sandbox with `POST /gateways
  {"servers":[…],"dynamic":bool}` and remove it with `DELETE /gateways/{id}`
  (`internal/mcpgateway/mcpgateway.go:109-298`; `/healthz` for readiness).
  Sub-gateways live in memory, so a gateway restart drops them.
- Server set: task `static_mcps`, else `--mcp-gateway-servers`; dynamic
  gateways additionally expose `mcp-find` / `mcp-add` over the whole catalog
  (wolf, task, workingman by default; off with `--mcp-gateway-static`).
- Lifecycle in `Manager` (`manager.go:26-392`): `WaitReady`, `Prepare`/`Mint`
  before `sbx create`, `Save`, `Ensure` (reuse if same servers and still alive),
  `Release`, `Sweep` after a 10 m grace. Records in
  `~/.workingman/gateways/<sandbox>.json`.
- Into the sandbox: `MCP_URL` (loopback rewritten to `host.docker.internal`),
  persisted to `/etc/sandbox-persistent.sh`; acp-kit registers it as the
  `okta-gateway` MCP server. The sandbox gets a network allow for the gateway
  port (`PolicyRules`, `mcpgateway.go:235-254`).
- **Auth:** none from orch to `/gateways`; the sandbox's only credential is the
  unguessable sub-gateway UUID path; backend credentials stay in the gateway.
- The daemon also holds its own long-lived github-only gateway (`orch-daemon`,
  `review_gateway.go:24-35`) for PR fingerprinting, and one for Slack.

## 11. Post-office messaging and channels

- `cmd/post-office` is a separate MCP server process (launchd plist in
  `launchd/`) at `127.0.0.1:7077/mcp`. It is separate so it survives orch restarts.
- Store: `~/.workingman/post-office/` (`inbox/`, `outbox/`, `done/<day>/`,
  `deliveries/`, `token`, lock; dirs 0700, files 0600). The directory is the
  integration point; there is no RPC between post-office, the daemon and the TUI
  (`postoffice/doc.go:1-21`). First answer wins via `link(2)` (`Store.Claim`,
  `store.go:393`).
- Model (`postoffice/model.go`): `kind: notification|request`; outcome
  `answered|cancelled|expired|dismissed`; recipient `operator` or a registered
  agent (only `step-up` in v1); sender `{agent, sandbox, project, task,
  session_id}` derived from the calling gateway, not from arguments.
- MCP tools (`mcpserver/server.go:241-271`): `send_notification`,
  `send_request`, `wait_for_response`, `check_response`, `cancel_request`,
  `list_requests`; receivers: `next_message`, `reply`, `list_inbox`.
- Only sandboxes whose gateway was minted with `post-office` in `static_mcps`
  may call it; `mcp-add` cannot grant it (`mcpserver/identity.go:26-48`).
- Humans answer in the TUI inbox (`I`) or a channel; the daemon's dispatcher
  fans operator messages out to channels (`postoffice/dispatch.go`).
- Channels (`docs/channels.md`): WhatsApp (Cloud webhook or local Baileys
  bridge), Signal (signal-cli), Slack (via the gateway's Slack MCP). Config in
  `~/.workingman/channels.yaml`, secrets in `secrets.yaml` (0600); all
  default-deny via `allow_from`. The router sends inbound text to the wolf, a
  post-office answer, or the workingman agent.

## 12. The TUI: layout, keys and `:` commands

### 12.1 Layout (`internal/tui/tui.go`)

Three columns plus a footer (`View`, `tui.go:2094-2153`):
- **Left – Agent Sessions** (`renderSessions`, `:1199`), shown by default
  (toggle `tl`): one box per session with harness, model, mounts, MCPs, sandbox.
- **Centre – Work Streams** gallery (`renderProjects`, `:1567`): cards whose
  border shows archive-ready (blue), cron, PR-watch or load error; and below it
  **Tasks** (`renderTasks`, `:2185`): name, model, mcps, rules, status, with a
  dependency tree above when the graph has more than one task (`taskDepTree`, `:2354`).
- **Right – Project detail** (`project_detail.go:26-264`), hidden by default
  (toggle `tr`): badges, repos, parallelism/queue section, task list.
- **Footer**: inbox badge, then status message or key hints.
- **Overlays** (full screen): command picker (`:`), archive confirm, inbox (`I`),
  ACP tab view (`a`), zoom (`z`).

### 12.2 Keybindings

| Context | Key | Action | Ref |
|---|---|---|---|
| any | `ctrl+c` | quit | `tui.go:363` |
| normal | `q` | quit | `:419` |
| normal | `:` | command picker | `:421-435` |
| normal | `a` | ACP tab view | `:436-442` |
| normal | `h`/`k`, `j`/`l` | previous / next item in focused pane | `:443-453` |
| normal | `tl` / `tr` / `tp` | toggle left column / right column / hidden streams (300 ms chord) | `:402-414` |
| normal | `alt+j`/`alt+k` (`∆`/`˚`) | centre focus Projects ↔ Tasks | `:464-481` |
| normal | `alt+h`/`alt+l` (`˙`/`¬`) | focus column left / right | `:482-502` |
| normal | `z` | zoom focused pane | `:503` |
| normal | `m` | toggle mouse capture | `:508` |
| normal | `y` | copy (OSC 52) tab name / project dir / task path / detail | `:510`, `clipboard.go:59-96` |
| normal | `enter` | attach to selected session (ACP tab or terminal tab) | `:512-515, 849-881` |
| normal | `I` | inbox | `:372` |
| picker | `j`/`k`/arrows, `enter`, `esc` | move, run, close | `command_picker.go:68-87` |
| archive confirm | `y` / `n`/`esc` | archive / cancel | `archive.go:37-56` |
| ACP view | `esc`/`a`, `q` | back, quit | `tui.go:592-595` |
| ACP view | `l`/`alt+j`, `h`/`alt+k` | next / previous tab | `:596-602` |
| ACP view | `j`/`k`, `ctrl+f`/`ctrl+b`, `G` | scroll line / page, follow | `:603-614` |
| ACP view | `z`, `m`, `y` | expand tool output, mouse, copy transcript | `:615-628` |
| ACP view | `x` | dismiss a dead tab and delete its session dir | `:629-645` |
| ACP view | `enter`/`i` | compose a message (persistent sessions only); `enter` sends, `esc` stops, `ctrl+u` clears | `:646-706` |
| inbox | `j`/`k`, `ctrl+f`/`ctrl+b`, `y` | select, scroll, copy | `inbox.go:299-367` |
| inbox | `enter` | type a reply (operator requests) | |
| inbox | `1`–`9` | answer with numbered option | `:348-364` |
| inbox | `d` | dismiss an operator notification | `:333-347` |
| inbox | `I`/`esc` | close | |

### 12.3 `:` commands (`projectCommands`, `command_picker.go:28-40`; dispatch `:98-234`)

None take arguments; all act on the selected work stream (and the selected task
where noted).

| Command | Effect | Maps to |
|---|---|---|
| `:session` | open/resume interactive claude in a terminal tab; from Tasks, in the task's worktree | `Daemon.OpenInteractiveSession` / `OpenTaskSession` → `sbx run --name session-<ws>[-<task>]` |
| `:wolf` | summon the wolf | `project.yaml`: `status: blocked` (+ reason), as agent |
| `:orch` | host bash tab in the orch dir | `Daemon.OpenOrchDirTab` |
| `:workspace` | host bash tab at the wsp workspace root (or task worktree) | `Daemon.OpenWorkspaceDirTab` / `OpenTaskWorktreeDirTab` |
| `:review` | watch the PR / poll it now (only on `idle`) | `review: true` or `review_now: true`, as agent |
| `:cleanup` | request the archive agent | `cleanup: true`, as agent |
| `:archive` | archive after y/n (needs `archive: true`) | TUI removes worktrees, `.orch`, `wsp rm`, moves dir to `<root>.backup/` |
| `:stop` | pause; daemon kills agents, unschedules cron | `stopped_from`, `status: stopped`, as agent |
| `:start` | resume | `status: <stopped_from>`, as agent |
| `:hide` / `:show` | gallery filter | `hide: true/false`, as daemon |

`:session`, `:orch`, `:workspace` need the integrated mode (`InteractiveLauncher`,
`tui/interactive.go:20-52`); in `orch tui` they report unavailable.

### 12.4 How the TUI writes

Most commands are `project.Load` → change a field → `project.SaveAs` with the
appropriate writer. The interactive commands call daemon methods in-process.
`:archive` runs filesystem and `wsp` operations itself. The inbox answers with
`postoffice.Store.Claim`. ACP writes: prompts over `agent.sock`, `prompt_count`
in `session.json`, removal of dead session dirs.

## 13. Audit log

- `--audit-log` (append mode, `cmd/orch/main.go:118, 152-160`). Line format
  (`audit.Logger.Log`, `internal/audit/audit.go:71-88`):
  `2026-05-12T18:04:00Z <event> key=val key="val with spaces"`. The logger keeps
  the last 500 lines in memory for the snapshot (`Recent`, `OnLog`).
  `read.go` parses lines (`ParseLine`, `TailFile`); `redact.go` masks secrets.
- Event families (non-exhaustive; emit sites in `internal/daemon` unless noted):
  - **process:** `daemon_start`/`daemon_stop`/`daemon_error` (`cmd/orch/main.go`),
    `startup_scan`, `watch_added`, `watcher_error`, `git_preflight`, `state_snapshot_error`.
  - **project:** `project_seed_created`, `project_unpopulated`, `project_created_stamped`,
    `project_updated`, `project_idle`, `project_idle_watching`, `project_blocked`,
    `project_block_additional`, `project_merged`, `project_load_error`, `project_save_error`,
    `seed_replan`, `power_deferred`.
  - **intake/planning:** `intake_queued`, `intake_empty`, `intake_no_project`,
    `intake_plan_while_working`, `planning_intake`, `planning_retry`, `repo_sync_*`.
  - **cron:** `cron_fired`, `cron_replan_requested`, `cron_replan_skipped`,
    `cron_run_counted`, `cron_run_not_counted`, `cron_unscheduled`, `cron_missed`,
    `clock_jump`, `catchup_fired`, `sleepy_head_*`.
  - **tasks:** `task_file_updated`, `task_running`, `task_released`, `task_retry`,
    `task_retry_exhausted`, `task_completed_stamped`, `taskgraph_error`,
    `taskgraph_repair`, `task_deferred_overlap`, `task_deferred_cap`,
    `task_waiting_integration`, `no_ready_tasks`, `sandbox_retained`.
  - **worktrees/integration:** `worktree_created`/`_reused`/`_removed`/`_stale_moved`/`_skipped_legacy`,
    `integration_started`/`_succeeded`/`_conflicted`/`_partial`/`_retry`,
    `integration_verify_check_{passed,failed,skipped}`, `integration_verify_failed`,
    `touches_exceeded`. Contract: `docs/state-snapshot.md` and `docs/parallel-task-worktrees.md` §D3.
  - **sessions:** `session_started`, `acp_session_started` (`internal/runner`),
    `session_ended`, `session_detached`, `session_stopped`, `session_skip_duplicate`,
    `session_reaped`, `session_reconciled`, `wolf_dispatch`, `archive_dispatch`,
    `archive_incomplete`, `cleanup_flag_cleared`, `interactive_open`,
    `workingman_agent_*`, `step_up_agent_*`, `step_up_op`.
  - **review:** `review_dispatch`, `review_poll_fired`, `review_poll_quiet`,
    `review_poll_forced`, `review_clean`, `review_fix_cycle`, `review_error`,
    `review_baseline`, `<name>_gateway_*`, `<name>_mcp_*`.
  - **channels/router/post-office:** `channels_configured`, `channel_send_error`,
    `router_*`, `postoffice_*`.
- Step-up keeps its own `audit.jsonl` for every request outcome.

## 14. PR handling and the review loop

- `review: true` is set by the project/planning agent when the goal is
  PR-shaped (`project.tmpl:62-70`), by a human, or by `:review`.
- When every task is committed, `transitionProjectComplete` (`review.go:62-90`)
  goes to idle-watching if `WatchingPR()`, else idle.
- `ensureReviewPoll` (`review.go:104-126`) arms `#review` on the adaptive
  ladder. Each tick (`onReviewPoll`, `:155-201`) computes a host-side
  **fingerprint** over the daemon's own GitHub MCP session (`pull_request_read`
  methods `get`, `get_reviews`, `get_comments`, `get_review_comments`,
  `get_check_runs`, `get_status`; `review_mcp.go:37-66`): quiet → back off;
  changed/no baseline → run the review agent; unverified → run it, keep backing off.
- The review agent (`review.tmpl`) has the `github` MCP only, and does not use
  `gh`, `git push` or clone. It finds the PR per repo, **creates it with
  `create_pull_request`** if the branch is pushed with commits ahead, otherwise
  inserts a `pr-push` task; turns unresolved threads into `pr-comment` fix
  tasks and failed checks into `pr-check` fix tasks; resolves threads after
  fixes land; batches commit-only fixes behind a push task; records each PR's
  fingerprint in `pull_requests`; sets `working` (tasks created) or `idle`.
- Push tasks skip the task agent and go straight to the commit queue; the
  commit agent runs `git push` in push mode (`commit.tmpl:166-185`). These and
  the archive agent are the only pushes.
- `afterReviewSession` (`review.go:293-379`): 5 consecutive errors or more than
  10 fix cycles block the project. `maybeTransitionMerged` (`:612-698`) moves
  idle → `merged` when every branch is merged.
- The only `gh` CLI use is `gh repo view`/`create` for `new_repos` (`wsp.go:232-261`).

## 15. Network listeners and sockets

**`orch` itself opens no TCP listener**, except the optional WhatsApp Cloud
webhook. A grep of non-test code for `net.Listen`, `http.Server{`,
`ListenAndServe` and `"unix"` finds only:

| Listener | Process | Address | Auth |
|---|---|---|---|
| ACP session socket | `acp-wrapper` (one per session) | unix `~/.workingman/sessions/<id>/agent.sock` (`acpwrapper.go:1233`; dir 0o755, `:1070`) | **none**: filesystem permissions only. Any local client can drive the agent. |
| post-office `/mcp` | `cmd/post-office` | TCP `127.0.0.1:7077` (`--listen`; `postoffice/store.go:62`; `cmd/post-office/main.go:142, 188`) | `Authorization: Bearer <~/.workingman/post-office/token>` (0600), constant-time compare (`mcpserver/token.go:81-92`); caller identity from `X-Codemode-Gateway-Id` |
| WhatsApp Cloud webhook | `orch` daemon, if configured | TCP `127.0.0.1:8090` default, `/whatsapp/webhook`, `/health` (`channels/whatsapp/cloud_webhook.go:31-35, 241`) | `X-Hub-Signature-256` HMAC with the app secret; verify token; warns when not on loopback |
| WhatsApp Baileys bridge | child process of orch | TCP `127.0.0.1:<random>` (`bridge_channel.go:283`) | random per-run token in `WORKINGMAN_BRIDGE_TOKEN` |

Outbound only: the MCP gateway (`localhost:3333`, no client auth), signal-cli
(`127.0.0.1:8080`), Slack via the gateway, and the step-up probe to
`127.0.0.1:7077`. Sandboxes reach host services through `host.docker.internal`
under `sbx` network policy.

## 16. Binaries and `orch` subcommands

| Binary | Purpose |
|---|---|
| `orch` | daemon + embedded TUI |
| `acp-wrapper` | one process per ACP session: sandbox, `claude-acp-client`, `agent.sock` |
| `post-office` | messaging MCP server: `serve` (default), `setup [--rotate] [--print]` |
| `op-read` | resolve one 1Password `op://` ref via a service account (used by `start.sh`) |

`orch` subcommands (`cmd/orch/main.go:46-103`): *(none)* daemon (+TUI unless
`--headless`); `tui` (TUI only, no daemon, no ACP tabs, no `:session`); `status
[--json] [--root …]`; `catchup <project.yaml>`; `help`; `whatsapp
pair|setup|status|test`; `signal setup|status|test`; `slack setup|status|test`;
`service install|uninstall|status` (macOS launchd agent
`com.workingman.orch` running `orch --headless --require-ac`).

## 17. Facts that matter most for a GUI

These are observations about the current design, stated so the proposal can
cite them; they are not recommendations.

1. **Files are the control plane.** Every human action that changes daemon
   behaviour is a `project.yaml` field write as `updated_by: agent`, or a new
   `project.md` / `intake/*.md` / `catchup-request` file (§3). A write as
   `daemon` is ignored. There is no API today.
2. **Some actions are not file writes.** `:archive` is performed by the TUI
   process itself; `:session`, `:orch`, `:workspace` are in-process daemon
   calls that open terminal tabs on the host; inbox answers are
   `postoffice.Store.Claim`.
3. **There is no task-level control.** No retry, cancel or re-run of a single
   task from the UI; retries are automatic (3 attempts) and anything else goes
   through the wolf.
4. **The snapshot is the closest thing to a read API**, but it omits per-task
   capability fields (`static_mcps`, `policies`, `model`, `resolved_model`,
   `harness`, `resolved_harness`), task descriptions and commits; those live in
   the task files. It is atomic, versioned, redacted, and rewritten within
   ~150 ms of any change, with a 15 s heartbeat.
5. **The TUI is not a passive viewer.** It sends every ACP session's opening
   prompt (`acpwatch.go:494-503`). Without a TUI watching (`--headless`),
   planning/task/commit/wolf sessions launch but sit idle
   (`agents.md:575-579`, `docs/channels.md:124-130`); only the workingman and
   step-up agents are primed by the daemon. Any GUI that replaces the TUI must
   take over this job, or the daemon must.
6. **Live sessions are reachable as unix sockets** speaking ACP JSON-RPC, with
   multi-client fan-out, `stream.log` replay, and an `acpchat.Adopt` path for a
   second client to join the same conversation. The sockets have no auth.
   Finished sessions' directories (and their logs) are deleted.
7. **Each sandbox already is a distinct, named workload** with a recorded
   harness image, model, MCP server set, policies, mounts and prompt files
   (§8.5); the data is split across the task file, `session.json`, the gateway
   record and the harness record.
8. **Least-privilege decisions are spread out**: the planner (per-task
   capabilities by prompt), hard-coded daemon grants (review, archive,
   step-up), the runner (dynamic gateways, mounts), and step-up (runtime
   escalation with operator approval). Tool permissions inside the sandbox are
   bypassed.
9. **Existing listeners are loopback and token-protected** (post-office) or
   filesystem-protected (ACP sockets); `orch` exposes no HTTP port of its own.

## 18. Where the docs and the code disagree

- `design.md` is the original spec: it lists statuses `done|ready|working|blocked`
  (code: `ready|working|blocked|idle|stopped|merged`, `done` is a legacy alias);
  says the daemon reacts to task-file changes (it only logs them; reactions come
  from session ends); says "dispatch one task" (up to `max_parallel_tasks` now);
  says agents run in terminal tabs with `--dangerously-skip-permissions` (they are
  ACP sandbox sessions by default); and calls the project agent interactive (it
  is autonomous, normally seeded by `project.md`).
- `agents.md` §7 still describes `status: reviewing`, `no_review` and `done`
  (`agents.md:704-710, 756-765`); none exist in code.
- `state-machine.md` refers to `dispatchReadyTask` (`:776`) and "dispatch the first
  ready task" (`:294-299`); both are now `schedule`/`schedulePass` with a commit queue.
- `docs/channels.md:129-130` suggests running `orch tui` to prime a headless wolf;
  standalone `orch tui` passes no sessions root and so has no ACP watcher
  (`cmd/orch/main.go:532`, `tui/tui.go:2957`). Only the integrated TUI primes sessions.
- Stale comments: `agent/agent.go:43` says the workingman agent mounts orch state
  read-only (the roots are writable, `workingman_agent.go:191-226`);
  `runner.go:297` calls the wolf "the one interactive kind" (sleepy-head is too);
  `project.go:164` and `tui/command_picker.go:147-150` still mention `done`/`reviewing`;
  `README.md:4` lists only six agents.
