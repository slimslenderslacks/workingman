# orch — autonomous claude-code project daemon

`orch` watches one or more directories for `.project.yaml` files and runs
claude-code agents (project, planning, task, commit, wolf) through a state
machine until the project is done — or blocked, in which case you get a
macOS notification and a wolf-agent session to drop into.

```
status: ready    → planning agent (writes tasks/*.yaml, sets status: working)
status: working  → task agent → commit agent → next task → ... → status: done
status: blocked  → wolf agent + osascript notification
status: done     → terminal
```

The daemon also reads each project's `cron:` field; firings re-evaluate the
project as if its `.project.yaml` had been edited.

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
updated_by: agent
```

See `examples/.project.yaml` and `examples/tasks/*.yaml` for the full
schemas.

## Cleanup

```sh
rm ~/orch/my-feature/.project.yaml
rm -rf ~/orch/my-feature/{tasks,.orch}
wsp rm feat/healthz-probe
```
