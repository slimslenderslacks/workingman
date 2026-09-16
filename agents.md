# Agents

The orchestrator launches seven kinds of `claude` sessions over a project's
lifecycle: **project → planning → task → commit**, with **wolf**, **archive**,
and **review** stepping in for blocks, cleanup, and PR watching respectively.
`design.md` has the original spec; this document describes the agents as
implemented today, which has moved past that spec in places (most notably: an
ACP/sandbox launch path superseding the tmux+`sbx exec` path for non-interactive
agents, and a review agent design.md never mentions).

Every agent's role is defined by `agent.Kind` (`internal/agent/agent.go`).
`Kind.Interactive()` splits the seven into two launch families:

- **Autonomous** (project, planning, task, commit, review) — run under
  `claude --print` (single turn, then exit) as an **ACP session** backed by a
  per-session `acp-wrapper` host process (`internal/runner/runner.go:
  Runner.startACP`), when the daemon is wired with an `AcpLauncher` (production
  always is). The wrapper creates the sandbox (with `acp-kit` layered on),
  execs the ACP client, and serves `<SessionsRoot>/<id>/agent.sock` for the TUI
  to stream. There is no tmux window to attach to for these.
- **Interactive** (wolf, archive) — always take the legacy path: a tmux window
  inside the shared umbrella session `orch` (`internal/agent/tmux.go`), running
  `claude --dangerously-skip-permissions` (no `--print`) inside `sbx exec -it
  <sandbox>`, so a human can attach (`tmux attach -t orch`) and drive the
  conversation or answer a macOS notification.

Every launch, regardless of family, gets the same two-file handoff written
into its working directory by `internal/setup/setup.go` before the process
starts:

- `.orch/context.yaml` — the structured data (paths, branch, task, blocked
  reason, etc.) from `prompts.Data`/`setup.Context`.
- `.orch/instructions.md` — the rendered `internal/prompts/templates/<kind>.tmpl`.

The initial message piped to `claude` is always the same one-liner
(`internal/runner/runner.go: initialPrompt`):

> Read .orch/instructions.md and .orch/context.yaml, then follow the instructions.

Sandbox names are derived by `runner.SandboxNameFor` (`_` replaced with `-`
since sbx rejects underscores). Session/tmux/ACP-session names come from
`runner.sessionName`: `"<kind>-<task-name-or-branch-or-hash>"`.

---

## 1. Project agent

**Kind:** `agent.ProjectAgent`. **Template:** `project.tmpl`. **Interactive:** no.

**Trigger.** `internal/daemon/dispatch.go: dispatchProject` calls
`launchProjectRootAgent(path, agent.ProjectAgent, p)` whenever it observes an
"unpopulated" `.project.yaml` (`p.Unpopulated()`) — either the legacy
zero-byte file, or a `:new`-seeded file that has only `description:` set.

**Workspace/session.** No wsp workspace: `WorkingDir` is set directly to the
control directory holding `.project.yaml` (`filepath.Dir(projectPath)`), so
`Runner.resolveWorkingDir` skips workspace provisioning entirely. Sandbox name
is `"<work-stream>-project"` (`runner.SandboxNameFor`) — deliberately distinct
from planning's sandbox, since project and planning run back-to-back in the
same control dir and would otherwise collide on one sandbox with different
mounts. Runs via the ACP path (autonomous, `claude --print`).

**Reads/writes.** Reads the seed `description:` in `.project.yaml`. Writes the
fully populated `.project.yaml` — `repos`/`new_repos`, `branch`, optionally
`cron`/`cron_until`/`cron_max_runs`, `review`/`no_review` — with
`status: ready` and `updated_by: agent` on success, or `status: blocked` with
a specific `blocked_reason` when the description is genuinely insufficient
(the daemon then summons wolf). It is told not to write any other file.

**Prompt structure (`project.tmpl`).** Frames the job as: read the seed
description, then populate these fields autonomously — no interactive
interview:
- `description` (tighten wording, keep intent)
- `repos` (`org`/`name` pairs for existing repos to clone; `base_branch`
  defaults to `main` and is omitted when correct)
- `new_repos` (repos to create empty on the remote first; may set
  `visibility: public`)
- `branch` (explicit if named, else defaults to the project name)
- `cron` (only if the user asked for a schedule; requires a stop condition —
  `cron_until` or `cron_max_runs` — or the daemon blocks the project instead
  of scheduling it)
- `review` / `no_review` (whether project completion is gated on watching a PR)

It explicitly lists the fixed `status:` enum (`ready|working|blocked|done`)
and warns that any other value is rejected and the save silently won't take
effect.

**Completion signal / lifecycle.** `afterProjectSession`
(`dispatch_lifecycle.go`) is the crash-loop circuit breaker: if the file is
still unpopulated after the session ends, it retries up to `maxProjectRetries`
with backoff, then blocks (→ wolf) — this is a hard launch failure (`waitErr`
!= nil) is blocked immediately without consuming retry budget. Once populated
(`status: ready` or `status: blocked`), the counter resets and
`dispatchProject` re-routes based on the new status (ready → planning agent;
blocked → wolf).

---

## 2. Planning agent

**Kind:** `agent.PlanningAgent`. **Template:** `planning.tmpl`. **Interactive:** no.

**Trigger.** `dispatchProject`'s `switch p.Status` routes `status: ready` to
`launchProjectRootAgent(path, agent.PlanningAgent, p)`. This also fires after
a cron cycle re-arms a project (`requestCronReplan` sets `status: ready` +
`Replan: true`) and after a `:task` seed re-arms a resting `done`/`reviewing`
project (`hasPendingSeed`).

**Workspace/session.** Also runs with `WorkingDir` pinned to the control
directory (no wsp workspace of its own) — sandbox name is the bare
work-stream basename (`SandboxNameFor`, case `PlanningAgent`). But unlike
project/wolf, planning *also* provisions the project's wsp worktree
(`Runner.resolvePlanningWorktree`, idempotent — the later task agent's own
`Create` call returns the same directory) and mounts it as a **second**
sandbox workspace, so the planner can read real source to inform task
breakdown without cloning (the sandbox has no GitHub credentials). That path
is surfaced to the template as `{{.Worktree}}`.

**Reads/writes.** Reads `.project.yaml` and existing `tasks/*.yaml`. May
freely add/remove/edit files under `tasks/*.yaml` and edit `.project.yaml` —
nothing else. Finishes by setting `status: working` with `updated_by: agent`.

**Prompt structure (`planning.tmpl`).** Three main parts:
1. Task file schema and the *hard* naming constraint: `name` must be
   non-empty, lowercase kebab-case, unique, stable once referenced by a
   `depends_on`, and no longer than `62 - len(ProjectName)` characters (the
   sandbox name `<work-stream>-<task-name>` is an RFC-1123 DNS label capped at
   63 chars) — computed live via the template's `sub` function.
2. A conditional branch on `{{if .Replan}}`:
   - **Replan** (cron cycle): treat existing tasks as last cycle's record —
     for each, delete / reset-to-`ready` (clearing `summary`/`commits`/
     `created_files`/`completed_at`) / or leave `committed` (settled,
     once-only setup) — then add new tasks per the project `description`,
     which is the standing instruction, not the old task list.
   - **Else** (normal/incremental): if it finds a "seed" task (empty `name`,
     filled `description`, from a human's `:task`), flesh it out without
     touching any other existing task; otherwise do a first plan from scratch.
3. Optional per-task `static_mcps:` (sbx `--static-mcp` flags) and `policies:`
   (sbx policy rules, `action`/`type`/`resource`, applied in declaration order
   right after `sbx create`) fields, plus the fixed status enums for both
   project and task, and `model: default` on every new task.

**Completion signal / lifecycle.** `afterPlanningSession` mirrors the project
agent's circuit breaker (`maxPlanningRetries`, backoff, block on hard failure
or exhausted retries). Productive exit is "no longer `status: ready`" — the
daemon resets the counter, clears `Replan` (`clearReplan`, written as
`daemon`), and re-dispatches (`working` → first ready task).

---

## 3. Task agent

**Kind:** `agent.TaskAgent`. **Template:** `task.tmpl`. **Interactive:** no.

**Trigger.** `dispatchNextTask` → `dispatchReadyOrPending` →
`dispatchReadyTask` → `launchTaskAgent`, whenever a project is `status:
working` (or has pending manual tasks in `reviewing`/`done`) and has a task in
`Ready()` state in the DAG built by `internal/taskgraph`. A push task
(`t.IsPushTask()`) skips the task agent entirely and goes straight to the
commit agent (see below).

**Workspace/session.** *No* `WorkingDir` — `Runner.resolveWorkingDir` calls
`workspace.Manager.Create(ctx, branch, repos)` (wsp), provisioning/reusing a
workspace named after the project's `branch`, containing every repo in
`p.Repos` + `p.NewRepos` on that branch. Sandbox name is
`"<work-stream>-<task-name>"` — each task gets its **own** sandbox (so a
task's `static_mcps`/`policies` can differ from siblings), mounted with two
workspaces: the wsp worktree itself and the project's control/orch dir
(needed so the task agent's status write-back to `tasks/<name>.yaml` lands
somewhere that exists inside the sandbox).

**Reads/writes.** Reads its own `TaskPath` (task yaml) and the project file.
Does the actual code work in the workspace. Writes only `status:
success|failed` (+ `failure_reason` on failure) back to its task file — it
must NOT commit, push, or touch `.project.yaml`; that's the commit agent's and
daemon's job.

**Prompt structure (`task.tmpl`).** Short and direct — this is the exact text
this very orchestrator run received (see `.orch/instructions.md` in this
task's own sandbox): task name/paths up top, "read the description and do the
work," explicit instruction that even a git-no-op (a GitHub-API-only change)
should still exit `success` so the commit agent can finalize it, and the fixed
status enum with a call-out that `committed` from a task agent is an
invariant violation.

**Completion signal / lifecycle.** `afterTaskSession` re-reads the task file:
`success` → `launchCommitAgent`; `failed`/`running`/`ready` (agent crashed or
never started) → `handleTaskFailure`, which bumps `Attempts` and resets to
`ready` for a relaunch, up to `maxTaskAttempts` (3), after which it retains
the sandbox (`retainSandbox`, sets `save_sandbox: true` for post-mortem) and
blocks the project (→ wolf); `blocked` → retains the sandbox and blocks
immediately (no retry budget spent); `committed` → treated as an invariant
violation, blocks the project.

---

## 4. Commit agent

**Kind:** `agent.CommitAgent`. **Template:** `commit.tmpl`. **Interactive:** no.

**Trigger.** Two paths: (1) `afterTaskSession` on `status: success`; (2) the
"resume pending commit" recovery in `dispatchReadyOrPending` /
`afterCommitSession` / `dispatchNextTask`, which re-launches a commit agent
for any task stuck at `success` (covers a daemon restart between task-end and
commit-start). A push task (`t.IsPushTask()`) is dispatched straight here from
`dispatchReadyTask`, with `PushBranch: true`.

**Workspace/session.** Same shape as the task agent — no `WorkingDir`, so it
reuses/re-provisions the same wsp workspace, and shares the **same sandbox**
as the task it's committing (`SandboxNameFor` uses the same
`"<work-stream>-<task-name>"` for both `TaskAgent` and `CommitAgent`), forwarding
the task's `StaticMCPs`/`Policies`/`SaveSandbox` in case the commit happens to
be the sandbox's first launch (e.g. a retry after a crash).

**Reads/writes.** Reads the task file and does `git add`/`git commit` (and,
for a push task, `git push`) directly in each repo of the workspace, using
the git identity injected by the environment (never `git config
user.name`/`--author`). Writes `status: committed`, a `summary:`, a
`commits:` list (repo dir name + full SHA), and an optional `created_files:`
list back to the task file.

**Prompt structure (`commit.tmpl`).** Two modes gated by `{{if .PushBranch}}`:
- **Ordinary commit** (default): for every repo with modifications, stage,
  make one commit with a subject under 72 chars derived from the task
  description, capture the SHA — then write `status: committed` + `summary` +
  `commits`. Never push.
- **Push task** (review-fix batch publish): reframes the job as *publish, not
  write code* — push only `source.repo`'s working directory (or every
  ahead repo if `source.repo` is empty), via a plain `git push` (no
  `--force`, no `--set-upstream`), confirm `@{upstream}` now equals local
  `HEAD`. A rejected push sets `status: blocked` with a `blocked_reason`
  instead of forcing or falsely marking `committed`.

Both modes end with the same fixed status-enum reminder.

**Completion signal / lifecycle.** `afterCommitSession` requires `status:
committed`; anything else blocks the project for the wolf. On success it
stamps `completed_at` (once), reloads the task graph, and either
transitions the project to `done`/`reviewing` (`transitionProjectComplete`,
all committed) or dispatches the next ready task (`firstUncommittedSuccess`
first, then `g.Ready()`).

---

## 5. Wolf agent

**Kind:** `agent.WolfAgent`. **Template:** `wolf.tmpl`. **Interactive:** yes.

**Trigger.** `transitionProjectBlocked` (called from many failure paths —
task retries exhausted, taskgraph errors, hard launch failures, cron with no
stop condition, review-loop escalations, etc.) always ends by calling
`launchWolfAgent`. Also directly on `dispatchProject`'s `status: blocked`
branch (covers a block set by a human, the project agent, or a daemon
restart finding the file already blocked).

**Workspace/session.** `WorkingDir` = control dir (like project/planning), so
no wsp provisioning of its own, but `Repos`/`Branch` are still forwarded in
case the wolf needs to look at source — `SandboxNameFor` deliberately returns
`""` for `WolfAgent`: the wolf runs **outside any sandbox**, directly on the
host, so it can diagnose sandbox-related blocks too. It is tracked under a
session key distinct from the project's main slot
(`wolfSessionKey = projectPath + "#wolf"`), so it can run *in tandem* with a
task/commit/planning session rather than waiting for that slot to free up.
Being interactive, it takes the tmux path: a window inside the `orch` umbrella
session, `claude --dangerously-skip-permissions` with no `--print`, so the
process stays at the prompt for a human to drive.

**Reads/writes.** Reads the project's `blocked_reason`, the failed/blocked
tasks' `failure_reason`/`blocked_reason` (`FailedTasks`, paths gathered by
`failedOrBlockedTaskPaths`), and any prior durable diagnosis at
`project.BlockedSessionPath`. Writes/updates that same blocked-session record
(`blocked_reason`, `summary`, `attempted`) every time it investigates, and
ultimately updates `.project.yaml`/task files to move the project out of
`blocked`. Scoped strictly to this one project's files — never another
project's `.project.yaml`.

**Prompt structure (`wolf.tmpl`).** Opens with scope rules (only this
project's files), the blocked reason and failed-task list, and any inherited
prior summary/attempted list so a second wolf run doesn't re-derive
diagnosis from scratch. Then: "investigate... you may need input from the
user — send a macOS notification and wait for them to attach." Documents two
special cases: (1) a project blocked before it was ever populated (fill in
the missing fields the project agent couldn't infer, clear `blocked_reason`,
set `ready`); (2) a project blocked out of the PR-review loop (four specific
`blocked_reason` shapes from the review agent, and the instruction that the
"almost always" correct exit is back to `status: reviewing`, not `working` or
`done`). Ends with the fixed status enums for both project and task.

**Completion signal / lifecycle.** No enforced schema — the wolf just edits
whatever files resolve the block. When its tmux window closes,
`launchWolfAgent`'s `onEnd` callback calls `revisitProject`, which re-reads
`.project.yaml` from scratch and re-routes based on whatever status the wolf
left behind (bypassing the daemon-write filter, since wolf writes as
`updated_by: agent`).

---

## 6. Archive agent

**Kind:** `agent.ArchiveAgent`. **Template:** `archive.tmpl`. **Interactive:** yes.

**Trigger.** `dispatchProject` checks `p.Cleanup` *ahead of* all status
routing — set by the TUI's `:cleanup` — and calls `launchArchiveAgent`
regardless of current status, so it never runs concurrently with that
status's own agent (which would be committing into the very workspace archive
is trying to leave clean).

**Workspace/session.** No `WorkingDir`: like task/commit, `Repos`/`Branch`
are forwarded so `Runner` provisions/resolves the project's existing wsp
workspace. Sandbox name is `"<work-stream>-archive"` (its own, whole-project
sandbox, not reused from any task). Tracked under its own session key
(`archiveSessionKey = projectPath + "#archive"`), so a cleanup request can run
in tandem with the project's main-slot agent, deduped via a separate
`beginCleanup`/`endCleanup` in-flight guard (wider than the session's own
lifetime, to also cover the window before the `cleanup` flag is cleared).
Being interactive, it takes the tmux path (attachable, since it may need
`.gitignore`-edit approval).

**Reads/writes.** Per repo in the workspace: `git status --porcelain`,
classifies uncommitted files, optionally proposes (never applies without
approval) a `.gitignore` edit via a macOS notification + wait, makes at most
one final commit, and pushes only when `git rev-parse --abbrev-ref
--symbolic-full-name @{upstream}` and `git rev-list --count @{upstream}..HEAD`
together show real ahead-of-upstream work (never a hardcoded branch, never
`--force`/`--allow-empty`/`--set-upstream`). Writes back only `archive: true`
on full success — the project's `status:` field is left exactly as found,
since `archive` is an independent flag, not a status.

**Prompt structure (`archive.tmpl`).** A numbered procedure: (1) `git status`
per repo; (2) classify uncommitted paths, propose-and-wait for anything
"doesn't belong" (build artifacts, editor scratch, caches); (3) one commit if
needed; (4) push decision tree keyed strictly off the three shell variables
above, with an explicit "no workarounds" list; (5) report outcome by editing
`.project.yaml` (`archive: true` on full success, left unset with a clear
explanation of what's outstanding otherwise). Explicitly forbids any other
pre-commit work (no tests/lint/formatting/refactors).

**Completion signal / lifecycle.** `afterArchiveSession` always clears the
`cleanup` request flag first (written as `daemon`, whether or not `archive:
true` landed — a partial run logs `archive_incomplete` and is not retried
automatically; the user re-issues `:cleanup`), *then* calls `revisitProject`
so normal status routing resumes. The in-flight guard (`endCleanup`) is
released via `defer`, after the flag clear, so a late fsnotify event carrying
the agent's own `archive: true` write can't slip in and launch a second
archive session mid-cleanup.

---

## 7. Review agent

**Kind:** `agent.ReviewAgent`. **Template:** `review.tmpl`. **Interactive:** no.
*(Not in design.md — added to close the loop between "all tasks committed"
and a PR actually being merged.)*

**Trigger.** A project enters `status: reviewing` via
`transitionProjectComplete` (`internal/daemon/review.go`) once every task is
committed and either `p.Review` is set or the project has repos and hasn't
opted out (`p.NoReview`) and isn't an idle recurring-cron project. From there,
`ensureReviewPoll` registers an adaptive-backoff cron poll
(`reviewPollSpecs`: 2m/5m/15m/30m, keyed `projectPath + "#review"`) and kicks
an immediate reconcile. `onReviewPoll` re-fires it on each tick unless a cheap
host-side `gh pr view` fingerprint (head SHA + `updatedAt` + check rollup)
shows nothing changed since the last reconcile, in which case it only backs
off the cadence without launching an agent. A `:review` command
(`ReviewNow`) or a poll detecting change snaps the cadence back to the front
and dispatches immediately (`dispatchReviewAgent`, deduped against any
project-root session already running).

**Workspace/session.** `WorkingDir` = control dir (runs like planning — no
wsp workspace of its own), plus `Repos`/`Branch` forwarded, and
`StaticMCPs: []string{"github"}` plus a fixed network policy
(`reviewNetworkPolicies`: allow `api.github.com` and
`api.githubcopilot.com`) so the sandbox can reach GitHub's API through the
attached `github` MCP. `SandboxNameFor` gives it its own sandbox,
`"<work-stream>-review"`. Runs on the ACP path (autonomous, `claude --print`),
sharing the project's *main* session slot (not a side key like wolf/archive),
so it cannot run concurrently with a task/commit/planning agent for the same
project.

**Reads/writes.** Reads `.project.yaml` (`description`, `repos`, `branch`,
`review`, prior `pull_requests:`) and existing task files' `status`/`source`.
Uses the `github` MCP exclusively for PR access — explicitly forbidden from
running `gh`, `git push`, or cloning anything. Writes only within the control
dir: new task files (fix tasks and push tasks) and `.project.yaml` (the
`pull_requests:` list and `status:`). Never touches source, never commits.

**Prompt structure (`review.tmpl`).** A five-step reconcile procedure per
poll: (1) read the project's goal/state; (2) locate every open PR across the
project's repos for `{{.Branch}}`, record `pull_requests:`, and branch
immediately on "all merged/closed" (→ `done`) or "none open" (→ `done` unless
`review: true`, then stay `reviewing`); (3) for each still-open PR, gather
unresolved review threads and failed checks at the current head SHA only;
(4) disposition each signal against existing tasks correlated by
`source.kind`/`source.ref`/`source.repo` — reuse an in-flight or
already-published fix, create a new **commit-only fix task** (never pushes;
tagged with `source: {kind: pr-comment|pr-check, ref, repo, pr}`), reply and
resolve a thread that's out of scope, or escalate a contested comment/
non-converging check to `status: blocked` (→ wolf); (4b) batch
committed-but-unpublished fixes per repo into a **push task**
(`source.kind: pr-push`, `depends_on` every fix task it publishes — one push
task per repo, never dripped per-fix); (5) set the final status (`working` if
it created any task, `blocked` if it escalated, otherwise leave `reviewing`
unchanged). Ends with the two task-schema templates (fix / push) and the
fixed status enums, including the review-specific `reviewing` project state.

**Completion signal / lifecycle.** `afterReviewSession`
(`internal/daemon/review.go`) reads the resulting project status:
`reviewing` unchanged → clean cycle, reset the fix-cycle counter and store a
fresh PR fingerprint baseline; `working` → count a fix cycle
(`bumpReviewFixCycles`), unregister the poll while tasks run, and either
block (`maxReviewFixCycles` exceeded — churn without convergence) or
`revisitProject` to dispatch the new tasks; `done`/`blocked` → stop polling
and clear all review state, then `revisitProject`. A non-nil session
`waitErr` (the agent process itself failed, e.g. the github MCP sandbox
couldn't be created) is tracked separately (`bumpReviewErrors`) and, after
`maxReviewErrors` consecutive failures, blocks the project with a
diagnostic pointing at `sbx mcp ls`/`sbx mcp auth` rather than silently
retrying forever.
