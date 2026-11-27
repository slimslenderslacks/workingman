package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/slimslenderslacks/work/internal/api"
	"github.com/slimslenderslacks/work/internal/control"
	"github.com/slimslenderslacks/work/internal/postoffice"
)

// The API's write actions (docs/proposals/workingman-gui.md §A.4). Each one
// resolves the request's ids against the snapshot model and makes one file
// write through internal/control, the same code the TUI calls, so the API
// and the TUI cannot drift. The daemon reacts to the file as it does to a
// hand-made one; the response is the file state, never the reaction.

// IntakeWithdrawn is the state of an intake request withdrawn before the
// planner took it (<name>.withdrawn). The read endpoints do not list it.
const IntakeWithdrawn = "withdrawn"

// WriteInfo is a write response's "write" block: the file written and, for
// a project-file action, the fields it set.
type WriteInfo struct {
	Path   string         `json:"path"`
	Fields map[string]any `json:"fields,omitempty"`
}

// NewProjectResult is the POST /projects response (§A.4.3).
type NewProjectResult struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	// State is "seeded": project.md is written; the daemon writes
	// project.yaml next and the project agent fills it in.
	State string `json:"state"`
	// Warning says how the name will be shortened in sandbox names, if it
	// will.
	Warning string    `json:"warning,omitempty"`
	Write   WriteInfo `json:"write"`
	Expect  []string  `json:"expect"`
}

// IntakeWriteResult is the response of POST /projects/{id}/intake and
// DELETE /projects/{id}/intake/{name}.
type IntakeWriteResult struct {
	Intake IntakeRequest `json:"intake"`
	Noop   bool          `json:"noop,omitempty"`
	Write  WriteInfo     `json:"write"`
	Expect []string      `json:"expect"`
}

// controlErr maps a control refusal to the API error that picks its status;
// any other error (an I/O failure, an api error from a check) passes as is.
func controlErr(err error) error {
	var kind error
	switch {
	case errors.Is(err, control.ErrInvalid):
		kind = api.ErrInvalid
	case errors.Is(err, control.ErrExists), errors.Is(err, control.ErrProcessed):
		kind = api.ErrConflict
	case errors.Is(err, control.ErrNotFound):
		kind = api.ErrNotFound
	case errors.Is(err, control.ErrState):
		kind = api.ErrInvalidState
	default:
		return err
	}
	return fmt.Errorf("%w: %s", kind, err.Error())
}

func (s apiSource) NewProject(_ context.Context, _ api.WriteRequest, req api.NewProjectRequest) (api.WriteResult, error) {
	if req.Root < 0 || req.Root >= len(s.d.roots) {
		return api.WriteResult{}, fmt.Errorf("%w: root %d does not exist (the daemon watches %d)", api.ErrInvalid, req.Root, len(s.d.roots))
	}
	root, err := filepath.Abs(s.d.roots[req.Root])
	if err != nil {
		return api.WriteResult{}, err
	}
	path, err := control.NewProject(root, req.Name, req.Description)
	if err != nil {
		return api.WriteResult{}, controlErr(err)
	}
	res := NewProjectResult{
		ID: projectID(req.Root, req.Name), Path: path, State: "seeded",
		Warning: control.ProjectNameWarning(req.Name),
		Write:   WriteInfo{Path: path},
		Expect:  []string{"project.created", "project.status"},
	}
	redactStrings(&res)
	return api.WriteResult{Body: res, Path: path}, nil
}

func (s apiSource) QueueIntake(_ context.Context, w api.WriteRequest, id string, req api.QueueIntakeRequest) (api.WriteResult, error) {
	m := s.d.model()
	p, err := m.project(id)
	if err != nil {
		return api.WriteResult{}, err
	}
	// Create-only: there is no earlier version for If-Match to name.
	if w.IfMatch != "" {
		return api.WriteResult{}, fmt.Errorf("%w: a new intake request has no etag to match; send no If-Match", api.ErrPreconditionFailed)
	}
	name := req.Name
	if name == "" {
		name = control.DefaultIntakeName("gui", time.Now())
	}
	path, err := control.QueueIntake(p.Path, name, req.Body)
	if err != nil {
		return api.WriteResult{}, controlErr(err)
	}
	r := s.d.intakeResource(id, path, api.IntakePending)
	res := IntakeWriteResult{Intake: r, Write: WriteInfo{Path: path}, Expect: []string{"intake.created", "intake.processed"}}
	redactStrings(&res)
	return api.WriteResult{Body: res, ETag: r.ETag, Path: path, ETagAfter: r.ETag}, nil
}

func (s apiSource) WithdrawIntake(_ context.Context, w api.WriteRequest, id, name string) (api.WriteResult, error) {
	m := s.d.model()
	p, err := m.project(id)
	if err != nil {
		return api.WriteResult{}, err
	}
	var before string
	check := func(body []byte) error {
		before = resourceETag("in", contentTag(body))
		if !api.MatchETag(w.IfMatch, before) {
			cur := s.d.intakeResource(id, filepath.Join(control.IntakeDir(p.Path), name+control.IntakePendingExt), api.IntakePending)
			redactStrings(&cur)
			return api.WithCurrent(fmt.Errorf("%w: intake request %q changed since you loaded it (now %s)", api.ErrPreconditionFailed, name, before), cur)
		}
		return nil
	}
	path, noop, err := control.WithdrawIntake(p.Path, name, check)
	if err != nil {
		return api.WriteResult{}, controlErr(err)
	}
	r := s.d.intakeResource(id, path, IntakeWithdrawn)
	res := IntakeWriteResult{Intake: r, Noop: noop, Write: WriteInfo{Path: path}, Expect: []string{"intake.removed"}}
	redactStrings(&res)
	return api.WriteResult{Noop: noop, Body: res, Path: path, ETagBefore: before}, nil
}

// intakeResource describes the intake file at path in state, without its
// body.
func (d *Daemon) intakeResource(projectID, path, state string) IntakeRequest {
	file := filepath.Base(path)
	r := IntakeRequest{Project: projectID, Name: file[:len(file)-len(filepath.Ext(file))], File: file, State: state}
	if data, err := os.ReadFile(path); err == nil {
		r.ETag = resourceETag("in", contentTag(data))
	}
	if fi, err := os.Stat(path); err == nil {
		r.CreatedAt = fi.ModTime().UTC()
	}
	return r
}

var _ api.Actions = apiSource{}

// ---------------------------------------------------------------------------
// Project actions (§A.4.2)
// ---------------------------------------------------------------------------

// ProjectActionResult is the response of POST /projects/{id}/actions/{action}:
// the project as its file now is, what was written, and the events to watch
// for. The daemon's reaction (sessions closing, a wolf launching) is not in
// it: it arrives on the event stream.
type ProjectActionResult struct {
	Project ProjectDetail `json:"project"`
	// Mode is review's: "start" (review: true) or "kick" (review_now: true).
	Mode   string    `json:"mode,omitempty"`
	Noop   bool      `json:"noop,omitempty"`
	Write  WriteInfo `json:"write"`
	Expect []string  `json:"expect"`
}

// projectActionExpect is what each action's response tells the GUI to watch
// for. A hint, not a promise.
var projectActionExpect = map[string][]string{
	api.ActionStop:    {"project.status", "session.ended"},
	api.ActionStart:   {"project.status"},
	api.ActionWolf:    {"project.status", "session.started"},
	api.ActionReview:  {"project.updated", "session.started"},
	api.ActionCleanup: {"project.updated", "session.started"},
	api.ActionHide:    {"project.updated"},
	api.ActionShow:    {"project.updated"},
	api.ActionCatchup: {"audit"},
}

func (s apiSource) ProjectAction(_ context.Context, w api.WriteRequest, id, action string, req api.ProjectActionRequest) (api.WriteResult, error) {
	m := s.d.model()
	p, err := m.project(id)
	if err != nil {
		return api.WriteResult{}, err
	}
	path := p.Path
	var before string
	check := control.Check(func(data []byte) error {
		before = resourceETag("pj", contentTag(data))
		if !api.MatchETag(w.IfMatch, before) {
			return fmt.Errorf("%w: project.yaml changed since you loaded it (now %s)", api.ErrPreconditionFailed, before)
		}
		return nil
	})
	var noop bool
	res := ProjectActionResult{Write: WriteInfo{Path: path}, Expect: projectActionExpect[action]}
	switch action {
	case api.ActionStop:
		_, err = control.Stop(path, check)
	case api.ActionStart:
		_, err = control.Start(path, check)
	case api.ActionWolf:
		reason := req.Reason
		if strings.TrimSpace(reason) == "" {
			reason = control.SummonReasonGUI
		}
		_, err = control.SummonWolf(path, check, control.WolfReason(reason))
	case api.ActionReview:
		var kicked bool
		_, kicked, err = control.Review(path, check, control.ReportNoop(&noop))
		res.Mode = "start"
		res.Write.Fields = map[string]any{"review": true, "updated_by": "agent"}
		if kicked {
			res.Mode = "kick"
			res.Write.Fields = map[string]any{"review_now": true, "updated_by": "agent"}
		}
	case api.ActionCleanup:
		_, err = control.Cleanup(path, check, control.ReportNoop(&noop))
		res.Write.Fields = map[string]any{"cleanup": true, "updated_by": "agent"}
	case api.ActionHide, api.ActionShow:
		hide := action == api.ActionHide
		if hide {
			_, err = control.Hide(path, check, control.ReportNoop(&noop))
		} else {
			_, err = control.Show(path, check, control.ReportNoop(&noop))
		}
		// As the daemon, on purpose: a display toggle is never routed.
		res.Write.Fields = map[string]any{"hide": hide, "updated_by": "daemon"}
	case api.ActionCatchup:
		return s.catchup(id, path, w)
	default:
		return api.WriteResult{}, fmt.Errorf("%w: no project action %q", api.ErrNotFound, action)
	}
	if err != nil {
		err = controlErr(err)
		if errors.Is(err, api.ErrPreconditionFailed) || errors.Is(err, api.ErrInvalidState) {
			err = api.WithCurrent(err, s.d.projectNow(id))
		}
		return api.WriteResult{}, err
	}
	res.Project = s.d.projectNow(id)
	switch action {
	case api.ActionStop, api.ActionStart:
		res.Write.Fields = map[string]any{"status": res.Project.Status, "stopped_from": res.Project.StoppedFrom, "updated_by": "agent"}
	case api.ActionWolf:
		// blocked_reason is written only when the project had none.
		res.Write.Fields = map[string]any{"status": res.Project.Status, "blocked_reason": res.Project.Blocked, "updated_by": "agent"}
	}
	if noop {
		res.Noop, res.Write.Fields = true, nil
	}
	redactStrings(&res)
	return api.WriteResult{Noop: noop, Body: res, ETag: res.Project.ETag, Path: path, ETagBefore: before, ETagAfter: res.Project.ETag}, nil
}

// catchup creates the project's catch-up marker (§A.4.2: the same file
// `orch catchup` writes). If-Match, when sent, is checked against
// project.yaml, which the marker sits beside but does not change.
func (s apiSource) catchup(id, path string, w api.WriteRequest) (api.WriteResult, error) {
	cur := s.d.projectNow(id)
	if !api.MatchETag(w.IfMatch, cur.ETag) {
		return api.WriteResult{}, api.WithCurrent(fmt.Errorf("%w: project.yaml changed since you loaded it (now %s)", api.ErrPreconditionFailed, cur.ETag), cur)
	}
	marker, noop, err := control.RequestCatchup(path)
	if err != nil {
		err = controlErr(err)
		if errors.Is(err, api.ErrInvalidState) {
			err = api.WithCurrent(err, cur)
		}
		return api.WriteResult{}, err
	}
	res := ProjectActionResult{Project: cur, Noop: noop, Write: WriteInfo{Path: marker}, Expect: projectActionExpect[api.ActionCatchup]}
	redactStrings(&res)
	return api.WriteResult{Noop: noop, Body: res, ETag: cur.ETag, Path: marker, ETagBefore: cur.ETag, ETagAfter: cur.ETag}, nil
}

// projectNow is the project's detail resource as its file is now, for a
// write's response or a refusal's "current". It is zero when the project
// has gone.
func (d *Daemon) projectNow(id string) ProjectDetail {
	m := d.model()
	p, err := m.project(id)
	if err != nil {
		return ProjectDetail{}
	}
	det := m.detail(p, api.ProjectOptions{})
	redactStrings(&det)
	return det
}

// ---------------------------------------------------------------------------
// Inbox answers (§A.4.5)
// ---------------------------------------------------------------------------

// MessageWriteResult is the response of POST /messages/{id}/answer and
// /dismiss: the message, closed by this claim.
type MessageWriteResult struct {
	Message MessageResource `json:"message"`
	Write   WriteInfo       `json:"write"`
	Expect  []string        `json:"expect"`
}

func (s apiSource) AnswerMessage(_ context.Context, _ api.WriteRequest, id string, req api.AnswerRequest) (api.WriteResult, error) {
	store, m, err := s.operatorMessage(id)
	if err != nil {
		return api.WriteResult{}, err
	}
	if m.Kind != postoffice.KindRequest {
		return api.WriteResult{}, fmt.Errorf("%w: %s is a notification: dismiss it, there is nothing to answer", api.ErrInvalidState, id)
	}
	r := postoffice.Resolution{ID: id, Outcome: postoffice.OutcomeAnswered, Response: req.Response}
	if opt := strings.TrimSpace(req.Option); opt != "" {
		// An option is one of the request's own, as the TUI's 1-9 keys
		// send it: response and option both name it.
		match := ""
		for _, o := range m.Options {
			if o == opt {
				match = o
			}
		}
		if match == "" {
			return api.WriteResult{}, fmt.Errorf("%w: %q is not one of the request's options %q", api.ErrInvalid, opt, m.Options)
		}
		r.Option, r.Response = match, match
	}
	return s.claim(store, m, r)
}

func (s apiSource) DismissMessage(_ context.Context, _ api.WriteRequest, id string) (api.WriteResult, error) {
	store, m, err := s.operatorMessage(id)
	if err != nil {
		return api.WriteResult{}, err
	}
	if m.Kind != postoffice.KindNotification {
		return api.WriteResult{}, fmt.Errorf("%w: %s is a request: answer it; only a notification is dismissed", api.ErrInvalidState, id)
	}
	return s.claim(store, m, postoffice.Resolution{ID: id, Outcome: postoffice.OutcomeDismissed})
}

// operatorMessage finds an open message to the operator. One that is closed
// already is a 409 conflict with the claim that closed it, as the TUI shows
// it on its reply line.
func (s apiSource) operatorMessage(id string) (*postoffice.Store, postoffice.Message, error) {
	dir := s.d.postOfficeDir()
	if dir == "" {
		return nil, postoffice.Message{}, fmt.Errorf("%w: the daemon has no post-office store", api.ErrUnavailable)
	}
	if !postoffice.ValidID(id) {
		return nil, postoffice.Message{}, fmt.Errorf("%w: no message %q", api.ErrNotFound, id)
	}
	store := &postoffice.Store{Root: dir}
	m, res, err := store.Lookup(id)
	switch {
	case errors.Is(err, postoffice.ErrNotFound):
		return nil, postoffice.Message{}, fmt.Errorf("%w: no message %q", api.ErrNotFound, id)
	case err != nil:
		return nil, postoffice.Message{}, err
	case !m.ToOperator():
		// Agents' messages are theirs to answer; the GUI does not see them.
		return nil, postoffice.Message{}, fmt.Errorf("%w: no message %q for the operator", api.ErrNotFound, id)
	case res != nil:
		return nil, postoffice.Message{}, closedErr(m, *res)
	}
	return store, m, nil
}

// claim closes m with r, answered by the operator via the GUI. First claim
// wins (link(2), §A.4.5): a loser gets 409 with the winner.
func (s apiSource) claim(store *postoffice.Store, m postoffice.Message, r postoffice.Resolution) (api.WriteResult, error) {
	r.Responder = &postoffice.Responder{Kind: postoffice.ResponderOperator, Via: postoffice.ViaGUI}
	won, err := store.Claim(r)
	switch {
	case errors.Is(err, postoffice.ErrAlreadyClosed):
		return api.WriteResult{}, closedErr(m, won)
	case errors.Is(err, postoffice.ErrNotFound):
		return api.WriteResult{}, fmt.Errorf("%w: message %s is already closed", api.ErrConflict, m.ID)
	case err != nil:
		return api.WriteResult{}, err
	}
	path := store.OutboxPath(m.ID)
	res := MessageWriteResult{Message: messageResource(m, &won, time.Now()), Write: WriteInfo{Path: path}, Expect: []string{"message.closed"}}
	redactStrings(&res)
	return api.WriteResult{Body: res, Path: path}, nil
}

// closedErr is the 409 for a message another claim closed, with the message
// and that claim as "current".
func closedErr(m postoffice.Message, won postoffice.Resolution) error {
	cur := messageResource(m, &won, time.Now())
	redactStrings(&cur)
	msg := "message " + m.ID + " is already " + string(won.Outcome)
	if via := won.Via(); via != "" {
		msg += " via " + via
	}
	return api.WithCurrent(fmt.Errorf("%w: %s", api.ErrConflict, msg), cur)
}
