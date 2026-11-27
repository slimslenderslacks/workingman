package control

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/task"
	"github.com/slimslenderslacks/work/internal/taskgraph"
)

// The task actions (docs/proposals/workingman-gui.md §A.4.4). Each is a
// read-modify-write of one tasks/<name>.yaml through task.UpdateIf, so the
// whole load–mutate–save holds the in-process write lock for the path and a
// check (the API's If-Match) runs on the bytes it then mutates. A task file
// has no updated_by: the daemon observes task files for audit only and reacts
// to a task change on its next project dispatch, which is why [RetryTask]
// also writes project.yaml.

// ErrGraph is an edit that would leave the project's task graph unloadable
// (an unknown dependency, a cycle) or a hint the scheduler cannot read.
var ErrGraph = errors.New("invalid task graph")

// Dispatch outcomes of [RetryTask]: what its project.yaml write was, which
// says when the daemon picks the task up.
const (
	// DispatchNudged: the project is working; project.yaml was re-saved as
	// agent with no field changed, and the daemon schedules the task.
	DispatchNudged = "nudged"
	// DispatchUnblocked: the project was blocked by this task alone; it is
	// set back to working with blocked_reason cleared.
	DispatchUnblocked = "unblocked"
	// DispatchDeferred: no project write; the task waits for the project's
	// next dispatch.
	DispatchDeferred = "deferred"
)

// Retry adjusts [RetryTask].
type Retry struct {
	// ResetAttempts sets attempts back to 0, a fresh retry budget.
	ResetAttempts bool
	// Check runs on the task file's bytes under its write lock, before they
	// are read; an error from it is returned as is and nothing is written.
	Check func(data []byte) error
}

// RetryResult is what [RetryTask] did.
type RetryResult struct {
	// Dispatch is one of DispatchNudged, DispatchUnblocked, DispatchDeferred.
	Dispatch string
	// Reason says why the dispatch is deferred, when it is.
	Reason string
}

// Retryable reports whether a task in this state may be retried: failed or
// blocked, or ready with an integration report (a conflict re-run that is
// stuck). It returns the refusal text otherwise.
func Retryable(t *task.Task) error {
	switch {
	case t.Name == "":
		return refuse(ErrState, "this is a seed the planner has not named yet; there is nothing to retry")
	case t.Status == task.StatusFailed, t.Status == task.StatusBlocked:
		return nil
	case t.Status == task.StatusReady && t.Integration != nil:
		return nil
	}
	return refuse(ErrState, "task %s is %s: only a failed or blocked task (or a ready one stuck after a conflict) is retried", t.Name, t.Status)
}

// RetryUnblocks reports whether a project blocked with reason may be
// unblocked by retrying the task called name: the reason names that task
// (`task "<name>" …`, as the daemon writes it) and covers nothing else — no
// `; also: …` suffix, which the daemon appends for a second failure. The
// daemon's "no ready tasks remain …: <name> (failed)" with that one task
// counts too. A reason that covers more is the wolf's to untangle.
func RetryUnblocks(reason, name string) bool {
	reason = strings.TrimSpace(reason)
	if reason == "" || strings.Contains(reason, "; also: ") {
		return false
	}
	if strings.Contains(reason, "task "+strconv.Quote(name)) {
		return true
	}
	if rest, ok := strings.CutPrefix(reason, StuckReasonPrefix+": "); ok {
		return rest == name+" (failed)" || rest == name+" (blocked)"
	}
	return false
}

// StuckReasonPrefix starts the blocked_reason the daemon writes when no
// task can run and the project is not fully committed.
const StuckReasonPrefix = "no ready tasks remain but project is not fully committed"

// RetryTask is option R1 of §A.4.4, the wolf's "re-run it" path: it sets the
// task at taskPath back to ready, clears its failure_reason and
// blocked_reason, and (with ResetAttempts) its attempts; then it writes
// projectPath so the daemon notices, because a task edit alone waits for the
// next project dispatch:
//
//   - working: a re-save as agent, with no field changed (the nudge);
//   - blocked by this task alone ([RetryUnblocks]): status working and
//     blocked_reason cleared, as agent;
//   - anything else: no project write (DispatchDeferred).
//
// It is refused, with nothing written, on a stopped project, on a project
// blocked for more than this task (summon the wolf instead), and on a task
// [Retryable] refuses. The project's state is checked again under its lock
// when it is written: one that changed in between is left as is and the
// dispatch is deferred. The worktree is reused as is.
func RetryTask(projectPath, taskPath string, r Retry) (RetryResult, error) {
	p, err := project.Load(projectPath)
	if err != nil {
		return RetryResult{}, err
	}
	name := DisplayName(projectPath)
	var refusal error
	var taskName string
	err = task.UpdateIf(taskPath, r.Check, func(t *task.Task) bool {
		if refusal = Retryable(t); refusal != nil {
			return false
		}
		taskName = t.Name
		switch {
		case p.Status == project.StatusStopped:
			refusal = refuse(ErrState, "%s is stopped: start it before retrying a task", name)
		case p.Status == project.StatusBlocked && !RetryUnblocks(p.BlockedReason, t.Name):
			refusal = refuse(ErrState, "%s is blocked for more than task %s (%q): summon the wolf to untangle it", name, t.Name, p.BlockedReason)
		}
		if refusal != nil {
			return false
		}
		t.Status = task.StatusReady
		t.FailureReason = ""
		t.BlockedReason = ""
		if r.ResetAttempts {
			t.Attempts = 0
		}
		return true
	})
	if err != nil {
		return RetryResult{}, err
	}
	if refusal != nil {
		return RetryResult{}, refusal
	}
	res := RetryResult{Dispatch: DispatchDeferred}
	err = project.Update(projectPath, project.WriterAgent, func(cur *project.Project) bool {
		switch {
		case cur.Status == project.StatusWorking:
			res.Dispatch = DispatchNudged
			return true
		case cur.Status == project.StatusBlocked && RetryUnblocks(cur.BlockedReason, taskName):
			cur.Status = project.StatusWorking
			cur.BlockedReason = ""
			res.Dispatch = DispatchUnblocked
			return true
		case cur.Status == project.StatusBlocked:
			res.Reason = fmt.Sprintf("%s became blocked for more than this task; the wolf handles it", name)
		default:
			res.Reason = fmt.Sprintf("%s is %s; the task waits for its next dispatch", name, statusText(cur.Status))
		}
		return false
	})
	if err != nil {
		return RetryResult{}, fmt.Errorf("task %s is ready again, but the project write failed: %w", taskName, err)
	}
	return res, nil
}

func statusText(s project.Status) string {
	if s == "" {
		return "not filled in yet"
	}
	return string(s)
}

// SaveSandbox sets the task's save_sandbox flag: true keeps its sandbox when
// its agent exits, for inspection. check, when not nil, runs on the file's
// bytes under the lock. noop reports a flag that already had that value, in
// which case nothing is written.
func SaveSandbox(taskPath string, save bool, check func(data []byte) error) (noop bool, err error) {
	var refusal error
	err = task.UpdateIf(taskPath, check, func(t *task.Task) bool {
		if t.Name == "" {
			refusal = refuse(ErrState, "this is a seed the planner has not named yet; it has no sandbox")
			return false
		}
		if t.SaveSandbox == save {
			noop = true
			return false
		}
		t.SaveSandbox = save
		return true
	})
	if err == nil {
		err = refusal
	}
	return noop, err
}

// TaskEdit is a merge-patch of a task's inputs, the only fields a front end
// edits (§A.4.4): a nil field is left as is.
type TaskEdit struct {
	Description *string
	DependsOn   *[]string
	// SetTouches says Touches is in the patch; a nil Touches then removes
	// the hint (the task becomes exclusive unless it has areas).
	SetTouches bool
	Touches    *[]string
	Areas      *[]string
}

// Empty reports whether the edit changes nothing.
func (e TaskEdit) Empty() bool {
	return e.Description == nil && e.DependsOn == nil && !e.SetTouches && e.Areas == nil
}

// EditableStatus reports whether a task in status s may have its inputs
// edited: not while it runs, nor once its work is done.
func EditableStatus(s task.Status) bool {
	return s == task.StatusReady || s == task.StatusFailed || s == task.StatusBlocked
}

// EditTask applies e to the task at taskPath, through task.UpdateIf with
// check. It is refused (ErrState) unless the task is ready, failed or
// blocked, and (ErrGraph) when the result would not load: the task graph
// with the edit in place is loaded with taskgraph.Load, and an unknown
// dependency, a cycle or a bad touches glob or areas tag on this task is a
// refusal, not a file that blocks the project with a taskgraph error.
// noop reports an edit that changes nothing, which writes nothing.
func EditTask(taskPath string, e TaskEdit, check func(data []byte) error) (noop bool, err error) {
	var refusal error
	err = task.UpdateIf(taskPath, check, func(t *task.Task) bool {
		if t.Name == "" {
			refusal = refuse(ErrState, "this is a seed the planner has not named yet; queue intake or edit the file instead")
			return false
		}
		if !EditableStatus(t.Status) {
			refusal = refuse(ErrState, "task %s is %s: its inputs can be edited only while it is ready, failed or blocked", t.Name, t.Status)
			return false
		}
		before := *t
		if e.Description != nil {
			if strings.TrimSpace(*e.Description) == "" {
				refusal = refuse(ErrInvalid, "a task's description must not be blank")
				return false
			}
			t.Description = *e.Description
		}
		if e.DependsOn != nil {
			t.DependsOn = append([]string{}, (*e.DependsOn)...)
		}
		if e.SetTouches {
			t.Touches = nil
			if e.Touches != nil {
				ts := append([]string{}, (*e.Touches)...)
				t.Touches = &ts
			}
		}
		if e.Areas != nil {
			t.Areas = nil
			if len(*e.Areas) > 0 {
				t.Areas = append([]string{}, (*e.Areas)...)
			}
		}
		if sameInputs(&before, t) {
			noop = true
			return false
		}
		if refusal = checkGraph(taskPath, t); refusal != nil {
			return false
		}
		return true
	})
	if err == nil {
		err = refusal
	}
	return noop, err
}

func sameInputs(a, b *task.Task) bool {
	eq := func(x, y []string) bool {
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i] != y[i] {
				return false
			}
		}
		return true
	}
	if (a.Touches == nil) != (b.Touches == nil) || (a.Touches != nil && !eq(*a.Touches, *b.Touches)) {
		return false
	}
	return a.Description == b.Description && eq(a.DependsOn, b.DependsOn) && eq(a.Areas, b.Areas)
}

// checkGraph loads the task graph of taskPath's tasks/ directory with t in
// place of the file at taskPath, as the scheduler would: a copy of every
// task file goes into a temp <work-stream>/tasks directory, so the name
// budget is the project's, and taskgraph.Load runs on it.
func checkGraph(taskPath string, t *task.Task) error {
	dir := filepath.Dir(taskPath)
	tmp, err := os.MkdirTemp("", "wm-taskgraph-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	copyDir := filepath.Join(tmp, filepath.Base(filepath.Dir(dir)), filepath.Base(dir))
	if err := os.MkdirAll(copyDir, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, en := range entries {
		if en.IsDir() || !strings.HasSuffix(en.Name(), ".yaml") || en.Name() == filepath.Base(taskPath) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, en.Name()))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return err
		}
		if err := os.WriteFile(filepath.Join(copyDir, en.Name()), data, 0o644); err != nil {
			return err
		}
	}
	if err := task.Save(filepath.Join(copyDir, filepath.Base(taskPath)), t); err != nil {
		return err
	}
	g, err := taskgraph.Load(copyDir)
	if err != nil {
		return refuse(ErrGraph, "the edit would break the task graph: %s", strings.ReplaceAll(err.Error(), copyDir, dir))
	}
	hint := "task " + strconv.Quote(t.Name) + " has an invalid hint"
	for _, w := range g.Warnings() {
		if strings.Contains(w, hint) {
			return refuse(ErrGraph, "%s", strings.TrimPrefix(w, "taskgraph: "))
		}
	}
	return nil
}
