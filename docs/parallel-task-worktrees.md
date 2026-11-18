# Parallel tasks via per-task worktrees

**Status:** plan (no code changes yet).

## Goal

Today a project's tasks run strictly one at a time, and every task agent works
— and its commit agent commits — directly in the single wsp workspace checked
out on the project's shared branch. This plan changes that so that:

- **Every task agent works and commits in its own git worktree on a per-task
  branch**, isolated from every other task in the project.
- **A single-threaded-per-project commit agent integrates** each finished
  task's commits back onto the one branch shared by all agents in the project,
  serializing those integrations through a per-project commit queue.
- With task work isolated and integration serialized, the daemon can **run
  unrelated tasks in parallel** (bounded by a per-project concurrency limit)
  instead of one at a time.

The rest of this document first records how the system behaves today (verified
against the code on this branch), then lays out the design and a phased
rollout.

## Contents

1. [Current behavior](#current-behavior)
2. [Worktrees](#worktrees) — where per-task worktrees are created, named and
   cleaned up
3. [Integration & conflicts](#integration--conflicts) — merge vs rebase vs
   cherry-pick onto the shared branch; block / retry / re-run on conflict
4. [Scheduler & dispatch](#scheduler--dispatch) — parallel task dispatch, the
   per-project concurrency limit, the serialized commit queue
5. [Planning independence hints](#planning-independence-hints) — how the
   planning agent marks tasks as safe to run concurrently
6. [Schema/state changes](#schemastate-changes) — task YAML, `project.yaml`,
   audit events, the state snapshot
7. [Interactions](#interactions) — review agent, PRs, cron, wolf, stop/start,
   daemon restart recovery
8. [Risks & open questions](#risks--open-questions)
9. [Phased rollout](#phased-rollout)

---

## Current behavior

Everything in this section was checked against the code. Where `agents.md`,
`design.md`, `state-machine.md` or `docs/state-snapshot.md` say something
different, the code wins and the difference is noted. References are
`file:function` and are relative to the repo root.

### 1. How work reaches the daemon (watcher and event routing)

- `internal/daemon/daemon.go:Run` installs an fsnotify watch on every directory
  under each `--root` (`internal/daemon/watcher.go:addTree`). Next it runs
  `reconcileSessions` (adopts live ACP sessions left by a previous daemon, §7),
  then `startupScan`, then starts `reapLoop`, then enters the event loop.
- `internal/daemon/daemon.go:dispatchEvent` runs each fsnotify event on its own
  goroutine, **chained per containing directory**. Events for one project's
  directory run strictly in arrival order. Events for different projects run
  in parallel. This is the only cross-project parallelism that exists today. It
  was added so that one project's slow `wsp new` cannot stall another project
  (see `internal/daemon/concurrency_test.go:TestSlowWorkspaceProvisioningDoesNotDelayOtherProject`
  and `TestSameProjectEventsStillProcessInOrder`).
- `internal/daemon/watcher.go:handlerFor` routes by filename:
  `project.yaml` → `handleProject`, `project.md` → `handleProjectSeed`,
  `intake/*.md` → `handleIntakeFile`, `tasks/*.yaml` → `handleTask`.
  **Task files are observation-only.** `internal/daemon/dispatch.go:handleTask`
  only writes a `task_file_updated` audit line. Every task-lifecycle decision is
  made in session-end callbacks (§3), because an agent can write
  `status: success` and exit before the daemon would have noticed the file
  change.
- `internal/daemon/dispatch.go:handleProject` drops events whose
  `updated_by` is `daemon`, which breaks write→event loops.
  `revisitProject` re-reads the file and skips that filter. Session-end
  callbacks, cron and the startup scan use it.
- `internal/daemon/watcher.go:startupScan` walks every `project.yaml` with a
  fan-out of `startupScanConcurrency = 8` projects. For each one it calls
  `releaseOrphanedTasks` (§7) and then `revisitProject`.

### 2. How the daemon picks the next task — one at a time

`internal/daemon/dispatch.go:dispatchProject` is the single router. It handles,
in order:

1. a power deferral;
2. `stopped` → `enforceStopped`;
3. an unpopulated project → the project agent;
4. the `created_at` stamp;
5. cron registration;
6. `cleanup: true` → the archive agent;
7. the seed/intake re-arm;
8. the `status` switch.

For `status: working` it can launch planning for unoffered intake. It then
always calls `dispatchNextTask`. For `idle` it calls `dispatchPendingTasks`
for manually added tasks.

`internal/daemon/dispatch.go:dispatchNextTask`:

1. Loads the graph with `loadTaskGraph`. That calls `taskgraph.Load` on
   `<control-dir>/tasks/`. A structural error blocks the project, unless a
   planner is in flight (`deferToPlanner`).
2. If `g.AllCommitted()`, it calls `transitionProjectComplete` (in
   `internal/daemon/review.go`). That writes `idle`, or idle-and-watching
   when a PR is involved.
3. Otherwise it calls `dispatchReadyOrPending`:
   - **Commit recovery first.** `firstUncommittedSuccess` returns the first
     task, by name, still at `success`, and `launchCommitAgent` is called for
     it. This resumes a task→commit handoff that a restart interrupted.
   - Otherwise it takes `g.Ready()` and dispatches **only `ready[0]`** via
     `dispatchReadyTask`.

`internal/taskgraph/graph.go:Ready` returns every task with
`status: ready` whose `depends_on` are all `committed` (`depsCommitted`),
sorted by name. The graph can already list several runnable tasks, but the
daemon takes the first and ignores the rest. `depends_on` is the only ordering
or independence signal in the schema (`internal/task/task.go:Task`). There is
no notion of file or area hints.

`internal/daemon/dispatch_lifecycle.go:dispatchReadyTask` sends a push task
(`t.IsPushTask()`) straight to `launchCommitAgent`. It has no code work of its
own. Every other task goes to `launchTaskAgent`.

**Strict serialization comes from the session map, not from the scheduler.**

- `Daemon.sessions` (`internal/daemon/daemon.go`) is a
  `map[string]sessionEntry` keyed by project path.
- `trackConfiguredSession` refuses a second entry under an existing key.
- `hasSession(key)` is checked before every main-slot launch.
- Project, task, commit and review agents all share the **bare project path**
  as their key, the project's "main slot". So at most one of them runs per
  project at any moment.
- Only the planner (`#planning`), wolf (`#wolf`) and archive (`#archive`)
  agents have suffixed keys and can run alongside the main slot.
  See `planningSessionKey`, `wolfSessionKey`, `archiveSessionKey` and
  `runsAlongsidePlanning` (task/commit only).

`internal/scheduler/scheduler.go` is not a task scheduler. It is a thin keyed
wrapper over `robfig/cron`. It drives:

- project cron schedules (`dispatch.go:registerCronIfAny` → `onCronFired` →
  `runCron`);
- the PR poll (`reviewPollKey`, `review.go:onReviewPoll`).

A cron firing on a `working` project only re-enters `revisitProject`
(`requestCronReplan` deliberately refuses to re-plan mid-flight). That works
as a recovery poll for `dispatchNextTask`.

### 3. The task → commit → next-task chain

The chain is built entirely from session-end callbacks. `trackConfiguredSession`
removes the map entry **before** it calls `onEnd`, so each callback is free to
launch the next agent under the same key.

- `internal/daemon/dispatch.go:launchTaskAgent`:
  1. Returns early (`session_skip_duplicate`) if the main slot is busy.
  2. Builds a `runner.Plan{Kind: TaskAgent, Branch: p.Branch, Repos:
     workspaceReposFor(p), TaskPath, TaskName, StaticMCPs, Policies,
     SaveSandbox, Model, Harness}` with **no `WorkingDir`**.
  3. Persists `ready → running` (`dispatch_lifecycle.go:markTaskRunning`,
     which uses `task.Update` and only moves a task that is still `ready`).
  4. Calls `startSessionTracked(projectPath, …)`.
  5. Its callback is `afterTaskSession`. If the launch fails, it calls
     `releaseRunningTask` and then `transitionProjectBlocked`.
- `internal/daemon/dispatch_lifecycle.go:afterTaskSession` re-reads the task:
  - `success` → `launchCommitAgent`;
  - `failed`/`running`/`ready` → `handleTaskFailure`. That bumps `attempts`,
    resets to `ready` and relaunches immediately, up to
    `maxTaskAttempts = 3`; after that it calls `retainSandbox` and blocks.
  - `blocked` → `retainSandbox` and block;
  - `committed` → block, because a task agent must not commit.
  - If the project was stopped meanwhile (`isStopped`), it only calls
    `releaseRunningTask`.
- `internal/daemon/dispatch_lifecycle.go:launchCommitAgent` builds the same
  workspace-shaped plan (`Kind: CommitAgent`, same `Branch`/`Repos`, same
  `TaskName`, so the **same sandbox name**). It sets `PushBranch:
  t.IsPushTask()` and takes the main slot.
- `internal/daemon/dispatch_lifecycle.go:afterCommitSession`:
  1. Requires `status: committed`, otherwise it blocks, using
     `runner.StartFailure` for the reason.
  2. Stamps `completed_at`.
  3. Reloads the graph.
  4. Then, in order:
     - all committed → `transitionProjectComplete`;
     - another `success` task → commit it;
     - nothing ready → block (`"no ready tasks remain but project is not fully
       committed"`), unless a planner is running;
     - otherwise → `dispatchReadyTask(ready[0])`.

So the steady state for one project is a strict pipeline:
task₁ → commit₁ → task₂ → commit₂ → … Each step waits for the previous
session to end.

### 4. How an agent is launched and where it works

`internal/runner/runner.go:StartConfigured` is the single launch path. It is
called by `internal/daemon/dispatch.go:startSessionTracked`.

- **Choosing the checkout.** `resolveWorkingDir` uses `Plan.WorkingDir` if it
  is set. Otherwise it calls `r.Workspaces.Create(ctx, p.Branch, p.Repos)`.
  The rule per agent kind:
  - Control dir (`WorkingDir = filepath.Dir(projectPath)`): project, planning,
    wolf and review agents.
  - The wsp workspace (no `WorkingDir`): task, commit and archive agents.
  - The workspace is keyed **only on the project branch**. Every task and
    commit agent in a project therefore resolves to **the same directory**.
  - Planning and an ACP wolf also mount that same workspace, read-only by
    convention, as a second mount (`resolvePlanningWorktree`,
    `resolveWolfWorktree`).
- **Prompt and context.** `prompts.Render` fills the `.tmpl` for the kind.
  `internal/setup/setup.go:Apply` then writes `.orch/instructions.md` and
  `.orch/context.yaml` into **the root of the working dir**. For task and
  commit agents that is the shared wsp workspace root. Two agents launched
  into the same workspace at once would overwrite each other's `.orch/` files.
  Today this cannot happen only because the main slot is single-occupancy.
- **Task prompt** (`internal/prompts/templates/task.tmpl`): "The workspace
  already contains the repos listed in the project file, all on branch
  `{{.Branch}}` … Do not commit, push, or change `project.yaml`". The agent
  writes `status: success|failed` to its task file.
- **Commit prompt** (`internal/prompts/templates/commit.tmpl`): for every repo
  with modifications, stage everything, make one commit, and capture
  `git rev-parse HEAD`. It does not push, except for a push task. It writes
  `status: committed`, `summary`, `commits: [{repo, hash}]` and
  `created_files`. In other words, the commit agent commits **whatever is in
  the shared working tree** directly onto the project branch. No integration
  step exists, because the task worked on that branch in the first place. Push
  tasks run `git -C <repo> push` for `source.repo`, with no force. A rejection
  sets the task `blocked`.
- **Sandbox and mounts.**
  - `runner.go:sandboxWorkspaces` mounts `[workspace, control-dir]` for
    task/commit/archive. The control-dir mount is how the agent's write to
    `tasks/<name>.yaml` lands.
  - It mounts `[control-dir, workspace]` for planning and the wolf.
  - Sandbox names come from `SandboxNameFor` / `ACPSandboxNameFor`. A task
    gets `<work-stream>-<task-name>`, and its commit agent reuses that
    sandbox. Planning gets `<work-stream>`. The rest get
    `-project`/`-review`/`-archive`/`-wolf`.
  - All names are sanitized and length-fitted by
    `internal/sbx/name.go:SanitizeName`.
  - So sandboxes are already per task. Only the **git checkout** is shared.
- **Launch mechanism.**
  - In production every autonomous kind goes through `runner.go:startACP`. It
    writes `session.json` (with `ProjectPath`, `TaskPath`, `Kind` and
    `Workspaces`) under the sessions root, then starts an `acp-wrapper` host
    process via `r.AcpLauncher`.
  - The wrapper (`internal/acpwrapper/acpwrapper.go:Run`, `ensureSandbox`)
    creates the sandbox and runs the ACP client.
  - On exit `removeSandboxOnExit` runs `sbx rm --force` unless
    `save_sandbox` is set or `keepForTaskStatus` holds (failed, blocked or
    running).
  - Without an ACP launcher, the legacy path runs `sbx exec -it -w
    <workspace> <sandbox> claude …` in a terminal tab via `r.Launcher`
    (`internal/agent/agent.go:Launcher`, `internal/agent/process.go`).
- `internal/agent` only defines `Kind`, `Spec`, `Session{Name, Wait, Close}`
  and `Launcher`. It knows nothing about checkouts.

### 5. How wsp workspaces are created, mounted and removed

- `internal/workspace/manager.go:Manager` has four methods:
  - `Create(ctx, branch, repos)` — "the branch name is also used as the
    workspace name and the per-repo feature branch";
  - `Path(branch)`;
  - `Remove(ctx, branch, force)`;
  - `AddRepos(ctx, branch, repos)`.
- `internal/workspace/wsp.go:WspManager.Create`:
  1. `ensureCreated` creates `new_repos` on GitHub via `gh`.
  2. `ensureRegistered` adds missing repos to the wsp registry.
  3. Runs `wsp new --json <branch> <repo…>`.
  4. On "already exists" it returns `Path(branch)` and does **not** reset
     HEAD. That idempotency is what lets every task/commit launch call
     `Create` again.
  5. Only on first creation, `applyBaseBranches` runs `git fetch origin
     <base>` and `git checkout -B <branch> origin/<base>` per repo.
  - The result is one wsp workspace (by default `~/dev/workspaces/<branch>/`)
    holding one clone or worktree per repo, all on `<branch>`.
- `WspManager.AddRepos` (`wsp repo add`) clones repos added to an existing
  workspace. It is driven from
  `internal/daemon/repo_sync.go:syncPlannedRepos` at planning launch.
- `internal/workspace/stub.go:StubManager` is the test/dev implementation. It
  just does `mkdir <root>/<branch>`.
- **Removal.** The daemon never removes a workspace. The only caller of
  `Remove` is the TUI's `:archive` (`internal/tui/archive.go`). It first
  deletes `<workspace>/.orch` (`removeWorkspaceOrch`), then calls `wsp rm
  --force <branch>`, after the archive agent has set `archive: true`.
- The daemon never runs `git worktree` itself, and there is no per-task
  branch anywhere.

### 6. Existing concurrency guards

Everything below is per project unless stated otherwise.

| Guard | Where | What it prevents |
|---|---|---|
| Per-directory event chain | `daemon.go:dispatchEvent` | Two events for the same project running concurrently. |
| Main-slot single occupancy | `daemon.go:trackConfiguredSession`, `hasSession`, checked in `launchTaskAgent` / `launchCommitAgent` / `launchProjectRootAgent` / `dispatchReviewAgent` | More than one project/task/commit/review agent per project. |
| Planning reservation | `dispatch.go:beginPlanning` / `endPlanning` / `planningInFlight` | Two planners, including during the seconds `runner.Start` spends provisioning. `deferToPlanner` holds off graph-wide decisions (complete/block) while a planner may be mid-write. Review also stands aside for a planner. |
| Cleanup reservation | `dispatch_lifecycle.go:beginCleanup` / `endCleanup` | Two archive agents for one `:cleanup`. |
| `markTaskRunning` / `releaseRunningTask` | `dispatch_lifecycle.go` | Re-picking a task already handed out. Undoes the running write on a failed launch, a stop, or an orphaned task. |
| Stop | `stop.go:enforceStopped`, `killProjectSessions`, `isStopped` | Kills the bare key and every `path#…` key and unregisters cron and the `#review` poll. `afterTaskSession` / `afterCommitSession` check `isStopped` and bail instead of retrying or blocking. |
| Reaper | `reaper.go:reapLoop` / `reapStrandedSessions` / `strandedVerdict` / `stageComplete` | A wrapper that never exits holding the main slot forever. Every 30s it closes a session whose task reached a terminal status for its kind (task: success/failed; commit: committed) for at least 90s (`sessionDoneGrace`), or that has been idle for 20m (`defaultSessionIdleTimeout`). Closing it fires the normal `onEnd`. Interactive (wolf) and workingman sessions are never reaped. |
| Restart reconcile | `reconcile.go:reconcileSessions` | Duplicate agents after a daemon restart. Adopts `running`/`starting` ACP sessions from `session.json` under the same key they had. Re-takes the planning and cleanup reservations. Task and commit sessions are adopted under the bare project path with an `onEnd` that calls `releaseRunningTask` and then `revisitProject`, **not** `afterTaskSession`/`afterCommitSession`. |
| Orphan release | `dispatch_lifecycle.go:releaseOrphanedTasks` (from `startupScan`) | A task left at `running` with no adopted session never being re-dispatched. |
| Startup fan-out bound | `watcher.go:startupScanConcurrency = 8` | A burst of `wsp new` calls on restart. |

All of these guards assume **one main-slot session per project**. Several of
them key their state on the bare project path:

- `taskSessionLive` compares a single `sessions[projectPath].taskName`;
- `reconcileSessions` adopts task/commit sessions under the bare key;
- the reaper loads tasks by `e.taskName` from that key.

A parallel design has to re-key all of them.

### 7. Where the docs differ from the code

- `docs/state-snapshot.md` lists session keys as "the `project.yaml` path,
  plus `#wolf` / `#archive`". The code also uses `#planning`
  (`planningSessionKey`), and planning is tracked outside the main slot.
- `agents.md`, task agent: "`dispatchNextTask` … has a task in `Ready()`" is
  accurate, but note that only `Ready()[0]` is ever dispatched.
- The comment on `internal/daemon/dispatch.go:dispatchPendingTasks` still
  mentions `reviewing`/`done` statuses. The code now routes from `idle`
  (with `WatchingPR`) and `merged` (`internal/project/status.go`).
- `internal/runner/runner.go:StartConfigured` comments say planning mounts
  "the same wsp worktree the task/commit agents will later use". That is
  accurate, and it is a reason the planner currently sees in-progress,
  uncommitted task edits in its source mount.

### Summary of the starting point

- Exactly one wsp workspace per project. It is keyed on `project.branch` and
  shared by every task, commit and archive agent, and also mounted by
  planning and the wolf.
- Task agents edit the shared tree. The commit agent then commits that tree
  directly on the shared branch. No per-task branch or integration step
  exists.
- The daemon dispatches only `Ready()[0]`. The single main session slot per
  project, not the scheduler, enforces one task-or-commit agent at a time.
- Sandboxes are already per task (`<work-stream>-<task>`), with the commit
  agent reusing its task's sandbox. That part of the isolation is already in
  place.

---

## Worktrees

_To be written (task `design-worktrees-integration`)._

## Integration & conflicts

_To be written (task `design-worktrees-integration`)._

## Scheduler & dispatch

_To be written (task `design-scheduler-dispatch`)._

## Planning independence hints

_To be written (task `design-scheduler-dispatch`)._

## Schema/state changes

_To be written (task `design-state-interactions`)._

## Interactions

_To be written (task `design-state-interactions`)._

## Risks & open questions

_To be written (task `write-rollout-and-risks`)._

## Phased rollout

_To be written (task `write-rollout-and-risks`)._
