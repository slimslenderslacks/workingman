package taskgraph

import (
	"sort"

	"github.com/slimslenderslacks/work/internal/task"
)

// Queued returns the project's commit queue (docs/parallel-task-worktrees.md
// S5) in integration order, head first: every task at `success` that is not a
// push task, plus every push task at `ready` whose dependencies are all
// committed (a push task has no task-agent phase and joins the queue
// directly). The queue is ordered by `succeeded_at`, earliest first; a task
// with no `succeeded_at` (a legacy file, or one a human set to success by
// hand) sorts before every stamped one, and name breaks ties.
//
// Membership is derived from the task files alone, so the queue is the same
// whether it is read by the daemon, a snapshot built offline or the TUI. It
// takes a plain slice rather than a Graph so lenient readers that load task
// files one by one can use it too.
func Queued(tasks []*task.Task) []*task.Task {
	status := make(map[string]task.Status, len(tasks))
	for _, t := range tasks {
		status[t.Name] = t.Status
	}
	var out []*task.Task
	for _, t := range tasks {
		switch {
		case t.Status == task.StatusSuccess && !t.IsPushTask():
		case t.Status == task.StatusReady && t.IsPushTask() && depsAt(t, status, task.StatusCommitted):
		default:
			continue
		}
		out = append(out, t)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].SucceededAt, out[j].SucceededAt
		switch {
		case a == nil && b != nil:
			return true
		case a != nil && b == nil:
			return false
		case a != nil && b != nil && !a.Equal(*b):
			return a.Before(*b)
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// WaitingOnIntegration returns the dependencies of t that are at `success`:
// finished, but not yet integrated, so t is not Ready until they land (S5).
// Order follows t.DependsOn. A dependency missing from tasks is ignored.
func WaitingOnIntegration(t *task.Task, tasks []*task.Task) []string {
	status := make(map[string]task.Status, len(tasks))
	for _, o := range tasks {
		status[o.Name] = o.Status
	}
	var out []string
	for _, dep := range t.DependsOn {
		if status[dep] == task.StatusSuccess {
			out = append(out, dep)
		}
	}
	return out
}

// Queued is the package-level Queued over the graph's tasks.
func (g *Graph) Queued() []*task.Task { return Queued(g.Tasks()) }

func depsAt(t *task.Task, status map[string]task.Status, want task.Status) bool {
	for _, dep := range t.DependsOn {
		if s, ok := status[dep]; !ok || s != want {
			return false
		}
	}
	return true
}
