package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/slimslenderslacks/work/internal/api"
	"github.com/slimslenderslacks/work/internal/control"
	"github.com/slimslenderslacks/work/internal/terminal"
)

// The task actions of docs/proposals/workingman-gui.md §A.4.4 and the host
// tabs of §A.4.6. Retry, save-sandbox and the task edit are writes of
// tasks/<name>.yaml through internal/control, checked against the task's
// etag under the file's lock; retry also writes project.yaml so the daemon
// picks the task up. The host tabs call the daemon's own Open* functions,
// the ones behind the TUI's :session, :orch and :workspace.

// TaskActionResult is the response of a task write: the task as its file
// now is, what was written, and the events to watch for.
type TaskActionResult struct {
	Task TaskResource `json:"task"`
	// Dispatch is retry's: "nudged" (project.yaml re-saved, the daemon
	// schedules the task), "unblocked" (the project was blocked by this
	// task alone and is working again) or "deferred" (no project write;
	// the task waits for the next dispatch, see DispatchReason).
	Dispatch       string    `json:"dispatch,omitempty"`
	DispatchReason string    `json:"dispatch_reason,omitempty"`
	Noop           bool      `json:"noop,omitempty"`
	Write          WriteInfo `json:"write"`
	// ProjectWrite is the project.yaml write retry made, if any.
	ProjectWrite *WriteInfo `json:"project_write,omitempty"`
	Expect       []string   `json:"expect"`
}

// HostTabResult is the response of open-session and open-shell: the tab the
// daemon opened in its terminal, or focused when it was already open.
type HostTabResult struct {
	// Kind is the TUI's name for it: session, task session, orch,
	// workspace or worktree.
	Kind   string `json:"kind"`
	Target string `json:"target"`
	// AlreadyOpen reports a tab that was open already and was focused (or,
	// when FocusError is set, could not be).
	AlreadyOpen bool   `json:"already_open,omitempty"`
	FocusError  string `json:"focus_error,omitempty"`
	Note        string `json:"note"`
}

// taskRef finds a task of a project for a write.
func (s apiSource) taskRef(id, name string) (*apiModel, *ProjectSnapshot, TaskSnapshot, error) {
	m := s.d.model()
	p, err := m.project(id)
	if err != nil {
		return nil, nil, TaskSnapshot{}, err
	}
	for _, ts := range p.Tasks {
		if ts.Name == name && ts.path != "" {
			return m, p, ts, nil
		}
	}
	return nil, nil, TaskSnapshot{}, fmt.Errorf("%w: project %s has no task %q", api.ErrNotFound, id, name)
}

// taskNow is the task resource as its file is now, for a write's response
// or a refusal's "current"; zero when the task has gone.
func (d *Daemon) taskNow(id, name string) TaskResource {
	m := d.model()
	p, err := m.project(id)
	if err != nil {
		return TaskResource{}
	}
	for _, ts := range p.Tasks {
		if ts.Name == name {
			r := m.task(p, ts)
			redactStrings(&r)
			return r
		}
	}
	return TaskResource{}
}

// taskCheck is If-Match against the task file's bytes, run under its lock.
// before records the etag the check saw.
func taskCheck(w api.WriteRequest, name string, before *string) func([]byte) error {
	return func(data []byte) error {
		*before = resourceETag("tk", contentTag(data))
		if !api.MatchETag(w.IfMatch, *before) {
			return fmt.Errorf("%w: task %s changed since you loaded it (now %s)", api.ErrPreconditionFailed, name, *before)
		}
		return nil
	}
}

func (s apiSource) TaskAction(ctx context.Context, w api.WriteRequest, id, name, action string, req api.TaskActionRequest) (api.WriteResult, error) {
	_, p, ts, err := s.taskRef(id, name)
	if err != nil {
		return api.WriteResult{}, err
	}
	switch action {
	case api.ActionOpenSession, api.ActionOpenShell:
		return s.taskTab(ctx, p.Path, ts, action)
	}
	var before string
	check := taskCheck(w, name, &before)
	res := TaskActionResult{Write: WriteInfo{Path: ts.path}}
	switch action {
	case api.ActionRetry:
		// The commit agent integrating this task, or a live session on it,
		// owns its file now (§A.4.4).
		if s.d.integratingTask(p.Path) == name || s.d.landingTask(p.Path) == name {
			return api.WriteResult{}, api.WithCurrent(fmt.Errorf("%w: the commit agent is integrating task %s; wait for it to finish", api.ErrInvalidState, name), s.d.taskNow(id, name))
		}
		if s.d.hasSession(taskSessionKey(p.Path, name)) {
			return api.WriteResult{}, api.WithCurrent(fmt.Errorf("%w: task %s has a live session; it is not done yet", api.ErrInvalidState, name), s.d.taskNow(id, name))
		}
		var r control.RetryResult
		r, err = control.RetryTask(p.Path, ts.path, control.Retry{ResetAttempts: req.ResetAttempts, Check: check})
		if err == nil {
			res.Dispatch, res.DispatchReason = r.Dispatch, r.Reason
			res.Write.Fields = map[string]any{"status": "ready", "failure_reason": "", "blocked_reason": ""}
			if req.ResetAttempts {
				res.Write.Fields["attempts"] = 0
			}
			switch r.Dispatch {
			case control.DispatchNudged:
				res.ProjectWrite = &WriteInfo{Path: p.Path, Fields: map[string]any{"updated_by": "agent"}}
				res.Expect = []string{"task.status", "session.started"}
			case control.DispatchUnblocked:
				res.ProjectWrite = &WriteInfo{Path: p.Path, Fields: map[string]any{"status": "working", "blocked_reason": "", "updated_by": "agent"}}
				res.Expect = []string{"task.status", "project.status", "session.started"}
			default:
				res.Expect = []string{"task.status"}
			}
			kv := []string{"path", p.Path, "task", name, "dispatch", r.Dispatch, "reset_attempts", fmt.Sprint(req.ResetAttempts), "client", w.Client}
			if note := strings.TrimSpace(req.Note); note != "" {
				kv = append(kv, "note", note)
			}
			s.d.audit.Log("api_task_retry", kv...)
		}
	case api.ActionSaveSandbox:
		var noop bool
		noop, err = control.SaveSandbox(ts.path, *req.Save, check)
		res.Noop = noop
		res.Write.Fields = map[string]any{"save_sandbox": *req.Save}
		res.Expect = []string{"task.updated"}
	default:
		return api.WriteResult{}, fmt.Errorf("%w: no task action %q", api.ErrNotFound, action)
	}
	if err != nil {
		return api.WriteResult{}, s.taskErr(id, name, err)
	}
	return s.taskWritten(id, name, before, res)
}

func (s apiSource) EditTask(_ context.Context, w api.WriteRequest, id, name string, patch api.TaskPatch) (api.WriteResult, error) {
	_, _, ts, err := s.taskRef(id, name)
	if err != nil {
		return api.WriteResult{}, err
	}
	var before string
	e := control.TaskEdit{Description: patch.Description, DependsOn: patch.DependsOn, SetTouches: patch.TouchesSet, Touches: patch.Touches, Areas: patch.Areas}
	noop, err := control.EditTask(ts.path, e, taskCheck(w, name, &before))
	if err != nil {
		return api.WriteResult{}, s.taskErr(id, name, err)
	}
	fields := map[string]any{}
	if patch.Description != nil {
		fields["description"] = *patch.Description
	}
	if patch.DependsOn != nil {
		fields["depends_on"] = *patch.DependsOn
	}
	if patch.TouchesSet {
		fields["touches"] = patch.Touches
	}
	if patch.Areas != nil {
		fields["areas"] = *patch.Areas
	}
	res := TaskActionResult{Noop: noop, Write: WriteInfo{Path: ts.path, Fields: fields}, Expect: []string{"task.updated"}}
	return s.taskWritten(id, name, before, res)
}

// taskErr maps a task write's error to the API's, with the task as it is
// now on a 412 or a 409.
func (s apiSource) taskErr(id, name string, err error) error {
	if errors.Is(err, control.ErrGraph) {
		err = fmt.Errorf("%w: %s", api.ErrUnprocessable, err.Error())
	} else {
		err = controlErr(err)
	}
	if errors.Is(err, api.ErrPreconditionFailed) || errors.Is(err, api.ErrInvalidState) || errors.Is(err, api.ErrUnprocessable) {
		err = api.WithCurrent(err, s.d.taskNow(id, name))
	}
	return err
}

func (s apiSource) taskWritten(id, name, before string, res TaskActionResult) (api.WriteResult, error) {
	res.Task = s.d.taskNow(id, name)
	if res.Noop {
		res.Write.Fields = nil
	}
	redactStrings(&res)
	return api.WriteResult{Noop: res.Noop, Body: res, ETag: res.Task.ETag, Path: res.Write.Path, ETagBefore: before, ETagAfter: res.Task.ETag}, nil
}

// ---------------------------------------------------------------------------
// Host tabs (§A.4.6)
// ---------------------------------------------------------------------------

// projectTab opens a project's :session, :orch or :workspace tab.
func (s apiSource) projectTab(ctx context.Context, path, action, where string) (api.WriteResult, error) {
	switch {
	case action == api.ActionOpenSession:
		target, err := s.d.OpenInteractiveSession(ctx, path)
		return tabResult("session", target, err)
	case where == api.ShellOrch:
		target, err := s.d.OpenOrchDirTab(ctx, path)
		return tabResult("orch", target, err)
	default:
		target, err := s.d.OpenWorkspaceDirTab(ctx, path)
		return tabResult("workspace", target, err)
	}
}

// taskTab opens a task's :session or :workspace tab, in its own worktree.
// A task without one has nothing to open: the project's tabs are the shared
// tree.
func (s apiSource) taskTab(ctx context.Context, projectPath string, ts TaskSnapshot, action string) (api.WriteResult, error) {
	if ts.Worktree == "" {
		return api.WriteResult{}, fmt.Errorf("%w: task %s has no worktree; open the project's session or workspace instead", api.ErrInvalidState, ts.Name)
	}
	if action == api.ActionOpenSession {
		target, err := s.d.OpenTaskSession(ctx, projectPath, ts.path)
		return tabResult("task session", target, err)
	}
	target, err := s.d.OpenTaskWorktreeDirTab(ctx, projectPath, ts.path)
	return tabResult("worktree", target, err)
}

// tabResult is a host tab's response. An already-open tab is a 200 no-op:
// nothing new was opened. Any other failure (no runner, no workspace, a
// missing directory) is a 409: the daemon cannot open that tab now.
func tabResult(kind, target string, err error) (api.WriteResult, error) {
	res := HostTabResult{Kind: kind, Target: target}
	var already *terminal.AlreadyOpenError
	switch {
	case errors.As(err, &already):
		res.AlreadyOpen = true
		res.Note = "the " + kind + " tab for " + already.Title + " is already open"
		if already.FocusErr != nil {
			res.FocusError = already.FocusErr.Error()
			res.Note += "; switch to it yourself"
		} else {
			res.Note += "; switched to it"
		}
	case err != nil:
		return api.WriteResult{}, fmt.Errorf("%w: open %s: %s", api.ErrInvalidState, kind, err.Error())
	default:
		res.Note = "opened " + kind + " in a new tab of the daemon's terminal → " + target
	}
	redactStrings(&res)
	return api.WriteResult{Noop: res.AlreadyOpen, Body: res, Path: target}, nil
}
