package daemon

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/api"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/task"
)

// eventsFixture is a daemon over writeFixtureRoot with an event publisher
// that a test drives pass by pass.
type eventsFixture struct {
	d     *Daemon
	root  string
	alpha string // alpha's project.yaml
}

func newEventsFixture(t *testing.T) *eventsFixture {
	t.Helper()
	root := writeFixtureRoot(t)
	d, err := newDaemon(t, []string{root}, audit.New(&safeBuf{}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d.ctx = ctx
	d.events = newEventPublisher(d)
	d.events.prime(ctx)
	return &eventsFixture{d: d, root: root, alpha: filepath.Join(root, "alpha", "project.yaml")}
}

func (f *eventsFixture) pass(t *testing.T) []api.Event {
	t.Helper()
	return f.d.events.pass(context.Background())
}

// write rewrites a file under the root with a later mtime, so the stat
// caches see it change even within the filesystem's time resolution.
func (f *eventsFixture) write(t *testing.T, rel, body string) {
	t.Helper()
	p := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	var mt time.Time
	if fi, err := os.Stat(p); err == nil {
		mt = fi.ModTime()
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if !mt.IsZero() {
		later := mt.Add(time.Second)
		_ = os.Chtimes(p, later, later)
	}
}

func ofType(evs []api.Event, typ string) []api.Event {
	var out []api.Event
	for _, e := range evs {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func types(evs []api.Event) string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return strings.Join(out, ",")
}

func decode[T any](t *testing.T, e api.Event) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(e.Data, &v); err != nil {
		t.Fatalf("%s data: %v in %s", e.Type, err, e.Data)
	}
	return v
}

// A pass with nothing changed publishes nothing.
func TestEventsNoChangeNoEvents(t *testing.T) {
	f := newEventsFixture(t)
	if evs := f.pass(t); len(evs) != 0 {
		t.Errorf("idle pass published %s", types(evs))
	}
}

// A task file's status change is a task.status and a task.updated naming
// the changed fields; the project's counts change with it.
func TestEventsTaskStatusDiff(t *testing.T) {
	f := newEventsFixture(t)
	f.write(t, "alpha/tasks/ship.yaml", "name: ship\ndescription: ship it\ndepends_on: [build]\nstatus: running\nattempts: 1\n")
	evs := f.pass(t)

	st := ofType(evs, api.EventTaskStatus)
	if len(st) != 1 {
		t.Fatalf("task.status events = %d in %s", len(st), types(evs))
	}
	ts := decode[TaskStatusEvent](t, st[0])
	if ts.Project != "p_0.alpha" || ts.Name != "ship" || ts.From != "ready" || ts.To != "running" || ts.Attempts != 1 {
		t.Errorf("task.status = %+v", ts)
	}
	if st[0].Project != "p_0.alpha" || st[0].Cause != api.CauseFile {
		t.Errorf("envelope = %+v", st[0])
	}
	up := ofType(evs, api.EventTaskUpdated)
	if len(up) != 1 {
		t.Fatalf("task.updated events = %d", len(up))
	}
	tu := decode[TaskUpdatedEvent](t, up[0])
	if strings.Join(tu.Changed, ",") != "attempts,etag,status" || tu.Task.Status != "running" || tu.ETag == "" {
		t.Errorf("task.updated = changed %v, task %+v", tu.Changed, tu.Task)
	}
	pu := ofType(evs, api.EventProjectUpdated)
	if len(pu) != 1 || !strings.Contains(strings.Join(decode[ProjectUpdatedEvent](t, pu[0]).Changed, ","), "task_counts") {
		t.Errorf("project.updated = %s", types(evs))
	}
	if len(ofType(evs, api.EventProjectStatus)) != 0 {
		t.Error("project.status without a project status change")
	}
	// The seqs are contiguous and after the cursor the first pass left.
	for i, e := range evs {
		if e.Seq != uint64(i+1) {
			t.Errorf("event %d seq = %d", i, e.Seq)
		}
	}
	if f.d.events.hub.Cursor() != uint64(len(evs)) {
		t.Errorf("hub cursor %d after %d events", f.d.events.hub.Cursor(), len(evs))
	}
}

// A task that reaches success joins the commit queue: queue.updated.
func TestEventsQueueUpdated(t *testing.T) {
	f := newEventsFixture(t)
	f.write(t, "alpha/tasks/build.yaml", "name: build\ndescription: build it\ndepends_on: []\nstatus: success\nattempts: 0\n")
	evs := f.pass(t)
	q := ofType(evs, api.EventQueueUpdated)
	if len(q) != 1 {
		t.Fatalf("queue.updated = %d in %s", len(q), types(evs))
	}
	qe := decode[QueueUpdatedEvent](t, q[0])
	if qe.Project != "p_0.alpha" || strings.Join(qe.CommitQueue, ",") != "build" || qe.Integrating != "" || q[0].Cause != api.CauseFile {
		t.Errorf("queue.updated = %+v cause %s", qe, q[0].Cause)
	}

	// The commit agent starting is a live change of the queue (integrating).
	f.d.trackSession(commitSessionKey(f.alpha), newStubSession("commit-alpha"), agent.CommitAgent, "build", nil)
	evs = f.pass(t)
	q = ofType(evs, api.EventQueueUpdated)
	if len(q) != 1 || decode[QueueUpdatedEvent](t, q[0]).Integrating != "build" || q[0].Cause != api.CauseLive {
		t.Errorf("after commit start: %s %v", types(evs), q)
	}
	if len(ofType(evs, api.EventProjectLive)) != 1 || len(ofType(evs, api.EventSessionStarted)) != 1 {
		t.Errorf("after commit start: %s, want project.live and session.started", types(evs))
	}
	if len(ofType(evs, api.EventProjectUpdated)) != 0 {
		t.Errorf("a live-only change produced project.updated: %s", types(evs))
	}
}

// A project status change; a created and a removed task; a created and a
// removed project.
func TestEventsProjectAndTaskLifecycle(t *testing.T) {
	f := newEventsFixture(t)
	f.write(t, "bravo/project.yaml", "description: stuck\nbranch: bravo-branch\nstatus: working\nupdated_by: agent\n")
	f.write(t, "alpha/tasks/docs.yaml", "name: docs\ndescription: d\ndepends_on: []\nstatus: ready\n")
	if err := os.Remove(filepath.Join(f.root, "alpha", "tasks", "ship.yaml")); err != nil {
		t.Fatal(err)
	}
	f.write(t, "charlie/project.yaml", "description: new\nbranch: c\nstatus: ready\n")
	if err := os.RemoveAll(filepath.Join(f.root, "broken")); err != nil {
		t.Fatal(err)
	}
	evs := f.pass(t)

	ps := ofType(evs, api.EventProjectStatus)
	if len(ps) != 1 {
		t.Fatalf("project.status = %s", types(evs))
	}
	if pe := decode[ProjectStatusEvent](t, ps[0]); pe.ID != "p_0.bravo" || pe.From != "blocked" || pe.To != "working" {
		t.Errorf("project.status = %+v", pe)
	}
	if c := ofType(evs, api.EventTaskCreated); len(c) != 1 || decode[TaskResource](t, c[0]).Name != "docs" {
		t.Errorf("task.created = %v", c)
	}
	if r := ofType(evs, api.EventTaskRemoved); len(r) != 1 || decode[TaskRemovedEvent](t, r[0]).Name != "ship" {
		t.Errorf("task.removed = %v", r)
	}
	if c := ofType(evs, api.EventProjectCreated); len(c) != 1 || decode[ProjectSummary](t, c[0]).ID != "p_0.charlie" {
		t.Errorf("project.created = %v", c)
	}
	if r := ofType(evs, api.EventProjectRemoved); len(r) != 1 || decode[ProjectRemovedEvent](t, r[0]).ID != "p_0.broken" {
		t.Errorf("project.removed = %v", r)
	}
	// Removals come after everything else.
	if last := evs[len(evs)-1]; last.Type != api.EventProjectRemoved {
		t.Errorf("last event = %s in %s", last.Type, types(evs))
	}
	for _, e := range evs {
		if strings.Contains(string(e.Data), secretToken) || strings.Contains(string(e.Data), "hunter2") {
			t.Errorf("%s leaks a secret: %s", e.Type, e.Data)
		}
	}
}

// Intake files: created, processed, removed.
func TestEventsIntake(t *testing.T) {
	f := newEventsFixture(t)
	f.write(t, "alpha/intake/add-svg.md", "Please add SVG wireframes\n")
	evs := f.pass(t)
	if c := ofType(evs, api.EventIntakeCreated); len(c) != 1 || decode[IntakeRequest](t, c[0]).Name != "add-svg" {
		t.Fatalf("intake.created = %s", types(evs))
	}
	dir := filepath.Join(f.root, "alpha", "intake")
	if err := os.Rename(filepath.Join(dir, "add-svg.md"), filepath.Join(dir, "add-svg.processed")); err != nil {
		t.Fatal(err)
	}
	evs = f.pass(t)
	if c := ofType(evs, api.EventIntakeDone); len(c) != 1 || decode[IntakeRequest](t, c[0]).State != api.IntakeProcessed {
		t.Fatalf("intake.processed = %s", types(evs))
	}
	if len(ofType(evs, api.EventIntakeRemoved)) != 0 {
		t.Errorf("a processed request was also removed: %s", types(evs))
	}
	if err := os.Remove(filepath.Join(dir, "add-svg.processed")); err != nil {
		t.Fatal(err)
	}
	if r := ofType(f.pass(t), api.EventIntakeRemoved); len(r) != 1 {
		t.Errorf("intake.removed = %v", r)
	}
}

// A session's start and end; the end's outcome comes from the audit line
// that ended it; audit lines are audit events tied to their project.
func TestEventsSessionsAndAudit(t *testing.T) {
	f := newEventsFixture(t)
	key := taskSessionKey(f.alpha, "ship")
	sess := newStubSession("task-alpha-ship")
	f.d.trackSession(key, sess, agent.TaskAgent, "ship", nil)
	evs := f.pass(t)
	ss := ofType(evs, api.EventSessionStarted)
	if len(ss) != 1 {
		t.Fatalf("session.started = %s", types(evs))
	}
	if s := decode[SessionResource](t, ss[0]); s.ID != "task-alpha-ship" || s.Task != "ship" || s.Project != "p_0.alpha" {
		t.Errorf("session.started = %+v", s)
	}
	// The task gains its session back-link.
	if tu := ofType(evs, api.EventTaskUpdated); len(tu) != 1 || tu[0].Cause != api.CauseSession ||
		strings.Join(decode[TaskUpdatedEvent](t, tu[0]).Changed, ",") != "session" || decode[TaskUpdatedEvent](t, tu[0]).Task.Session != "task-alpha-ship" {
		t.Errorf("task.updated on session start = %v", tu)
	}

	f.d.audit.Log("session_reaped", "key", key, "name", sess.Name(), "kind", "task", "cause", "idle_timeout")
	sess.Close()
	waitUntil(t, "condition", func() bool { return !f.d.hasSession(key) })
	evs = f.pass(t)
	se := ofType(evs, api.EventSessionEnded)
	if len(se) != 1 {
		t.Fatalf("session.ended = %s", types(evs))
	}
	if e := decode[SessionEndedEvent](t, se[0]); e.ID != "task-alpha-ship" || e.Outcome != SessionOutcomeReaped || e.Task != "ship" || e.Kind != "task" {
		t.Errorf("session.ended = %+v", e)
	}
	au := ofType(evs, api.EventAudit)
	var names []string
	for _, e := range au {
		a := decode[APIAuditEvent](t, e)
		names = append(names, a.Event)
		if a.Seq == 0 || e.Project != "p_0.alpha" {
			t.Errorf("audit event %+v project %q", a, e.Project)
		}
	}
	if strings.Join(names, ",") != "session_reaped,session_ended" {
		t.Errorf("audit events = %v", names)
	}
	if len(f.d.events.outcomes) != 0 {
		t.Errorf("outcomes left behind: %v", f.d.events.outcomes)
	}
}

// The state replay a cursorless client gets is the current state as
// created/started events.
func TestEventsReplayState(t *testing.T) {
	f := newEventsFixture(t)
	f.d.trackSession(taskSessionKey(f.alpha, "ship"), newStubSession("task-alpha-ship"), agent.TaskAgent, "ship", nil)
	f.pass(t)
	evs := f.d.events.prev.replay()
	if n := len(ofType(evs, api.EventProjectCreated)); n != 3 {
		t.Errorf("project.created = %d, want alpha, bravo and broken", n)
	}
	if n := len(ofType(evs, api.EventTaskCreated)); n != 2 {
		t.Errorf("task.created = %d", n)
	}
	if n := len(ofType(evs, api.EventSessionStarted)); n != 1 {
		t.Errorf("session.started = %d", n)
	}
	if n := len(ofType(evs, api.EventPRUpdated)); n != 1 {
		t.Errorf("pr.updated = %d", n)
	}
	if evs[0].Type != api.EventProjectCreated {
		t.Errorf("replay starts with %s", evs[0].Type)
	}
}

// The daemon serves GET /events over its socket: a cursorless client gets
// the state, then sees task.status and queue.updated as the files change;
// a reconnect with Last-Event-ID resumes without a gap; an id from another
// daemon process is a reset.
func TestDaemonEventStream(t *testing.T) {
	root := writeFixtureRoot(t)
	wm := apiTestDir(t)
	cfg, err := api.ParseConfig("unix", "", wm)
	if err != nil {
		t.Fatal(err)
	}
	d, err := newDaemon(t, []string{root}, audit.New(&safeBuf{}),
		WithRuntimeInfo(RuntimeInfo{WorkspaceManager: "stub", Headless: true}), WithAPI(cfg))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	waitForSocket(t, cfg.Listen)
	tok, err := api.ReadToken(cfg.TokenPath)
	if err != nil {
		t.Fatal(err)
	}
	c := &api.Client{Addr: cfg.Listen, Token: tok}

	// A first client, from nothing.
	open := func(ctx context.Context, lastID string, q url.Values) chan api.StreamEvent {
		ch := make(chan api.StreamEvent, 256)
		go c.Events(ctx, lastID, q, func(e api.StreamEvent) error { ch <- e; return nil })
		return ch
	}
	sctx, scancel := context.WithCancel(ctx)
	frames := open(sctx, "", url.Values{"project": {"p_0.alpha"}})
	next := func(want string) api.StreamEvent {
		t.Helper()
		timeout := time.After(10 * time.Second)
		for {
			select {
			case e := <-frames:
				if e.Type == want {
					return e
				}
			case <-timeout:
				t.Fatalf("no %s event", want)
			}
		}
	}
	hello := next(api.EventHello)
	if h := decode[api.Hello](t, hello.Event); h.Replay != api.ReplayFull || h.Epoch == "" || hello.ID != "" {
		t.Errorf("hello = %+v id %q", h, hello.ID)
	}
	if pc := next(api.EventProjectCreated); pc.Project != "p_0.alpha" || pc.ID != "" {
		t.Errorf("replayed project.created = %+v", pc)
	}

	// The daemon's dispatch writes nothing for these tasks (it only watches
	// project files for lifecycle reactions), so the files are ours.
	shipPath := filepath.Join(root, "alpha", "tasks", "ship.yaml")
	writeTask := func(status task.Status) {
		t.Helper()
		body := "name: ship\ndescription: ship it\ndepends_on: []\nstatus: " + string(status) + "\nattempts: 0\n"
		if err := os.WriteFile(shipPath, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeTask(task.StatusSuccess)
	st := next(api.EventTaskStatus)
	if ts := decode[TaskStatusEvent](t, st.Event); ts.Name != "ship" || ts.To != "success" || st.ID == "" {
		t.Errorf("task.status = %+v id %q", ts, st.ID)
	}
	q := next(api.EventQueueUpdated)
	if qe := decode[QueueUpdatedEvent](t, q.Event); !strings.Contains(strings.Join(qe.CommitQueue, ","), "ship") {
		t.Errorf("queue.updated = %+v", qe)
	}
	lastID := q.ID
	scancel()

	// While nobody listens, the task moves on.
	lastSeq, err := strconv.ParseUint(strings.TrimPrefix(lastID, d.events.hub.Epoch()+"."), 10, 64)
	if err != nil {
		t.Fatalf("id %q: %v", lastID, err)
	}
	writeTask(task.StatusCommitted)
	waitUntil(t, "condition", func() bool {
		evs, _ := d.events.hub.Since(lastSeq)
		return len(ofType(evs, api.EventTaskStatus)) > 0
	})

	// Resume: the missed task.status arrives, after a delta hello.
	sctx, scancel = context.WithCancel(ctx)
	frames = open(sctx, lastID, url.Values{"types": {"task.status,queue.*"}})
	hello = next(api.EventHello)
	if h := decode[api.Hello](t, hello.Event); h.Replay != api.ReplayDelta {
		t.Errorf("resume hello = %+v", h)
	}
	st = next(api.EventTaskStatus)
	if ts := decode[TaskStatusEvent](t, st.Event); ts.From != "success" || ts.To != "committed" {
		t.Errorf("resumed task.status = %+v", ts)
	}
	if st.Seq <= lastSeq {
		t.Errorf("resumed seq %d not after %d", st.Seq, lastSeq)
	}
	scancel()

	// An id from another daemon process: reset.
	sctx, scancel = context.WithCancel(ctx)
	defer scancel()
	frames = open(sctx, "1.5", nil)
	if h := decode[api.Hello](t, next(api.EventHello).Event); h.Replay != api.ReplayReset {
		t.Errorf("foreign epoch hello = %+v", h)
	}
	if r := decode[api.CursorData](t, next(api.EventReset).Event); r.Cursor == 0 {
		t.Errorf("reset cursor = %d", r.Cursor)
	}

	// The list cursor is the event seq, so a list then events?since= is
	// gap-free.
	var tasks listOf[TaskResource]
	if err := c.Get(ctx, api.Prefix+"/projects/p_0.alpha/tasks", &tasks); err != nil {
		t.Fatal(err)
	}
	if tasks.Cursor == 0 || tasks.Cursor > d.events.hub.Cursor() {
		t.Errorf("list cursor %d, hub %d", tasks.Cursor, d.events.hub.Cursor())
	}
}
