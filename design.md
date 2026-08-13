We're going to create an agent orchestrator in golang. The daemon is configured with a list of root directories that it watches recursively. If any watched directory contains an empty .project.yaml then our daemon should detect that empty .project.yaml file and start an interactive project agent to help you populate that .project.yaml file with a set of information including:

* a description of the project
* the collection of github repos that might be necessary to complete the project
* the name of the branch that should accumulate any target changes to complete the project
* status of the project (done, ready, working, blocked)
* a cron schedule for when the project should wake up
* an `updated_by` field (`agent` or `daemon`) used so the daemon can ignore fsnotify events caused by its own writes
* a `cleanup` flag — the *request* for an archive agent run, set by the TUI's `:cleanup` and cleared by the daemon when that run ends
* an `archive` flag — the archive agent's *result*, set only when the cleanup succeeded; it gates the `:archive` command and turns the project's card blue in the TUI

Neither `cleanup` nor `archive` is a status: the status enum is fixed (ready, working, blocked, done) and a project keeps whatever status it had while it is being cleaned up.

When this project agent is launched, the first task of the project agent is to work with the user to populate the .project.yaml file. We should have a daemon process that uses fsnotify to detect the new .project.yaml file, and then start an interactive agent to populate that file.

Once the agent has completed this task, it should update the .project.yaml with any updates that have been completed during the agent session and always put the project into the status ready.  

Anytime the daemon detects that a .project.yaml file has been updated, the daemon should start up a background agent that looks read the .project.yaml file. 

If the `cleanup` flag is set then it should start an archive agent, ahead of any status routing — a cleanup can be requested from any status, and the status's own agent must not run at the same time (it would be committing into the very workspace the archive agent is trying to leave clean).
If the status is ready then it should start an planning agent 
If the status is working then it should build the task graph and dispatch one task to a task agent.
If the status is blocked then it should invoke the wolf agent to problem solve.

# planning agent

A planning agent breaks a project into a set of tasks.  This agent should write out tasks as yaml files in the ./tasks/*.yaml.  Each task should include details about:

* the name of the task
* any dependencies on other tasks to be completed (by name)
* the number of times the task has been completed
* the status of the task (running, ready, success, and failed, blocked, committed)
* the reason for the task having failed
* the reason that the task is blocked
* description of the task that needs to be completed

While writing out tasks, the agent can read the .project.yaml file and any existing tasks already in the ./tasks/*.yaml directory. The agent should make any changes it likes to the .project.yaml or any of the ./tasks. It should constrain itself to only making updates to .project.yaml or task files. When complete, it should change the project status to working.

# task agent

A task agent is started in a workspace that has been setup by the deamon.  It has context from the current task and the overall project. It must work on the project in the workspace that has been provided. Once complete, the agent must update the relevant task.yaml file to signal that it is completed. It must change the task status to either success or failed to signal to the daemon that we are ready to continue.  If the task is failed, then it should write the reason for the failure.

# wolf agent

The wolf agent should look at failed or blocked tasks, and .project.yaml status to try to determine what is causing an issue. The wolf agent does not block on a tmux session for input — instead it sends a notification out to the user (via macOS `osascript` for now) when it needs human input, and the user can attach to the tmux session to respond.

# commit agent

The commit agent is given a task when a change is complete in a wsp workspace. Commits should be made for each repo in this workspace.  Use a succinct commit message that describes the change.

# archive agent

The archive agent (the "cleanup" agent) prepares a finished project to be archived. It is dispatched by the daemon when it observes `cleanup: true` on a `.project.yaml`, and it runs interactively in the project's wsp workspace. Its job is to leave every repo clean, committing and pushing only what actually needs it — a repo with nothing to commit and nothing to publish is finished as-is. For each repo in that workspace it:

* checks for uncommitted changes;
* classifies them — anything that plainly does not belong in the repo (build artifacts, editor scratch files, local caches) is neither committed nor deleted. The agent instead PROPOSES the exact `.gitignore` edit, notifies the user, and waits for approval; it applies the edit only if the human says yes, and gives up (reporting what is outstanding) if they say no or never answer;
* makes one final commit — only if there is something to commit; an already-clean tree gets no empty commit;
* pushes to the branch that is actually checked out in the workspace — read per repo with `git rev-parse --abbrev-ref HEAD`, never a hardcoded branch, never a force-push — and only if that branch is ahead of its upstream. Whether an upstream exists is settled first, with `git rev-parse --abbrev-ref --symbolic-full-name @{upstream}`, and only then is the distance counted with `git rev-list --count @{upstream}..HEAD`; the count is never used to infer that no upstream is configured, since it also fails (empty, non-zero exit) in exactly that case. So the push happens in two situations only — an upstream exists and the count is above zero, or the `rev-parse` positively reported no upstream at all. A repo already level with its upstream is reported as "already clean, nothing to push", with no no-op push and no `--allow-empty` / `--force` / `--set-upstream` workaround to manufacture one.

There are no other pre-commit steps: no tests, no lint, no formatting, no scratch-file cleanup. Those checks, plus whichever of the commit and the push they show is actually needed, are the whole job.

On success — every repo left clean, including repos that needed neither a commit nor a push — the agent sets `archive: true` on the project file and leaves `status:` alone. If it cannot finish, it leaves `archive` unset and says what a human needs to do. It never writes the `cleanup` request flag itself; the daemon owns clearing that.

# archiving a project

`:archive` in the TUI retires a project, and it refuses to run on a project that has not been cleaned up: without `archive: true` it warns "has not been cleaned up — run :cleanup first" and touches nothing. When the guard passes and the user confirms, the archive deletes the `.orch` directory at the root of the project's wsp workspace, removes the workspace itself, and then moves the project directory to the sibling backup root (`<root>/<name>` → `<root>.backup/<name>`). `.orch` — the orchestrator's own control directory, resolved from the same project `branch` the workspace removal keys off — goes first so wsp neither trips over it nor leaves it behind; a workspace with no branch, no resolvable path, or no `.orch` skips that step cleanly. Both removals precede the move so that a failure in either aborts the archive with the project still in place, rather than leaving a half-archived project whose workspace still exists. The daemon needs no notification: the project has left the watched root, so the next scan drops it.

# daemon

If the daemon detects that the .project.yaml file is updated and the status is ready that we need another phase of planning from the planning agent. If status is working then the daemon must build the task graph and if there is a task that is ready to be worked on then give it to a task agent. If all tasks are complete, then the project should transition to the done status. All project status updates should be logged to logs/audit.log

The daemon also reads the cron schedule from `.project.yaml` and, on each firing, wakes the project (e.g. by re-evaluating its status as if `.project.yaml` had been updated). The planning agent writes task dependencies into each task file; the daemon computes the ready set from that DAG.

The daemon also owns the cleanup request's lifecycle. When it observes `cleanup: true` it launches the archive agent under a session key of its own — separate from the project's main session slot — and logs `archive_dispatch`; a second request while that run is in flight is deduped (`session_skip_duplicate`), so one `:cleanup` produces exactly one archive session. When the session ends the daemon clears `cleanup` (writing as `daemon`, so the clear cannot retrigger dispatch), leaves the agent's `archive` value exactly as it found it, logs `cleanup_flag_cleared`, and resumes normal status routing. A run that ended without `archive: true` is logged as `archive_incomplete` and otherwise left alone — not finishing is not a reason to block the project, and the request is not retried; the user re-issues `:cleanup`.

The deamon also watches for updates to the task.yaml files. When a task change status, it should look if the task is failed, and if it has been restarted less than 3 times, then it should restart it.  If the restart limit has been reached and the task is still failed then the the project should be updated to blocked.  If the updated task is marked as success then trigger the commit agent for this branch. If the updated task is marked as committed then build the task graph again and assign another task agent some work.  If the task is blocked then mark the project as blocked too. Each status update should be logged to logs/audit.log with the name of the task and the transition.

# running agents

Whenever we run an agent, we must create a workspace for the agent to run in. Do this with the cli tool wsp.  Use the .project.yaml file to figure out which repos should be added to the workspace.  The name of the workspace is the target branch listed in the project. Run the agent, by starting a claude code process in the workspace dir with --dangerously-skip-permissions flag.  Start the claude code session in a tmux session with a name for the task.  This will allow us to join the session any time the agent appears to be stuck or we just want to watch it work.  When you start the agent, we may want to add task specific skills to the workspaces .claude folder.

# TUI

The TUI's work-stream (project) commands live behind `:` on the work streams pane. Two of them drive the retirement flow: `:cleanup` writes the `cleanup: true` request — as `updated_by: agent`, because the daemon drops events carrying its own `daemon` marker and would never see a request it wrote itself — and `:archive` runs the guarded archive described above behind a yes/no confirmation. `:cleanup` refuses a project that already has `archive: true`: it is cleaned up, and the next step is `:archive`.

The gallery renders a project with `archive: true` inside a blue border, so a work stream that is cleaned up and waiting to be archived is visible at a glance. Selection takes precedence over the blue: selection is transient state that has to stay visible wherever the cursor lands, while the archive flag is durable and becomes legible again as soon as the cursor moves on.


