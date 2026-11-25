# Agents

> **Package map:** [docs/packages.md](docs/packages.md) is the generated index of
> every `internal/` package — its role (daemon / tui / shared / wrapper),
> synopsis, exported types and the import graph. Read it first to find your way
> around the code; [docs/package-docs.md](docs/package-docs.md) explains the
> `doc.go` convention that feeds it.
>
> **In progress:** [docs/parallel-task-worktrees.md](docs/parallel-task-worktrees.md)
> is the design for running a project's tasks in parallel, each in its own git
> worktree, with the commit agent serializing integration onto the shared
> branch. Phases 1 and 2 have shipped: every non-push task already runs in
> its own worktree (concurrency is still 1), and the commit agent integrates
> it by rebasing onto the shared tip and fast-forwarding, as the task and
> commit agent sections below describe.

The orchestrator launches ten kinds of `claude` sessions. Seven run over a
project's lifecycle: **project → planning → task → commit**, with **wolf**,
**archive**, and **review** stepping in for blocks, cleanup, and PR watching
respectively. The other three belong to no project. The **workingman agent** is
a daemon-owned, always-on observer that answers questions about the orch state
(§8). The **Step-Up agent** is a daemon-owned, always-on receiver of other
agents' requests for more sandbox capability (§9). The **sleepy-head** agent
reports cron ticks missed while the machine slept (`internal/daemon/sleepy_head.go`).
`design.md` has the original spec; this document describes the agents as
implemented today, which has moved past that spec in places (most notably: an
ACP/sandbox launch path superseding the terminal-tab+`sbx exec` path for non-interactive
agents, and a review agent design.md never mentions).

Every agent's role is defined by `agent.Kind` (`internal/agent/agent.go`).
`Kind.Interactive()` splits the project-lifecycle seven into two families, but the *launch path*
is no longer exactly that split (see the wolf):

- **Autonomous** (project, planning, task, commit, review, archive) — run under
  `claude --print` (single turn, then exit) as an **ACP session** backed by a
  per-session `acp-wrapper` host process (`internal/runner/runner.go:
  Runner.startACP`), when the daemon is wired with an `AcpLauncher` (production
  always is). The wrapper creates the sandbox (with `acp-kit` layered on),
  execs the ACP client, and serves `<SessionsRoot>/<id>/agent.sock` for the TUI
  to stream. There is no terminal tab to switch to for these. The wrapper is
  launched `--exit-when-empty`: it ends when the TUI watcher that drove the one
  prompt disconnects.
- **Interactive** (wolf) — a human is expected in the loop.
  - The **wolf** is a *persistent* ACP session (conversational: no
    exit-when-empty; it ends when its work is done, see §5) when an
    `AcpLauncher` is configured, so the TUI, the daemon and messaging channels
    can all tune in to one conversation. `orch --wolf-host` (`Runner.WolfOnHost`)
    keeps the pre-ACP host wolf — in a new terminal tab (`internal/terminal`,
    docs/terminal-tabs.md) — as an escape hatch. `Runner.UsesACP(kind)`
    is the single place that answers "does this kind run under ACP?".

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
since sbx rejects underscores). Session/ACP-session names come from
`runner.sessionName`: `"<kind>-<task-name-or-branch-or-hash>"`; a terminal tab
is tracked by that name and titled with the project name (`runner.tabTitle`).

---

## 1. Project agent

**Kind:** `agent.ProjectAgent`. **Template:** `project.tmpl`. **Interactive:** no.

**Trigger.** `internal/daemon/dispatch.go: dispatchProject` calls
`launchProjectRootAgent(path, agent.ProjectAgent, p)` whenever it observes an
"unpopulated" `project.yaml` (`p.Unpopulated()`) — either the legacy
zero-byte file, or a `:new`-seeded file that has only `description:` set.

**Workspace/session.** No wsp workspace: `WorkingDir` is set directly to the
control directory holding `project.yaml` (`filepath.Dir(projectPath)`), so
`Runner.resolveWorkingDir` skips workspace provisioning entirely. Sandbox name
is `"<work-stream>-project"` (`runner.SandboxNameFor`) — deliberately distinct
from planning's sandbox, since project and planning run back-to-back in the
same control dir and would otherwise collide on one sandbox with different
mounts. Runs via the ACP path (autonomous, `claude --print`).

**Reads/writes.** Reads the seed `description:` in `project.yaml`. Writes the
fully populated `project.yaml` — `repos`/`new_repos`, `branch`, optionally
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
`launchPlanningAgent(path, p)`. This also fires after
a cron cycle re-arms a project (`requestCronReplan` sets `status: ready` +
`Replan: true`), after a pending intake file or task seed re-arms a resting
`idle`/`merged` project (`seed_replan`), and — new — on a **`working`**
project with intake no planning run has been handed yet (`unofferedIntake`):
`handleIntakeFile` dispatches a working project directly instead of flipping
it to `ready`, and the planner runs alongside its tasks. Intake that lands
while a planner is already running gets a follow-up run once that one ends
(`afterPlanningSession` re-dispatches), never a second concurrent planner.

**Workspace/session.** Also runs with `WorkingDir` pinned to the control
directory (no wsp workspace of its own) — sandbox name is the bare
work-stream basename (`SandboxNameFor`, case `PlanningAgent`). It is tracked
under a **dedicated planning slot**, `planningSessionKey = projectPath +
"#planning"`, beside the task (`#task:<name>`) and commit (`#commit`) slots,
so a planner can run while task and commit agents are mid-flight. Design choice: task and commit
agents work in the wsp workspace and only touch their own task file, while the
planner only adds task files in the control dir, so they do not conflict;
sharing one slot is what used to strand intake on a working project. The
one-planner rule is enforced by an in-memory reservation (`beginPlanning` /
`endPlanning`, `planningBusy`) taken before `runner.Start` and released in
`afterPlanningSession`, re-taken by `reconcileSessions` for an adopted planner.
Planning still waits for the bare slot when it holds a project or review agent
(both write the control dir), and a cron re-plan waits for every task and
commit slot and commit reservation to clear (a re-plan may delete tasks). While a planner is in flight, task dispatch defers
project completion and graph-error/no-ready-task blocks to
`afterPlanningSession`, and review dispatch stands aside. But unlike
project/wolf, planning *also* provisions the project's wsp worktree
(`Runner.resolvePlanningWorktree`, idempotent — the later task agent's own
`Create` call returns the same directory) and mounts it as a **second**
sandbox workspace, so the planner can read real source to inform task
breakdown without cloning (the sandbox has no GitHub credentials). That path
is surfaced to the template as `{{.Worktree}}`.

**Reads/writes.** Reads `project.yaml` and existing `tasks/*.yaml`. May
freely add/remove/edit files under `tasks/*.yaml` and edit `project.yaml` —
nothing else. Finishes by setting `status: working` with `updated_by: agent`.

**Prompt structure (`planning.tmpl`).** Three main parts:
1. Task file schema and the *hard* naming constraint: `name` must be
   non-empty, lowercase kebab-case, unique, stable once referenced by a
   `depends_on`, and no longer than `task.MaxNameLenFor(ProjectName)`
   characters (the sandbox name `<work-stream>-<task-name>` is an RFC-1123 DNS
   label capped at `sbx.MaxNameLen` = 63 chars) — computed live via the
   template's `nameBudget` function, which calls that same code so the number
   an agent is told can't drift from the one the loader enforces.

   The constraint is advisory in the prompt but **enforced in code**, because
   agents got it wrong anyway: `taskgraph.Load` repairs an over-budget name
   (shortened + digest-suffixed, logged as `taskgraph_repair`), and
   `runner.SandboxNameFor` passes every derived name through
   `sbx.SanitizeName` as a final backstop. An over-long name is therefore a
   cosmetic problem, never the "`sbx create` fails on every relaunch and the
   task never runs" failure it used to be.
2. A conditional branch on `{{if .Replan}}`:
   - **Replan** (cron cycle): treat existing tasks as last cycle's record —
     for each, delete / reset-to-`ready` (clearing `summary`/`commits`/
     `created_files`/`completed_at`) / or leave `committed` (settled,
     once-only setup) — then add new tasks per the project `description`,
     which is the standing instruction, not the old task list.
   - **Else** (normal/incremental): if it finds a "seed" task (empty `name`,
     filled `description`, from a human's `:task`), flesh it out without
     touching any other existing task; otherwise do a first plan from scratch.
3. Optional per-task `static_mcps:` (the server names of the sandbox's
   per-sandbox MCP gateway — see "MCP gateway" in README.md; no longer sbx
   `--static-mcp` flags) and `policies:`
   (sbx policy rules, `action`/`type`/`resource`, applied in declaration order
   right after `sbx create`) fields. Two paragraphs say when a task must name
   a server: `logseq` whenever the request mentions Logseq, and
   `post-office` when the agent may need to ask the operator or another agent
   something (see [Messaging capability](#messaging-capability-post-office)).
   The template also carries the fixed status enums for both
   project and task, and `model: default` on every new task. An optional
   per-task `harness:` picks the agent harness; empty (the norm) means the
   daemon's default `claude-acp` (the `--acp-kit` kit on `--acp-image`), the
   only one implemented — `Runner.resolveHarness` rejects any other name.

**Completion signal / lifecycle.** `afterPlanningSession` releases the planning
slot, then mirrors the project
agent's circuit breaker (`maxPlanningRetries`, backoff, block on hard failure
or exhausted retries). Productive exit is "no longer `status: ready`" — the
daemon resets the counter, clears `Replan` (`clearReplan`, written as
`daemon`), and re-dispatches (`working` → first ready task, plus a follow-up
planning run for intake that arrived mid-run). An intake file the run was
handed but left unprocessed is logged (`planning_intake_unprocessed`) and not
relaunched on a working project — it is re-armed through the breaker once the
project comes to rest.

---

## 3. Task agent

**Kind:** `agent.TaskAgent`. **Template:** `task.tmpl`. **Interactive:** no.

**Trigger.** A scheduling pass (`schedule`, `internal/daemon/schedule.go`)
on a project that is `status: working` (or `idle` with pending manual tasks)
admits tasks from `Ready()` in the DAG built by `internal/taskgraph` —
`Graph.Runnable`, up to `max_parallel_tasks` (default 1) minus the tasks
already in flight, within the global `--max-parallel-tasks` cap, skipping
tasks whose hints overlap one in flight — marks each running, and calls
`launchTaskAgent` for it. Every task agent has a session slot of its own,
`<project.yaml>#task:<name>`. A push task (`t.IsPushTask()`) skips the task
agent entirely and joins the commit queue (see below).

**Workspace/session.** *No* `WorkingDir`. The daemon's `launchTaskAgent`
first provisions/reuses the project's **shared workspace** with
`workspace.Manager.Create(ctx, branch, repos)` (wsp) — named after the
project's `branch`, containing every repo in `p.Repos` + `p.NewRepos` on that
branch — and then, in `prepareTaskWorktree` (`internal/daemon/worktree.go`),
gives the task its **own worktree**: `<shared>/.worktrees/<task>/`, one
`git worktree` per repo on the task branch `wm/<branch>/<task>`
(`internal/worktree`). The first dispatch cuts it from the shared tip; every
later dispatch (a retry, `:start` after a stop, a wolf reset) reuses it
exactly as the last run left it, dirty or not. The daemon stamps `worktree`,
`task_branch` and `base` on the task file, and a failure to cut it releases
the task and blocks the project (`worktree_create_error`). The agent's cwd
is the worktree root (`Plan.TaskWorktree`; `.orch/` is written there), and
sandbox name `"<work-stream>-<task-name>"` — each task gets its **own**
sandbox (so a task's `static_mcps`/`policies` can differ from siblings) —
mounts the worktree, each repo's git common dir (so the worktree's `.git`
pointer resolves and its commits land in the shared repo), the shared
`.gomodcache` when staged, and the project's control/orch dir (so the status
write-back to `tasks/<name>.yaml` lands somewhere that exists inside the
sandbox). It never mounts the shared working trees.

Legacy path: with a workspace manager that isn't git-backed (the plain stub),
for a push task, or while legacy work is still in the shared tree (another
task with no `task_branch` at `running`/`success`, or uncommitted changes in a
shared repo; `worktree_skipped_legacy`), the task runs in the shared
workspace as it did before worktrees.

**Reads/writes.** Reads its own `TaskPath` (task yaml) and the project file.
Does the actual code work in its worktree; it may commit on its own task
branch (or leave the work uncommitted) but must not touch any other ref or
push. Writes only `status: success|failed` (+ `failure_reason` on failure)
back to its task file — it must NOT touch `project.yaml`; integrating the
work is the commit agent's and daemon's job. (On the legacy path it must not
commit either.) A task whose `static_mcps` includes `post-office` can also
message the operator or another agent while it works (see
[Messaging capability](#messaging-capability-post-office)).
The agent's `acp-wrapper` also annotates the task file (via `task.Update`,
`internal/acpwrapper/record.go`): `resolved_harness:` (the sbx template the
sandbox was created from) once the sandbox exists, and `resolved_model:` (the
concrete model id claude-agent-acp reports in `usage_update._meta
["_claude/model"]`) on the first usage report, re-asserted after the agent
exits. `model:`/`harness:` stay the requested values; the TUI shows the
resolved ones when present.

**Prompt structure (`task.tmpl`).** Short and direct — this is the exact text
this very orchestrator run received (see `.orch/instructions.md` in this
task's own sandbox): task name/paths up top, "read the description and do the
work" — for a worktree task, that it is on its own branch and must leave
every other ref alone, plus, on a retry (`attempts > 0`) or a resumed run, a
block saying the worktree may hold the earlier run's committed and
uncommitted changes, how to inspect them against `base`, and that it may
keep, fix or undo them; on a re-run after an integration conflict or a
failed build check (the task's `integration:` block is set), a block naming
the conflicting repo and files (or the build output) and the tasks that
landed meanwhile, telling it to rebase its own branch onto the shared branch
and resolve them — an explicit instruction that even a git-no-op (a GitHub-API-only change)
should still exit `success` so the commit agent can finalize it, and the fixed
status enum with a call-out that `committed` from a task agent is an
invariant violation.

**Completion signal / lifecycle.** `afterTaskSession` re-reads the task file:
`success` → re-stamp `base` (merge base with the shared branch) and
`launchCommitAgent`; `failed`/`running`/`ready` (agent crashed or
never started) → `handleTaskFailure`, which bumps `Attempts` and resets to
`ready` for a relaunch in the **same** worktree, up to `maxTaskAttempts` (3), after which it retains
the sandbox (`retainSandbox`, sets `save_sandbox: true` for post-mortem) and
blocks the project (→ wolf); `blocked` → retains the sandbox and blocks
immediately (no retry budget spent); `committed` → treated as an invariant
violation, blocks the project.

---

## 4. Commit agent

**Kind:** `agent.CommitAgent`. **Template:** `commit.tmpl`. **Interactive:** no.

**Trigger.** The commit queue (`taskgraph.Queued`: tasks at `success`, plus
push tasks at `ready` whose dependencies are committed, FIFO by the
daemon-stamped `succeeded_at`). Every scheduling pass starts the commit agent
for the queue's head unless the project's commit reservation is held, so one
integration runs per project at a time, in the `<project.yaml>#commit` slot.
The queue is rebuilt from disk, so a daemon restart between task-end and
commit-start resumes it with no special case. A push task runs here with
`PushBranch: true`; a transient integration failure relaunches the commit
agent for the same task (`launchCommitAgent`).

**Workspace/session.** No `WorkingDir`, so it runs in the project's shared
wsp workspace — where the shared branch is checked out, and under which the
task's worktree sits at `.worktrees/<task>/`. It has a sandbox of its **own**,
`"<work-stream>-<task-name>-commit"` (`SandboxNameFor`), mounting the shared
workspace and the control dir: its mounts differ from the task sandbox's, so
reusing that one would make acp-wrapper recreate it on every hand-off. It
gets the task's `StaticMCPs`/`Policies`/`SaveSandbox`/model/harness.

**Reads/writes.** Reads the task file. For a worktree task
(`Plan.TaskWorktree` set from the task's `task_branch`) it commits any
leftovers in the task worktree, rebases the task branch onto the shared tip
there, and fast-forwards the shared branch to it (`git merge --ff-only`); a legacy or push task does `git
add`/`git commit` (and, for a push task, `git push`) directly in each repo of
the shared workspace. Either way it uses
the git identity injected by the environment (never `git config
user.name`/`--author`). Writes `status: committed`, a `summary:`, a
`commits:` list (repo dir name + full SHA), and an optional `created_files:`
list back to the task file — or, when a worktree integration stops,
`status: failed` with an `integration:` report (`result: conflict |
verify_failed | partial | error`, `repo`, `files`, `onto`, `output`,
`published`).

**Prompt structure (`commit.tmpl`).** A worktree task (`{{if .TaskWorktree}}`)
gets the I3 procedure of docs/parallel-task-worktrees.md, written to be
safe to re-run from the top after an interrupted attempt: step 0 commit
leftovers in the task worktree and skip repos whose task branch still equals
`base`; step 1 check each shared checkout is clean and on the shared branch
(else `status: blocked`) and snapshot its tip; step 2 rebase every repo's
task branch onto its tip before publishing any — on a conflict abort it and
reset every repo already rebased, so nothing is published, and report
`result: conflict`; when the project sets `verify:` and something was
rebased (and the daemon did not find the task docs-only), run that build
check and report `result: verify_failed` on failure; step 3 fast-forward
each shared repo with `--ff-only`, going back to step 1 at most twice if the
tip moved (`result: partial` if some repo was already published, else
`result: error`); step 4 record each repo's shared tip. Otherwise, two modes gated by `{{if .PushBranch}}`:
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

Only push-task commits get the wrapper's ssh push preflight (`--push`, from
`runner.Plan.Pushes`); an ordinary commit's sandbox has no route to
`github.com:22`, so probing it there would abort every local commit. When the
wrapper does give up before the agent runs (sandbox create or preflight
failure) it records `status: failed` plus `error` in `session.json`: the TUI's
tab shows that error instead of a "starting" placeholder, and
`afterCommitSession` puts it in the project's `blocked_reason`.

**Before launch** (worktree task): `prepareIntegration`
(`internal/daemon/integration.go`) aborts a rebase an interrupted run left in
the task worktree (`integration_rebase_aborted`), removes stale `index.lock`
files (`integration_lock_cleared`), and blocks the project instead of
launching when a shared repo has uncommitted changes. It records each repo's
shared tip, which tells the fast path (nothing to rebase) from a real rebase.

**Completion signal / lifecycle.** For a legacy or push task
`afterCommitSession` requires `status: committed`; anything else blocks the
project for the wolf. A worktree task's other outcomes are routed on its
`integration:` report (`routeIntegrationFailure`): `conflict` or
`verify_failed` → re-run the task in the same worktree with `attempts++`
(`task_retry cause=conflict|verify`), or block with a reason naming the
tasks that landed meanwhile once `maxTaskAttempts` is spent; `error`, no
report, or the task still at `success` (a transient failure, including a
commit agent that could not start) → relaunch only the commit agent, up to
`maxIntegrationAttempts` (2, counted in `integration.attempts`), then block;
`partial` or `blocked` → block. For a committed worktree
task `finishCommittedTask` then checks that every `commits:` hash and the
task branch are ancestors of the shared branch, and that no repo with work
is missing from `commits:` (`integration_verify_failed` → block, worktree
kept), clears `integration:`, and removes the worktree and branch — the only point in a task's life
where they go (`worktree_removed`; a failed removal is only logged). On success it
stamps `completed_at` (once), reloads the task graph, and either
transitions the project to `done`/`reviewing` (`transitionProjectComplete`,
all committed) or runs a scheduling pass, which starts the next integration
and fills the task slots the committed task freed.

---

## 5. Wolf agent

**Kind:** `agent.WolfAgent`. **Template:** `wolf.tmpl`. **Interactive:** yes.

**Trigger.** `transitionProjectBlocked` (called from many failure paths —
task retries exhausted, taskgraph errors, hard launch failures, cron with no
stop condition, review-loop escalations, etc.) always ends by calling
`launchWolfAgent`. Also directly on `dispatchProject`'s `status: blocked`
branch (covers a block set by a human, the project agent, or a daemon
restart finding the file already blocked).

**Two launch modes.** Which one runs is decided by `Runner.UsesACP`:

| | ACP wolf (default with `--acp-kit`) | host wolf (`--wolf-host`, or no `--acp-kit`) |
|---|---|---|
| process | `acp-wrapper --persistent` host process → sandboxed `claude-acp-client` | new terminal tab (titled after the project) running `claude --dangerously-skip-permissions` |
| where it runs | sbx sandbox `<work-stream>-wolf` (`runner.ACPSandboxNameFor`) | directly on the host, no sandbox (`SandboxNameFor` returns `""`) |
| who can talk to it | anything that can dial `<SessionsRoot>/<id>/agent.sock`: the TUI tab, the daemon, a messaging channel | a human at that terminal tab |
| survives an `orch` restart | yes (`detachable`; re-adopted under the wolf key by `reconcileSessions`) | no (closed on shutdown, like before) |
| ends | see "Completion" below | when claude in its tab exits (or the tab is closed) |

**Workspace/session.** `WorkingDir` = control dir (like project/planning), so
no wsp provisioning of its own; `Repos`/`Branch` are still forwarded in case
the wolf needs to look at source. It is tracked under a session key distinct
from the project's main slot (`wolfSessionKey = projectPath + "#wolf"`), so it
can run *in tandem* with a task/commit session (or a planner, which has a slot
of its own too) rather than waiting for that slot to free up. `Plan.Persistent` is set for every wolf launch; it
only has an effect on the ACP path.

**Host access: what the ACP wolf gets and loses.** The decision (goal 3): the
wolf runs sandboxed by default, because anything that must be reachable by the
daemon, the TUI and a messaging channel has to be a long-lived ACP session, and
ACP sessions are sandboxed (the wrapper owns sandbox creation). To keep it
useful it is given:

- **Mounts:** the control dir (primary/cwd — it holds `project.yaml`,
  `tasks/`, `blocked-session.yaml` and `.orch/`, i.e. everything the wolf
  writes) and, when the project has `Repos`+`Branch`, the project's wsp
  worktree as a second mount (provisioned idempotently by
  `Runner.resolveWolfWorktree`, **best-effort**: a wsp failure is audited as
  `wolf_worktree_unavailable` and the wolf launches without the source mount,
  since a broken wsp is exactly the kind of thing it is summoned to look at).
- **Prompt:** `prompts.Data.Sandboxed` / `.orch/context.yaml: sandboxed: true`
  switch `wolf.tmpl` to the sandboxed wording: what it can see, what it can't
  do, and to ask the user to perform host-only fixes.

What it **loses** versus the host wolf: it cannot run `sbx`, so it cannot
`sbx exec` into or inspect the sandbox a failed task/commit agent ran in (the
wrapper deliberately keeps those for post-mortems); no `wsp`, no `osascript`,
no host git credentials or SSH signing agent, no access to the daemon's audit
log / sessions dir (outside the mounts), and sbx's default network policy. So
**sandbox-, sbx-, wsp- and credential-related blocks are better diagnosed with
`--wolf-host`.** Everything that is decided from files — task
`failure_reason`s, the blocked-session record, the source, the project/task
YAML it must edit — works unchanged. `--wolf-host` is a daemon-wide switch
(`Runner.WolfOnHost`, logged as `wolf_host` at `daemon_start`); there is no
per-project override yet.

**Reads/writes.** Reads the project's `blocked_reason`, the failed/blocked
tasks' `failure_reason`/`blocked_reason` (`FailedTasks`, paths gathered by
`failedOrBlockedTaskPaths`), and any prior durable diagnosis at
`project.BlockedSessionPath`. Writes/updates that same blocked-session record
(`blocked_reason`, `summary`, `attempted`) every time it investigates, and
ultimately updates `project.yaml`/task files to move the project out of
`blocked`. Scoped strictly to this one project's files — never another
project's `project.yaml`.

**Prompt structure (`wolf.tmpl`).** Opens with scope rules (only this
project's files; "you run on the host" vs "you run inside a sandbox …"), the
blocked reason and failed-task list, and any inherited prior summary/attempted
list so a second wolf run doesn't re-derive diagnosis from scratch. Sandboxed
wolves then get an ENVIRONMENT paragraph (what they can't do). Then:
"investigate…". When it needs the user, the *host* wolf is told to send a macOS
notification and wait for them to switch to its terminal tab; the *sandboxed* wolf is told
to ask in the conversation and stop — the message appears in the TUI's wolf tab
and is relayed to whatever messaging channel is configured (the template stays
channel-agnostic), and the answer arrives as its next prompt — and to make the
`blocked → …` status change its **last** write (see Completion). Documents two
special cases: (1) a project blocked before it was ever populated (fill in the
missing fields the project agent couldn't infer, clear `blocked_reason`, set
`ready`); (2) a project blocked out of the PR-review loop (four specific
`blocked_reason` shapes from the review agent, and the instruction that the
"almost always" correct exit is back to `status: idle` with the PR watch data
intact). An "integration conflicts" paragraph explains task worktrees and the
`integration:` block and gives three ways out of an integration block (reset
the task to `ready` with `attempts: 0`, reusing or first resetting its
worktree; resolve by hand in the worktree and set `success`; finish a partial
publish and set `committed`), followed by the shared-branch warning: never
move the shared branch or leave the shared tree dirty while an integration
is in flight (any task at `success`), and route changes through a task
worktree and the queue instead. Ends with the fixed status enums for both
project and task.

**Completion signal / lifecycle.** No enforced schema — the wolf just edits
whatever files resolve the block. Whatever ends the session, the daemon's
`launchWolfAgent` `onEnd` callback calls `revisitProject`, which re-reads
`project.yaml` from scratch and re-routes based on whatever status the wolf
left behind (bypassing the daemon-write filter, since wolf writes as
`updated_by: agent`).

How an **ACP wolf ends** (the wrapper owns all of this, so it works headless and
across daemon restarts; the host wolf just lives until its terminal tab closes). The
first turn finishing is *not* an end — the wolf typically finishes its first
turn by asking the user something. It ends on the first of:

1. **Project unblocked (the normal exit).** `acp-wrapper --unblock-grace D`
   (default 2m, `Runner.PersistentUnblockGrace` / `orch --wolf-unblock-grace`,
   negative disables) polls the project file and, once its `status` has been
   anything but `blocked` *continuously* for D (a re-block inside the grace
   resets the clock; a deleted file counts as unblocked), closes the ACP
   client's stdin so the agent exits. The grace lets the wolf finish its closing
   message after flipping the status — hence the template's "make the status
   change your last write".
2. **Idle timeout.** `acp-wrapper --idle-timeout D` (default 24h,
   `Runner.PersistentIdleTimeout` / `orch --wolf-idle-timeout`, negative
   disables) ends the session after D with no ACP frame in either direction.
   Merely having a TUI open doesn't count as activity. It is the backstop for a
   block nobody answered; the durable `blocked-session.yaml` means little is
   lost, and a re-summon starts a fresh wolf from it. (The stranded-session
   reaper still never reaps the wolf — it is `Interactive()` — so this is the
   only timer.)
3. **The agent exits / the wrapper is signalled / the daemon closes it**
   (`:stop`, `session.Close()`): the usual paths.

On a clean self-exit the wrapper removes the sandbox (no task file to retain it
for); on a signal it keeps it (so a restart can resume), and it is reused
because the sandbox name is stable per project.

A second block arriving while a wolf is still inside its unblock grace dedups
against the live session (`launchWolfAgent` → `session_skip_duplicate`): the
old conversation continues (its wrapper sees `blocked` again and does not end),
so the user keeps talking to the same wolf — which has the *old* block's context
in its prompt; it re-reads `project.yaml`/the blocked-session record if asked.

**TUI.** An ACP wolf appears in the sessions pane with its sandbox name, and
**enter/click on that row opens the ACP tab view on the wolf's tab**
(`attachSelected`; there is no terminal tab to switch to). The wolf's tab
is marked *interactive* (`session.json: persistent` → `acpTabEvent.interactive`):
the watcher (`internal/tui/acpwatch.go`) drives the opening prompt once and then
**stays connected**, relaying messages typed in the tab (`acpInputs`,
`servePersistentInput`) as successive turns. In the tab: **enter** starts
composing, **enter** sends, **esc** stops composing (keeps the draft), **ctrl+u**
clears, ⌥j/⌥k switch tabs, ctrl+f/ctrl+b scroll; the usual single-letter
commands (q, z, h, l, and the j/k/G scroll keys) are disabled only while
composing. The host wolf (`--wolf-host`) runs in its own
terminal tab: enter on its row switches to that tab (where the terminal
supports it — see docs/terminal-tabs.md). On any ACP tab whose session is dead (failed to
start, errored, disconnected), **x** dismisses the tab and removes its session
directory; live and still-starting tabs refuse it.

**Messaging channels: start/end notifications and conversation.** With
`channels.yaml` configured (`docs/channels.md`) the wolf is also reachable from
a phone. `launchWolfAgent` creates a `wolfAnnouncement` and, once the session is
really up (not on a failed launch, not on the dedup no-op above), sends on topic
`wolf` — asynchronously, under a timeout, so a slow channel never stalls
dispatch — `🐺 wolf is running for <work-stream>`, the (truncated) blocked reason,
the failed tasks and "Reply to this message to talk to the wolf." The returned
message id is recorded in the conversation index (`ConversationTarget` = project
path, `#wolf` key, session id). When the session ends, `🐺 wolf finished for
<work-stream>: project now <status>` follows — only if the start message went
out. At most one start message per work stream per `notify.wolf_start_interval`
(default 10m; `wolf_start_suppressed`). A *reply* to the start message is
routed by the router to that wolf's session (`acpchat.Attach`) and the wolf's
answer comes back labelled `[wolf <work-stream>]`; `/wolf` binds a whole chat to
the wolf, and anything the TUI user types into the wolf is relayed to the bound
chat (observe mode), as are its tool-permission requests (rejected on timeout).
The wolf prompt stays channel-agnostic: it just asks in the conversation and
stops. Code: `internal/daemon/channels_notify.go`, `internal/router`.

**Known limits.** (a) The opening prompt, like every ACP agent's, is delivered
by a TUI watcher, so a `--headless` daemon with nobody watching never prompts an
ACP wolf (the same is already true of planning/task/commit; use `--wolf-host` for
a TUI-less setup). The one exception is the workingman agent (§8): the router
creates and primes its session itself, because nothing else would. (b) `acpclient.Connect`
issues `session/new`, so a TUI that restarts mid-conversation reconnects to the
same `claude-acp-client` process but starts a fresh ACP session (the replayed
transcript is only history; the agent's context for new turns is new). Binding
to the existing session id is what `acpchat` (below) does for a *second*
client; the TUI's own reconnect still re-runs `session/new`. (c) The hub fans
every frame out to every client, but JSON-RPC ids are per-client, so two
simultaneous *prompting* clients can see each other's responses; use the
daemon-side attach primitive (`internal/acpchat`, below), not a second raw
client, for that.

**Tuning in from the daemon (`internal/acpchat`).** `acpchat.Attach(ctx, idOrDir,
opts)` joins a *running* session's conversation while the TUI stays attached:

- It adopts the existing ACP session id (`acpclient.Client.Adopt`) instead of
  running `session/new` (a second, empty conversation) or `session/load` (the
  agent would replay the whole history to every client, duplicating the TUI's
  scrollback). The id comes from the newest `sessionId` in the session's
  `stream.log`, else from the first frame on the live socket (`DiscoverTimeout`;
  `CreateIfMissing` runs the full handshake for a TUI-less daemon, and
`Conversation.Created()` tells the caller it did, so the session still needs its
opening prompt — the router sends it for the workingman agent). If another
  client later runs `session/new`, the conversation follows it
  (`EventSessionChanged`).
- Its request ids live in a private range (`acpclient.Options.DistinctIDs`), so
  the hub's response fan-out can't cross-complete the two clients' calls.
- `Ask(ctx, text, OnProgress(..), FinalSegmentOnly())` sends one prompt and
  returns the assistant text with reasoning/tool noise stripped; a second
  concurrent `Ask` gets `ErrBusy`; a cancelled `ctx` sends `session/cancel`.
- `Events()` is the observe-only stream: assistant text from turns *other*
  clients started (`Own == false`), closed by an `EventTurnEnd` carrying the full
  reply — what a channel relays when the human types into the wolf.
- `SetModel(ctx, model)` switches the live session's model for every client
  sharing it (ACP `session/set_config_option`, `configId: "model"`; an alias
  like `opus`/`sonnet`/`haiku` or an advertised model id), from the next turn
  on — the launch-time `--model`/`ANTHROPIC_MODEL` only picks the starting one.
  `Model()` is the last value the agent reported (from `session/new`, a
  `SetModel` reply, or a `config_option_update`). Same primitive on
  `acpclient.Client` (`SetModel`/`SetConfigOption`/`ConfigOptions`).
- Agent permission requests go to `Options.OnPermission` (default: reject) and
  are also surfaced as `EventPermission`.
- A dropped socket (the hub evicts slow clients) is reconnected on the same
  session id; `ErrSessionGone` when the session directory is gone, its status is
  exited/failed, or the socket stays unreachable for `ConnectTimeout`.

Limits: if two clients start turns at the same instant their chunks can't be told
apart (the hub doesn't echo one client's requests to the other), so the tail of a
turn that began *before* our `Ask` is attributed correctly but a truly
simultaneous one is not. Adoption is verified against an in-process fake agent;
confirm it once against a live `claude-acp-client` (a `session/prompt` to a
session id this connection never created).

---

## 6. Archive agent

**Kind:** `agent.ArchiveAgent`. **Template:** `archive.tmpl`. **Interactive:** no.

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
Being autonomous it takes the ACP path like task/commit: a one-shot
(`--exit-when-empty`) `acp-wrapper` session with the wsp worktree and the orch
dir mounted. Because it pushes, `launchArchiveAgent` attaches an
`allow network github.com` policy to the Plan (`archivePolicies`) — sbx denies
egress by default and the archive agent has no task file to declare one — and
`runner.Plan.Pushes` passes the wrapper `--push`, which runs the ssh push
preflight for it.
Reconciliation after an orch restart re-takes the `beginCleanup` reservation
before adopting it, or the startup scan would read the still-set `cleanup: true`
and dispatch a second one.

**Reads/writes.** Per repo in the workspace: `git status --porcelain`,
classifies uncommitted files, never edits `.gitignore` itself (it reports the
edit it would have made and leaves that repo unfinished — nobody is attached to
approve one), makes at most one final commit, and pushes only when `git
rev-parse --abbrev-ref
--symbolic-full-name @{upstream}` and `git rev-list --count @{upstream}..HEAD`
together show real ahead-of-upstream work (never a hardcoded branch, never
`--force`/`--allow-empty`/`--set-upstream`). Writes back only `archive: true`
on full success — the project's `status:` field is left exactly as found,
since `archive` is an independent flag, not a status.

**Prompt structure (`archive.tmpl`).** A numbered procedure: (1) `git status`
per repo; (2) classify uncommitted paths, and report (never apply, never wait
on) a `.gitignore` proposal for anything that "doesn't belong" (build
artifacts, editor scratch, caches); (3) one commit if
needed; (4) push decision tree keyed strictly off the three shell variables
above, with an explicit "no workarounds" list; (5) report outcome by editing
`project.yaml` (`archive: true` on full success, left unset with a clear
explanation of what's outstanding otherwise). Explicitly forbids any other
pre-commit work (no tests/lint/formatting/refactors).

**Completion signal / lifecycle.** `afterArchiveSession` always clears the
`cleanup` request flag first (written as `daemon`, whether or not `archive:
true` landed — a partial run logs `archive_incomplete` and is not retried
automatically; the user re-issues `:cleanup`), *then* calls `revisitProject`
so normal status routing resumes. The in-flight guard (`endCleanup`) is
released via `defer`, after the flag clear, so a late fsnotify event carrying
the agent's own `archive: true` write can't slip in and launch a second
archive session mid-cleanup. The reaper treats `archive: true` on the project
file as this kind's stage-complete signal (stripping the `#archive` marker off
the session key to find it), so a wrapper that outlives the agent is reaped;
a run that gave up has no such signal and is covered by the idle timeout only.

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
(`reviewPollSpecs`: 2m/4m/8m/16m/32m/1h, keyed `projectPath + "#review"`) and
kicks an immediate reconcile. `onReviewPoll` re-fires it on each tick unless a
cheap host-side GitHub MCP fingerprint (head SHA, latest comment/review ids,
thread counts, check states) matches the baseline from the last reconcile, in
which case it only backs off the cadence without launching an agent. When the
MCP probe is unavailable it falls back to the fingerprint the review agent
recorded in `project.yaml` as the baseline, and fails open (runs the agent)
while still backing off. A `:review` command
(`ReviewNow`) or a poll detecting change snaps the cadence back to the front
and dispatches immediately (`dispatchReviewAgent`, deduped against any
project-root session already running).

**Workspace/session.** `WorkingDir` = control dir (runs like planning — no
wsp workspace of its own), plus `Repos`/`Branch` forwarded, and
`StaticMCPs: []string{"github"}` (the `github` server of its per-sandbox
MCP gateway) plus a fixed network policy
(`reviewNetworkPolicies`: allow `api.github.com` and
`api.githubcopilot.com`) so the sandbox can reach GitHub's API through the
`github` MCP. `SandboxNameFor` gives it its own sandbox,
`"<work-stream>-review"`. Runs on the ACP path (autonomous, `claude --print`),
sharing the project's *main* session slot (not a side key like
wolf/archive/planning), so it cannot run concurrently with a task/commit agent
for the same project; it also stands aside while a planner is in flight
(`planningInFlight`), since both write the control dir.

**Reads/writes.** Reads `project.yaml` (`description`, `repos`, `branch`,
`review`, prior `pull_requests:`) and existing task files' `status`/`source`.
Uses the `github` MCP exclusively for PR access — explicitly forbidden from
running `gh`, `git push`, or cloning anything. Writes only within the control
dir: new task files (fix tasks and push tasks) and `project.yaml` (the
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

---

## 8. Workingman agent

**Kind:** `agent.WorkingmanAgent` (`String()` = `"workingman"`, appended to the
iota block so no existing value shifts). **Template:** `workingman.tmpl`.
**Interactive:** no. *(Not in design.md. A different animal from the other
seven: it is not part of any project's state machine, has no project and no
task, and is started by the daemon rather than dispatched by a status change.)*

**What it is.** A long-lived, always-on assistant that sits in a sandbox of its
own and answers a human's questions — relayed over messaging channels, e.g.
WhatsApp or Signal — about the orch state: which projects are open, which tasks are
running/blocked/failed and why, what the wolf is doing, what happened recently
in the audit log. On request (and after confirming) it also **changes** that
state, by editing the files under the orch roots exactly as a human would —
every TUI `:` command is a `project.yaml` edit, so it can stop/start, summon
the wolf, kick a review, request a cleanup, retry a task, unblock a project,
queue intake work or start a new project. It is autonomous-ACP (nobody drives its prompt by hand;
`Interactive()` is false) but **persistent**, using the same
`acp-wrapper --persistent` mode as the wolf, so its conversation outlives every
client that attaches to it (the TUI, the message router). Unlike the wolf it
has **no end condition**: no `--unblock-grace` (there is no project to watch)
and no `--idle-timeout` (silence between questions is its normal state), and
the stranded-session reaper exempts it (`strandedVerdict` in `reaper.go`).

**Launch/identity.** `runner.Plan{Kind: WorkingmanAgent, WorkingDir: <scratch>,
SessionName: "workingman-agent", Persistent: true, WritableMounts, ReadOnlyMounts, Observe}`.
Requires the ACP path (`Runner.Start` refuses otherwise — the terminal-tab path
has no read-only mounts, and silently launching it with write access would defeat the
point). The ACP session id and the sbx sandbox are both the fixed name
`workingman-agent` (`runner.WorkingmanAgentSession` / `WorkingmanAgentSandbox`,
`ACPSandboxNameFor`), independent of any project path. The daemon tracks it
under the session-map key `"workingman-agent"` — not a project path and not a
`path#marker` key, so `ListSessions` labels its row `workingman` and the
snapshot gives it no work stream / project path. `session.json` carries
`kind: workingman`, no `project_path`, no `task_path`.

**MCP.** Because its sandbox is reused across every daemon restart, it is
launched with `acp-wrapper --require-okta-gateway` whenever orch has a gateway
URL. Its gateway is dynamic (`--mcp-gateway-servers` plus `mcp-find` /
`mcp-add`, so it can attach servers such as Slack itself) unless
`--workingman-agent-mcp-dynamic=false` or `--mcp-gateway-static`: the wrapper
recreates the sandbox when it was built from a different kit, image or gateway
server set / dynamic setting, refuses
to start without `MCP_URL`, and verifies inside the sandbox that `okta-gateway`
is registered and the stock `mcp-gateway` is gone before the agent starts. See
[docs/mcp-gateway.md](docs/mcp-gateway.md#the-workingman-agents-sandbox).
A recreate also drops any LLM gateway secret scoped to the
`workingman-agent` sandbox (sbx deletes it with the sandbox), leaving the agent
on the direct API, which a TLS-inspecting network breaks with "Self-signed
certificate detected"; keep that secret global (see the README section on the LLM gateway).
The LLM TLS preflight every ACP launch
runs fails the launch with that diagnosis.

**Mounts (read-only enforced by the mount).** The scratch dir is the agent's
cwd and holds the usual `.orch/` handoff files. Every orch `--root` is a
second, **writable** workspace (`Plan.WritableMounts`): the agent edits
`project.yaml` / `tasks/*.yaml` and creates `project.md` / `intake/*.md` there.
The guard on *what* it edits is the prompt (see below), not the mount. All other
observed paths are a separate `--workspace <path>:ro` (sbx's read-only
bind-mount syntax, understood by `acp-wrapper --workspace`):

- the snapshot's directory (the file is replaced by rename, so the directory is
  mounted, not the file) — the daemon's live in-memory view;
- the audit log's directory;
- the ACP sessions root — `session.json` and `stream.log` of **every** session,
  including each wolf's, which is how it reads a wolf conversation.

`runner.workingmanWorkspaces` makes them absolute, de-duplicates, and drops a
read-only mount that lies under another one (or that would contain, or sit
inside, the scratch dir or a writable root). The scratch dir defaults to
`<sessions-root>/../workingman-agent` and must lie outside every `--root`
(`New` rejects an overlap — a write there would feed the daemon's own watcher).
Because the snapshot, audit log and sessions are mounted read-only, the
agent has no write path to them, and gets no static MCP
and no extra network policy: no GitHub or other secrets beyond what claude
itself needs. `Plan.Policies` / `WorkingmanAgentConfig.Policies` are forwarded
to `sbx policy` like any task's, for a filesystem rule limiting writes to the
scratch dir where sbx supports it; none is applied by default, since the mounts
already give the guarantee and the filesystem-policy semantics are sbx's.
(`acp-wrapper` treats the first `--workspace` as cwd and rejects a read-only
one; it compares existing sandboxes on host paths only, so a relaunch reuses
the sandbox.) Note the audit-log directory also holds `channels.log`,
`daemon.log` and `acp-wrapper.log` if the audit log lives beside them; if that
directory lies inside a `--root` the read-only mount is dropped in favour of the
writable root, so the prompt, not the mount, keeps the agent off the logs.

**Prompt structure (`workingman.tmpl`).** It states the agent observes and, on
request, edits the orch roots; lays out the orch directory (per work stream:
`project.md`, `project.yaml`, `tasks/`, `intake/*.md` / `*.processed`,
`blocked-session.yaml`, `last-planned.yaml`, `.orch/`) and every log at its real
path — the audit log and its siblings `daemon.log`, `acp-wrapper.log`,
`channels.log`, `whatsapp-bridge.log`, the state dir's
`channel-conversations.json`, each session's `session.json`/`stream.log` (from
`prompts.Data.Roots`, `SnapshotFile`, `AuditLog`, `SessionsRoot`, mirrored into
`.orch/context.yaml`; the template's `dir` func derives the sibling paths);
says to START by reading the snapshot (checking `generated_at` /
`daemon.state` for a dead or stale daemon) and to re-read it for every
question; summarises the snapshot schema; embeds a condensed project/task
state machine (the semantics of `state-machine.md`); explains finding a wolf
session in `sessions[]` and reading `<sessions-root>/<id>/stream.log`; sets
the answer style (replies go out over WhatsApp or Signal: short, plain text, no wide
tables, lead with the answer, cite `blocked_reason`/`failure_reason`); and
gives the editing contract ("Making changes"): only on the human's request and
after confirming the exact change; always `updated_by: agent` (a `daemon` write
is ignored by the daemon's self-write filter); edit fields in place and write
atomically (temp file + `mv`); valid status values only; beware a live
task/commit agent overwriting its task file; never touch the logs, snapshot,
sessions, `last-planned.yaml`, `.orch/`, `*.processed` or the wolf-owned
`blocked-session.yaml`, and never mark a task `success`/`committed`. It spells
out each TUI command's equivalent edit (stop/start via `stopped_from`, wolf,
review/`review_now`, cleanup, hide) plus retry/unblock recipes, and says
`:archive` (host-side: moves the directory, removes the wsp workspace) is for
the human.

**Supervision (`internal/daemon/workingman_agent.go`).**
`daemon.WithWorkingmanAgent(WorkingmanAgentConfig{...})` enables it; `Run`
starts a supervisor goroutine right after `reconcileSessions` (so a live agent a
prior daemon left behind is adopted, not duplicated), before the startup scan.
The supervisor starts the agent, waits for it to end, and relaunches it with
exponential backoff — 5s doubling to a 5-minute cap, restarting from the minimum
once a launch survived 10 minutes — whether it exited or failed to launch. A
failed launch is only audit-logged (`workingman_agent_start_error`,
`workingman_agent_restart_scheduled`); it runs on its own goroutine, so it can
never delay or block project dispatch. Unlike the other ACP agents it is **not
detachable**: it stops with the daemon (`shutdown` closes it, the supervisor
stops relaunching) and the next boot starts a fresh one — it is owned by this
run's flags, not by any project. It appears in the TUI session list like any
other session.

**Flag.** `orch --workingman-agent[=auto|on|off]`. The default `auto` turns it
on whenever `--acp-kit` is set, whether or not `channels.yaml` configures any
channels: a human can talk to it from its TUI session tab as well as from an
inbound channel. `--workingman-agent` / `=on` forces it
on (and is a startup error without `--acp-kit`); `=off` disables it.

**Attaching.** `Daemon.WorkingmanAgentSession()` returns the live session's
`ID`, `Dir` (`<sessions-root>/workingman-agent`), `SocketPath`, `SandboxName` and
`StartedAt`, or `false` when it isn't running (disabled, between restarts, launch
failing). The inbound-message router attaches through it with `acpchat` (see
`docs/channels.md`; live wolves are listed by `Daemon.WolfSessions`).

**Who starts its conversation.** In a TUI session the watcher discovers the
persistent session, runs `session/new` and sends the usual opening prompt. Under
`--headless` nobody does, so the router attaches with `CreateIfMissing`, and when
`Conversation.Created()` reports it had to create the ACP session it first sends
`router.OpeningPrompt` ("Read .orch/instructions.md and .orch/context.yaml, then
follow the instructions.") and discards the reply, so the agent has read
`workingman.tmpl` before the first question reaches it. A question that arrives
while the agent is still booting gets *"The workingman agent is starting up, try
again in a minute."* rather than a hang. Replies go back to the asking chat
(a `workingman` topic route is optional); every reply passes `audit.Redact`.

---

## 9. Step-Up agent

**Kind:** `agent.StepUpAgent` (`String()` = `"step-up"`, appended to the iota
block). **Template:** `step-up.tmpl`. **Interactive:** no. *(Design:
[docs/messaging-design.md](docs/messaging-design.md) §8. Operator guide —
turning it on, approving, `config.yaml`, the kill switch and a smoke test:
[docs/messaging.md § Step-Up](docs/messaging.md#step-up).)*

**What it is.** An always-on agent that other agents ask for more than their
sandbox gives them, most often a network route ("I need
`https://proxy.golang.org` for `go mod download`"). It is a post-office
**recipient**: an agent with the messaging capability sends it a request with
`send_request(recipient: "step-up", …)` and waits with `wait_for_response`.
Step-Up takes the request with `next_message`, judges it, asks the operator
with `send_request` (to the operator, with `in_reply_to` set to the agent's
request) when the ask is reasonable, and answers the agent with `reply`. It
always replies, refusals included.

**Applying changes: the executor.** Step-Up never changes a sandbox itself.
It writes a change request, `requests/<op-id>.yaml`, into its request
directory, and the daemon's executor (`internal/daemon/step_up_exec.go`)
checks it, applies it and writes `results/<op-id>.yaml` (`status: applied |
rejected | failed`, `detail`, and for `list_sandbox_policy` the `sbx policy
ls` output). The request format:

```yaml
version: 1
id: op_…                  # names the file; op_ + [A-Za-z0-9_-]{1,64}, unique
op: sandbox_network_allow # or sandbox_network_revoke, sandbox_filesystem_allow,
                          #    sandbox_filesystem_revoke, list_sandbox_policy
for_request: msg_…        # the agent's post-office request (always)
approval: msg_…           # Step-Up's approval request to the operator (allows only)
args: {resource: proxy.golang.org}
reason: "…"
```

Each op runs one `sbx` command on the **derived** sandbox — `policy allow
<kind> --sandbox <s> <resource>`, `policy rm <kind> --sandbox <s> --resource
<resource>`, or `policy ls --sandbox <s>` — and an allow or revoke is then
added to (or removed from) the target task's `policies:`, so the rule
survives a new sandbox and, because acp-wrapper re-applies a task's
`policies:` on reuse, a reused one. If the task file cannot be updated, the
`sbx` change is undone. The checks, all in the daemon (design §8.4):

- strict YAML: unknown fields, a second document, a bad `version`, an `id`
  that differs from the file name, an op outside the five, a missing
  `for_request`, a missing or superfluous `approval` or `args` → `rejected`;
- **target**: `from.sandbox` of `for_request`, which must be a request
  addressed to `step-up`, open or closed less than an hour ago; the sandbox
  must have a live `task` or `commit` session still running the task that
  asked; never `step-up`, `workingman-agent`, `sleepy-head` or a daemon
  gateway;
- **approval** (allows only): a request from Step-Up to the operator with
  `in_reply_to == for_request`, answered by the operator with option
  `approve` less than 24 h ago, whose body has the line `CHANGE: <op>
  <resource> → <sandbox>` exactly (`daemon.StepUpChangeLine`), and not used by
  an earlier applied op;
- **resources**: network — a lower-case hostname, or `*.` and a domain of two
  or more labels, optional `:port`; no IPs, `localhost`, `*.docker.internal`,
  local TLDs or config denylist matches. Filesystem — an absolute, cleaned
  path glob (`*`, `**` only) that does not cover `/`, the home directory or
  an ancestor of it, and does not overlap `~/.ssh`, `~/.aws`, `~/.config/gh`,
  `~/.workingman`, `~/.gnupg`, `~/.docker`, `~/.kube`, `/etc`, the orch
  roots, the request and post-office directories, or the config denylist;
- a revoke only removes a rule whose last applied Step-Up change was an allow;
- **rate limits**: at most `per_hour` (10) applied allows and revokes in any
  hour, and `per_sandbox_per_hour` (3) per sandbox;
- **kill switch**: `<step-up-dir>/disabled` rejects everything.

Every request, rejections included, is appended to
`<step-up-dir>/audit.jsonl` (the request as written, the target, the approval
record, the `sbx` argv and output, the outcome) and logged as `step_up_op` in
the daemon audit log. Each applied change sends the operator a post-office
notification (`🔓 Step-Up allowed proxy.golang.org for task fix-ci (proj)
(approved via slack)`; `🔒 … revoked …`). `<step-up-dir>/config.yaml`, read
by the daemon only, holds `deny: {network: [...], filesystem: [...]}` and
`limits: {per_hour, per_sandbox_per_hour}`; while it is invalid (an unknown
key, say) every request is rejected. There is no auto-approve list.

**Launch/identity.** `runner.Plan{Kind: StepUpAgent, WorkingDir: <scratch>,
SessionName: "step-up-agent", Persistent: true, StaticMCPs: ["post-office"],
WritableMounts, ReadOnlyMounts, Observe}`, built by `Daemon.stepUpPlan`. Like
the workingman agent it requires the ACP path (`Runner.Start` refuses
otherwise), its wrapper is `--persistent` with no `--idle-timeout` and no
`--unblock-grace`, and it gets `--require-okta-gateway`. The sbx sandbox is
`step-up` (`runner.StepUpSandbox`, `ACPSandboxNameFor`); post-office identifies
the caller by that name (its recipient registry, `mcpserver.DefaultRecipients`,
maps `step-up` → sandbox `step-up`), so the two must stay in step. The ACP
session id and the daemon's session-map key are `step-up-agent`
(`runner.StepUpSession`). `session.json` carries `kind: step-up`, no project and
no task; `ListSessions` labels the row `step-up` and the snapshot gives it no
work stream.

**MCP.** `post-office` only, on a **static** gateway: `MCPGatewayDynamic`
returns false for it whatever the flags say, so it has no `mcp-find` /
`mcp-add`. Its gateway record lists `post-office`, which is what post-office's
capability check needs (see "Messaging capability" below). No GitHub and no
other server.

**Mounts.** No orch roots, read-only or read-write: every state change goes
through the daemon's typed request interface, where it is checked.

- the scratch dir (cwd, `.orch/` handoff), default
  `<sessions-root>/../step-up-agent`;
- read-write: `<step-up-dir>/requests/`;
- read-only: `<step-up-dir>/results/`, the snapshot's directory, the audit
  log's directory and the ACP sessions root (so it can see which sandboxes,
  tasks and sessions exist).

The request directory (`StepUpAgentConfig.StepUpDir`) defaults to
`<sessions-root>/../step-up` (`~/.workingman/step-up`); `stepUpPlan` creates
`requests/` and `results/`. The scratch dir, the request directory and the
post-office store must all lie outside every `--root` (`New` rejects an
overlap).

**Who gives it its turns.** Nobody types into Step-Up, so the daemon drives it
(`driveStepUp` in `internal/daemon/step_up.go`). The driver attaches to the
live session with `acpchat` (`CreateIfMissing`, so a headless daemon works;
with the TUI running, the TUI's watcher creates the ACP session and sends the
opening prompt) and watches the post-office store (`inbox/` and `done/`,
fsnotify plus a 10 s poll). It sends `StepUpPrompt` — "serve your post-office
inbox … and end your turn" — when:

- a message addressed to `step-up` has arrived that it has not taken yet;
- one of its own requests (its approval requests to the operator) has closed;
- it holds a request it has not answered, as a reminder every 10 minutes;
- it created the ACP session itself, once, so the agent reads its
  instructions.

It never prompts during another client's turn (the TUI's opening prompt, or a
human typing in the agent's tab), at most once per 5 s, and each turn is
bounded to 30 minutes. The prompt and template tell the agent never to block
inside a turn: `next_message` with `timeout_seconds: 0`, `check_response`
rather than `wait_for_response`. Audit events: `step_up_driver_attached`,
`step_up_prompted`, `step_up_turn_error`, `step_up_driver_detached`.

**Prompt structure (`step-up.tmpl`).** Who it is (it never does the requesting
agent's work); that the daemon makes changes from its request files and it
must never say a change was made unless the result says `applied`; the
post-office tools it has (receiving, asking, replying) and how to use each
without blocking; the turn loop (`list_inbox`, drain `next_message`,
`check_response` its open requests, read its results, end the turn); always
reply; how to decide (specific and justified by the task; the narrowest
resource; outright refusals for wildcards, IPs, localhost, private ranges,
other sandboxes and secret paths; anything that is not a sandbox rule goes
back to the operator, with a notification); the five ops; the approval
request (`options: ["approve", "deny"]`, the exact `CHANGE:` line, one change
per approval); what to do for `approve`, `deny`, free text or no answer; the
request-file format, written under a dot-name and renamed; the result
statuses and what to reply for each; and the limits the daemon enforces. `prompts.Data.StepUpDir` (mirrored as `step_up_dir` in
`.orch/context.yaml`), `SnapshotFile` and `SessionsRoot` fill the paths.

**Supervision and lifecycle.** `daemon.WithStepUpAgent(StepUpAgentConfig{…})`
enables it. `Run` starts it right after the workingman agent: the same
supervisor (`superviseAgent`, shared with the workingman agent) restarts it with
the same backoff (5 s doubling to 5 min, reset after 10 min stable), and
`beginStepUpAgent` starts the driver. Without an ACP launcher it is skipped
(`step_up_agent_unavailable`), and also without `--mcp-gateway-url`, since its
only tools come through the gateway. Exactly where the workingman agent has
them, it has special cases: not `detachable` (it stops with the daemon), adopted
by `reconcileSessions` under its key when this daemon supervises one, exempt
from the stranded-session reaper, and labelled by its kind in `ListSessions`
and the snapshot. `Daemon.StepUpAgentSession()` returns the live session.

**Flag.** `orch --step-up-agent[=auto|on|off]`. `auto` (the default) turns it
on when `--acp-kit` is set **and** post-office accepts connections on
`127.0.0.1:7077` (`postoffice.DefaultAddr`) at startup. `--step-up-agent` /
`=on` forces it on (a startup error without `--acp-kit`); `=off` disables it.
The decision is audit-logged as `step_up_agent_flag`.

---

## Messaging capability (`post-office`)

Not a kind of agent but a capability any sandboxed agent can be given: the
**post-office** MCP server (`cmd/post-office`, `internal/postoffice/mcpserver`,
design in [docs/messaging-design.md](docs/messaging-design.md) §4, operator
guide in [docs/messaging.md](docs/messaging.md)). With it an
agent can `send_notification` (one-way) or `send_request` (expects an answer,
then `wait_for_response` / `check_response` / `cancel_request` /
`list_requests`) to the human operator or, by `recipient`, to another agent
such as `step-up`. Without it, those tools do not exist for the agent.

**The capability is the gateway being minted with `post-office`.**

- **Granting it.** A task lists `post-office` in its `static_mcps`. The
  planning agent adds it when the task description says the agent may need to
  ask the operator or another agent something, or keep the operator informed
  (the paragraph in `planning.tmpl`). Because `static_mcps` replaces the
  default set (`MCPServersFor`), such a task lists every other server it needs
  too. Agents whose plan the daemon builds itself get it by hard-coding it, as
  review hard-codes `github`; that is how the Step-Up agent gets it (§9).
  `post-office` is **never** in `--mcp-gateway-servers`, so no agent has it by
  default.
- **The catalog entry** (standalone-codemode `mcp-servers.json` and
  `mcp-servers-personal.json`): `url: http://127.0.0.1:7077/mcp`,
  `Authorization: Bearer ${post_office_token}` (the secret
  `mcp/post-office/post_office_token`, from `post-office setup --print`) and
  `dynamic_addable: false`.
- **`mcp-add` cannot grant it.** Task, wolf and workingman gateways are
  dynamic (`MCPGatewayDynamic`), so their agents can `mcp-add` catalog
  servers. Two independent checks stop that adding post-office:
  1. the catalog's `dynamic_addable: false` makes the gateway refuse
     `mcp-add post-office` (and hides it from `mcp-find`). This is defence in
     depth;
  2. the actual check, in post-office: every request carries the calling
     sub-gateway's id (`X-Codemode-Gateway-Id`, injected by
     standalone-codemode), and post-office looks it up in
     `~/.workingman/gateways/<sandbox>.json`. `Record.Servers` is what the
     gateway was **minted** with, which `mcp-add` never changes. A record
     without `post-office` gets `post-office is not enabled for this sandbox
     (add static_mcps: [post-office] to the task)` on `tools/list`,
     `tools/call` and `resources/read`.
- **Identity.** The sender (`from`) is never a tool argument: post-office
  derives it from the gateway record's sandbox and that sandbox's newest live
  `session.json` (kind, project, task), or from the fixed sandbox name for
  `workingman-agent`, `sleepy-head` and `step-up`. A sandbox with no live
  session is refused.
- **Tested** end to end by `TestStaticMCPsCapabilityEndToEnd`
  (`internal/postoffice/mcpserver/capability_e2e_test.go`): a task with
  `static_mcps: [post-office]` calls `send_notification` through its gateway
  and is attributed correctly; a task without it gets nothing through its
  gateway, and is refused even after post-office is attached to its dynamic
  gateway as `mcp-add` would. standalone-codemode's
  `TestGatewayIDAndDynamicAddableE2E` covers the header and the `mcp-add`
  refusal on the real gateway, and `TestSampleCatalogLoads` pins the entry in
  both catalogs.
- **Receiving (agent recipients).** `recipient` must be `operator` (the
  default) or a **registered recipient**: a static map in post-office
  (`mcpserver.DefaultRecipients`, v1 `step-up` → sandbox `step-up`), so a typo
  is rejected rather than stored. A caller whose sandbox is a registered
  recipient also gets `next_message` (the oldest open message addressed to it
  that it has not received; sets `received_at`, waits up to the max wait,
  `status: empty` otherwise; each message is returned once, even across
  restarts), `reply` (answers with `responder: {kind: agent, via: step-up}`,
  or `status: closed` with the outcome that won) and `list_inbox` (open
  messages addressed to it, received or not). A notification is closed as
  `dismissed` once received. On `send_request` it gets `in_reply_to`,
  which links its own request (such as an approval request to the operator)
  to the request it serves: set automatically when it holds exactly one
  received, unanswered request, otherwise passed explicitly; the Step-Up
  executor checks it (design §8.4). Other callers do not see these tools or
  `in_reply_to` once post-office knows who they are, and are refused if they
  call them. A gateway that lists tools before workingman saved its record
  (standalone-codemode attaches post-office during `POST /gateways` and keeps
  that list) sees them all; see design §4.5.
- **Where operator messages go.** Not to an agent: post-office writes them
  under `~/.workingman/post-office/`, the TUI inbox (`I`) reads them, and the
  daemon's Dispatcher sends them to every configured channel. The first answer
  wins, wherever it comes from. The end-to-end smoke test (a task asks a
  question answered in the TUI, one answered in Slack, then asks Step-Up for a
  network route) is in [docs/messaging.md](docs/messaging.md#end-to-end-smoke-test).
