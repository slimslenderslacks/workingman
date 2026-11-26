package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/api"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/mcpgateway"
	"github.com/slimslenderslacks/work/internal/postoffice"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/sbx"
	"github.com/slimslenderslacks/work/internal/scheduler"
	"github.com/slimslenderslacks/work/internal/session"
	"github.com/slimslenderslacks/work/internal/task"
)

// apiFixture is a daemon over writeFixtureRoot plus what the resource
// endpoints read beside it: a richer task file, intake files, a wolf record,
// an ACP session with its stream log, prompt files, gateway and harness
// records, a post-office message and a scripted `sbx ls`.
type apiFixture struct {
	d        *Daemon
	h        http.Handler
	tok      string
	root     string
	alpha    string // alpha's project.yaml
	sessRoot string
	worktree string
	sbxCalls *atomic.Int32
	sbxFail  *atomic.Bool
}

const alphaID = "p_0.alpha"

func newAPIFixture(t *testing.T) *apiFixture {
	t.Helper()
	root := writeFixtureRoot(t)
	alpha := filepath.Join(root, "alpha", "project.yaml")
	alphaDir := filepath.Dir(alpha)
	base := t.TempDir()
	sessRoot := filepath.Join(base, "sessions")
	worktree := filepath.Join(base, "wt", "build")
	mk := func(p, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// build: the failed task, now with workload fields, an integration
	// block and a PR source.
	mk(filepath.Join(alphaDir, "tasks", "build.yaml"), `name: build
description: build it, token=`+secretToken+`
depends_on: []
status: failed
attempts: 3
failure_reason: "clone failed token=`+secretToken+`"
model: default
resolved_model: claude-opus-5-5
resolved_harness: img:1
static_mcps: [github]
policies:
  - action: allow
    type: network
    resource: api.github.com
touches: [widgets/**]
source: {kind: pr-comment, ref: PRRT_1, repo: acme/widgets, pr: 7}
summary: did things
commits: [{repo: widgets, hash: abc123}]
integration:
  result: verify_failed
  repo: widgets
  output: "go test failed password=hunter2"
  onto: [{repo: widgets, hash: def456}]
worktree: `+worktree+`
task_branch: wm/alpha-branch/build
`)
	mk(filepath.Join(alphaDir, "tasks", "zz-seed.yaml"), "name: \"\"\ndescription: please add a thing\nstatus: ready\n")
	mk(filepath.Join(alphaDir, "intake", "add-svg.md"), "Please add SVG wireframes\n")
	mk(filepath.Join(alphaDir, "intake", "blank.md"), "  \n")
	mk(filepath.Join(alphaDir, "intake", "old.processed"), "done already\n")
	mk(filepath.Join(alphaDir, "intake", "notes.txt"), "ignored")
	mk(project.BlockedSessionPath(alpha), "blocked_reason: was stuck\nsummary: fix the clone\nattempted: [retry]\nupdated_by: agent\n")

	// The ACP session of build's task agent.
	store := session.Store{Root: sessRoot}
	const sid = "task-alpha-build"
	if err := store.Write(session.Session{
		ID: sid, SandboxName: "alpha-build", Status: session.StatusRunning, CreatedAt: time.Now(),
		ProjectPath: alpha, TaskPath: filepath.Join(alphaDir, "tasks", "build.yaml"), Kind: "task",
		Workspaces: []string{worktree}, Kit: "/kits/acp", PromptCount: 1, SigningBroken: true,
	}); err != nil {
		t.Fatal(err)
	}
	mk(filepath.Join(store.Dir(sid), "stream.log"),
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1"}}`+"\n"+
			`{"jsonrpc":"2.0","method":"note","params":{"text":"token=`+secretToken+`","token":"`+secretToken+`","n":12345678901234567890}}`+"\n"+
			`{"partial":`)
	mk(filepath.Join(worktree, ".orch", "instructions.md"), "You are a task agent.\n")
	mk(filepath.Join(worktree, ".orch", "context.yaml"), "kind: task\n")
	if err := (mcpgateway.Store{Dir: mcpgateway.StateDir(sessRoot)}).Save(mcpgateway.Record{
		Sandbox: "alpha-build", ID: "gw-secret-id", URL: "http://gw/secret-path", SandboxURL: "http://sb/secret-path",
		Requested: []string{"github"}, Servers: []string{"github"}, Dynamic: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	harness := filepath.Join(base, "sandboxes")
	mk(filepath.Join(harness, "alpha-build.json"), `{"sandbox":"alpha-build","kit":"/kits/acp","image":"img:1","mcp_servers":["github"],"mcp_dynamic":true,"created_at":"2026-11-25T09:00:00Z"}`)
	mk(filepath.Join(harness, "alpha-ship.json"), `{"sandbox":"alpha-ship","kit":"/kits/acp","image":"img:1","created_at":"2026-11-25T08:00:00Z"}`)

	// Post-office: one open request for the operator, one for an agent, and
	// one answered request.
	po, err := postoffice.Open(postoffice.DefaultDir(sessRoot))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	msg := func(subject, recipient string) postoffice.Message {
		return postoffice.Message{
			Version: postoffice.Version, ID: postoffice.NewID(now), Kind: postoffice.KindRequest,
			From:      postoffice.Sender{Agent: "step-up", Sandbox: "step-up"},
			Recipient: recipient, Subject: subject, Body: "allow it? token=" + secretToken,
			Options: []string{"approve", "deny"}, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		}
	}
	open, other, answered := msg("open one", postoffice.RecipientOperator), msg("for an agent", "workingman"), msg("answered one", postoffice.RecipientOperator)
	for _, m := range []postoffice.Message{open, other, answered} {
		if err := po.Put(m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := po.Claim(postoffice.Resolution{ID: answered.ID, Outcome: postoffice.OutcomeAnswered, Response: "approve",
		Responder: &postoffice.Responder{Kind: postoffice.ResponderOperator, Via: "tui"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := po.Finalize(answered.ID); err != nil {
		t.Fatal(err)
	}

	r := &runner.Runner{AcpLauncher: stubLauncher{}, SessionsRoot: sessRoot}
	d, err := newDaemon(t, []string{root}, audit.New(&safeBuf{}), WithRunner(r))
	if err != nil {
		t.Fatal(err)
	}
	f := &apiFixture{d: d, root: root, alpha: alpha, sessRoot: sessRoot, worktree: worktree, sbxCalls: &atomic.Int32{}, sbxFail: &atomic.Bool{}}
	d.sbxList = &sbx.ListCache{Client: sbx.Client{Attempts: 1, Run: func(context.Context, string, ...string) ([]byte, []byte, error) {
		f.sbxCalls.Add(1)
		if f.sbxFail.Load() {
			return nil, nil, errors.New("sbx: daemon not running")
		}
		return []byte(`[{"name":"alpha-build","workspaces":["` + worktree + `"]},{"name":"alpha-ship"},{"name":"someone-elses"}]`), nil, nil
	}}}
	d.trackConfiguredSession(taskSessionKey(alpha, "build"), newStubSession(sid), agent.TaskAgent, "build",
		runner.SessionConfig{Harness: "img:1", Model: "default", Mounts: []string{worktree}, MCPs: []string{"github"}, MCPDynamic: true}, nil)
	d.audit.Log("task_running", "path", filepath.Join(alphaDir, "tasks", "build.yaml"), "task", "build")
	d.audit.Log("project_blocked", "path", filepath.Join(root, "bravo", "project.yaml"), "reason", "password=hunter2")
	d.audit.Log("watch_root", "path", root)

	tokPath := filepath.Join(t.TempDir(), "token")
	tok, _, err := api.EnsureToken(tokPath, false)
	if err != nil {
		t.Fatal(err)
	}
	d.events = newEventPublisher(d)
	d.events.prime(context.Background())
	f.h = api.New(api.Config{Mode: api.ModeUnix, TokenPath: tokPath}, apiSource{d}).Handler("")
	f.tok = tok
	return f
}

// get requests path and decodes a 200 body into v (when non-nil).
func (f *apiFixture) get(t *testing.T, path string, v any) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Host = "localhost"
	req.Header.Set("Authorization", "Bearer "+f.tok)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), secretToken) || strings.Contains(rec.Body.String(), "hunter2") {
		t.Errorf("GET %s leaks a secret: %s", path, rec.Body)
	}
	if v != nil {
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
			t.Fatalf("GET %s: %v in %s", path, err, rec.Body)
		}
	}
	return rec
}

type listOf[T any] struct {
	Items       []T       `json:"items"`
	GeneratedAt time.Time `json:"generated_at"`
	Cursor      uint64    `json:"cursor"`
}

func TestAPIProjects(t *testing.T) {
	f := newAPIFixture(t)
	var l listOf[ProjectSummary]
	f.get(t, api.Prefix+"/projects", &l)
	if l.Cursor != f.d.events.hub.Cursor() || l.GeneratedAt.IsZero() {
		t.Errorf("envelope cursor=%d (event seq %d) generated_at=%v", l.Cursor, f.d.events.hub.Cursor(), l.GeneratedAt)
	}
	if len(l.Items) != 3 {
		t.Fatalf("projects = %d, want alpha, bravo and broken", len(l.Items))
	}
	a := l.Items[0]
	if a.ID != alphaID || a.WorkStream != "alpha" || a.Root != f.root || a.Path != f.alpha || a.Status != "working" {
		t.Errorf("alpha summary = %+v", a)
	}
	if !strings.HasPrefix(a.ETag, `"pj-`) || !a.Flags.Review || a.CronMaxRuns != 5 || a.UpdatedBy != "agent" {
		t.Errorf("alpha etag/flags/cron = %q %+v %d %q", a.ETag, a.Flags, a.CronMaxRuns, a.UpdatedBy)
	}
	if len(a.PullReqs) != 1 || a.PullReqs[0].HeadSHA != "deadbeef" || a.PullReqs[0].Fingerprint != nil {
		t.Errorf("alpha PRs = %+v", a.PullReqs)
	}
	if a.Live == nil || len(a.Live.RunningTasks) != 1 || a.Live.RunningTasks[0] != "build" {
		t.Errorf("alpha live = %+v", a.Live)
	}
	if a.Links["tasks"] != api.Prefix+"/projects/p_0.alpha/tasks" {
		t.Errorf("links = %v", a.Links)
	}
	if b := l.Items[2]; b.WorkStream != "broken" || b.LoadError == "" {
		t.Errorf("broken = %+v", b)
	}

	for q, want := range map[string]string{
		"?status=blocked":              "bravo",
		"?status=working,idle":         "alpha",
		"?q=STUCK":                     "bravo",
		"?q=thing&status=working":      "alpha",
		"?root=0&status=blocked":       "bravo",
		"?hidden=true":                 "",
		"?root=1":                      "",
		"?status=working&hidden=any":   "alpha",
		"?q=nope-not-in-any-name-text": "",
	} {
		var l listOf[ProjectSummary]
		f.get(t, api.Prefix+"/projects"+q, &l)
		var got []string
		for _, p := range l.Items {
			got = append(got, p.WorkStream)
		}
		if strings.Join(got, ",") != want {
			t.Errorf("%s = %v, want %q", q, got, want)
		}
	}
	for _, q := range []string{"?hidden=maybe", "?root=x"} {
		if rec := f.get(t, api.Prefix+"/projects"+q, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", q, rec.Code)
		}
	}
}

func TestAPIProjectDetail(t *testing.T) {
	f := newAPIFixture(t)
	var p ProjectDetail
	f.get(t, api.Prefix+"/projects/"+alphaID+"?include=fingerprint", &p)
	if p.Description != "build the thing" || len(p.Repos) != 1 || p.Repos[0].Org != "acme" || p.NewRepos == nil {
		t.Errorf("detail = %+v", p)
	}
	if p.BlockedSession == nil || p.BlockedSession.Summary != "fix the clone" || len(p.BlockedSession.Attempted) != 1 {
		t.Errorf("blocked_session = %+v", p.BlockedSession)
	}
	if strings.Join(p.PendingIntake, ",") != "add-svg.md" {
		t.Errorf("pending_intake = %v", p.PendingIntake)
	}
	if rec := f.get(t, api.Prefix+"/projects/p_0.nope", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown project: %d", rec.Code)
	}
	if rec := f.get(t, api.Prefix+"/projects/"+alphaID+"?include=everything", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad include: %d", rec.Code)
	}

	// The etag is the file's: it changes with project.yaml, and a
	// revalidation with the old response etag gets a fresh body.
	rec := f.get(t, api.Prefix+"/projects/"+alphaID, nil)
	before := p.ETag
	if err := os.WriteFile(f.alpha, []byte("description: changed\nbranch: b\nstatus: idle\nupdated_by: agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var after ProjectDetail
	f.get(t, api.Prefix+"/projects/"+alphaID, &after)
	if after.ETag == before || after.Status != "idle" || after.Description != "changed" {
		t.Errorf("after edit: etag %s -> %s, status %q", before, after.ETag, after.Status)
	}
	req := httptest.NewRequest("GET", api.Prefix+"/projects/"+alphaID, nil)
	req.Host, req.Header = "localhost", http.Header{"Authorization": {"Bearer " + f.tok}, "If-None-Match": {rec.Header().Get("ETag")}}
	rr := httptest.NewRecorder()
	f.h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("stale If-None-Match after an edit: %d, want 200", rr.Code)
	}
}

func TestAPITasks(t *testing.T) {
	f := newAPIFixture(t)
	var l listOf[TaskResource]
	f.get(t, api.Prefix+"/projects/"+alphaID+"/tasks", &l)
	if len(l.Items) != 3 {
		t.Fatalf("tasks = %+v", l.Items)
	}
	b := l.Items[0]
	if b.Name != "build" || b.Project != alphaID || !strings.HasPrefix(b.ETag, `"tk-`) || !strings.Contains(b.Description, "build it") {
		t.Errorf("build = %+v", b)
	}
	c := b.Capabilities
	if c.Model != "default" || c.ResolvedModel != "claude-opus-5-5" || c.Harness != "claude-acp" || c.ResolvedHarness != "img:1" ||
		strings.Join(c.StaticMCPs, ",") != "github" || len(c.Policies) != 1 || c.Policies[0].Type != "network" || c.Policies[0].Resource != "api.github.com" {
		t.Errorf("capabilities = %+v", c)
	}
	if b.Source == nil || b.Source.PR != 7 || b.Exclusive || b.Touches == nil {
		t.Errorf("source/hints = %+v exclusive=%v touches=%v", b.Source, b.Exclusive, b.Touches)
	}
	if b.Integration == nil || b.Integration.Result != "verify_failed" || len(b.Integration.Onto) != 1 || !strings.Contains(b.Integration.Output, "go test failed") {
		t.Errorf("integration = %+v", b.Integration)
	}
	if b.Output.Summary != "did things" || len(b.Output.Commits) != 1 || b.Output.Commits[0].Hash != "abc123" {
		t.Errorf("output = %+v", b.Output)
	}
	if b.Session != "task-alpha-build" || b.Sandbox != "alpha-build" || b.CommitSandbox != "alpha-build-commit" {
		t.Errorf("back-links = %q %q %q", b.Session, b.Sandbox, b.CommitSandbox)
	}
	if s := l.Items[1]; s.Name != "ship" || !s.Exclusive || s.Sandbox != "alpha-ship" || s.Session != "" || s.Capabilities.StaticMCPs == nil {
		t.Errorf("ship = %+v", s)
	}
	if seed := l.Items[2]; seed.Name != "zz-seed" || !seed.Seed || seed.Sandbox != "" || seed.CommitSandbox != "" {
		t.Errorf("seed = %+v", seed)
	}

	var ready listOf[TaskResource]
	f.get(t, api.Prefix+"/projects/"+alphaID+"/tasks?status=failed", &ready)
	if len(ready.Items) != 1 || ready.Items[0].Name != "build" {
		t.Errorf("status=failed = %+v", ready.Items)
	}
	var one TaskResource
	f.get(t, api.Prefix+"/projects/"+alphaID+"/tasks/ship", &one)
	if one.Name != "ship" || strings.Join(one.DependsOn, ",") != "build" {
		t.Errorf("one task = %+v", one)
	}
	if rec := f.get(t, api.Prefix+"/projects/"+alphaID+"/tasks/nope", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown task: %d", rec.Code)
	}
}

// The task files are parsed once and re-read only when they change.
func TestAPITaskFileCache(t *testing.T) {
	f := newAPIFixture(t)
	path := filepath.Join(filepath.Dir(f.alpha), "tasks", "ship.yaml")
	first, tag1, err := f.d.apiFiles.tasks.get(path)
	if err != nil {
		t.Fatal(err)
	}
	again, tag2, _ := f.d.apiFiles.tasks.get(path)
	if first != again || tag1 != tag2 {
		t.Error("an unchanged task file was parsed again")
	}
	if err := os.WriteFile(path, []byte("name: ship\ndescription: ship it now, longer\nstatus: running\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, tag3, _ := f.d.apiFiles.tasks.get(path)
	if changed == first || tag3 == tag1 || changed.Description != "ship it now, longer" {
		t.Errorf("changed file served from the cache: %+v", changed)
	}
	os.Remove(path)
	if _, _, err := f.d.apiFiles.tasks.get(path); !os.IsNotExist(err) {
		t.Errorf("removed file: %v", err)
	}
}

func TestAPIGraph(t *testing.T) {
	f := newAPIFixture(t)
	var g Graph
	f.get(t, api.Prefix+"/projects/"+alphaID+"/graph", &g)
	if len(g.Nodes) != 3 || len(g.Edges) != 1 || g.Edges[0] != (GraphEdge{From: "build", To: "ship"}) {
		t.Errorf("nodes=%+v edges=%+v", g.Nodes, g.Edges)
	}
	// build failed, so ship is not ready; nothing runs.
	if len(g.Ready) != 0 || len(g.Runnable) != 0 || len(g.Errors) != 0 || g.Project != alphaID {
		t.Errorf("ready=%v runnable=%v errors=%v", g.Ready, g.Runnable, g.Errors)
	}
	if g.MaxAttempts != task.MaxAttempts {
		t.Errorf("max_attempts = %d, want %d", g.MaxAttempts, task.MaxAttempts)
	}

	tasks := filepath.Join(filepath.Dir(f.alpha), "tasks")
	if err := os.WriteFile(filepath.Join(tasks, "build.yaml"), []byte("name: build\nstatus: committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.get(t, api.Prefix+"/projects/"+alphaID+"/graph", &g)
	if strings.Join(g.Ready, ",") != "ship" || strings.Join(g.Runnable, ",") != "ship" {
		t.Errorf("after build committed: ready=%v runnable=%v", g.Ready, g.Runnable)
	}
	if err := os.WriteFile(filepath.Join(tasks, "loop.yaml"), []byte("name: loop\nstatus: ready\ndepends_on: [nothing-here]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.get(t, api.Prefix+"/projects/"+alphaID+"/graph", &g)
	if len(g.Errors) != 1 || len(g.Nodes) != 4 || len(g.Ready) != 0 {
		t.Errorf("broken graph: errors=%v nodes=%d ready=%v", g.Errors, len(g.Nodes), g.Ready)
	}
}

func TestAPIIntake(t *testing.T) {
	f := newAPIFixture(t)
	var l listOf[IntakeRequest]
	f.get(t, api.Prefix+"/projects/"+alphaID+"/intake", &l)
	var got []string
	for _, r := range l.Items {
		got = append(got, r.Name+":"+r.State)
		if r.Body != nil || !strings.HasPrefix(r.ETag, `"in-`) || r.CreatedAt.IsZero() {
			t.Errorf("list item %+v", r)
		}
	}
	if strings.Join(got, ",") != "add-svg:pending,blank:empty,old:processed" {
		t.Errorf("intake = %v", got)
	}
	f.get(t, api.Prefix+"/projects/"+alphaID+"/intake?state=pending", &l)
	if len(l.Items) != 1 || l.Items[0].Name != "add-svg" {
		t.Errorf("state=pending = %+v", l.Items)
	}
	var one IntakeRequest
	f.get(t, api.Prefix+"/projects/"+alphaID+"/intake/add-svg", &one)
	if one.Body == nil || *one.Body != "Please add SVG wireframes\n" || one.File != "add-svg.md" {
		t.Errorf("one = %+v", one)
	}
	for _, name := range []string{"nope", "%2E%2E", "a%2Fb", "notes"} {
		if rec := f.get(t, api.Prefix+"/projects/"+alphaID+"/intake/"+name, nil); rec.Code != http.StatusNotFound {
			t.Errorf("intake %q: %d, want 404", name, rec.Code)
		}
	}
	if rec := f.get(t, api.Prefix+"/projects/"+alphaID+"/intake?state=weird", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad state: %d", rec.Code)
	}
}

func TestAPIPulls(t *testing.T) {
	f := newAPIFixture(t)
	var l listOf[PullRequest]
	f.get(t, api.Prefix+"/projects/"+alphaID+"/pulls", &l)
	if len(l.Items) != 1 {
		t.Fatalf("pulls = %+v", l.Items)
	}
	pr := l.Items[0]
	if pr.Number != 7 || pr.HeadSHA != "deadbeef" || pr.Watching || strings.Join(pr.FixTasks, ",") != "build" {
		t.Errorf("pr = %+v", pr)
	}
	// No poll armed: no next poll, but the ladder and budgets are there.
	if pr.ReviewPoll != nil || pr.NextPollAt != nil {
		t.Errorf("review_poll = %+v, next_poll_at = %v", pr.ReviewPoll, pr.NextPollAt)
	}
	if strings.Join(pr.PollLadder, ",") != strings.Join(reviewPollSpecs, ",") {
		t.Errorf("poll_ladder = %v", pr.PollLadder)
	}
	if pr.Budgets != (ReviewBudgets{MaxReviewFixCycles: maxReviewFixCycles, MaxReviewErrors: maxReviewErrors}) {
		t.Errorf("budgets = %+v", pr.Budgets)
	}

	// An armed poll on a started scheduler: the rung, the next poll time
	// and the counters.
	alpha := f.alpha
	f.d.scheduler = scheduler.New()
	f.d.scheduler.Start()
	t.Cleanup(func() { _ = f.d.scheduler.Stop(context.Background()) })
	if err := f.d.scheduler.Register(reviewPollKey(alpha), reviewPollSpecs[2], func() {}); err != nil {
		t.Fatal(err)
	}
	f.d.reviewMu.Lock()
	f.d.reviewBackoff[alpha] = 2
	f.d.reviewFixCycles[alpha] = 1
	f.d.reviewMu.Unlock()
	f.get(t, api.Prefix+"/projects/"+alphaID+"/pulls", &l)
	pr = l.Items[0]
	if pr.ReviewPoll == nil || pr.ReviewPoll.BackoffIndex != 2 || pr.ReviewPoll.Schedule != "@every 8m" {
		t.Errorf("review_poll = %+v", pr.ReviewPoll)
	}
	if pr.NextPollAt == nil || time.Until(*pr.NextPollAt) > 8*time.Minute || time.Until(*pr.NextPollAt) < 7*time.Minute {
		t.Errorf("next_poll_at = %v", pr.NextPollAt)
	}
	if pr.Budgets.ReviewFixCycles != 1 || pr.Budgets.ReviewErrors != 0 {
		t.Errorf("budgets = %+v", pr.Budgets)
	}
}

func TestAPIAudit(t *testing.T) {
	f := newAPIFixture(t)
	var l listOf[APIAuditEvent]
	f.get(t, api.Prefix+"/audit", &l)
	if len(l.Items) < 3 {
		t.Fatalf("audit = %+v", l.Items)
	}
	last := l.Items[len(l.Items)-1]
	if last.Event != "watch_root" || last.Seq != f.d.audit.LastSeq() || l.Cursor != f.d.events.hub.Cursor() {
		t.Errorf("last = %+v cursor=%d", last, l.Cursor)
	}
	for i := 1; i < len(l.Items); i++ {
		if l.Items[i].Seq <= l.Items[i-1].Seq {
			t.Errorf("seqs not increasing: %d then %d", l.Items[i-1].Seq, l.Items[i].Seq)
		}
	}

	var proj listOf[APIAuditEvent]
	f.get(t, api.Prefix+"/projects/"+alphaID+"/audit", &proj)
	for _, e := range proj.Items {
		if !auditInProject(e.Fields, filepath.Dir(f.alpha)) || e.Project != alphaID {
			t.Errorf("project audit has %+v", e)
		}
	}
	if len(proj.Items) == 0 {
		t.Error("no events for alpha (task_running names its task file)")
	}
	var byName listOf[APIAuditEvent]
	f.get(t, api.Prefix+"/audit?event=project_blocked&project=p_0.bravo", &byName)
	if len(byName.Items) != 1 || byName.Items[0].Fields["reason"] != "password="+audit.Redacted || byName.Items[0].Project != "p_0.bravo" {
		t.Errorf("event filter = %+v", byName.Items)
	}

	// since/limit page forward from a cursor.
	seq := l.Items[len(l.Items)-3].Seq
	var page listOf[APIAuditEvent]
	f.get(t, api.Prefix+"/audit?since="+strconv.FormatUint(seq, 10)+"&limit=1", &page)
	if len(page.Items) != 1 || page.Items[0].Seq != seq+1 {
		t.Errorf("since=%d limit=1 = %+v", seq, page.Items)
	}
	var tail listOf[APIAuditEvent]
	f.get(t, api.Prefix+"/audit?limit=2", &tail)
	if len(tail.Items) != 2 || tail.Items[1].Seq != f.d.audit.LastSeq() {
		t.Errorf("limit=2 = %+v", tail.Items)
	}
	for _, q := range []string{"?limit=0", "?limit=501", "?since=-1"} {
		if rec := f.get(t, api.Prefix+"/audit"+q, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", q, rec.Code)
		}
	}
}

func TestAPISessions(t *testing.T) {
	f := newAPIFixture(t)
	var l listOf[SessionResource]
	f.get(t, api.Prefix+"/sessions", &l)
	if len(l.Items) != 1 {
		t.Fatalf("sessions = %+v", l.Items)
	}
	s := l.Items[0]
	if s.ID != "task-alpha-build" || s.Kind != "task" || s.Project != alphaID || s.Task != "build" ||
		s.TaskPath != filepath.Join(filepath.Dir(f.alpha), "tasks", "build.yaml") || s.PromptCount != 1 || s.Harness != "img:1" {
		t.Errorf("session = %+v", s)
	}
	sb := s.Sandbox
	if sb == nil || sb.Name != "alpha-build" || sb.Live == nil || !*sb.Live || sb.Image != "img:1" || sb.Kit != "/kits/acp" ||
		!sb.SigningBroken || strings.Join(sb.Workspaces, ",") != f.worktree || sb.CreatedAt == nil {
		t.Errorf("sandbox = %+v", sb)
	}
	if g := s.Gateway; g == nil || !g.Dynamic || strings.Join(g.Servers, ",") != "github" {
		t.Errorf("gateway = %+v", g)
	}
	if s.ACP == nil || s.ACP.Status != "running" || s.ACP.Attachable {
		t.Errorf("acp = %+v (no socket file, so not attachable)", s.ACP)
	}
	rec := f.get(t, api.Prefix+"/sessions", nil)
	for _, leak := range []string{"secret-path", "gw-secret-id", "agent.sock", "socket_path", "session_dir"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("sessions body has %q: %s", leak, rec.Body)
		}
	}

	for q, n := range map[string]int{"?project=" + alphaID: 1, "?project=p_0.bravo": 0, "?kind=commit,wolf": 0, "?kind=task": 1, "?live=true": 1, "?live=false": 0} {
		var l listOf[SessionResource]
		f.get(t, api.Prefix+"/sessions"+q, &l)
		if len(l.Items) != n {
			t.Errorf("%s: %d sessions, want %d", q, len(l.Items), n)
		}
	}
	var one SessionResource
	f.get(t, api.Prefix+"/sessions/task-alpha-build", &one)
	if one.ID != s.ID {
		t.Errorf("one = %+v", one)
	}
	if rec := f.get(t, api.Prefix+"/sessions/nope", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown session: %d", rec.Code)
	}

	// sbx is cached: the requests above ran it once. When it fails, sandbox
	// liveness is unknown (null), not false.
	if n := f.sbxCalls.Load(); n != 1 {
		t.Errorf("sbx ls ran %d times, want 1 (cached)", n)
	}
	f.sbxFail.Store(true)
	f.d.sbxList.Invalidate()
	f.get(t, api.Prefix+"/sessions", &l)
	if l.Items[0].Sandbox.Live != nil {
		t.Errorf("live with sbx failing = %v, want null", *l.Items[0].Sandbox.Live)
	}
}

func TestAPITranscriptAndPrompt(t *testing.T) {
	f := newAPIFixture(t)
	rec := f.get(t, api.Prefix+"/sessions/task-alpha-build/transcript", nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("transcript: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	lines := strings.Split(strings.TrimSuffix(rec.Body.String(), "\n"), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], audit.Redacted) || strings.Contains(rec.Body.String(), "partial") {
		t.Errorf("frames = %q (the partial last line must wait)", lines)
	}
	for _, ln := range lines {
		if !json.Valid([]byte(ln)) {
			t.Errorf("redacted frame is not JSON: %s", ln)
		}
	}
	if !strings.Contains(lines[1], `"token":"`+audit.Redacted+`"`) || !strings.Contains(lines[1], "12345678901234567890") {
		t.Errorf("frame = %s, want the token field redacted and the number kept exactly", lines[1])
	}
	next, _ := strconv.ParseInt(rec.Header().Get(api.HeaderTranscriptNext), 10, 64)
	size, _ := strconv.ParseInt(rec.Header().Get(api.HeaderTranscriptSize), 10, 64)
	if next <= 0 || next >= size {
		t.Errorf("next=%d size=%d", next, size)
	}
	page := f.get(t, api.Prefix+"/sessions/task-alpha-build/transcript?limit=1", nil)
	if strings.Count(page.Body.String(), "\n") != 1 {
		t.Errorf("limit=1: %q", page.Body)
	}
	rest := f.get(t, api.Prefix+"/sessions/task-alpha-build/transcript?after="+page.Header().Get(api.HeaderTranscriptNext), nil)
	if strings.Count(rest.Body.String(), "\n") != 1 || !strings.Contains(rest.Body.String(), "note") {
		t.Errorf("after: %q", rest.Body)
	}
	if rec := f.get(t, api.Prefix+"/sessions/nope/transcript", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown session transcript: %d", rec.Code)
	}
	if rec := f.get(t, api.Prefix+"/sessions/task-alpha-build/transcript?after=-3", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad after: %d", rec.Code)
	}

	var p PromptFiles
	f.get(t, api.Prefix+"/sessions/task-alpha-build/prompt", &p)
	if p.InstructionsMD != "You are a task agent.\n" || p.ContextYAML != "kind: task\n" || p.Dir != f.worktree {
		t.Errorf("prompt = %+v", p)
	}
}

func TestAPISandboxes(t *testing.T) {
	f := newAPIFixture(t)
	var l listOf[SandboxResource]
	f.get(t, api.Prefix+"/sandboxes", &l)
	if len(l.Items) != 3 {
		t.Fatalf("sandboxes = %+v", l.Items)
	}
	b, s, o := l.Items[0], l.Items[1], l.Items[2]
	if b.Name != "alpha-build" || b.Session != "task-alpha-build" || b.Kind != "task" || !b.Managed || b.Retained || b.Gateway == nil || b.Image != "img:1" {
		t.Errorf("alpha-build = %+v", b)
	}
	if b.Project != alphaID || b.Task != "build" || b.Role != "task" || b.TaskStatus != "failed" {
		t.Errorf("alpha-build owner = %q %q %q %q", b.Project, b.Task, b.Role, b.TaskStatus)
	}
	if s.Name != "alpha-ship" || !s.Managed || !s.Retained || s.Session != "" {
		t.Errorf("alpha-ship = %+v", s)
	}
	// A retained sandbox maps back to its task by name (§G.2).
	if s.Project != alphaID || s.Task != "ship" || s.Role != "task" || s.TaskStatus == "" {
		t.Errorf("alpha-ship owner = %q %q %q %q", s.Project, s.Task, s.Role, s.TaskStatus)
	}
	if o.Name != "someone-elses" || o.Managed || o.Retained || o.Project != "" || o.Role != "" {
		t.Errorf("someone-elses = %+v", o)
	}
	f.get(t, api.Prefix+"/sandboxes?retained=true", &l)
	if len(l.Items) != 1 || l.Items[0].Name != "alpha-ship" {
		t.Errorf("retained = %+v", l.Items)
	}
	f.sbxFail.Store(true)
	f.d.sbxList.Invalidate()
	if rec := f.get(t, api.Prefix+"/sandboxes", nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("sbx failing: %d, want 503", rec.Code)
	}
}

func TestAPIMessages(t *testing.T) {
	f := newAPIFixture(t)
	var l listOf[MessageResource]
	f.get(t, api.Prefix+"/messages", &l)
	if len(l.Items) != 1 || l.Items[0].Subject != "open one" || l.Items[0].State != "open" || len(l.Items[0].Options) != 2 {
		t.Fatalf("open = %+v", l.Items)
	}
	f.get(t, api.Prefix+"/messages?state=closed", &l)
	if len(l.Items) != 1 || l.Items[0].Subject != "answered one" || l.Items[0].Resolution == nil ||
		l.Items[0].Resolution.Response != "approve" || l.Items[0].Resolution.ClaimID != "" {
		t.Fatalf("closed = %+v", l.Items)
	}
	f.get(t, api.Prefix+"/messages?state=all", &l)
	if len(l.Items) != 2 {
		t.Errorf("all = %+v", l.Items)
	}
	f.get(t, api.Prefix+"/messages?state=closed&since="+time.Now().Add(time.Hour).UTC().Format(time.RFC3339), &l)
	if len(l.Items) != 0 {
		t.Errorf("since the future = %+v", l.Items)
	}
	if rec := f.get(t, api.Prefix+"/messages?since=yesterday", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad since: %d", rec.Code)
	}
}

func TestAPIAgents(t *testing.T) {
	f := newAPIFixture(t)
	var l listOf[AgentResource]
	f.get(t, api.Prefix+"/agents", &l)
	var kinds []string
	for _, a := range l.Items {
		kinds = append(kinds, a.Kind)
	}
	if strings.Join(kinds, ",") != "workingman,sleepy-head,step-up" {
		t.Errorf("agents = %v", kinds)
	}
	if l.Items[0].Enabled || l.Items[2].Enabled || !l.Items[1].Enabled {
		t.Errorf("enabled = %+v (only sleepy-head runs without explicit options)", l.Items)
	}
}

// The resource endpoints carry ETags and answer a matching If-None-Match
// with 304, like the first slice's endpoints.
func TestAPIResourceETags(t *testing.T) {
	f := newAPIFixture(t)
	for _, path := range []string{"/projects", "/projects/" + alphaID + "/tasks", "/sessions/task-alpha-build/transcript"} {
		rec := f.get(t, api.Prefix+path, nil)
		etag := rec.Header().Get("ETag")
		req := httptest.NewRequest("GET", api.Prefix+path, nil)
		req.Host, req.Header = "localhost", http.Header{"Authorization": {"Bearer " + f.tok}, "If-None-Match": {etag}}
		rr := httptest.NewRecorder()
		f.h.ServeHTTP(rr, req)
		if etag == "" || rr.Code != http.StatusNotModified {
			t.Errorf("%s: etag %q, revalidation %d, want 304", path, etag, rr.Code)
		}
	}
}
