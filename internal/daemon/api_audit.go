package daemon

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/slimslenderslacks/work/internal/api"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/postoffice"
)

// APIAuditEvent is an audit event with its seq (§A.1.5). Project is the id
// of the project one of its path fields lies under, if any, so a timeline
// row can link to it.
type APIAuditEvent struct {
	Seq     uint64            `json:"seq"`
	Time    time.Time         `json:"time"`
	Event   string            `json:"event"`
	Project string            `json:"project,omitempty"`
	Fields  map[string]string `json:"fields,omitempty"`
}

// auditPathFields are the fields that tie an audit event to a project: an
// event belongs to a project when one of them names a path under the
// project's directory (a session-map key is the project.yaml path, maybe
// with a #marker).
var auditPathFields = []string{"path", "key", "project", "project_path", "task_path"}

func (s apiSource) Audit(_ context.Context, q api.AuditQuery) ([]any, error) {
	m := s.d.model()
	projectDir := ""
	if q.Project != "" {
		p, err := m.project(q.Project)
		if err != nil {
			return nil, err
		}
		projectDir = filepath.Dir(p.Path)
	}
	dirs := m.projectDirs()
	// The ring holds the last 500 lines. A since older than the ring returns
	// what it still holds: the client sees the gap in the first seq, and the
	// older lines are only in the audit log, which carries no seq.
	var lines []audit.Line
	if q.HasSince {
		lines, _ = s.d.audit.Since(q.Since)
	} else {
		lines = s.d.audit.RecentLines(api.MaxAuditLimit)
	}
	var events []APIAuditEvent
	for _, ln := range lines {
		ev, ok := audit.ParseLine(ln.Text)
		if !ok {
			continue
		}
		if len(q.Events) > 0 && !slices.Contains(q.Events, ev.Name) {
			continue
		}
		if projectDir != "" && !auditInProject(ev.Fields, projectDir) {
			continue
		}
		events = append(events, APIAuditEvent{Seq: ln.Seq, Time: ev.Time.UTC(), Event: ev.Name, Project: auditProject(ev.Fields, dirs), Fields: ev.Fields})
	}
	if len(events) > q.Limit {
		if q.HasSince {
			events = events[:q.Limit]
		} else {
			events = events[len(events)-q.Limit:]
		}
	}
	out := make([]any, 0, len(events))
	for _, e := range events {
		if len(e.Fields) == 0 {
			e.Fields = nil
		}
		redactStrings(&e)
		out = append(out, e)
	}
	return out, nil
}

// projectDirs maps each project's directory to its id (the project that
// keeps the id, when two share one).
func (m *apiModel) projectDirs() map[string]string {
	dirs := make(map[string]string, len(m.projects))
	for i, p := range m.projects {
		if id := m.id(p); m.ids[id] == i {
			dirs[filepath.Dir(p.Path)] = id
		}
	}
	return dirs
}

func auditInProject(fields map[string]string, dir string) bool {
	for _, k := range auditPathFields {
		v := fields[k]
		if v == "" {
			continue
		}
		v, _, _ = strings.Cut(v, "#")
		if within(v, dir) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// MessageResource is a post-office message for the operator (§A.1.7).
type MessageResource struct {
	ID         string                 `json:"id"`
	Kind       string                 `json:"kind"`
	From       postoffice.Sender      `json:"from"`
	Recipient  string                 `json:"recipient"`
	Subject    string                 `json:"subject"`
	Body       string                 `json:"body"`
	Options    []string               `json:"options"`
	CreatedAt  time.Time              `json:"created_at"`
	ExpiresAt  time.Time              `json:"expires_at"`
	InReplyTo  string                 `json:"in_reply_to,omitempty"`
	State      string                 `json:"state"`
	Expired    bool                   `json:"expired"`
	Resolution *postoffice.Resolution `json:"resolution"`
}

// postOfficeDir is the post-office store the daemon reads: the Step-Up
// agent's, else the default beside the sessions root.
func (d *Daemon) postOfficeDir() string {
	if d.stepUp != nil && d.stepUp.poDir != "" {
		return d.stepUp.poDir
	}
	if root := d.sessionsRootOrEmpty(); root != "" {
		return postoffice.DefaultDir(root)
	}
	return ""
}

func messageResource(m postoffice.Message, res *postoffice.Resolution, now time.Time) MessageResource {
	r := MessageResource{
		ID: m.ID, Kind: string(m.Kind), From: m.From, Recipient: m.Recipient, Subject: m.Subject, Body: m.Body,
		Options: cloneOrEmpty(m.Options), CreatedAt: m.CreatedAt.UTC(), ExpiresAt: m.ExpiresAt.UTC(), InReplyTo: m.InReplyTo,
		State: api.MessagesOpen, Expired: m.Expired(now),
	}
	if res != nil {
		rr := *res
		rr.ClaimID = "" // a claimer's private token
		r.State, r.Resolution = api.MessagesClosed, &rr
	}
	return r
}

func (s apiSource) Messages(_ context.Context, q api.MessageQuery) ([]any, error) {
	dir := s.d.postOfficeDir()
	if dir == "" {
		return nil, nil
	}
	// A plain Store, not postoffice.Open: reading must not create or chmod
	// the store's directories.
	store := &postoffice.Store{Root: dir}
	now := time.Now()
	out, err := inboxMessages(store, now)
	if err != nil {
		return nil, err
	}
	if q.State != api.MessagesOpen {
		done, err := store.ListDone(q.Since)
		if err != nil {
			return nil, err
		}
		for _, dr := range done {
			if dr.ToOperator() {
				res := dr.Resolution
				out = append(out, messageResource(dr.Message, &res, now))
			}
		}
	}
	var items []any
	for _, r := range out {
		switch {
		case q.State == api.MessagesOpen && r.State != api.MessagesOpen:
			continue
		case q.State == api.MessagesClosed && r.State != api.MessagesClosed:
			continue
		case r.State == api.MessagesClosed && !q.Since.IsZero() && r.Resolution.At.Before(q.Since):
			continue
		}
		redactStrings(&r)
		items = append(items, r)
	}
	return items, nil
}

// inboxMessages lists the operator's messages still in the inbox: open ones,
// and claimed ones (closed) that post-office has not finalised yet.
func inboxMessages(store *postoffice.Store, now time.Time) ([]MessageResource, error) {
	open, err := store.ListOpen()
	if err != nil {
		return nil, err
	}
	var out []MessageResource
	for _, m := range open {
		if !m.ToOperator() {
			continue
		}
		// A claimed message is closed; it leaves the inbox once post-office
		// finalises it.
		var res *postoffice.Resolution
		if r, ok, err := store.Claimed(m.ID); err == nil && ok {
			res = &r
		}
		out = append(out, messageResource(m, res, now))
	}
	return out, nil
}
