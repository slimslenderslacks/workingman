package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/slimslenderslacks/work/internal/api"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/postoffice"
	"github.com/slimslenderslacks/work/internal/sbx"
)

// The event stream's publisher (docs/proposals/workingman-gui.md §A.3.2). It
// hooks markSnapshotDirty, which fires on every audit line, session start
// and end, and project/task/intake file event, and also wakes on a tick, for
// what changes without a kick (session.json, the post-office, `sbx ls`).
// Each pass rebuilds the API's resource model, which re-parses only the
// projects whose files changed (the collector and the fileCaches are keyed
// on mtime and size), diffs it per resource against the previous pass, and
// publishes one typed event per change plus one audit event per new audit
// line. State is always taken from the diff, never from audit lines, so a
// write made outside the daemon produces its events too.

const (
	// eventsDebounce coalesces a burst of kicks into one pass. It is much
	// shorter than the snapshot's: a GUI wants changes promptly.
	eventsDebounce = 25 * time.Millisecond
	// eventsTick is how often a pass runs without a kick.
	eventsTick = 2 * time.Second
	// endOutcomeTTL is how long an audit line's session outcome (reaped,
	// stopped) waits for the session to leave the map.
	endOutcomeTTL = time.Minute
)

// eventPublisher turns state changes into events on its hub.
type eventPublisher struct {
	d    *Daemon
	hub  *api.Hub
	kick chan struct{}

	// The previous pass's state; only the publisher goroutine touches it
	// (and the first, synchronous pass, before the goroutine starts).
	prev     *eventState
	auditSeq uint64
	outcomes map[string]sessionEnd // session-map key -> how it ended
}

// sessionEnd is what the audit log said about a session's end.
type sessionEnd struct {
	outcome string
	err     string
	at      time.Time
}

func newEventPublisher(d *Daemon) *eventPublisher {
	epoch := strconv.FormatInt(d.snapshot.startedAt.UnixMilli(), 10)
	return &eventPublisher{
		d: d, hub: api.NewHub(epoch, api.DefaultRingSize),
		kick: make(chan struct{}, 1), outcomes: map[string]sessionEnd{},
	}
}

// poke asks for a pass soon. Cheap, non-blocking and nil-safe.
func (p *eventPublisher) poke() {
	if p == nil {
		return
	}
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

// startEvents takes the first state synchronously, so the first client
// already gets a full replay, and then publishes until ctx is done.
func (d *Daemon) startEvents(ctx context.Context) {
	if d.events == nil {
		return
	}
	d.events.prime(ctx)
	go d.events.loop(ctx)
}

func (p *eventPublisher) prime(ctx context.Context) {
	p.auditSeq = p.d.audit.LastSeq()
	st := p.build(ctx)
	p.prev = st
	p.hub.Publish(nil, st.replay)
}

func (p *eventPublisher) loop(ctx context.Context) {
	t := time.NewTicker(eventsTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-p.kick:
			select {
			case <-time.After(eventsDebounce):
			case <-ctx.Done():
				return
			}
			select {
			case <-p.kick:
			default:
			}
		}
		p.pass(ctx)
	}
}

// pass rebuilds, diffs and publishes once, and returns what it published.
func (p *eventPublisher) pass(ctx context.Context) []api.Event {
	// Audit lines first: they name how sessions ended, which the diff below
	// reports. Lines logged while the model is built go to the next pass.
	auditEvs := p.auditEvents()
	st := p.build(ctx)
	evs := p.diff(p.prev, st)
	p.prev = st
	evs = append(evs, auditEvs...)
	p.hub.Publish(evs, st.replay)
	return evs
}

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

// resource is one resource of a pass: its value, as served, and its JSON
// fields, for a field-level diff.
type resource struct {
	v      any
	fields map[string]json.RawMessage
}

func newResource(v any) resource {
	b, _ := json.Marshal(v)
	var f map[string]json.RawMessage
	_ = json.Unmarshal(b, &f)
	return resource{v: v, fields: f}
}

// changed lists the fields whose JSON differs, sorted, leaving out skip.
func (r resource) changed(old resource, skip ...string) []string {
	var out []string
	for k, v := range r.fields {
		if !slices.Contains(skip, k) && !bytes.Equal(v, old.fields[k]) {
			out = append(out, k)
		}
	}
	for k := range old.fields {
		if _, ok := r.fields[k]; !ok && !slices.Contains(skip, k) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// eventState is one pass's resources, each map keyed by resource id, and
// each list of keys in the order events are emitted.
type eventState struct {
	projects map[string]resource // ProjectSummary by project id
	tasks    map[string]resource // TaskResource by project id + "/" + name
	sessions map[string]resource // SessionResource by session id
	intake   map[string]resource // IntakeRequest by project id + "/" + name
	pulls    map[string]resource // PullRequest by project id + "/" + repo#number
	messages map[string]resource // MessageResource by message id
	order    struct{ projects, tasks, sessions, intake, pulls, messages []string }
	// projectDirs maps a project directory to its id, for audit lines.
	projectDirs map[string]string
}

func (st *eventState) add(m map[string]resource, order *[]string, key string, v any) {
	if _, dup := m[key]; dup {
		return
	}
	m[key] = newResource(v)
	*order = append(*order, key)
}

// build takes the current state, redacted.
func (p *eventPublisher) build(ctx context.Context) *eventState {
	d := p.d
	st := &eventState{
		projects: map[string]resource{}, tasks: map[string]resource{}, sessions: map[string]resource{},
		intake: map[string]resource{}, pulls: map[string]resource{}, messages: map[string]resource{},
		projectDirs: map[string]string{},
	}
	m := d.model()
	for i := range m.projects {
		pr := &m.projects[i]
		id := m.id(*pr)
		if _, dup := st.projects[id]; dup {
			continue
		}
		sum := m.summary(pr)
		redactStrings(&sum)
		st.add(st.projects, &st.order.projects, id, sum)
		st.projectDirs[filepath.Dir(pr.Path)] = id
		for _, ts := range pr.Tasks {
			t := m.task(pr, ts)
			redactStrings(&t)
			st.add(st.tasks, &st.order.tasks, id+"/"+t.Name, t)
		}
		for _, r := range d.intakeRequests(id, pr.Path, false, "") {
			key := id + "/" + r.Name
			redactStrings(&r)
			if _, dup := st.intake[key]; dup {
				// A name both pending and processed: the pending one is
				// the live one, as GET /intake/{name} has it.
				if r.State != api.IntakeProcessed {
					st.intake[key] = newResource(r)
				}
				continue
			}
			st.add(st.intake, &st.order.intake, key, r)
		}
		for _, r := range m.pulls(pr) {
			st.add(st.pulls, &st.order.pulls, id+"/"+r.Repo+"#"+strconv.Itoa(r.Number), r)
		}
	}
	// `sbx ls` only while a client is listening and there are sessions
	// whose sandbox it would report; the cache keeps it to one call per
	// TTL. Without it sandbox.live is null, and turns true or false (a
	// session.updated) on the first pass after a client connects.
	var live map[string]sbx.Sandbox
	if len(m.sessions) > 0 && p.hub.Subscribers() > 0 {
		live, _ = d.liveSandboxes(ctx)
	}
	for _, s := range d.sessionResources(m, live) {
		redactStrings(&s)
		st.add(st.sessions, &st.order.sessions, s.ID, s)
	}
	if dir := d.postOfficeDir(); dir != "" {
		if msgs, err := inboxMessages(&postoffice.Store{Root: dir}, time.Now()); err == nil {
			for _, r := range msgs {
				redactStrings(&r)
				st.add(st.messages, &st.order.messages, r.ID, r)
			}
		}
	}
	return st
}

// replay is the state as events, for a client that connects without a
// cursor (hello{replay:"full"}).
func (st *eventState) replay() []api.Event {
	var out []api.Event
	for _, id := range st.order.projects {
		out = append(out, api.NewEvent(api.EventProjectCreated, id, api.CauseDaemon, st.projects[id].v))
	}
	for _, k := range st.order.tasks {
		t := st.tasks[k].v.(TaskResource)
		out = append(out, api.NewEvent(api.EventTaskCreated, t.Project, api.CauseDaemon, t))
	}
	for _, k := range st.order.sessions {
		s := st.sessions[k].v.(SessionResource)
		out = append(out, api.NewEvent(api.EventSessionStarted, s.Project, api.CauseDaemon, s))
	}
	for _, k := range st.order.intake {
		r := st.intake[k].v.(IntakeRequest)
		out = append(out, api.NewEvent(api.EventIntakeCreated, r.Project, api.CauseDaemon, r))
	}
	for _, k := range st.order.pulls {
		r := st.pulls[k].v.(PullRequest)
		out = append(out, api.NewEvent(api.EventPRUpdated, r.Project, api.CauseDaemon, r))
	}
	for _, k := range st.order.messages {
		out = append(out, api.NewEvent(api.EventMessageCreated, "", api.CauseDaemon, st.messages[k].v))
	}
	return out
}

// ---------------------------------------------------------------------------
// Payloads (§A.3.3)
// ---------------------------------------------------------------------------

// ProjectUpdatedEvent is project.updated's data.
type ProjectUpdatedEvent struct {
	ID      string         `json:"id"`
	ETag    string         `json:"etag,omitempty"`
	Changed []string       `json:"changed"`
	Project ProjectSummary `json:"project"`
}

// ProjectStatusEvent is project.status's data.
type ProjectStatusEvent struct {
	ID            string `json:"id"`
	From          string `json:"from"`
	To            string `json:"to"`
	BlockedReason string `json:"blocked_reason"`
	StoppedFrom   string `json:"stopped_from"`
}

// ProjectRemovedEvent is project.removed's data.
type ProjectRemovedEvent struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

// ProjectLiveEvent is project.live's data.
type ProjectLiveEvent struct {
	ID   string       `json:"id"`
	Live *ProjectLive `json:"live"`
}

// TaskUpdatedEvent is task.updated's data.
type TaskUpdatedEvent struct {
	Project string       `json:"project"`
	Name    string       `json:"name"`
	ETag    string       `json:"etag,omitempty"`
	Changed []string     `json:"changed"`
	Task    TaskResource `json:"task"`
}

// TaskStatusEvent is task.status's data.
type TaskStatusEvent struct {
	Project       string `json:"project"`
	Name          string `json:"name"`
	From          string `json:"from"`
	To            string `json:"to"`
	Attempts      int    `json:"attempts"`
	FailureReason string `json:"failure_reason"`
	BlockedReason string `json:"blocked_reason"`
}

// TaskRemovedEvent is task.removed's data.
type TaskRemovedEvent struct {
	Project string `json:"project"`
	Name    string `json:"name"`
}

// QueueUpdatedEvent is queue.updated's data.
type QueueUpdatedEvent struct {
	Project        string   `json:"project"`
	CommitQueue    []string `json:"commit_queue"`
	Integrating    string   `json:"integrating"`
	CommitReserved bool     `json:"commit_reserved"`
}

// SessionUpdatedEvent is session.updated's data.
type SessionUpdatedEvent struct {
	ID      string          `json:"id"`
	Changed []string        `json:"changed"`
	Session SessionResource `json:"session"`
}

// Session end outcomes in [SessionEndedEvent].
const (
	SessionOutcomeEnded    = "ended"
	SessionOutcomeReaped   = "reaped"
	SessionOutcomeStopped  = "stopped"
	SessionOutcomeDetached = "detached"
)

// SessionEndedEvent is session.ended's data.
type SessionEndedEvent struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Project string `json:"project,omitempty"`
	Task    string `json:"task,omitempty"`
	Outcome string `json:"outcome"`
	Err     string `json:"err,omitempty"`
}

// IntakeRemovedEvent is intake.removed's data.
type IntakeRemovedEvent struct {
	Project string `json:"project"`
	Name    string `json:"name"`
}

// ---------------------------------------------------------------------------
// Diff
// ---------------------------------------------------------------------------

// diff lists the events that take a client from old to cur: projects, then
// tasks, then commit queues, sessions, intake, PRs and messages, and
// removals last (a project's tasks and intake before the project).
func (p *eventPublisher) diff(old, cur *eventState) []api.Event {
	var evs []api.Event
	emit := func(typ, project, cause string, data any) {
		evs = append(evs, api.NewEvent(typ, project, cause, data))
	}

	for _, id := range cur.order.projects {
		nr := cur.projects[id]
		n := nr.v.(ProjectSummary)
		or, existed := old.projects[id]
		if !existed {
			emit(api.EventProjectCreated, id, api.CauseFile, n)
			continue
		}
		o := or.v.(ProjectSummary)
		if ch := nr.changed(or, "live"); len(ch) > 0 {
			if n.Status != o.Status {
				emit(api.EventProjectStatus, id, api.CauseFile, ProjectStatusEvent{ID: id, From: o.Status, To: n.Status, BlockedReason: n.Blocked, StoppedFrom: n.StoppedFrom})
			}
			emit(api.EventProjectUpdated, id, api.CauseFile, ProjectUpdatedEvent{ID: id, ETag: n.ETag, Changed: ch, Project: n})
		}
		if len(nr.changed(or, nonLive(nr, or)...)) > 0 {
			emit(api.EventProjectLive, id, api.CauseLive, ProjectLiveEvent{ID: id, Live: n.Live})
		}
	}
	for _, k := range cur.order.tasks {
		nr := cur.tasks[k]
		n := nr.v.(TaskResource)
		or, existed := old.tasks[k]
		if !existed {
			emit(api.EventTaskCreated, n.Project, api.CauseFile, n)
			continue
		}
		o := or.v.(TaskResource)
		ch := nr.changed(or)
		if len(ch) == 0 {
			continue
		}
		if n.Status != o.Status {
			emit(api.EventTaskStatus, n.Project, api.CauseFile, TaskStatusEvent{Project: n.Project, Name: n.Name, From: o.Status, To: n.Status,
				Attempts: n.Attempts, FailureReason: n.FailureReason, BlockedReason: n.BlockedReason})
		}
		cause := api.CauseFile
		if onlyFields(ch, "session", "sandbox") {
			cause = api.CauseSession
		}
		emit(api.EventTaskUpdated, n.Project, cause, TaskUpdatedEvent{Project: n.Project, Name: n.Name, ETag: n.ETag, Changed: ch, Task: n})
	}
	// The queue after the tasks: a task reaching success is the cause of
	// the queue changing.
	for _, id := range cur.order.projects {
		or, existed := old.projects[id]
		if !existed {
			continue
		}
		oq, nq := queueOf(or.v.(ProjectSummary)), queueOf(cur.projects[id].v.(ProjectSummary))
		if !slices.Equal(oq.CommitQueue, nq.CommitQueue) || oq.Integrating != nq.Integrating || oq.CommitReserved != nq.CommitReserved {
			cause := api.CauseLive
			if !slices.Equal(oq.CommitQueue, nq.CommitQueue) {
				cause = api.CauseFile
			}
			emit(api.EventQueueUpdated, id, cause, nq)
		}
	}
	for _, k := range cur.order.sessions {
		nr := cur.sessions[k]
		n := nr.v.(SessionResource)
		or, existed := old.sessions[k]
		if !existed {
			emit(api.EventSessionStarted, n.Project, api.CauseSession, n)
			continue
		}
		if ch := nr.changed(or); len(ch) > 0 {
			emit(api.EventSessionUpdated, n.Project, api.CauseSession, SessionUpdatedEvent{ID: n.ID, Changed: ch, Session: n})
		}
	}
	for _, k := range old.order.sessions {
		if _, ok := cur.sessions[k]; ok {
			continue
		}
		o := old.sessions[k].v.(SessionResource)
		end := p.takeOutcome(o.Key)
		emit(api.EventSessionEnded, o.Project, api.CauseSession, SessionEndedEvent{ID: o.ID, Kind: o.Kind, Project: o.Project, Task: o.Task, Outcome: end.outcome, Err: end.err})
	}
	p.pruneOutcomes()
	for _, k := range cur.order.intake {
		nr := cur.intake[k]
		n := nr.v.(IntakeRequest)
		or, existed := old.intake[k]
		switch {
		case !existed:
			typ := api.EventIntakeCreated
			if n.State == api.IntakeProcessed {
				typ = api.EventIntakeDone
			}
			emit(typ, n.Project, api.CauseFile, n)
		case n.State == api.IntakeProcessed && or.v.(IntakeRequest).State != api.IntakeProcessed:
			emit(api.EventIntakeDone, n.Project, api.CauseFile, n)
		case len(nr.changed(or)) > 0:
			emit(api.EventIntakeUpdated, n.Project, api.CauseFile, n)
		}
	}
	for _, k := range cur.order.pulls {
		nr := cur.pulls[k]
		if or, existed := old.pulls[k]; !existed || len(nr.changed(or)) > 0 {
			n := nr.v.(PullRequest)
			emit(api.EventPRUpdated, n.Project, api.CauseFile, n)
		}
	}
	for _, k := range cur.order.messages {
		nr := cur.messages[k]
		n := nr.v.(MessageResource)
		or, existed := old.messages[k]
		switch {
		case !existed && n.State == api.MessagesClosed:
			emit(api.EventMessageClosed, "", api.CauseFile, n)
		case !existed:
			emit(api.EventMessageCreated, "", api.CauseFile, n)
		case n.State == api.MessagesClosed && or.v.(MessageResource).State != api.MessagesClosed:
			emit(api.EventMessageClosed, "", api.CauseFile, n)
		}
	}
	for _, k := range old.order.messages {
		if _, ok := cur.messages[k]; ok {
			continue
		}
		// Finalised: it left the inbox for done/, with its resolution.
		o := old.messages[k].v.(MessageResource)
		if o.State == api.MessagesClosed {
			continue // its message.closed went out with the claim
		}
		if dir := p.d.postOfficeDir(); dir != "" {
			if dr, ok, err := (&postoffice.Store{Root: dir}).Done(o.ID); err == nil && ok {
				res := dr.Resolution
				r := messageResource(dr.Message, &res, time.Now())
				redactStrings(&r)
				emit(api.EventMessageClosed, "", api.CauseFile, r)
			}
		}
	}

	// Removals: intake files and tasks before the project they belong to.
	for _, k := range old.order.intake {
		if _, ok := cur.intake[k]; !ok {
			o := old.intake[k].v.(IntakeRequest)
			emit(api.EventIntakeRemoved, o.Project, api.CauseFile, IntakeRemovedEvent{Project: o.Project, Name: o.Name})
		}
	}
	for _, k := range old.order.tasks {
		if _, ok := cur.tasks[k]; !ok {
			o := old.tasks[k].v.(TaskResource)
			emit(api.EventTaskRemoved, o.Project, api.CauseFile, TaskRemovedEvent{Project: o.Project, Name: o.Name})
		}
	}
	for _, id := range old.order.projects {
		if _, ok := cur.projects[id]; !ok {
			o := old.projects[id].v.(ProjectSummary)
			emit(api.EventProjectRemoved, id, api.CauseFile, ProjectRemovedEvent{ID: id, Path: o.Path})
		}
	}
	return evs
}

// nonLive is every field of a project summary but live, so changed(...,
// nonLive...) reports whether live changed.
func nonLive(a, b resource) []string {
	var out []string
	for k := range a.fields {
		if k != "live" {
			out = append(out, k)
		}
	}
	for k := range b.fields {
		if k != "live" {
			out = append(out, k)
		}
	}
	return out
}

func queueOf(s ProjectSummary) QueueUpdatedEvent {
	q := QueueUpdatedEvent{Project: s.ID, CommitQueue: cloneOrEmpty(s.CommitQueue)}
	if s.Live != nil {
		q.Integrating, q.CommitReserved = s.Live.Integrating, s.Live.CommitReserved
	}
	return q
}

// onlyFields reports whether every name in changed is one of allowed.
func onlyFields(changed []string, allowed ...string) bool {
	for _, c := range changed {
		if !slices.Contains(allowed, c) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Audit
// ---------------------------------------------------------------------------

// auditEvents turns the audit lines logged since the last pass into audit
// events, and notes how sessions ended. If the ring overflowed between two
// passes, the lines it lost are gone from the stream too; the audit log
// still has them.
func (p *eventPublisher) auditEvents() []api.Event {
	lines, _ := p.d.audit.Since(p.auditSeq)
	if len(lines) == 0 {
		return nil
	}
	p.auditSeq = lines[len(lines)-1].Seq
	var dirs map[string]string
	if p.prev != nil {
		dirs = p.prev.projectDirs
	}
	out := make([]api.Event, 0, len(lines))
	for _, ln := range lines {
		ev, ok := audit.ParseLine(ln.Text)
		if !ok {
			continue
		}
		p.noteSessionEnd(ev)
		e := APIAuditEvent{Seq: ln.Seq, Time: ev.Time.UTC(), Event: ev.Name, Fields: ev.Fields}
		if len(e.Fields) == 0 {
			e.Fields = nil
		}
		redactStrings(&e)
		out = append(out, api.NewEvent(api.EventAudit, auditProject(ev.Fields, dirs), api.CauseAudit, e))
	}
	return out
}

// auditProject is the id of the project an audit line belongs to, or "".
func auditProject(fields map[string]string, dirs map[string]string) string {
	for dir, id := range dirs {
		if auditInProject(fields, dir) {
			return id
		}
	}
	return ""
}

// noteSessionEnd records what an audit line says about a session's end, for
// the session.ended that follows when the session leaves the map.
func (p *eventPublisher) noteSessionEnd(ev audit.Event) {
	key := ev.Fields["key"]
	if key == "" {
		return
	}
	e := p.outcomes[key]
	switch ev.Name {
	case "session_reaped":
		e.outcome = SessionOutcomeReaped
	case "session_stopped":
		e.outcome = SessionOutcomeStopped
	case "session_detached":
		e.outcome = SessionOutcomeDetached
	case "session_ended":
		e.err = ev.Fields["err"]
	default:
		return
	}
	e.at = time.Now()
	p.outcomes[key] = e
}

func (p *eventPublisher) takeOutcome(key string) sessionEnd {
	e := p.outcomes[key]
	delete(p.outcomes, key)
	if e.outcome == "" {
		e.outcome = SessionOutcomeEnded
	}
	return e
}

// pruneOutcomes forgets outcomes no session claimed (a line about a session
// the API never listed), so the map stays small.
func (p *eventPublisher) pruneOutcomes() {
	for k, e := range p.outcomes {
		if time.Since(e.at) > endOutcomeTTL {
			delete(p.outcomes, k)
		}
	}
}
