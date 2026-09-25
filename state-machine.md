# The state machines

This document describes the two state machines the daemon (`internal/daemon`)
actually drives today: the **project status** machine and the **task status**
machine. `design.md` is the original spec and `README.md` is the user-facing
guide — both are useful background, but each has drifted from the
implementation in places. Every transition described here is grounded in the
current Go source; where an older doc describes something that isn't
implemented, this doc says so explicitly instead of carrying the discrepancy
forward.

Source of truth for the enums:

- Project status — `internal/project/status.go`
- Task status — `internal/task/task.go`

Source of truth for the transition logic — `internal/daemon/dispatch.go`,
`dispatch_lifecycle.go`, `intake.go`, `repo_sync.go`, `reconcile.go`,
`watcher.go`, `review.go`, `stop.go`, and `reaper.go`.

## 1. Project status

### Diagram

```mermaid
stateDiagram-v2
    [*] --> unpopulated: project.md dropped\n(daemon seeds .project.yaml)
    unpopulated --> ready: project agent populates the file
    unpopulated --> blocked: project agent fails/escalates

    ready --> working: planning agent writes tasks

    working --> working: task dispatch loop\n(see task status machine)
    working --> idle: all tasks committed

    idle --> working: unresolved review comment\nor failed check → new task\n(only when watching a PR)
    idle --> blocked: contested comment escalated\nto wolf\n(only when watching a PR)
    idle --> ready: cron fires, or a pending\nintake file / task seed re-arms

    blocked --> ready: wolf/human unblocks (replan)
    blocked --> working: wolf/human unblocks (resume tasks)
    blocked --> idle: wolf resolves a review-loop\nescalation, or decides work\nis no longer needed

    note right of unpopulated
      "unpopulated" is Status == ""
      (Project.Unpopulated), not a
      member of the Status enum.
    end note

    note right of idle
      idle covers what used to be two
      statuses ("done" and "reviewing").
      Whether an idle project is also
      being watched for PR review is a
      property of its DATA (review: true,
      or an unresolved pull_requests entry
      — see Project.WatchingPR), not a
      separate status value.
    end note
```

`status: stopped` is a human-requested pause layered on top of any of the
above — see [§4](#4-stopped-a-pause-not-a-normal-transition). `cleanup: true`
(→ archive agent) is a separate orthogonal side-flow — see
[§3](#3-the-cleanup--archive-side-flow-orthogonal-to-status).

### The enum

`internal/project/status.go` defines: `ready`, `working`, `blocked`, `idle`,
`stopped`. There is also the "unpopulated" case: `Status("")`, which is
**not** one of the five valid enum values but is accepted by
`UnmarshalYAML` as a sentinel — `Project.Unpopulated()` reports `Status ==
""`. A project.md-derived seed (description only, no status) and a legacy
empty file both present this way.

`idle` merges what used to be two separate statuses, `done` and `reviewing`.
The daemon no longer needs a distinct status value to know whether an idle
project is still being watched for PR review — that's answered by
`Project.WatchingPR()` (`internal/project/project.go`), which is true when
either `review: true` is set (a human/agent declared the goal PR-shaped, even
before a PR exists) or at least one entry in `pull_requests:` hasn't reached
a terminal (`merged`/`closed`) state. This mirrors how "does this idle
project wake itself up on a timer" was already answered by whether `cron:` is
set, rather than by a status value — `idle` is the one resting state, and
what (if anything) is watching it is read from the project's own data, not
chosen from an enum.

### unpopulated → ready / blocked (project bootstrap)

A new project starts by dropping a `project.md` file (a free-form
description of the work) into `<root>/<project-name>/`. The daemon's
`handleIntakeFile`'s sibling, `handleProjectSeed`
(`internal/daemon/dispatch.go`), notices it and converts its content into a
description-only `.project.yaml` seed — a file with `description:` set but no
`status:`, i.e. `Unpopulated()`. From there `dispatchProject` checks
`p.Unpopulated()` first and launches the **project agent**
(`launchProjectRootAgent`). The project agent's job is to turn that seed into
a populated file (setting `status: ready`) or, if the description isn't
actionable, escalate straight to `status: blocked`.

The session-end callback is `afterProjectSession`
(`internal/daemon/dispatch.go`), which is also this handoff's crash-loop
circuit breaker:

- File now populated (status set) → productive; the failure counter for this
  project is cleared and `dispatchProject` runs on the new state.
- Still unpopulated, the agent process itself failed to start/run
  (`waitErr != nil`) → an environment error retrying can't fix, so
  `transitionProjectBlocked` fires immediately, no retry consumed.
- Still unpopulated, clean exit but no progress → retried with backoff
  (`backoffPlanning`, shared with planning's retry step) up to
  `maxProjectRetries` (3), then blocked.

### ready → working (planning)

`dispatchProject`'s `switch p.Status` routes `StatusReady` to
`launchProjectRootAgent(..., agent.PlanningAgent, ...)`. The planning agent
writes `tasks/*.yaml` and is expected to flip the project to `status:
working` when done.

Before that launch, `syncPlannedRepos` (`internal/daemon/repo_sync.go`) runs
once: it loads `last-planned.yaml` (`project.PlannedSnapshot`, a sidecar file
next to `.project.yaml` — see `internal/project/planned_snapshot.go`), the
daemon's record of the project's intent fields (`description`, `repos`,
`new_repos`, `branch`, `cron`) as of the last successful plan, and diffs it
against the live file:

- Any repo present now that wasn't in the snapshot is cloned into the
  **already-provisioned** wsp workspace via `workspace.Manager.AddRepos`
  (`wsp repo add`, run with the workspace directory as its cwd) — the
  counterpart to `Create`, which is a no-op once a workspace exists and so
  can never pick up a repo added after the fact on its own. A project whose
  workspace predates this snapshot mechanism (no `last-planned.yaml` yet, but
  the workspace already exists) is *adopted*: every current repo is sent
  through `AddRepos` once unconditionally (`wsp repo add` is confirmed
  idempotent for a repo already present, so this is safe) so nothing the
  original `Create` call never saw is silently missed forever.
- Every changed intent field (not just repos) is rendered as a human-readable
  line and forwarded into the planning prompt as `runner.Plan.ProjectChanges`,
  so the agent knows what prompted this invocation instead of guessing.
- A failed `AddRepos` call is logged (`repo_sync_error`) and the snapshot's
  repo lists are deliberately **not** advanced, so the same addition is
  retried on the next planning cycle rather than silently dropped.
- On a genuinely first-ever plan (no snapshot, no workspace yet) this is a
  no-op beyond seeding the snapshot — `Create` is about to clone everything
  listed, so there's nothing to diff or add yet.

The daemon also scans `<project>/intake/*.md` (`pendingIntakeFiles`,
`internal/daemon/intake.go`) at launch time and forwards the list as
`runner.Plan.IntakeFiles` — see [`ready → working`, intake requests
below](#intake-requests-and-task-seeds) for the full contract.

`afterPlanningSession` (`internal/daemon/dispatch.go`) is the mirror-image
circuit breaker of `afterProjectSession`, with the same three-way decision
table and its own counter, `maxPlanningRetries` (3), backed by
`backoffPlanning` (delay = `attempt * planningBackoffStep`, capped at
`planningBackoffMax`; `planningBackoffStep = 2s`, `planningBackoffMax = 30s`).
On success, `afterPlanningSession` also clears any pending `Replan` flag
(`clearReplan`) before dispatching — the run that just finished is what
satisfied the replan request.

#### Intake requests and task seeds

A human queues new work into an existing project by dropping a markdown file
into `<project>/intake/`. `handleIntakeFile` (`internal/daemon/intake.go`)
flips the project to `status: ready` (clearing any `blocked_reason`) so
planning runs; the planning agent itself reads each pending file directly
(via `.IntakeFiles`, listed in its prompt) and is responsible for creating the
task(s) it describes and renaming the file to `*.processed` once done, so it
is never offered again. This is deliberately simpler than the old
`:task`-command design it replaced: the daemon does no on-disk seed
conversion, and the intake file itself — not yet renamed — is the durable
record of "there is unplanned work here."

A hand-written **task seed** (a task file with a blank `name:` but a filled-in
`description:`) is still honored as a legacy path: `taskgraph.Load`
(`internal/taskgraph/graph.go`) excludes it from the graph entirely (it has no
name and no place in the DAG yet) but counts it via
`HasPendingSeed()`/`hasPendingIntake()`, which `dispatchProject` uses to
re-arm planning for an **idle** project whose intake/seed-triggered `ready`
flip was skipped or clobbered while another agent held the project slot (the
orphan-recovery check, right before the main status switch).

### working → task dispatch loop → idle

`StatusWorking` routes to `dispatchNextTask`
(`internal/daemon/dispatch.go`), which loads the task graph
(`internal/taskgraph`) for the project and:

1. If `g.AllCommitted()` → `transitionProjectComplete` (see below).
2. Else if some task is stuck at `status:success` (task agent finished, no
   commit agent has run yet — the interrupted task→commit recovery case) →
   resume by launching the commit agent for it.
3. Else if `g.Ready()` is non-empty → dispatch the first ready task.
4. Else → log `no_ready_tasks` and wait (nothing dispatchable right now).

The full task-level loop that fills in step 2/3 is the subject of
[§2](#2-task-status). Once every task in the graph reaches `committed`,
`transitionProjectComplete` (`internal/daemon/review.go`) decides whether the
project settles idle-and-watching or idle-and-not-watching:

```go
func (d *Daemon) transitionProjectComplete(projectPath string, p *project.Project) {
    if d.scheduler != nil && p.WatchingPR() {
        d.transitionProjectIdleWatching(projectPath, p)
        return
    }
    d.transitionProjectIdle(projectPath, p)
}
```

Both branches write `status: idle` — the only difference is whether
`ensureReviewPoll` gets called. `p.WatchingPR()` is true when either
`review: true` was set explicitly (a human/agent said "this is PR-shaped") or
`pull_requests:` already has an unresolved entry (a fix-cycle return through
here must keep watching the same PR, not drop it). Absent both, the project
settles plain idle — there is no automatic probing for a PR that might exist;
a human starts the loop later via `:review`. The PR-resolution loop is
poll-driven (`d.scheduler`), so a scheduler-less daemon (tests, or a
deliberately simplified deployment) never arms it and keeps the plain-idle
behavior regardless of `WatchingPR()`.

### idle-and-watching → working / blocked / idle (the PR-resolution loop)

`transitionProjectIdleWatching` (`internal/daemon/review.go`) writes
`status: idle` and calls `ensureReviewPoll`, which registers the project's
`#review` scheduler entry and kicks an immediate review-agent run.

The poll (`onReviewPoll`) uses a **change-detection gate** to avoid running
an expensive review agent on every tick: `reviewPRFingerprint` hashes each
watched PR's `headRefOid`, `updatedAt`, and `statusCheckRollup` (via `gh pr
view`, no sandbox needed). If the fingerprint is unchanged since the last
reconcile, the tick is "quiet" — the cadence steps to the next, slower rung
and the review agent is *not* launched. A changed (or not-yet-known)
fingerprint runs the agent and snaps the cadence back to the fastest rung.
The backoff ladder, `reviewPollSpecs`, is:

```
@every 2m → @every 5m → @every 15m → @every 30m (capped)
```

A `:review` command on an already-watching project sets `ReviewNow`, which
`forceReviewNow` uses to bypass the gate and dispatch immediately, resetting
the cadence. `:review` on a plain-idle project instead sets `review: true` to
start the loop.

`afterReviewSession` (`internal/daemon/review.go`) is where the loop's
outcome is decided. The review agent always leaves the project at exactly one
of three statuses, but since `idle` no longer distinguishes "still watching"
from "done" by itself, `afterReviewSession` re-derives that from
`WatchingPR()`:

- **`idle`, still watching** (`WatchingPR()` true) → the PR is clean this
  cycle (or there's no PR yet with `review: true` still watching). The
  fix-cycle counter resets, a fresh fingerprint baseline is recorded, and
  polling continues (already armed; nothing more to do).
- **`working`** → the review agent turned an unresolved review comment or a
  failed check into one or more new tasks. The poll is unregistered while
  those tasks run (`clearReviewPolling`; a fresh baseline is taken only once
  the project returns to idle-and-watching), and a **fix-cycle counter**
  (`reviewFixCycles`) is bumped. If it exceeds `maxReviewFixCycles` (10)
  *consecutive* fix cycles that never converged back to a clean poll, the
  project is blocked instead ("PR review produced N consecutive fix cycles
  without converging; needs a human"). Otherwise `revisitProject` dispatches
  the new tasks per §2.
- **`idle`, not watching, or `blocked`** → the loop is over (every PR merged
  or closed and `review` isn't forcing further probing, or the review agent
  escalated a contested comment it can't resolve on its own). Polling stops,
  review state (`reviewFinger`/`reviewBackoff`/`reviewFixCycles`/
  `reviewErrors`) is cleared, and the project's new status is routed
  normally (idle → nothing further; blocked → wolf).

A non-nil `waitErr` (the review agent's *process* failed — most commonly its
sandbox couldn't be created, e.g. a github MCP auth problem) is **not**
treated as a clean "nothing to do" tick: it would otherwise be
indistinguishable from an unchanged idle-and-watching state. Consecutive
crashes are counted (`reviewErrors`) and once they reach `maxReviewErrors` (5)
the project is blocked with a diagnostic pointing at the likely cause.

While a project sits idle-and-watching, a human (or another agent) can still
queue manual tasks. `dispatchProject`'s `StatusIdle` case calls
`dispatchPendingTasks` first — if there's pending task work it dispatches
that (using the project's single agent slot; the project stays idle) and
returns before touching the poll. Only when there's nothing pending, and only
when `WatchingPR()` is true, does it fall through to (re-)arming the
`#review` poll.

### blocked → wolf → ready / working / idle

`StatusBlocked` routes to `launchWolfAgent`
(`internal/daemon/dispatch_lifecycle.go`). The wolf runs as an **interactive**
agent (`internal/daemon/reaper.go: strandedVerdict` explicitly never reaps it
on an idle timeout — it may sit waiting on a human indefinitely) under its
own session key (`wolfSessionKey`, a `#wolf` suffix) so it can run in tandem
with whatever else the project's main slot is doing, and it dedups against
that key rather than the project's.

`launchWolfAgent` reads back a durable `blocked-session.yaml` record
(`project.BlockedSessionPath`, `internal/project/blocked_session.go`) if a
prior wolf invocation left one, and folds its `Summary`/`Attempted` fields
into the new session's context — so a repeat invocation of the same block (or
a fresh one) starts from what was already learned rather than re-deriving
context from scratch. The wolf itself decides how to resolve the block, and
the session-end callback simply `revisitProject`s — the daemon has no
wolf-specific routing; whatever status the wolf leaves on disk is dispatched
exactly like any other observation. Per its prompt
(`internal/prompts/templates/wolf.tmpl`), the wolf's exit status depends on
what kind of block it was resolving:

- A project blocked before it was ever populated (no tasks, empty
  `repos`/`branch`) → gather the missing details from the user, fill in the
  project fields, and set `status: ready` so planning can run.
- A review-loop escalation (contested comment, a check that won't converge,
  too many fix cycles, or a wedged review-agent sandbox) → almost always
  back to `status: idle` with `review`/`pull_requests` left exactly as they
  were — resuming the PR watch is a side effect of that data being intact
  when the daemon next observes the project idle, not a status the wolf
  chooses, and it lets the review agent re-derive fix tasks itself rather
  than the wolf hand-authoring them. `working` is only correct if the wolf
  deliberately wants to author and dispatch fix tasks itself; clearing
  `review`/leaving every PR merged-or-closed (with `status: idle`) only if
  the PR work is genuinely finished and no more watching is wanted.
- Any other block (e.g. a task's retries exhausted) → typically `ready` (fresh
  plan) or `working` (resume the task graph) after fixing whatever the
  individual task/project files needed.

Any project-root agent (project, planning, task, commit) that fails to
*launch* also routes here via `transitionProjectBlocked`, as does a
`taskgraph.Load` structural error (unknown dependency, cycle, duplicate
name — see §2) and a task's retry-budget exhaustion. `transitionProjectBlocked`
persists `blocked_reason`, notifies the user, unregisters the `#review` poll
if one was running, clears in-memory review state, and launches the wolf —
uniformly, regardless of which caller triggered the block.

### idle and the cron-driven recurring cycle

`idle` is terminal for the *automatic* cycle in the non-cron case, but two
things keep it from being a dead end:

- **Manual tasks after completion.** `dispatchProject`'s `StatusIdle` case
  still calls `dispatchPendingTasks` — a human can append tasks to a finished
  project and they still run; a fully-committed graph is a no-op so a truly
  finished project stays idle (this path never re-runs the completion
  transition, so it can't spuriously start or drop a PR watch).
- **Cron replan.** A project with a live `cron:` schedule treats idle as its
  *resting* state between cycles, not a final state. See below.

Cron scheduling is registered per-project by `registerCronIfAny`
(`internal/daemon/dispatch.go`), called from `dispatchProject` before status
routing runs. Every cron schedule must declare a stop condition —
`cron_until` (an RFC3339 deadline) or `cron_max_runs` (a cycle count,
`project.CronMaxRuns`/`CronRuns`) — checked by `Project.CronUnbounded()`; a
schedule with neither is refused and the project is immediately blocked so a
human fixes the file (no default deadline is invented). A schedule whose stop
condition has already tripped (`CronStopReason()` non-empty) is not
registered (and any stale registration is dropped), so a daemon restart can't
revive an expired schedule.

The recurring cycle, driven by `onCronFired`
(`internal/daemon/dispatch.go`) on each firing:

1. **Entry check** — if the stop condition is already met on disk, unregister
   and stop; no work is attempted.
2. **`requestCronReplan`** decides whether this firing should start a new
   cycle. It only rewrites the project (`status: ready`, `replan: true`) when
   the project is **idle and not watching a PR** (the recurring case: last
   cycle finished, start the next) or **`status == ready`** (planning hasn't
   run yet, or is being retried; treat this firing as a re-plan request).
   Every other case is left alone on purpose:
   - `working` — tasks are running; rewriting the status mid-flight would
     re-plan the graph out from under live task agents. The firing falls
     through to the ordinary recovery poll instead.
   - `blocked` — needs a human/wolf; re-planning would silently paper over
     the failure and lose `blocked_reason`.
   - `idle` **and** `WatchingPR()` — a PR is still open, or one is expected.
     Re-planning out from under an active review loop would strand it, so
     this is treated the same as "not idle" here even though the status
     value is the same as the truly-resting case.
   - `cleanup: true` or `archive: true` — the project is being wound down;
     nothing to re-plan.
   A firing that starts no cycle logs `cron_run_not_counted` and still calls
   `revisitProject` (the poke is useful even without a full cycle) but is
   **not** charged against `cron_max_runs`.
3. **Count** — only a firing that *did* start a cycle (`requestCronReplan`
   returned `""`) is charged: `countCronRun` re-reads the file (to avoid
   clobbering the replan write just made) and increments/persists
   `cron_runs`.
4. **Exit check** — the stop condition is re-evaluated against the
   post-increment count; if it now trips, the schedule unregisters. This is
   why the Nth run still completes its cycle: the schedule stops *after* the
   Nth run, not instead of it.

`transitionProjectIdle` (`internal/daemon/dispatch_lifecycle.go`) is the
counterpart that must NOT unregister a live recurring schedule: it only
unregisters cron when the project isn't recurring (`Cron == ""`) or the
schedule has expired (`CronExpired()`) — otherwise idle would be a one-shot
dead end for every cron project. The `#review` poll, by contrast, is always
unregistered when a project settles idle-and-not-watching — the PR loop (if
any) is finished regardless of cron.

## 2. Task status

### Diagram

```mermaid
stateDiagram-v2
    [*] --> ready: planning agent creates task
    ready --> running: task agent dispatched\n(dependencies all committed)
    running --> success: task agent reports success
    running --> failed: task agent reports failure,\ncrashes, or is reaped
    running --> blocked: task agent reports blocked

    failed --> ready: attempts < 3, retried
    failed --> [*]: attempts >= 3,\nproject → blocked (wolf)

    success --> committed: commit agent commits
    committed --> [*]: next ready task dispatched,\nor project completes

    blocked --> [*]: project → blocked (wolf),\nno automatic retry

    note right of running
      A session-end callback that finds
      status still "running" or "ready"
      (agent crashed / never started)
      is handled identically to "failed".
    end note
```

### The enum

`internal/task/task.go` defines: `ready`, `running`, `success`, `failed`,
`blocked`, `committed`. Unlike project status there is no "unpopulated"
sentinel for tasks — a task file with a blank `name` and a non-blank
`description` is instead a **pending seed** (`internal/taskgraph/graph.go:
Load`), excluded from the graph entirely (not even loaded as a `Task`) until
the planning agent gives it a real name. `taskgraph.Graph.HasPendingSeed()`
exposes this so the daemon can re-arm planning for an orphaned seed sitting
on a resting (`idle`) project (`dispatchProject`'s pending-seed re-arm check,
`internal/daemon/dispatch.go`).

### ready → running (dispatch)

The **task graph** (`internal/taskgraph/graph.go`) is a pure snapshot of
`tasks/*.yaml`, computed fresh from disk every time the daemon needs it — it
is never cached across dispatch decisions. `Graph.Ready()` returns tasks
whose `status == ready` **and** every dependency (`depends_on`) is
`committed`; a task already running or finished is never re-included
regardless of its dependency state.

`dispatchReadyOrPending` (`internal/daemon/dispatch.go`) picks the first
`Ready()` task (deterministic name order) and hands it to
`dispatchReadyTask`, which special-cases **push tasks**
(`task.IsPushTask()`, `Source.Kind == "pr-push"`): a push task carries no code
work of its own — it exists only to publish the commit-only fix commits a
batch of review-agent tasks left in the worktree — so it is routed straight
to the **commit agent** in push mode, skipping the (work-less) task-agent
phase entirely. Every other ready task goes to `launchTaskAgent`, which
starts a task agent in a fresh (or existing, if `SaveSandbox` shared it)
workspace.

Structural graph errors — an unknown dependency, a dependency cycle, or a
genuine duplicate task name — are **not** repairable per-file and abort
`taskgraph.Load` entirely; `loadTaskGraph` (`internal/daemon/dispatch.go`)
responds by blocking the whole project for the wolf rather than silently
stranding it. A cosmetically-invalid name (blank, not kebab-case, or too
long), by contrast, *is* repaired in place (slugified, deduped) with a loud
`Warnings()` entry — one bad task file must not abort every other task in the
project.

### running → success / failed / blocked

`afterTaskSession` (`internal/daemon/dispatch_lifecycle.go`) is the single
place task-level transitions are decided, and it runs **only** on the task
agent's session-end callback — task files are deliberately not
fsnotify-watched for lifecycle purposes (`handleTask` only logs), to avoid a
race where an agent writes `status:success` and exits before the daemon's
session tracker observes the session ending.

- **`success`** → `launchCommitAgent` (see below).
- **`failed`, `running`, or `ready`** → `handleTaskFailure`. `running`/`ready`
  mean the agent's session ended without ever writing a terminal status (it
  crashed, or never started) — treated identically to an explicit `failed`.
- **`blocked`** → the agent decided it can't proceed. The task's sandbox is
  retained (`retainSandbox`, sets `save_sandbox: true`) so the wolf can
  inspect it, and the project is blocked immediately — **no retry** regardless
  of `Attempts`.
- **`committed`** → invariant violation (task agents must never commit
  directly) — the project is blocked to surface the bug rather than silently
  accepting it.

`internal/daemon/reaper.go` is the backstop that guarantees a session
actually reaches this callback even if the agent process wedges: a session
whose task has already reached a terminal-for-its-stage status
(`success`/`failed` for a task agent, `committed` for a commit agent) but
whose wrapper hasn't exited within `sessionDoneGrace` (90s) is force-closed
(`reapCauseStageComplete`); one that has produced no ACP stream activity for
longer than `sessionIdleTimeout` (default 20 minutes) is force-closed as a
mid-turn hang (`reapCauseIdleTimeout`). Either way, closing the session
unblocks the same `afterTaskSession`/`afterCommitSession` path a clean exit
would have taken.

### failed → ready (retry) or → blocked (exhausted)

`handleTaskFailure` (`internal/daemon/dispatch_lifecycle.go`) is a strict
attempt-budget check against `maxTaskAttempts` (3):

```go
if t.Attempts >= maxTaskAttempts {
    // retain sandbox, transitionProjectBlocked, return
}
// bump Attempts, status → ready, clear FailureReason, save, relaunch task agent
```

So a task's 1st, 2nd, and 3rd failures (`Attempts` 0→1, 1→2, 2→3) are retried
by resetting it to `ready` and relaunching a task agent; once `Attempts`
reaches 3 *before* this failure is even processed (i.e. this would be the
4th attempt), retries are exhausted, the sandbox is retained for inspection,
and the whole project blocks — a single task's terminal failure blocks
everything else in the graph too, since nothing downstream of it can ever
have its dependency satisfied.

### success → committed (commit agent) → next task or project completion

`launchCommitAgent` (`internal/daemon/dispatch_lifecycle.go`) runs the commit
agent in the same workspace shape as the task agent (it needs the repos
checked out to `git commit`/`git push` in them). `afterCommitSession` is its
session-end callback:

- If the task did **not** end in `status: committed`, that's a hard failure —
  block the project for the wolf (mirrors the task-agent side's "must reach a
  specific terminal status or it's an invariant violation" pattern).
- Otherwise it stamps `CompletedAt` (once, the first time committed is
  observed — the daemon writes this *after* the commit agent's session ends,
  so an agent rewriting the task file mid-run can never clobber it) and
  reloads the task graph:
  - `AllCommitted()` → `transitionProjectComplete` (§1: idle,
    watching or not).
  - Else if another task is stuck at `success` (interrupted commit handoff)
    → resume its commit agent.
  - Else if `Ready()` is non-empty → dispatch the next ready task
    (`dispatchReadyTask`, same push-task special-case as above).
  - Else → nothing ready but not all committed → block ("no ready tasks
    remain but project is not fully committed"); this is a stuck-graph
    invariant violation (e.g. every remaining task is `failed`/`blocked` with
    no path forward).

Only a **push task** (`Source.Kind == "pr-push"`) actually publishes to the
remote — `launchCommitAgent` sets `PushBranch: t.IsPushTask()` on its plan, so
an ordinary task's commit stays local. This is why individual review-fix
commits accumulate locally until a review-agent-inserted push task
(depending on that batch) publishes them together, rather than the PR seeing
a stream of individual commits. Regular (non-review) work is instead
published by the **archive agent** at cleanup time (§3), not by any commit
agent.

## 3. The cleanup / archive side-flow (orthogonal to status)

`cleanup: true` and `archive: true`/`false` are independent boolean flags on
`.project.yaml`, **not** part of the status enum — a project keeps whatever
status it had (working, blocked, idle) while a cleanup runs
(`internal/project/project.go`, `Project.Cleanup`/`Archive` doc comments).
`dispatchProject` (`internal/daemon/dispatch.go`) checks `p.Cleanup` **ahead
of** status routing (right after the cron registration check, before the
`switch p.Status`), so `:cleanup` works on a project in any status — and
short-circuits: the normal status agent must not run at the same time the
archive agent is trying to leave the very same workspace clean and pushed.

The contract, end to end:

1. The TUI's `:cleanup` sets `Cleanup = true` and saves as `WriterAgent` (a
   daemon-authored write is dropped by the daemon's own fsnotify filter and
   would never be observed).
2. `launchArchiveAgent` reserves a cleanup-in-flight guard
   (`beginCleanup`/`endCleanup`, keyed by project path) spanning from launch
   until the request flag is cleared — deliberately wider than the session's
   own lifetime, because a `.project.yaml` event from the agent's own
   `archive: true` write can arrive in the gap between the session ending and
   the flag being cleared; without the wider guard that late event would
   dispatch a second archive agent. The archive agent runs under its own
   session key (`archiveSessionKey`, an `#archive` suffix) so it doesn't
   contend with the project's shared agent slot.
3. `afterArchiveSession` clears `cleanup: true` (written as the daemon, so
   the clear can't retrigger dispatch) regardless of whether the agent
   succeeded — an agent that gave up should not be relaunched by the next
   unrelated file event; a human re-issues `:cleanup` to retry. Only after
   the clear lands does it call `revisitProject` to resume normal routing;
   if the clear fails to persist, the revisit is skipped so a second archive
   agent isn't dispatched against a request the file still shows as pending.

Whether the archive agent actually needs to push is decided per-repo by a
**two-step upstream-then-count check**, specified in its prompt template
(`internal/prompts/templates/archive.tmpl`) rather than in Go: first
`git rev-parse --abbrev-ref --symbolic-full-name @{upstream}` settles whether
an upstream exists at all (a non-zero exit / empty result means "never
published, needs pushing"), and only if that succeeds does
`git rev-list --count @{upstream}..HEAD` count how far ahead of it the branch
is. The two steps are not interchangeable: `rev-list --count @{upstream}..HEAD`
also exits non-zero/empty when there's no upstream configured, so reading an
empty `ahead` on its own would be ambiguous between "up to date" and "no
upstream yet" — the template explicitly forbids treating a missing `ahead` as
license to push, and requires the upstream question to be settled first, by
`rev-parse` alone. The archive agent sets `archive: true` only once every
repo in the workspace is clean (nothing to commit, and either nothing to push
or the push succeeded); if it can't finish (a rejected push, an unapproved
`.gitignore` change, unclassifiable working-tree state) it leaves `archive`
unset and reports what's outstanding rather than marking the project falsely
clean.

Cron's own idle-check (`requestCronReplan`, §1) treats `Cleanup` and
`Archive` as non-idle for the same reason `dispatchProject` gives cleanup
priority over status routing: a project being wound down has nothing left to
re-plan, and a planning run mid-cleanup would dirty the workspace the archive
agent is trying to leave clean.

**"Archived" is not a project status, on purpose.** `:archive`
(`internal/tui/archive.go: archiveProject`) only ever runs once `archive:
true` is set, and what it does is `os.Rename` the whole project directory out
of the watched `--root` into a sibling `<root>.backup/` tree. There is no
resting "archived" state to represent in the enum because an archived project
doesn't keep existing in the daemon's view at all — it physically leaves the
tree the daemon watches, the same way a project.md-bootstrapped project
doesn't exist until its directory is created. `archive`/`cleanup` are a
pre-condition and a request flag on a project that's still very much live;
archiving itself is removal, not a state transition.

## 4. `stopped`: a pause, not a normal transition

`status: stopped` (`internal/daemon/stop.go`, `internal/tui/stop.go`) is a
human-requested pause layered on top of whatever the project was doing, set
by `:stop` and lifted by `:start`. Unlike every status covered above, it is a
**dead end** for `dispatchProject`'s normal routing — no case in the status
switch launches anything for `stopped`, by design.

- **`:stop` → `requestStop`** loads the project, stashes the current
  `Status` into `StoppedFrom` (so `:start` knows what to restore), sets
  `Status = stopped`, and saves as `WriterAgent`. The daemon's
  `enforceStopped` (`internal/daemon/stop.go`) is what actually makes the
  pause effective: it kills every tracked agent session for the project
  (including the `#wolf` and `#review` tandem slots) and unregisters its
  cron and `#review` schedules. It is safe to call on every observation of an
  already-stopped project (a second `:stop`, a daemon restart onto one) since
  there may be nothing left to stop.
- **Session-end guards.** A session `enforceStopped` kills still runs its
  normal session-end callback (the process exit is real), so
  `afterTaskSession`/`afterCommitSession`/`afterReviewSession` each check
  `d.isStopped(projectPath)` first and bail out (`session_end_skipped_
  stopped`) rather than reading the kill as a crash to retry or escalate —
  otherwise a deliberate stop would look like a task failure or a review-agent
  crash to the very code paths it's trying to pause.
- **`:start` → `requestStart`** restores `Status` from `StoppedFrom` (falling
  back to `idle` if that field is somehow empty — a hand-edited file, say —
  since restoring to `""` would misroute to the project agent as an
  unpopulated seed) and clears `StoppedFrom`, then saves as `WriterAgent`.
  The resulting fsnotify event hands the project back to `dispatchProject`'s
  ordinary routing with no extra state left over — including, if it was
  idle-and-watching before the stop, resuming the PR watch, since `review`/
  `pull_requests` were never touched by the stop/start cycle.
