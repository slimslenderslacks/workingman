# Agents

The orchestrator launches eight kinds of `claude` sessions. Seven run over a
project's lifecycle: **project → planning → task → commit**, with **wolf**,
**archive**, and **review** stepping in for blocks, cleanup, and PR watching
respectively. The eighth, the **workingman agent**, belongs to no project: it is
a daemon-owned, always-on observer that answers questions about the orch state
(§8).
`design.md` has the original spec; this document describes the agents as
implemented today, which has moved past that spec in places (most notably: an
ACP/sandbox launch path superseding the tmux+`sbx exec` path for non-interactive
agents, and a review agent design.md never mentions).

Every agent's role is defined by `agent.Kind` (`internal/agent/agent.go`).
`Kind.Interactive()` splits the project-lifecycle seven into two families, but the *launch path*
is no longer exactly that split (see the wolf):

- **Autonomous** (project, planning, task, commit, review) — run under
  `claude --print` (single turn, then exit) as an **ACP session** backed by a
  per-session `acp-wrapper` host process (`internal/runner/runner.go:
  Runner.startACP`), when the daemon is wired with an `AcpLauncher` (production
  always is). The wrapper creates the sandbox (with `acp-kit` layered on),
  execs the ACP client, and serves `<SessionsRoot>/<id>/agent.sock` for the TUI
  to stream. There is no tmux window to attach to for these. The wrapper is
  launched `--exit-when-empty`: it ends when the TUI watcher that drove the one
  prompt disconnects.
- **Interactive** (wolf, archive) — a human is expected in the loop.
  - The **archive** agent always takes the legacy path: a tmux window inside the
    shared umbrella session `orch` (`internal/agent/tmux.go`), running `claude
    --dangerously-skip-permissions` (no `--print`) inside `sbx exec -it
    <sandbox>`, so a human can attach (`tmux attach -t orch`) and drive the
    conversation or answer a macOS notification.
  - The **wolf** is a *persistent* ACP session (conversational: no
    exit-when-empty; it ends when its work is done, see §5) when an
    `AcpLauncher` is configured, so the TUI, the daemon and messaging channels
    can all tune in to one conversation. `orch --wolf-host` (`Runner.WolfOnHost`)
    keeps the pre-ACP host/tmux wolf as an escape hatch. `Runner.UsesACP(kind)`
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

**Two launch modes.** Which one runs is decided by `Runner.UsesACP`:

| | ACP wolf (default with `--acp-kit`) | host wolf (`--wolf-host`, or no `--acp-kit`) |
|---|---|---|
| process | `acp-wrapper --persistent` host process → sandboxed `claude-acp-client` | tmux window running `claude --dangerously-skip-permissions` |
| where it runs | sbx sandbox `<work-stream>-wolf` (`runner.ACPSandboxNameFor`) | directly on the host, no sandbox (`SandboxNameFor` returns `""`) |
| who can talk to it | anything that can dial `<SessionsRoot>/<id>/agent.sock`: the TUI tab, the daemon, a messaging channel | a human attached to tmux |
| survives an `orch` restart | yes (`detachable`; re-adopted under the wolf key by `reconcileSessions`) | no (closed on shutdown, like before) |
| ends | see "Completion" below | when its tmux window closes |

**Workspace/session.** `WorkingDir` = control dir (like project/planning), so
no wsp provisioning of its own; `Repos`/`Branch` are still forwarded in case
the wolf needs to look at source. It is tracked under a session key distinct
from the project's main slot (`wolfSessionKey = projectPath + "#wolf"`), so it
can run *in tandem* with a task/commit/planning session rather than waiting
for that slot to free up. `Plan.Persistent` is set for every wolf launch; it
only has an effect on the ACP path.

**Host access: what the ACP wolf gets and loses.** The decision (goal 3): the
wolf runs sandboxed by default, because anything that must be reachable by the
daemon, the TUI and a messaging channel has to be a long-lived ACP session, and
ACP sessions are sandboxed (the wrapper owns sandbox creation). To keep it
useful it is given:

- **Mounts:** the control dir (primary/cwd — it holds `.project.yaml`,
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
ultimately updates `.project.yaml`/task files to move the project out of
`blocked`. Scoped strictly to this one project's files — never another
project's `.project.yaml`.

**Prompt structure (`wolf.tmpl`).** Opens with scope rules (only this
project's files; "you run on the host" vs "you run inside a sandbox …"), the
blocked reason and failed-task list, and any inherited prior summary/attempted
list so a second wolf run doesn't re-derive diagnosis from scratch. Sandboxed
wolves then get an ENVIRONMENT paragraph (what they can't do). Then:
"investigate…". When it needs the user, the *host* wolf is told to send a macOS
notification and wait for them to attach to tmux; the *sandboxed* wolf is told
to ask in the conversation and stop — the message appears in the TUI's wolf tab
and is relayed to whatever messaging channel is configured (the template stays
channel-agnostic), and the answer arrives as its next prompt — and to make the
`blocked → …` status change its **last** write (see Completion). Documents two
special cases: (1) a project blocked before it was ever populated (fill in the
missing fields the project agent couldn't infer, clear `blocked_reason`, set
`ready`); (2) a project blocked out of the PR-review loop (four specific
`blocked_reason` shapes from the review agent, and the instruction that the
"almost always" correct exit is back to `status: idle` with the PR watch data
intact). Ends with the fixed status enums for both project and task.

**Completion signal / lifecycle.** No enforced schema — the wolf just edits
whatever files resolve the block. Whatever ends the session, the daemon's
`launchWolfAgent` `onEnd` callback calls `revisitProject`, which re-reads
`.project.yaml` from scratch and re-routes based on whatever status the wolf
left behind (bypassing the daemon-write filter, since wolf writes as
`updated_by: agent`).

How an **ACP wolf ends** (the wrapper owns all of this, so it works headless and
across daemon restarts; the host wolf just lives until its window closes). The
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
in its prompt; it re-reads `.project.yaml`/the blocked-session record if asked.

**TUI.** An ACP wolf appears in the sessions pane with its sandbox name, and
**enter/click on that row opens the ACP tab view on the wolf's tab**
(`attachSelected`; there is no tmux window to `tmux attach` to). The wolf's tab
is marked *interactive* (`session.json: persistent` → `acpTabEvent.interactive`):
the watcher (`internal/tui/acpwatch.go`) drives the opening prompt once and then
**stays connected**, relaying messages typed in the tab (`acpInputs`,
`servePersistentInput`) as successive turns. In the tab: **enter** starts
composing, **enter** sends, **esc** stops composing (keeps the draft), **ctrl+u**
clears, ⌥j/⌥k switch tabs; the usual single-letter commands (q, z, h, l) are
disabled only while composing. The host wolf (`--wolf-host`) is unchanged: enter
on its row `tmux attach`es.

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
in the audit log. It is autonomous-ACP (nobody drives its prompt by hand;
`Interactive()` is false) but **persistent**, using the same
`acp-wrapper --persistent` mode as the wolf, so its conversation outlives every
client that attaches to it (the TUI, the message router). Unlike the wolf it
has **no end condition**: no `--unblock-grace` (there is no project to watch)
and no `--idle-timeout` (silence between questions is its normal state), and
the stranded-session reaper exempts it (`strandedVerdict` in `reaper.go`).

**Launch/identity.** `runner.Plan{Kind: WorkingmanAgent, WorkingDir: <scratch>,
SessionName: "workingman-agent", Persistent: true, WritableMounts, ReadOnlyMounts, Observe}`.
Requires the ACP path (`Runner.Start` refuses otherwise — the tmux path has no
read-only mounts, and silently launching it with write access would defeat the
point). The ACP session id and the sbx sandbox are both the fixed name
`workingman-agent` (`runner.WorkingmanAgentSession` / `WorkingmanAgentSandbox`,
`ACPSandboxNameFor`), independent of any project path. The daemon tracks it
under the session-map key `"workingman-agent"` — not a project path and not a
`path#marker` key, so `ListSessions` labels its row `workingman` and the
snapshot gives it no work stream / project path. `session.json` carries
`kind: workingman`, no `project_path`, no `task_path`.

**Mounts (read-only enforced by the mount).** The scratch dir is the agent's
cwd and holds the usual `.orch/` handoff files. Every orch `--root` is a
second, **writable** workspace (`Plan.WritableMounts`), so the agent can create
`<root>/<project>/project.md` and `<root>/<project>/intake/<task>.md`; the
prompt limits it to creating new files, never editing existing state. All other
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
(`New` rejects an overlap — a write there would feed the daemon's own watcher,
). Because the snapshot, audit log and sessions are mounted read-only, the
agent has no write path to them, and gets no static MCP
and no extra network policy: no GitHub or other secrets beyond what claude
itself needs. `Plan.Policies` / `WorkingmanAgentConfig.Policies` are forwarded
to `sbx policy` like any task's, for a filesystem rule limiting writes to the
scratch dir where sbx supports it; none is applied by default, since the mounts
already give the guarantee and the filesystem-policy semantics are sbx's.
(`acp-wrapper` treats the first `--workspace` as cwd and rejects a read-only
one; it compares existing sandboxes on host paths only, so a relaunch reuses
the sandbox.) Note the audit-log directory also holds `channels.log`,
`daemon.log` and `acp-wrapper.log` if the audit log lives beside them.

**Prompt structure (`workingman.tmpl`).** It states the agent is a read-only
observer/answerer whose only write exception is creating new
`<root>/<project>/project.md` and `<root>/<project>/intake/<task>.md` files
(kebab-case names, human confirmation first, never overwriting, never touching
`.project.yaml`, `tasks/*.yaml`, `blocked-session.yaml` or other files); lists every observed path (from `prompts.Data.Roots`,
`SnapshotFile`, `AuditLog`, `SessionsRoot`, mirrored into `.orch/context.yaml`);
says to START by reading the snapshot (checking `generated_at` /
`daemon.state` for a dead or stale daemon) and to re-read it for every
question; summarises the snapshot schema; embeds a condensed project/task
state machine (the semantics of `state-machine.md`); explains finding a wolf
session in `sessions[]` and reading `<sessions-root>/<id>/stream.log`; sets
the answer style (replies go out over WhatsApp or Signal: short, plain text, no wide
tables, lead with the answer, cite `blocked_reason`/`failure_reason`); and
otherwise forbids claiming to change state — to act, it names the TUI command (`:` →
`stop`/`start`/`wolf`/`review`/`cleanup`/`archive`) or the YAML edit (project
`status`, task `status`, an `intake/*.md` file) for the human to use.

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
on only when `--acp-kit` is set **and** `channels.yaml` has an enabled
inbound-capable channel (`channels.Config.HasInboundChannel`) — until a human
can message it there is nobody to answer. `--workingman-agent` / `=on` forces it
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
