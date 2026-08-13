# orch — autonomous claude-code project daemon

`orch` watches one or more directories for `.project.yaml` files and runs
claude-code agents (project, planning, task, commit, wolf, archive) through a
state machine until the project is done — or blocked, in which case you get a
macOS notification and a wolf-agent session to drop into.

```
status: ready    → planning agent (writes tasks/*.yaml, sets status: working)
status: working  → task agent → commit agent → next task → ... → status: done
status: blocked  → wolf agent + osascript notification
status: done     → terminal

cleanup: true    → archive agent (checked ahead of the status routing;
                   commits + pushes whatever needs it, then sets archive: true)
```

The daemon also reads each project's `cron:` field; firings re-evaluate the
project as if its `.project.yaml` had been edited. Every schedule needs an
explicit stop condition (`cron_until:` or `cron_max_runs:`) — see
[Example `.project.yaml`](#example-projectyaml).

## Prerequisites

- Go 1.22+
- `tmux` — every agent runs in a detached tmux session you can attach to
- `claude` (claude-code CLI) — the actual agent worker
- `wsp` — multi-repo workspace manager. Repos must be registered (`wsp registry add ...`) before the planning step finishes. If you don't have wsp set up, run with `--workspace-manager=stub` instead.
- macOS for `osascript` notifications (optional; wolf still launches without them)

## Build

```sh
go build -o orch ./cmd/orch
```

## Start the daemon

Pick a directory tree where you'll keep your project files. `orch` watches
this tree recursively; any subdirectory with a `.project.yaml` becomes a
project.

```sh
mkdir -p ~/orch
./orch --root ~/orch --audit-log ~/orch/audit.log
```

Repeat `--root` for multiple trees. Watch the audit log in another shell:

```sh
tail -f ~/orch/audit.log
```

## Walk through a project

### 1. Create an empty project file

```sh
mkdir -p ~/orch/my-feature
touch ~/orch/my-feature/.project.yaml
```

The daemon logs `project_empty` and launches the **project agent** in a
tmux session. Attach to it:

```sh
tmux list-sessions          # find the session name (orch-project-<hash>)
tmux attach -t orch-project-<hash>
```

The agent reads `.orch/instructions.md` and interviews you about the
project. When it's done it writes the `.project.yaml` itself with
`status: ready`. Detach (`Ctrl-b d`) any time — the session keeps running.

### 2. Planning

The moment the project file is saved with `status: ready`, the daemon
launches the **planning agent** in the same directory. It writes
`tasks/*.yaml` (one file per task, with `depends_on` edges), then flips
`.project.yaml` to `status: working` and exits.

### 3. Tasks

`status: working` triggers the daemon to:

1. Build the task DAG from `tasks/*.yaml`.
2. Pick the first ready task (no uncommitted deps).
3. Provision a wsp workspace at `~/dev/workspaces/<branch>/` with the repos
   from `.project.yaml`.
4. Launch a **task agent** in that workspace.
5. When the task agent exits with `status: success`, launch a **commit agent**
   in the same workspace.
6. When the commit agent exits with `status: committed`, pick the next ready
   task. Repeat until everything is committed, then mark the project
   `status: done`.

Task agents retry up to 3 times on `status: failed`. After the 3rd failure
the project goes `status: blocked`, you get an osascript notification, and
a wolf agent launches.

### 4. Stop

`Ctrl-C` the daemon. In-flight sessions are closed; tmux sessions go away.

## Stub mode (no wsp / no repos)

If you want to exercise the orchestration without wsp registered or repos
to clone:

```sh
./orch --root ~/orch --workspace-manager=stub
```

Task and commit agents run in `$TMPDIR/orch-workspaces/<branch>/` — empty
dirs the daemon creates on demand. The project and planning agents are
unaffected (they don't need a wsp workspace).

## Files the daemon writes

Inside each agent's working directory:

```
.orch/
  context.yaml       # paths + branch + task name (whatever applies to this Kind)
  instructions.md    # rendered prompt the agent reads at startup
```

In the project root:

```
tasks/*.yaml         # written by the planning agent; observed in audit
```

Daemon-owned writes carry `updated_by: daemon` in `.project.yaml` so the
daemon ignores its own fsnotify events.

## Observability

- **Audit log** (`--audit-log`): one line per event. `tail -f` it.
- **Tmux sessions**: `tmux list-sessions` shows every live agent. Attach
  with `tmux attach -t <name>` to watch claude work or drive an interactive
  agent (project / wolf).

## Example `.project.yaml`

```yaml
description: |
  Add a /healthz endpoint to the gateway and a matching probe in the
  deploy manifests.

repos:
  - org: docker
    name: gateway
  - org: docker
    name: deploy-manifests

branch: feat/healthz-probe
status: ready
cron: "*/15 * * * *"   # optional; daemon re-evaluates every 15 minutes
cron_max_runs: 96      # required with cron (or cron_until): stop after 96 firings
cron_runs: 12          # daemon-owned firing counter
cleanup: true          # optional; "please run the archive agent" (set by :cleanup)
archive: true          # optional; "the cleanup finished" (set by the archive agent)
updated_by: agent
```

A `cron:` schedule must come with a stop condition — either
`cron_until: <RFC3339 timestamp>` (an absolute deadline) or
`cron_max_runs: <int>` (a firing limit). They are two expressions of the same
idea, either one is enough, and if both are set whichever trips first wins. The
daemon counts firings in `cron_runs`, unregisters the schedule as soon as the
condition is met (logging `cron_unscheduled`), and reaching `status: done`
unschedules it too. A project with `cron:` and *no* stop condition is not
scheduled at all: the daemon blocks it and summons the wolf agent, since a
schedule that can never end would wake the project up forever.

See `examples/.project.yaml` and `examples/tasks/*.yaml` for the full
schemas.

## Cleanup and archiving

A finished project is retired in two steps — a **cleanup** that leaves every
repo clean, committing and pushing only what needs it, then an **archive**
that moves the project out of
the watched root. Both are driven from the TUI's `:` command menu on the work
streams pane, and both are recorded on `.project.yaml` by two independent
boolean flags (neither one is a `status:`, so a project keeps whatever status
it already had):

| Field      | Meaning                                             | Written by |
|------------|-----------------------------------------------------|------------|
| `cleanup:` | *request*: "run the archive agent on this project"  | the TUI's `:cleanup` (as `updated_by: agent`, so the daemon sees the event); cleared by the daemon when the agent's session ends |
| `archive:` | *result*: the cleanup succeeded, safe to archive     | the archive agent, on success only |

### `:cleanup` — the archive agent

`:cleanup` sets `cleanup: true` and returns immediately; the daemon notices the
flag ahead of its normal status routing and launches the **archive agent** in
the project's wsp workspace. It's an interactive agent, so it may need you to
attach. In every repo of the workspace it:

1. Checks for uncommitted work (`git status --porcelain`).
2. Classifies what's there. Anything that plainly doesn't belong in the repo
   (build artifacts, editor scratch, local caches) is **not** committed and
   **not** deleted — the agent *proposes* a `.gitignore` edit, notifies you, and
   waits for approval before applying it. If approval never comes, it stops and
   reports instead of committing.
3. Makes one final commit — only if there is something to commit.
4. Pushes to the branch **actually checked out in that workspace**
   (`git rev-parse --abbrev-ref HEAD`) — not a hardcoded branch name, and never
   a force-push — and only if there is something to push. It first asks whether
   an upstream exists at all (`git rev-parse --abbrev-ref
   --symbolic-full-name @{upstream}`), then counts the distance
   (`git rev-list --count @{upstream}..HEAD`). It pushes when that count is
   above zero, or when there genuinely is no upstream configured. A repo
   already level with its upstream is reported as *"already clean, nothing to
   push"* — no no-op push, and no empty commit or force/`--set-upstream`
   workaround to invent one.

There are no other pre-commit steps: no tests, no lint, no formatting, no
scratch-file cleanup. A repo that needed neither a commit nor a push is a
success. On success the agent sets `archive: true` on
`.project.yaml` and leaves `status:` untouched. If it can't finish, it leaves
`archive` unset and says what's outstanding.

The daemon clears `cleanup:` when the session ends either way (an unfinished run
is logged as `archive_incomplete`), so a cleanup is never retried behind your
back — re-run `:cleanup` yourself. Exactly one archive agent runs per request,
and a second `:cleanup` while one is in flight is a no-op. A project that is
already cleaned up (`archive: true`) is refused outright — its next step is
`:archive`, not another cleanup.

### `:archive`

`:archive` only archives a project whose `.project.yaml` carries
`archive: true`. Anything else is refused on the status line with "has not been
cleaned up — run :cleanup first", and nothing moves. When the guard passes and
you confirm, the archive:

1. removes the project's **wsp workspace** (`wsp rm <branch>`), then
2. moves the project directory to the sibling backup root, keeping its name
   (`~/orch/my-feature` → `~/orch.backup/my-feature`).

The workspace goes first on purpose: if `wsp rm` fails, the archive aborts with
the project still in place and the failure on the status line. The daemon drops
the project on its next scan — the move is the only cleanup needed.

In the TUI's work-stream gallery, a project with `archive: true` is drawn with a
**blue border** — it's cleaned up and waiting for `:archive`. Selection still
wins over the blue, so the cursor never disappears onto a blue card. See
[Gallery border colours](#gallery-border-colours) for the full precedence.

Doing it by hand instead:

```sh
rm ~/orch/my-feature/.project.yaml
rm -rf ~/orch/my-feature/{tasks,.orch}
wsp rm feat/healthz-probe
```

## Gallery border colours

Each card in the TUI's work-streams gallery carries one border colour, so a scan
of the pane tells you what state its project is in:

| Border | Meaning |
|--------|---------|
| **pink** | the selected card — where the cursor is |
| **blue** | `archive: true` — cleaned up, waiting for `:archive` |
| **green** | a live `cron:` schedule — the stop condition hasn't tripped, so the project still wakes itself up |
| grey | nothing special |

Only one colour shows at a time, and they win in that order: **selected > blue
archive > green cron > grey**. Selection is on top so the cursor never
disappears onto a coloured card. Blue beats green because the blue border is
what tells you `:archive` will be accepted, and a cleaned-up project is
effectively finished even if a schedule is still registered against it — so a
project that is both loses its green border until it's archived.

The green border tracks the same stop condition the daemon unschedules on
(`cron_until` / `cron_max_runs` vs. `cron_runs`, see
[Example `.project.yaml`](#example-projectyaml)), so it clears itself on the
gallery's next refresh once a schedule expires — no file edit needed.
