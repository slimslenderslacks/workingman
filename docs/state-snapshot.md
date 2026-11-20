# Daemon state snapshot

An external agent (the future *workingman agent*) running in a sandbox can read
the orch roots (`<root>/<work-stream>/project.yaml`, `tasks/*.yaml`), the audit
log, and the ACP session dirs. It cannot see what only the daemon's **memory**
knows: which sessions are live per project, whether a wolf is in flight, the
planning/project/review failure counters, the PR-poll state. The state snapshot
publishes that, together with the on-disk project/task state and the audit
tail, as one versioned JSON document.

There are two ways to get it:

| | Daemon-published file | `orch status` |
|---|---|---|
| Needs a running daemon | yes | no (with `--root`) |
| Live in-memory state | yes | only when reading the daemon's file |
| Output | JSON file, rewritten atomically | text, or `--json` |

## Publishing: `--state-file`

```
orch --root ~/orch --audit-log ~/orch/audit.log [--state-file PATH|off]
```

- Default path: `<sessions-root>/../state/snapshot.json` — with the default
  sessions root that is `~/.workingman/state/snapshot.json`, next to the ACP
  session dirs. `--state-file off` disables publishing.
- The path **must be outside every `--root`**. The daemon watches the roots with
  fsnotify; a snapshot written inside them would be an event the daemon causes
  itself. `orch` refuses to start (`daemon: state file ... overlaps watched
  root`) rather than risk it. Snapshot reads/writes never touch the roots, so
  they cannot trigger the watcher or project dispatch.
- **Atomic**: written to a temp file in the same directory and `rename`d over
  the target, so a reader sees the previous complete file or the new one, never
  a partial write. Make sure the reader's sandbox can read that directory
  (mount `~/.workingman/state` read-only alongside `sessions`).
- **Refresh**: on a 2 s ticker, and (debounced ~150 ms) after every audit
  event, session start/end, and project/task/intake file event. A write is
  skipped when nothing but `generated_at` would change, except a heartbeat at
  least every 15 s — so `generated_at` doubles as a liveness signal. Per-project
  parsing is cached on file mtime/size, so an idle tick re-parses nothing.
- On clean shutdown the daemon writes one last snapshot with
  `daemon.state = "stopped"` and `live_state_available = false` (ACP sessions
  are detached, not killed, on shutdown, so those are listed from disk).
  A `running` snapshot whose `generated_at` is older than ~1 minute means the
  daemon died without cleaning up.
- **Redaction**: every string in the document is passed through a secret
  scrubber before it is written — GitHub/Slack/AWS/OpenAI-style tokens, JWTs,
  `Bearer`/`Basic` credentials, `user:pass@` URLs, PEM private keys, and any
  `name=value` / `name: value` whose name contains `token`, `secret`,
  `password`, `api_key`, `access_key`, `private_key`, `authorization` or
  `credential`. Matches become `[REDACTED]`. Redaction is best-effort pattern
  matching; do not rely on it as a substitute for keeping secrets out of audit
  lines and task text. Git commit SHAs are deliberately left alone.

## `orch status`

```
orch status [--json] [--root DIR]... [--audit-log FILE] [--sessions-root DIR]
            [--state-file FILE] [--audit-events N]
```

- **With `--root`** it reads the roots, the ACP session dirs and the audit log
  tail directly. This works with no daemon running. The result has
  `source: "offline"`, `live_state_available: false`, and a note saying so:
  `sessions` then lists ACP session dirs found on disk (`source: "disk"`), and
  the per-project `live` blocks (failure counters, wolf/review in-flight, poll
  state) are absent because that data cannot be recovered from disk.
- **Without `--root`** it prints the running daemon's published snapshot
  (`--state-file`, default as above), including live state. It warns when the
  snapshot is stale.
- `--json` prints the same document as the state file; otherwise a readable
  summary. The text adds, per project, a `tasks:` line that names the running
  tasks and splits `success` into `integrating` and `queued`, a `queue:` line,
  `verify checks:` counts, and per task its queue position, `waiting_on`,
  integration report and (until it is committed) worktree.

## Schema (version 1)

`version` is bumped only for breaking changes (renamed/removed/retyped fields).
New fields may be added without a bump, so readers should ignore unknown keys.
Optional fields are omitted when empty.

### Top level

| field | type | notes |
|---|---|---|
| `version` | int | `1` |
| `generated_at` | RFC3339 time (UTC) | when this document was built |
| `source` | string | `"daemon"` or `"offline"` |
| `live_state_available` | bool | true only for a running daemon's snapshot |
| `notes` | string[] | caveats, e.g. why live state is missing |
| `daemon` | object | absent when `source` is `offline` (see below) |
| `roots` | string[] | watched / scanned roots |
| `projects` | object[] | one per `project.yaml`, sorted by path |
| `sessions` | object[] | live sessions (daemon) or ACP dirs on disk (offline), oldest first |
| `audit_events` | object[] | last N audit events, oldest first (default 50) |

### `daemon`

| field | notes |
|---|---|
| `state` | `"running"` or `"stopped"` |
| `pid`, `started_at` | the daemon process |
| `workspace_manager` | `--workspace-manager` (`wsp`/`stub`) |
| `acp_kit`, `acp_image`, `acp_wrapper`, `sessions_root` | the matching flags (resolved sessions root) |
| `audit_log`, `state_file` | absolute paths |
| `headless` | `--headless` |
| `roots` | the `--root` list |

### `projects[]`

| field | notes |
|---|---|
| `work_stream` | basename of the project dir |
| `path` | absolute path of `project.yaml` |
| `status` | `ready`, `working`, `blocked`, `idle`, `stopped` (empty for an unpopulated seed) |
| `blocked_reason` | why a blocked project is blocked |
| `branch`, `description` | description truncated to 400 chars |
| `load_error` | set when `project.yaml` can't be parsed; most other fields are then empty |
| `review`, `review_now`, `cleanup`, `archive`, `replan` | the project-file flags |
| `hidden` | `hide: true` — filtered out of the TUI gallery; display only, dispatch is unaffected |
| `watching_pr` | idle **and** still watched for PR review |
| `cron`, `cron_active` | schedule and whether its stop condition hasn't tripped |
| `pull_requests[]` | `{repo, number, url, state}` — `state` is `open`/`merged`/`closed` |
| `task_counts` | object with a count for **every** task status (`ready`, `running`, `success`, `failed`, `blocked`, `committed`), zeros included. `running` may exceed 1 once a project runs tasks in parallel |
| `task_total` | sum of `task_counts` |
| `max_parallel_tasks` | effective task limit (`max_parallel_tasks`, default 1). Always present |
| `commit_queue` | names of the tasks waiting to be integrated onto the shared branch, head first: every non-push task at `success`, plus every push task at `ready` whose deps are all committed, ordered by `succeeded_at` (unstamped first, then name). Derived from the task files, so offline snapshots have it too. Always present (`[]` when empty) |
| `verify_checks` | `{run, failed, skipped}`: the pre-integration build check (`verify:`) since the daemon started. `run` counts every check that ran, `failed` those of them that failed, `skipped` integrations that skipped it (fast path or docs-only). Daemon snapshots only, and only for a project that sets `verify:` |
| `tasks[]` | one per task, sorted by name, below. A not-yet-named planning seed appears under its filename stem |
| `updated_at` | mtime of `project.yaml` |
| `live` | daemon-only block, below |

### `projects[].tasks[]`

| field | notes |
|---|---|
| `name`, `status`, `attempts` | as in the task file. `success` means queued for integration, not done |
| `failure_reason`, `blocked_reason` | as in the task file |
| `depends_on` | always present (`[]` when none) |
| `touches`, `areas` | the planner's independence hints, as on disk. `touches` is omitted when absent and `[]` when it declares that it touches nothing |
| `exclusive` | the task has no hints at all, so it never runs beside another task |
| `worktree`, `task_branch` | the task's own worktree root and branch (`wm/<project-branch>/<task>`). Omitted for a legacy task in the shared tree; `worktree` is cleared once the task's worktree is cleaned up after it is committed |
| `base` | `[{repo, hash}]`: the shared tips the worktree was cut from, re-stamped as the merge base at `success` |
| `succeeded_at`, `completed_at` | RFC3339: when the daemon saw the task at `success` (orders the queue) / at `committed` |
| `queue_position` | 1-based position in `commit_queue`; omitted when not queued |
| `waiting_on` | dependencies at `success`: finished, not integrated yet, so this task cannot start. Omitted when empty |
| `integration` | `{result, repo, files, attempts, landed}`: the latest failed attempt to land the task. `result` is `conflict`, `verify_failed`, `error` or `partial`; `files` are the conflicting paths; `attempts` the commit-agent retries; `landed` the tasks integrated since the task's `base`. A `ready` task with `integration` set is a conflict re-run |

### `projects[].live` (daemon snapshots only)

| field | notes |
|---|---|
| `sessions` | agent kinds running for this project, e.g. `["task","wolf"]`. A kind repeats when several sessions of it run (`["task","task","commit"]`), so use `running_tasks` to see which tasks run |
| `running_tasks` | names of the tasks a task agent is working on, sorted. Always present |
| `integrating` | name of the task whose commit agent is running; absent when none |
| `commit_reserved` | true from a commit agent's launch until the daemon has routed its outcome (relaunches included), which is longer than the session. The wolf must not commit on the shared branch while this or `integrating` is set |
| `wolf_in_flight` | a wolf agent is running |
| `cleanup_in_flight` | archive agent dispatched and its `cleanup` flag not yet cleared |
| `review_agent_in_flight` | a review agent run is active |
| `planning_failures`, `project_failures` | consecutive cycles that didn't advance the project (circuit breakers at 3) |
| `review_fix_cycles`, `review_errors` | consecutive review fix cycles (cap 10) / review-agent crashes (cap 5) |
| `review_poll` | `{schedule, backoff_index, has_baseline, baseline_source}` when a PR poll is armed. `backoff_index` is the rung on the 2m→4m→8m→16m→32m→1h ladder (0 = fastest): it steps up on every tick that shows no change — quiet ticks, and "unverified" ticks where the MCP fingerprint was unavailable (those still run the review agent) — and resets to 0 on a detected change or a `:review` kick. `has_baseline` is true once a fingerprint baseline exists (recorded after a clean reconcile, or seeded from the agent-recorded fingerprint in `project.yaml` after a restart); `baseline_source` is `mcp` (daemon GitHub MCP probe) or `agent` (agent-recorded fallback), omitted without a baseline |
| `cron_schedule` | cron spec registered with the scheduler |
| `deferred` | `{overlap, cap, power}`: why ready tasks were not started on the project's last scheduling pass. `overlap` names ready tasks held back because their hints overlap a task in flight, `cap` is true while the project waits for a global task token (`--max-parallel-tasks`), `power` while it waits for AC power. Absent when nothing is held back |

### `sessions[]`

| field | notes |
|---|---|
| `key` | daemon session-map key (absent for disk sessions). See the markers below |
| `kind` | `project`, `planning`, `task`, `commit`, `wolf`, `archive`, `review` |
| `work_stream`, `project_path`, `task` | what it works on (`task` only for task/commit agents) |
| `worktree` | task agents running in their own task worktree: the agent's working directory, so a reader can open the right checkout |
| `started_at` | when tracking began |
| `interactive` | only the wolf waits for a human; it is an ACP session too unless `--wolf-host`, in which case it has a `tab_name` and no `sandbox_name` |
| `source` | `"daemon"` or `"disk"` |
| `sandbox_name` | the `sbx` sandbox (ACP sessions) |
| `tab_name` | session name of the terminal tab a non-ACP session runs in (see `docs/terminal-tabs.md`) |
| `acp` | ACP-backed sessions: `{session_id, session_dir, socket_path, status}`; `socket_path` is `<session_dir>/agent.sock`, `status` is from `session.json` |

#### Session keys

| key | agent |
|---|---|
| `<path>` | the project's main slot: the project and review agents |
| `<path>#task:<name>` | the task agent working task `<name>`; one per task in flight |
| `<path>#commit` | the commit agent (the head of the project's commit queue) |
| `<path>#planning` | the planning agent, which runs outside the main slot so intake can be planned while task or commit agents work |
| `<path>#wolf` | the wolf |
| `<path>#archive` | the archive (cleanup) agent |
| `workingman-agent` | the workingman agent (no project) |
| `sleepy-head` | the sleepy-head agent (no project) |

`<path>` is the absolute `project.yaml` path. Task and commit agents have
slots of their own (`docs/parallel-task-worktrees.md` S2), so a project can
run several tasks at once. A reader looking for a project's sessions should
match `project_path`, or `<path>` as a prefix of `key`, rather than the exact
`key`. Task names never contain `#`, so the marker is always what follows the
last `#` (`task:<name>` for a task agent).

### `audit_events[]`

`{time, event, fields}` — the audit line `2026-… session_ended key=/x err="…"`
becomes `{"time": "2026-…", "event": "session_ended", "fields": {"key": "/x",
"err": "…"}}`. Values are redacted.

#### Worktree and integration events

Task events carry `path` (the `project.yaml`) and `task` (the task name).
The full contract, including events of later phases, is D3 in
`docs/parallel-task-worktrees.md`.

| event | extra fields |
|---|---|
| `worktree_created` | `worktree`, `branch`, `base` (`repo=sha,…`) |
| `worktree_reused` | `worktree`, `attempt`, `dirty` |
| `worktree_stale_moved` | `from`, `to` |
| `worktree_skipped_legacy` | `why`: the task ran in the shared tree |
| `worktree_create_error` | `repo`, `err`; followed by `task_released why=worktree_failed` and `project_blocked` |
| `worktree_removed` | `worktree`, `reason` |
| `task_worktree_remove_error` | `err` |
| `integration_started` | `queue_depth`, `base`, `onto` |
| `integration_succeeded` | `commits`, `fast_forward` |
| `integration_conflicted` | `repo`, `files`, `landed`, `attempts` |
| `integration_verify_check_passed` | `onto` |
| `integration_verify_check_failed` | `repo`, `onto`, `attempts` |
| `integration_verify_check_skipped` | `why` (`fast_path`, `docs_only`, `not_configured`) |
| `integration_retry` | `integration_attempts`, `err` |
| `integration_verify_failed` | `repo`, `hash`, `why`; followed by `project_blocked` |
| `integration_partial` | `published`, `repo`; followed by `project_blocked` |
| `task_retry`, `task_retry_exhausted` | `attempts`, `cause` (`failure`, `conflict`, `verify`; absent means `failure`) |
| `task_released` | `task_path`, `why` (`launch_failed`, `stopped`, `orphaned`, `adopted_session_ended`, `worktree_failed`) |

## Example

```json
{
  "version": 1,
  "generated_at": "2026-10-02T23:25:23Z",
  "source": "daemon",
  "live_state_available": true,
  "daemon": {"state": "running", "pid": 4242, "started_at": "2026-10-02T21:00:00Z",
             "workspace_manager": "wsp", "acp_kit": "…", "headless": false,
             "sessions_root": "/Users/me/.workingman/sessions",
             "state_file": "/Users/me/.workingman/state/snapshot.json",
             "audit_log": "/Users/me/orch/audit.log", "roots": ["/Users/me/orch"]},
  "roots": ["/Users/me/orch"],
  "projects": [{
    "work_stream": "my-feature", "path": "/Users/me/orch/my-feature/project.yaml",
    "status": "working", "branch": "my-feature",
    "review": false, "review_now": false, "cleanup": false, "archive": false,
    "replan": false, "hidden": false, "watching_pr": false, "cron_active": false,
    "pull_requests": [],
    "max_parallel_tasks": 1,
    "commit_queue": ["lint"],
    "task_counts": {"ready": 0, "running": 1, "success": 1, "failed": 0, "blocked": 0, "committed": 2},
    "task_total": 4,
    "tasks": [
      {"name": "build", "status": "running", "attempts": 1, "depends_on": [], "exclusive": true,
       "worktree": "/Users/me/ws/my-feature/.worktrees/build", "task_branch": "wm/my-feature/build",
       "base": [{"repo": "widgets", "hash": "3f2a…"}]},
      {"name": "lint", "status": "success", "attempts": 0, "depends_on": [], "touches": ["widgets/.golangci.yml"],
       "exclusive": false, "worktree": "/Users/me/ws/my-feature/.worktrees/lint", "task_branch": "wm/my-feature/lint",
       "base": [{"repo": "widgets", "hash": "3f2a…"}], "succeeded_at": "2026-10-02T23:10:00Z", "queue_position": 1}
    ],
    "live": {"sessions": ["task"], "running_tasks": ["build"], "commit_reserved": false, "wolf_in_flight": false, "cleanup_in_flight": false,
             "review_agent_in_flight": false, "planning_failures": 0, "project_failures": 0,
             "review_fix_cycles": 0, "review_errors": 0}
  }],
  "sessions": [{"key": "/Users/me/orch/my-feature/project.yaml", "kind": "task",
                "work_stream": "my-feature", "task": "build", "source": "daemon",
                "worktree": "/Users/me/ws/my-feature/.worktrees/build",
                "started_at": "2026-10-02T23:20:00Z", "interactive": false,
                "sandbox_name": "my-feature-build",
                "acp": {"session_id": "task-my-feature-build-1",
                        "session_dir": "/Users/me/.workingman/sessions/task-my-feature-build-1",
                        "socket_path": "/Users/me/.workingman/sessions/task-my-feature-build-1/agent.sock",
                        "status": "running"}}],
  "audit_events": [{"time": "2026-10-02T23:20:00Z", "event": "acp_session_started",
                    "fields": {"kind": "task", "session_id": "task-my-feature-build-1"}}]
}
```

## Implementation map

- `internal/daemon/snapshot.go` — model, project/task collector (stat-signature
  cache), offline builder, redaction walk.
- `internal/daemon/snapshot_live.go` — `--state-file` plumbing, the publish
  loop, in-memory state capture, atomic write.
- `internal/daemon/snapshot_text.go` — text rendering, `ReadSnapshot`.
- `internal/audit` — in-memory audit tail, line parser, `TailFile`, `Redact`.
- `cmd/orch/status.go` — `orch status`.
