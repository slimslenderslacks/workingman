We're going to create an agent orchestrator in golang. The daemon is configured with a list of root directories that it watches recursively. If any watched directory contains an empty .project.yaml then our daemon should detect that empty .project.yaml file and start an interactive project agent to help you populate that .project.yaml file with a set of information including:

* a description of the project
* the collection of github repos that might be necessary to complete the project
* the name of the branch that should accumulate any target changes to complete the project
* status of the project (done, ready, working, blocked)
* a cron schedule for when the project should wake up
* an `updated_by` field (`agent` or `daemon`) used so the daemon can ignore fsnotify events caused by its own writes

When this project agent is launched, the first task of the project agent is to work with the user to populate the .project.yaml file. We should have a daemon process that uses fsnotify to detect the new .project.yaml file, and then start an interactive agent to populate that file.

Once the agent has completed this task, it should update the .project.yaml with any updates that have been completed during the agent session and always put the project into the status ready.  

Anytime the daemon detects that a .project.yaml file has been updated, the daemon should start up a background agent that looks read the .project.yaml file. 

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

# daemon

If the daemon detects that the .project.yaml file is updated and the status is ready that we need another phase of planning from the planning agent. If status is working then the daemon must build the task graph and if there is a task that is ready to be worked on then give it to a task agent. If all tasks are complete, then the project should transition to the done status. All project status updates should be logged to logs/audit.log

The daemon also reads the cron schedule from `.project.yaml` and, on each firing, wakes the project (e.g. by re-evaluating its status as if `.project.yaml` had been updated). The planning agent writes task dependencies into each task file; the daemon computes the ready set from that DAG.

The deamon also watches for updates to the task.yaml files. When a task change status, it should look if the task is failed, and if it has been restarted less than 3 times, then it should restart it.  If the restart limit has been reached and the task is still failed then the the project should be updated to blocked.  If the updated task is marked as success then trigger the commit agent for this branch. If the updated task is marked as committed then build the task graph again and assign another task agent some work.  If the task is blocked then mark the project as blocked too. Each status update should be logged to logs/audit.log with the name of the task and the transition.

# running agents

Whenever we run an agent, we must create a workspace for the agent to run in. Do this with the cli tool wsp.  Use the .project.yaml file to figure out which repos should be added to the workspace.  The name of the workspace is the target branch listed in the project. Run the agent, by starting a claude code process in the workspace dir with --dangerously-skip-permissions flag.  Start the claude code session in a tmux session with a name for the task.  This will allow us to join the session any time the agent appears to be stuck or we just want to watch it work.  When you start the agent, we may want to add task specific skills to the workspaces .claude folder.


