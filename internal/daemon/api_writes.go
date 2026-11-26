package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/slimslenderslacks/work/internal/api"
	"github.com/slimslenderslacks/work/internal/control"
)

// The API's write actions (docs/proposals/workingman-gui.md §A.4). Each one
// resolves the request's ids against the snapshot model and makes one file
// write through internal/control, the same code the TUI calls, so the API
// and the TUI cannot drift. The daemon reacts to the file as it does to a
// hand-made one; the response is the file state, never the reaction.

// IntakeWithdrawn is the state of an intake request withdrawn before the
// planner took it (<name>.withdrawn). The read endpoints do not list it.
const IntakeWithdrawn = "withdrawn"

// WriteInfo is a write response's "write" block: the file written.
type WriteInfo struct {
	Path string `json:"path"`
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
