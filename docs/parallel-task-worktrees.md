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

This section says where each task's isolated checkout lives, how it is named,
which commit it starts from, how it reaches the sandbox, and when it goes away.
It builds on the [current behavior](#current-behavior) above: one wsp workspace
per project (§5), task sandboxes already separate (§4), and only the git
checkout shared.

### W1. Terms

- **Shared workspace.** The project's existing wsp workspace, from
  `workspace.Manager.Create(branch, repos)`. By default this is
  `~/dev/workspaces/<project-branch>/`. It has one directory per repo
  (`workspace.Repo.DirName`), and each repo's main working tree is checked out
  on the **shared branch**, `project.branch`. Nothing in this design changes
  how it is created, mounted for planning and the wolf, or removed by
  `:archive`.
- **Task worktree.** A per-task directory holding one `git worktree` per
  project repo. Each is checked out on that task's **task branch**.
- **Shared tip.** `git -C <shared>/<repo> rev-parse refs/heads/<project-branch>`
  for a repo. It is the local ref, not `origin/…`, because integrated commits
  stay local until a push task or the archive agent publishes them.

### W2. Path layout

```
~/dev/workspaces/<project-branch>/          # shared workspace (wsp-managed)
├── .wsp.yaml
├── .orch/                                  # commit/archive agent handoff (today's location)
├── <repo-a>/                               # main working tree, on <project-branch>
│   └── .git/                               # common git dir for all worktrees of repo-a
│       └── worktrees/<task-name>/          # git's per-worktree admin dir
├── <repo-b>/
└── .worktrees/
    └── <task-name>/                        # task worktree root = task agent cwd
        ├── .orch/                          # task agent's instructions.md / context.yaml
        ├── <repo-a>/                       # git worktree of repo-a on the task branch
        └── <repo-b>/                       # git worktree of repo-b on the task branch
```

Why this layout:

- **The task worktree root has the same shape as a wsp workspace:** one
  `DirName()` directory per repo. The task prompt's "the workspace contains the
  repos …", `task.Commit.Repo` ("workspace-relative directory name") and the
  `.gomodcache` probe in `acpwrapper` (`goModCacheDir`, which looks at every
  workspace mount) all keep working without changes.
- **`.orch/` moves to a per-task directory.** `setup.Apply` writes into the
  root of the working dir. Giving each task agent its own root fixes the
  `.orch/` overwrite hazard that §4 notes would appear as soon as two agents
  share the workspace root.
- **Inside the shared workspace, not next to it.** `wsp rm --force` on
  `:archive` deletes `.worktrees/` along with the repos whose `.git` holds the
  worktree metadata. Nothing is left behind outside the directory wsp
  manages, and no new root needs configuring. The workspace root already holds
  orch-owned content (`.orch/`, which `removeWorkspaceOrch` handles), so
  `.worktrees/` follows that precedent. The daemon owns `.worktrees/`; wsp
  never reads it.
- **Multi-repo projects get a worktree for every repo in
  `workspaceReposFor(p)`,** not just the repos the task is expected to touch.
  No reliable per-task repo list exists before the task runs (the
  file/area hints in [Planning independence hints](#planning-independence-hints)
  are advisory). A `git worktree add` costs only a checkout, and integration
  skips repos with no new commits (I3). Repos added later through
  `AddRepos` / `syncPlannedRepos` show up in the next task worktree created
  after them. An existing task worktree is never extended mid-flight.
- Today's repo is a full clone (`.git/` is a directory). If wsp ever makes
  the per-repo directory itself a worktree, `git worktree add` still works,
  because git resolves the common dir. The mount rule in W5 uses
  `git rev-parse --git-common-dir` rather than assuming `<repo>/.git`.

`<task-name>` is the task's `name:`. That is already a valid sandbox-name
suffix (`sbx.NameBudget`), so it is filesystem-safe and unique within the
project.

### W3. Branch naming

The obvious name, `<project-branch>/task/<task-name>`, **does not work**. Git
stores branches as files under `refs/heads/`, so a ref
`refs/heads/<project-branch>` and a directory
`refs/heads/<project-branch>/…` cannot both exist. I verified this:
`git branch feat/task/a` fails with `'refs/heads/feat' exists; cannot create
'refs/heads/feat/task/a'`. Task branches therefore live in their own
namespace:

```
wm/<project-branch>/<task-name>
```

- The `wm/` prefix keeps task branches apart from user and PR branches. It
  makes the reaper's GC (W7) a simple prefix scan (`git for-each-ref
  refs/heads/wm/<project-branch>/`), and it says plainly that workingman owns
  these branches and may delete them.
- `<project-branch>` may itself contain `/` (for example `feat/x`), which is
  fine. Two projects whose branches nest (`a` and `a/b`) could in theory
  collide (`wm/a/b` as task `b` of project `a`, versus the `wm/a/b/` directory
  of project `a/b`). This needs two projects with nested branch names that
  share a repo, which is unlikely. The daemon checks for it when it creates a
  worktree: if `git worktree add -b` fails on a ref-lock error, the task is
  blocked with a clear reason, rather than escaping branch names up front.
- Task branches are **never pushed**. Push tasks and the archive agent push
  only the shared branch (`commit.tmpl`: "push ONLY that repo … do NOT push a
  different branch"). That property is what makes rebasing them safe (I2).
- A retry or re-run reuses the same branch name (see W6). The branch is
  recreated, not appended to, so the name always means "the current attempt".
  The previous attempt's tip is kept under
  `refs/wm/<project-branch>/<task-name>/attempt-<n>` when it carried commits.
  That is a non-branch ref, so it never shows in `git branch` or gets pushed.
  The re-run agent and the wolf can still diff against it.

### W4. Base commit

A task worktree is created **when the task is dispatched**, by the daemon, on
the host, right before `launchTaskAgent` starts the session. This is the point
where `markTaskRunning` flips `ready → running` today. It is cut, per repo,
from the **shared tip at that moment**:

```
git -C <shared>/<repo> worktree add -b wm/<pb>/<task> \
    <shared>/.worktrees/<task>/<repo> refs/heads/<pb>
```

- **Dependencies are already in.** `taskgraph.Ready` only returns a task once
  every `depends_on` entry is `committed`. Under this design `committed` means
  "integrated onto the shared branch" (see I5). So the shared tip at dispatch
  already contains the work of everything the task depends on. Independent
  tasks dispatched at the same time all start from the same tip.
- **Not `origin/<base_branch>`.** `applyBaseBranches` uses that only when a
  workspace is first created. After that the local shared branch is the truth,
  and may be ahead of any remote.
- **Record the base.** The daemon writes the per-repo base SHAs into the task
  file (`base: [{repo, hash}]`, the same shape as `commits:`; the schema work
  belongs to [Schema/state changes](#schemastate-changes)). Integration (I3)
  uses it to tell a fast-forward from a rebase. The wolf uses it to see how
  stale a blocked task was.
- **Push tasks get no worktree.** A push task (`t.IsPushTask()`) has no code
  work of its own (`dispatchReadyTask` sends it straight to
  `launchCommitAgent`). It publishes the shared branch, so it runs in the
  shared workspace, through the commit queue (I1), exactly as it does today.
- **Failure creating the worktree** (a git error, disk full, the branch
  already exists and could not be cleared) is handled like a failed launch:
  `releaseRunningTask` and then `transitionProjectBlocked`. The wsp workspace
  provisioning failures that `startSessionTracked` reports today are handled
  the same way.

Creation is idempotent, which matters for restart recovery. If
`.worktrees/<task>/<repo>` already exists **and** is registered in
`git worktree list --porcelain` on `wm/<pb>/<task>`, the daemon reuses it.
Anything else (a stray directory, a branch on the wrong commit, a worktree
whose directory vanished) is removed and recreated (W6). `git worktree add`
refuses to check out a branch that is checked out elsewhere, which gives a
second guard against two sessions sharing one task worktree.

### W5. Mounts into the sandbox

sbx bind-mounts each workspace at its **host path** (inside this sandbox,
`/Users/…` paths resolve exactly as on the host). That matters because a
worktree's `.git` is a file holding an absolute `gitdir:` pointer into the
main repo's `.git/worktrees/<name>`, and the commits a task makes land as
objects in the **common** git dir. A task sandbox therefore needs:

| Agent | Mounts (first = cwd) | Mode |
|---|---|---|
| task | `<shared>/.worktrees/<task>/` | rw |
|  | `<shared>/<repo>/.git` for each repo (`git rev-parse --git-common-dir`) | rw (objects, refs and the worktree's admin dir are written there) |
|  | control dir (`filepath.Dir(projectPath)`) | rw (as today, for `tasks/<name>.yaml`) |
| commit (non-push) | `<shared>` (cwd, as today) | rw |
|  | control dir | rw |
| commit (push task) | unchanged: `<shared>`, control dir | rw |
| planning, wolf | unchanged: control dir, `<shared>` | rw by convention, see below |

- **The task agent never mounts the shared working trees.** Only the repos'
  `.git` dirs are mounted. It cannot see or edit other tasks' in-progress
  files, or the shared tree. Git itself refuses `git checkout <project-branch>`
  inside the worktree, because that branch is checked out in the main working
  tree. The common-dir mount is still rw, so a misbehaving agent *could* run
  `git update-ref refs/heads/<pb> …`. The task prompt forbids touching any ref
  other than its own branch, and integration verifies the shared tip
  compare-and-swap style (I3 step 1), so such a move is caught and blocks
  rather than corrupting history.
- **The commit agent's cwd stays the shared workspace.** That is where the
  shared branch is checked out and where the fast-forward has to happen. The
  task worktree lives under it at `.worktrees/<task>/`, so no extra mount is
  needed. The commit agent keeps reusing the task's sandbox name
  (`SandboxNameFor(CommitAgent, …, taskName)`). Its mounts now differ from the
  task's, so the acp-wrapper's `ensureSandbox` must recreate the sandbox
  rather than reuse it when the mount set has changed. The alternative is a
  distinct `-commit` sandbox suffix. Both are listed in the rollout, and a
  distinct suffix is simpler and recommended.
- **Planning and the wolf are unchanged.** The planner's second mount is now
  better than it was: it shows only *integrated* work, not some task's
  half-finished edits (the caveat in §7). The wolf additionally gets each
  blocked or failed task's worktree, already visible under
  `<shared>/.worktrees/`, so it can inspect a conflicted or failed attempt
  directly.
- **Runner plumbing.** `runner.Plan` gains `TaskWorktree string`. For a
  `TaskAgent` with it set, `resolveWorkingDir` returns it, and
  `sandboxWorkspaces` returns `[taskWorktree, gitCommonDirs…, orchDir]`. The
  `Plan.WorkingDir` contract ("set → no wsp workspace") stays as it is. The
  shared workspace is still provisioned first, through `Workspaces.Create`,
  because the task worktree is cut from it.
- **Signing and identity** need no change. `gitsign.IdentityEnvArgs` and
  `SigningEnvArgs` are environment variables, not `.git/config`, so they apply
  to worktrees exactly as they do to the main checkout.

### W6. Cleanup

Removal is always done by the **daemon on the host**, never by an agent:

```
git -C <shared>/<repo> worktree remove --force <shared>/.worktrees/<task>/<repo>
git -C <shared>/<repo> branch -D wm/<pb>/<task>
rmdir/rm -rf <shared>/.worktrees/<task>      # the root, incl. its .orch/
```

It runs per repo and is best-effort. A failure is logged to the audit log
(`task_worktree_remove_error`) and left for the reaper (W7). It is never
fatal to the pipeline.

| When | What happens | Hook |
|---|---|---|
| **Successful integration** (task reaches `committed`) | First verify, for each repo with commits, `git merge-base --is-ancestor wm/<pb>/<task> refs/heads/<pb>`. Then remove the worktree, delete the branch and drop any `refs/wm/…/attempt-*`. The shared branch and the task file's `commits:` are the permanent record. If the ancestor check fails, nothing is removed; the integration did not really land, so this is treated like I4's "commit agent lied" case (block). | `afterCommitSession`, before the graph reload that dispatches the next task |
| **Failure with automatic retry** (`handleTaskFailure`, `attempts < maxTaskAttempts`) | If the failed attempt's branch has commits, save its tip as `refs/wm/<pb>/<task>/attempt-<n>`, keep the working tree diff as a commit on that ref (`git stash create`-style) when it is dirty, then remove the worktree and branch. The retry cuts a **fresh** worktree from the **current** shared tip at its dispatch. That differs from today, where a retry inherits the failed attempt's uncommitted edits in the shared tree, mixed with whatever else is there. A clean start is the more predictable behavior. The previous attempt stays diff-able through the saved ref. | `handleTaskFailure`, before `launchTaskAgent` |
| **Re-run after an integration conflict** (I4) | Same as a retry: save the attempt ref, remove, and re-cut from the new tip. | conflict path in `afterCommitSession` |
| **Terminal failure / blocked** (retries exhausted, agent wrote `blocked`, integration blocked) | **Keep** the worktree and branch, mirroring `retainSandbox`, so the wolf (and a human) can inspect them. The task file records `worktree: <path>`. When the wolf (or `:start` after a manual fix) resets the task to `ready`, the next dispatch replaces the worktree with a fresh one (W4 idempotency), after saving its ref. | `retainSandbox` call sites |
| **Stop** (`enforceStopped`) | Keep everything. A stopped task is released back to `ready` (`releaseRunningTask`), and `:start` re-dispatches it into a fresh worktree. A task stopped at `success` keeps its worktree, because that branch is exactly what the commit queue will integrate on `:start`. | — |
| **Archive** (`cleanup: true` → archive agent → `archive: true` → TUI `:archive`) | Before the archive agent launches, the daemon removes worktrees whose tasks are `committed` and runs `git worktree prune`. Any task worktree still holding **unintegrated** commits (a task at `success`/`failed`/`blocked`) is listed in the archive agent's context, so its report names the work that would be lost. `wsp rm --force` then deletes whatever is left with the workspace. | `launchArchiveAgent`; `internal/tui/archive.go` unchanged |
| **Project file deleted / project gone** | Nothing new. The workspace was never removed by the daemon, and it still is not. | — |

### W7. Orphan GC in the reaper

`reapLoop` gains a slower second duty: a worktree sweep, run every few
minutes rather than every 30s `reapInterval`, and also once from
`startupScan`, after `releaseOrphanedTasks`. For each project with a resolvable
shared workspace, the sweep works through four steps:

1. Run `git worktree prune` in each repo. This drops git admin entries whose
   directory is gone (for example, a user deleted `.worktrees/`).
2. For each `.worktrees/<task>/`, decide its fate from the task file and the
   session map:
   - **Task file missing:** the planner deleted or renamed the task. Save the
     attempt ref if the branch has commits, then remove.
   - **`committed`:** cleanup after a successful integration was interrupted
     (crash or restart). Remove.
   - **`ready` with no live session:** an orphaned dispatch that
     `releaseOrphanedTasks` reset. Remove. The next dispatch re-cuts anyway,
     and removing early frees disk and avoids a stale base.
   - **`running` with a live session** (`taskSessionLive`, re-keyed per task
     by [Scheduler & dispatch](#scheduler--dispatch)) and **`success`**
     (waiting in the commit queue): keep.
   - **`failed` / `blocked`, or `save_sandbox: true`:** keep (retained for
     the wolf).
3. For each `refs/heads/wm/<pb>/*` branch with no worktree, apply the same
   decision on the task's status. That covers a crash between `worktree
   remove` and `branch -D`.
4. Delete `refs/wm/<pb>/<task>/attempt-*` refs once their task is
   `committed`.

The sweep never touches a worktree whose task has a live session, and it
never deletes the only copy of uncommitted work without first saving an
attempt ref, unless the task is `committed`, whose leftovers are by
definition `created_files`. Each removal is audited
(`task_worktree_gc`, with `task`, `reason`).

## Integration & conflicts

Once a task agent writes `status: success`, its work sits on
`wm/<pb>/<task>` in its task worktree. Some of it may be committed by the task
agent, and some may still be uncommitted edits (see I3 step 0). **Integration**
is the commit agent's job of getting that work onto `refs/heads/<pb>`. This
section picks the git mechanism and defines what happens when it fails.
Integrations for one project are serialized through the commit queue
(specified in [Scheduler & dispatch](#scheduler--dispatch)). Everything below
assumes **at most one integration per project at a time**. Without that
assumption the shared tip could move mid-integration and none of this would
be safe.

### I1. Options compared

| | **Merge** (`git merge --no-ff <task-branch>` on the shared branch) | **Rebase + fast-forward** (rebase the task branch onto the shared tip in the task worktree, then `git merge --ff-only` on the shared branch) | **Cherry-pick** (`git cherry-pick base..task-branch` onto the shared branch in the shared tree) |
|---|---|---|---|
| History | One merge commit per task. A 10-task project produces 10 merge bubbles on the PR, mostly against trees where nothing else changed. | Linear. Each task's commits sit on top of the previous task's, just as today's sequential pipeline produces. | Linear, same result as rebase. |
| Signed commits (`gitsign`) | The task's commits keep their original signatures, and the merge commit is a new commit that must be signed too. | Rebased commits are **new** commits and must be re-signed. A fast-forward creates no commits, so it preserves the signatures already made. | Every picked commit is new and must be re-signed. |
| Where conflicts land | In the **shared** working tree. A half-merged shared tree blocks every later integration until it is cleaned up, and planning and the wolf would see the markers. | In the **task worktree**. The shared branch and its tree are not touched until the fast-forward, which cannot conflict. | In the **shared** working tree, the same problem as merge. |
| Multi-repo | Each repo merges independently. No natural two-phase shape. | Two-phase by construction: rebase every repo (prepare), then fast-forward every repo (publish). A conflict in any repo aborts before anything is published. | Each repo picks independently. A conflict in repo B after repo A was picked leaves A published. |
| Matches the `commits:` record | Yes, but `commits[].hash` would have to be the merge commit or the task's tip, which is ambiguous. | Yes: the post-rebase tip is the hash on the shared branch. | Yes, same as rebase. |
| Branch bookkeeping | Task branch tip stays an ancestor, so the "is it integrated?" check (W6) is trivial. | After the fast-forward the task branch tip **is** the shared tip, so the same check is trivial. | The task branch is never an ancestor, so the "is it integrated?" check needs patch-id comparison. |
| PR review | Reviewers see merge commits nobody asked for. Squash-merge PRs hide them, but rebase-merge PRs do not. | Reads like a hand-written series: one commit per task, in landing order. | Same as rebase. |

Squash (one commit per task) is not a separate option. Today's commit prompt
already makes one commit per repo per task, so rebase over a one-commit
branch is a squash in effect. Rebase also keeps working if task agents start
making several commits.

### I2. Recommendation: rebase onto the shared tip, then fast-forward only

Rebase the task branch in its own worktree, then `merge --ff-only` the shared
branch to it.

- **Conflicts never touch the shared tree.** That property alone decides it.
  Merge and cherry-pick both resolve on the shared branch's working tree,
  which is the one checkout every later integration, the planner and the wolf
  depend on.
- **Linear history** keeps the PR identical in shape to what the sequential
  pipeline produces today. Nothing about the review agent, push tasks or
  rebase-merge PRs has to change.
- **Rewriting is safe** because task branches are private and never pushed
  (W3). Only the shared branch is ever published, and it only fast-forwards,
  so push tasks keep their "no force" rule.
- **Signing works without host involvement.** Integration runs **inside the
  commit agent's sandbox**, where `gitsign.SigningEnvArgs` sets
  `commit.gpgsign=true` and `gpg.ssh.program=ssh-keygen` against the
  forwarded agent. `git rebase` honours `commit.gpgsign` for every commit it
  recreates, so rebased commits are signed with the same key, and the
  committer identity comes from `IdentityEnvArgs`. The fast-forward creates
  nothing to sign. Running the rebase **on the host** from the daemon was
  considered and rejected. The host's signer may be `op-ssh-sign`, as the
  `SigningEnvArgs` comment notes, which can prompt interactively and would
  stall an unattended daemon. Re-signing also needs the sandbox's ssh-agent,
  which `Preflight` already verifies for commit agents. If the preflight
  disables signing (`SigningKey` cleared), rebased commits come out unsigned,
  exactly as commits do today in that situation. That is no regression.
- **Fast path.** If the shared tip still equals the task's recorded `base`
  (W4), as with a sequential project or the first of several parallel
  finishers, the rebase is a no-op and is skipped. The fast-forward lands the
  task's commits with their original hashes and signatures.

### I3. The commit agent's integration procedure

`commit.tmpl` gains a worktree branch, chosen when `Plan.TaskWorktree` is set.
Push tasks keep today's template unchanged. For a project with repos `R`, the
steps run in order:

0. **Commit leftovers.** In each `.worktrees/<task>/<repo>` with
   modifications, stage and make one commit, as today. This keeps the current
   division of labour: the task agent may commit, and the commit agent
   guarantees nothing is left uncommitted. Repos where
   `git rev-list refs/heads/<pb>..wm/<pb>/<task>` is empty are skipped for
   the remaining steps.
1. **Snapshot the tip.** Record `onto[repo] = git rev-parse refs/heads/<pb>`
   for every remaining repo.
2. **Prepare (all repos before any publish).** In each task worktree, run
   `git rebase onto[repo]`, skipped when `onto == base`. On a conflict, run
   `git rebase --abort` in **that** repo and in every repo already rebased in
   this step. Reset each one to the pre-rebase tip recorded at the start of
   step 2 (`git reset --hard <saved-tip>`). Then report the conflict (I4)
   and stop.
3. **Publish.** In each shared repo, run
   `git -C <shared>/<repo> merge --ff-only wm/<pb>/<task>`. Because of the
   queue, the tip cannot have moved since step 1 unless something outside the
   queue moved it (a human, or the wolf committing on the shared branch). In
   that case `--ff-only` fails cleanly. The agent then re-runs steps 1–3 for
   the repos not yet published, at most twice, before reporting an
   integration failure (I4, "transient").
4. **Record.** Write `status: committed`, `summary`, `commits:` with the
   **post-integration** hash of each repo's shared tip (the same field as
   today), and `created_files`.

The daemon then verifies the result in `afterCommitSession`:
`git merge-base --is-ancestor <commits[i].hash> refs/heads/<pb>` for every
entry, plus the W6 ancestor check on the task branch. The agent's word alone
is not enough. That check is what makes `committed` mean "integrated".

The shared working tree must be clean for `merge --ff-only` to update it.
Nothing edits it in the target design: task agents have their own worktrees,
and planning and the wolf are rw by convention only. A dirty shared tree
therefore means someone broke a rule, and integration blocks with
`shared workspace <repo> has uncommitted changes` rather than guessing.

### I4. Conflict and failure handling

The commit agent classifies how it ended by writing the task file. The
daemon's `afterCommitSession` routes on that classification. `status` stays
within the existing enum. The new structured `integration:` block is defined
in [Schema/state changes](#schemastate-changes). The fields this section
relies on are `result`, `repo`, `files`, `onto`, and `landed` (the tasks
whose commits appear in `base..onto`, derived by the daemon from the
`commits:` of tasks committed since dispatch).

| Outcome | Commit agent writes | Daemon action | Counts against |
|---|---|---|---|
| **Integrated** | `status: committed`, `commits:` | Verify (I3). Clean up (W6). Stamp `completed_at`. Dispatch dependents. | — |
| **Textual conflict** during rebase | `status: failed`, `integration: {result: conflict, repo, files, onto}`, `failure_reason: "integration conflict in <repo>: <files>"` | **Re-run the task on the updated branch.** Save `attempt-<n>`. Remove the worktree (W6). Set `attempts++` and `status: ready`. Keep `integration:` for the next run. The scheduler re-cuts a worktree from the **current** tip. The re-run's task prompt tells the agent that a previous attempt exists at `refs/wm/<pb>/<task>/attempt-<n>`, which files conflicted, and which tasks landed in between. The usual cheap fix is `git cherry-pick` of its own attempt, resolving against code it can now read. | `attempts` (shared `maxTaskAttempts = 3` budget) |
| **Transient** failure: shared tip moved outside the queue, git `index.lock`, sandbox or wrapper start failure (`runner.StartFailure`), or the session ended with the task still `success` | nothing, or `integration: {result: error, …}` | **Auto-retry the commit agent only.** The task's work is fine, so it is not re-run. Relaunch `launchCommitAgent` for the same task, up to `maxIntegrationAttempts = 2`, tracked in `integration.attempts`. After that, block. | `integration.attempts` (not `attempts`) |
| **Dirty shared tree**, daemon verification failed (`commits:` not ancestors of the tip), or the agent wrote `committed` with nothing on the shared branch | anything | **Block** immediately. These mean an invariant broke. Re-running would just repeat it, and continuing would integrate on top of something unknown. | — |
| **Partial publish** (repo A fast-forwarded, then repo B conflicted on a re-snapshot) | `status: failed`, `integration: {result: partial, published: [A], repo: B, …}` | **Block.** A has landed, so re-running the whole task would double-apply A. The wolf finishes B by hand, from B's worktree, which is kept. This is only reachable when something outside the queue moved the tip mid-integration. | — |
| **Re-runs exhausted** (`attempts == maxTaskAttempts` when a conflict arrives) | as for conflict | **Block** with `task "<t>" conflicted with <landed tasks> in <files> after <n> attempts`. Keep the worktree and attempt refs (`retainSandbox`-style). | — |
| **Push task** rejected | `status: blocked` (unchanged) | **Block** (unchanged). | — |

Why re-run is the default for conflicts, and not commit-agent resolution:

- **The commit agent has the wrong context.** It sees two diffs, not the
  intent behind either. The task agent re-running on the new tip has the task
  description, the code as it now is, and its own previous attempt to crib
  from. That is the same position a human rebasing a stale PR is in.
- **A re-run is retry-shaped,** so it reuses the existing `handleTaskFailure`
  machinery (`attempts`, the `maxTaskAttempts` cap, `retainSandbox` on
  exhaustion, `task_retry` audit). The only difference is that the worktree is
  re-cut from a newer base. No new state-machine edge is needed.
- **Conflicts should be rare** if the planner's independence hints are any
  good. A conflict is evidence the hints were wrong. The scheduler can react
  by not co-scheduling the re-run with the tasks it conflicted with (the
  `landed` list); see [Scheduler & dispatch](#scheduler--dispatch).
  Repeated conflicts end at the wolf instead of looping.

A later, opt-in improvement is a per-project `conflict_resolution: agent`
setting that lets the commit agent attempt the resolution itself before
falling back to re-run. It belongs in the open questions, not in v1.

**Semantic conflicts** (a rebase that applies cleanly but breaks the build)
are **not** detected by integration, just as a sequential pipeline would not
detect them either. They surface in later tasks, in the review agent's CI
checks, or in an optional per-project `verify:` command run in the task
worktree after step 2 and before step 3. That command is deferred to
[Risks & open questions](#risks--open-questions).

### I5. `attempts` and `failure_reason` semantics

- **`attempts`** keeps meaning "how many times this task's agent has been
  re-launched". A conflict re-run is a re-launch, so it increments
  `attempts`. Integration retries do not, because the task agent did not run
  again. They have their own `integration.attempts`, which is reset to 0 each
  time the task re-runs.
- **`failure_reason`** stays agent-written and free-text. Today
  `handleTaskFailure` clears it on reset to `ready`. That behavior stays for
  ordinary failures. For a conflict re-run, the conflict details survive the
  reset in the structured `integration:` block, which is cleared only when the
  task reaches `committed`. The re-run agent therefore sees why it is running
  again, and `failure_reason` keeps its current meaning: the reason for the
  **latest** attempt's failure.
- **`committed`** now means "integrated onto the shared branch and verified"
  rather than "committed in the shared tree". `taskgraph.Ready` (`depsCommitted`)
  is unchanged, and that redefinition is exactly what makes W4's base rule hold.

### I6. What the wolf sees

When integration blocks the project, `transitionProjectBlocked` runs as
today, and the wolf is dispatched with the task in `FailedTasks`. The wolf
gets:

- **The `blocked_reason` on the project,** naming the task, the repo, the
  conflicting files, the tasks that landed since its base, and the attempt
  count. For example: `task "api-pagination" conflicted with ["api-auth"] in
  workingman: internal/api/handler.go after 3 attempts`.
- **The task file:** `attempts`, `failure_reason`, the `integration:` block
  (`result`, `repo`, `files`, `onto`, `landed`, `published` for partials),
  `base:`, and `worktree:`.
- **The retained task worktree,** visible inside its existing `<shared>`
  mount at `.worktrees/<task>/`, plus the `refs/wm/<pb>/<task>/attempt-*`
  refs. It can run `git -C .worktrees/<task>/<repo> diff <base>..attempt-<n>`
  to see what the task wanted, and `git log <base>..<pb>` to see what landed
  in between.
- **`wolf.tmpl`** gains a short "integration conflicts" paragraph with three
  ways out:
  1. Reset the task to `ready` (with `attempts: 0`) to re-run it on the
     current tip, optionally editing its description or `depends_on` so it
     waits for the task it conflicted with.
  2. Resolve by hand in the retained worktree (rebase onto `<pb>`, resolve,
     `git rebase --continue`), then set the task to `success`. The commit
     queue then integrates it, normally by the fast path.
  3. For a partial publish, finish the remaining repos and set `committed`
     with the right `commits:`. The daemon's ancestor check validates it.

The TUI shows the same block reason it shows today, with no new UI
required. A conflict re-run that eventually succeeds never reaches the wolf.
It is visible only in the audit log (`integration_conflict`,
`task_retry` with `cause: conflict`) and in the task's `attempts`.

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
