# Daemon state snapshot

An external agent (the future *workingman agent*) running in a sandbox can read
the orch roots (`<root>/<work-stream>/.project.yaml`, `tasks/*.yaml`), the audit
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
  summary.

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
| `projects` | object[] | one per `.project.yaml`, sorted by path |
| `sessions` | object[] | live sessions (daemon) or ACP dirs on disk (offline), oldest first |
| `audit_events` | object[] | last N audit events, oldest first (default 50) |

### `daemon`

| field | notes |
|---|---|
| `state` | `"running"` or `"stopped"` |
| `pid`, `started_at` | the daemon process |
| `workspace_manager` | `--workspace-manager` (`wsp`/`stub`) |
| `acp_kit`, `acp_image`, `acp_wrapper`, `sessions_root`, `tmux_session` | the matching flags (resolved sessions root) |
| `audit_log`, `state_file` | absolute paths |
| `headless` | `--headless` |
| `roots` | the `--root` list |

### `projects[]`

| field | notes |
|---|---|
| `work_stream` | basename of the project dir |
| `path` | absolute path of `.project.yaml` |
| `status` | `ready`, `working`, `blocked`, `idle`, `stopped` (empty for an unpopulated seed) |
| `blocked_reason` | why a blocked project is blocked |
| `branch`, `description` | description truncated to 400 chars |
| `load_error` | set when `.project.yaml` can't be parsed; most other fields are then empty |
| `review`, `review_now`, `cleanup`, `archive`, `replan` | the project-file flags |
| `hidden` | `hide: true` — filtered out of the TUI gallery; display only, dispatch is unaffected |
| `watching_pr` | idle **and** still watched for PR review |
| `cron`, `cron_active` | schedule and whether its stop condition hasn't tripped |
| `pull_requests[]` | `{repo, number, url, state}` — `state` is `open`/`merged`/`closed` |
| `task_counts` | object with a count for **every** task status (`ready`, `running`, `success`, `failed`, `blocked`, `committed`), zeros included |
| `task_total` | sum of `task_counts` |
| `tasks[]` | `{name, status, attempts, failure_reason, blocked_reason, depends_on[]}`, sorted by name. A not-yet-named planning seed appears under its filename stem |
| `updated_at` | mtime of `.project.yaml` |
| `live` | daemon-only block, below |

### `projects[].live` (daemon snapshots only)

| field | notes |
|---|---|
| `sessions` | agent kinds running for this project, e.g. `["task","wolf"]` |
| `wolf_in_flight` | a wolf agent is running |
| `cleanup_in_flight` | archive agent dispatched and its `cleanup` flag not yet cleared |
| `review_agent_in_flight` | a review agent run is active |
| `planning_failures`, `project_failures` | consecutive cycles that didn't advance the project (circuit breakers at 3) |
| `review_fix_cycles`, `review_errors` | consecutive review fix cycles (cap 10) / review-agent crashes (cap 5) |
| `review_poll` | `{schedule, backoff_index, has_baseline}` when a PR poll is armed; `backoff_index` 0 is the fastest rung |
| `cron_schedule` | cron spec registered with the scheduler |

### `sessions[]`

| field | notes |
|---|---|
| `key` | daemon session-map key: the `.project.yaml` path, plus `#wolf` / `#archive` for those agents (absent for disk sessions) |
| `kind` | `project`, `planning`, `task`, `commit`, `wolf`, `archive`, `review` |
| `work_stream`, `project_path`, `task` | what it works on (`task` only for task/commit agents) |
| `started_at` | when tracking began |
| `interactive` | wolf/archive wait for a human (the wolf is also an ACP session unless `--wolf-host`; it then has a `sandbox_name` and no `tmux_target`) |
| `source` | `"daemon"` or `"disk"` |
| `sandbox_name` | the `sbx` sandbox (ACP sessions) |
| `tmux_target` | tmux session name (non-ACP sessions) |
| `acp` | ACP-backed sessions: `{session_id, session_dir, socket_path, status}`; `socket_path` is `<session_dir>/agent.sock`, `status` is from `session.json` |

### `audit_events[]`

`{time, event, fields}` — the audit line `2026-… session_ended key=/x err="…"`
becomes `{"time": "2026-…", "event": "session_ended", "fields": {"key": "/x",
"err": "…"}}`. Values are redacted.

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
    "work_stream": "my-feature", "path": "/Users/me/orch/my-feature/.project.yaml",
    "status": "working", "branch": "my-feature",
    "review": false, "review_now": false, "cleanup": false, "archive": false,
    "replan": false, "hidden": false, "watching_pr": false, "cron_active": false,
    "pull_requests": [],
    "task_counts": {"ready": 1, "running": 1, "success": 0, "failed": 0, "blocked": 0, "committed": 2},
    "task_total": 4,
    "tasks": [{"name": "build", "status": "running", "attempts": 1, "depends_on": []}],
    "live": {"sessions": ["task"], "wolf_in_flight": false, "cleanup_in_flight": false,
             "review_agent_in_flight": false, "planning_failures": 0, "project_failures": 0,
             "review_fix_cycles": 0, "review_errors": 0}
  }],
  "sessions": [{"key": "/Users/me/orch/my-feature/.project.yaml", "kind": "task",
                "work_stream": "my-feature", "task": "build", "source": "daemon",
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
