package daemon

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/api"
	"github.com/slimslenderslacks/work/internal/control"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/task"
	"github.com/slimslenderslacks/work/internal/workspace"
)

// taskETag reads a task's etag through the API, as the GUI does.
func (f *apiFixture) taskETag(t *testing.T, id, name string) string {
	t.Helper()
	var r TaskResource
	f.get(t, api.Prefix+"/projects/"+id+"/tasks/"+name, &r)
	if r.ETag == "" {
		t.Fatalf("%s/%s has no etag", id, name)
	}
	return r.ETag
}

func taskResult(t *testing.T, body []byte) TaskActionResult {
	t.Helper()
	var res TaskActionResult
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return res
}

// gammaBlocked writes a project blocked by its task "build" alone, with a
// failed build and a ship that depends on it.
func gammaBlocked(t *testing.T, f *apiFixture, reason string) (projectPath, buildPath string) {
	t.Helper()
	projectPath = writeProject(t, f.root, "gamma", "description: d\nbranch: gamma\nstatus: blocked\nblocked_reason: '"+reason+"'\nupdated_by: daemon\n")
	dir := filepath.Join(filepath.Dir(projectPath), "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	buildPath = filepath.Join(dir, "build.yaml")
	for p, body := range map[string]string{
		buildPath:                         "name: build\ndescription: build it\nstatus: failed\nattempts: 3\nfailure_reason: go test failed\n",
		filepath.Join(dir, "ship.yaml"):   "name: ship\ndescription: ship it\ndepends_on: [build]\nstatus: ready\n",
		filepath.Join(dir, "docs.yaml"):   "name: docs\ndescription: write docs\nstatus: committed\n",
		filepath.Join(dir, "runner.yaml"): "name: runner\ndescription: running\nstatus: running\n",
	} {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return projectPath, buildPath
}

// Retry R1 (§A.4.4): the task back to ready with a fresh budget, and the
// project unblocked when this task alone blocked it.
func TestAPIRetryUnblocksProject(t *testing.T) {
	f := newAPIFixture(t)
	h := f.writeServer(t)
	gamma, build := gammaBlocked(t, f, `task "build" failed after 3 attempts: go test failed`)
	etag := f.taskETag(t, "p_0.gamma", "build")
	rec := send(t, h, f.tok, "POST", api.Prefix+"/projects/p_0.gamma/tasks/build/actions/retry", `{"reset_attempts":true,"note":"flaky runner"}`, map[string]string{"If-Match": etag})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("retry: %d %s", rec.Code, rec.Body)
	}
	res := taskResult(t, rec.Body.Bytes())
	if res.Dispatch != control.DispatchUnblocked || res.ProjectWrite == nil || res.ProjectWrite.Path != gamma || res.Write.Path != build {
		t.Errorf("result = %+v", res)
	}
	if res.Task.Status != "ready" || res.Task.Attempts != 0 || res.Task.ETag == etag || rec.Header().Get("ETag") != res.Task.ETag {
		t.Errorf("task = %+v, ETag %q", res.Task, rec.Header().Get("ETag"))
	}
	tk, _ := task.Load(build)
	if tk.Status != task.StatusReady || tk.Attempts != 0 || tk.FailureReason != "" {
		t.Errorf("build = %+v", tk)
	}
	if p, _ := project.Load(gamma); p.Status != project.StatusWorking || p.BlockedReason != "" || p.UpdatedBy != project.WriterAgent {
		t.Errorf("gamma = %+v", p)
	}
	if l := lastAudit(f, "api_task_retry"); !strings.Contains(l, "dispatch=unblocked") || !strings.Contains(l, "flaky runner") {
		t.Errorf("audit: %q", l)
	}
	if l := lastAudit(f, "api_write"); !strings.Contains(l, "action=task_retry") || !strings.Contains(l, "etag_before="+strings.Trim(etag, `"`)) {
		t.Errorf("audit: %q", l)
	}
}

// A block that covers more than this task is the wolf's, and a task the
// daemon is running is not retried.
func TestAPIRetryRefusals(t *testing.T) {
	f := newAPIFixture(t)
	h := f.writeServer(t)
	gamma, build := gammaBlocked(t, f, `task "build" failed after 3 attempts; also: task "ship" reported blocked: x`)
	before, _ := os.ReadFile(build)
	beforeP, _ := os.ReadFile(gamma)
	rec := send(t, h, f.tok, "POST", api.Prefix+"/projects/p_0.gamma/tasks/build/actions/retry", `{}`, map[string]string{"If-Match": f.taskETag(t, "p_0.gamma", "build")})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "summon the wolf") || !strings.Contains(rec.Body.String(), `"current"`) {
		t.Errorf("multi-task block: %d %s", rec.Code, rec.Body)
	}
	after, _ := os.ReadFile(build)
	afterP, _ := os.ReadFile(gamma)
	if string(after) != string(before) || string(afterP) != string(beforeP) {
		t.Error("a refused retry wrote a file")
	}
	for name, tc := range map[string]struct{ task, want string }{
		"running":   {"runner", "only a failed or blocked task"},
		"committed": {"docs", "only a failed or blocked task"},
	} {
		rec := send(t, h, f.tok, "POST", api.Prefix+"/projects/p_0.gamma/tasks/"+tc.task+"/actions/retry", `{}`, map[string]string{"If-Match": "*"})
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	// alpha's build has a live task session in the fixture.
	rec = send(t, h, f.tok, "POST", api.Prefix+"/projects/"+alphaID+"/tasks/build/actions/retry", `{}`, map[string]string{"If-Match": "*"})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "live session") {
		t.Errorf("live session: %d %s", rec.Code, rec.Body)
	}
	if rec := send(t, h, f.tok, "POST", api.Prefix+"/projects/p_0.gamma/tasks/nope/actions/retry", `{}`, map[string]string{"If-Match": "*"}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown task: %d %s", rec.Code, rec.Body)
	}
}

// The P.4 criterion for tasks: a write that lands between the GUI's read
// and its action (the TUI, an agent, a hand edit) is a 412, not a lost
// update.
func TestAPITaskConcurrentWriteIs412(t *testing.T) {
	f := newAPIFixture(t)
	h := f.writeServer(t)
	gamma, build := gammaBlocked(t, f, `task "build" failed after 3 attempts`)
	etag := f.taskETag(t, "p_0.gamma", "build")
	if _, err := control.SaveSandbox(build, true, nil); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(build)
	beforeP, _ := os.ReadFile(gamma)
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/tasks/build/actions/retry", `{"reset_attempts":true}`},
		{"PATCH", "/tasks/build", `{"description":"build it again"}`},
		{"POST", "/tasks/build/actions/save-sandbox", `{"save":false}`},
	} {
		rec := send(t, h, f.tok, tc.method, api.Prefix+"/projects/p_0.gamma"+tc.path, tc.body, map[string]string{"If-Match": etag})
		if rec.Code != http.StatusPreconditionFailed {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, rec.Code, rec.Body)
		}
		var eb struct {
			Error struct {
				Code    string       `json:"code"`
				Current TaskResource `json:"current"`
			} `json:"error"`
		}
		json.Unmarshal(rec.Body.Bytes(), &eb)
		if eb.Error.Code != api.CodePreconditionFailed || !eb.Error.Current.Capabilities.SaveSandbox || eb.Error.Current.ETag == etag {
			t.Errorf("%s: 412 body %s", tc.path, rec.Body)
		}
	}
	after, _ := os.ReadFile(build)
	afterP, _ := os.ReadFile(gamma)
	if string(after) != string(before) || string(afterP) != string(beforeP) {
		t.Error("a refused write rewrote a file")
	}
}

func TestAPISaveSandbox(t *testing.T) {
	f := newAPIFixture(t)
	h := f.writeServer(t)
	_, build := gammaBlocked(t, f, "x")
	rec := send(t, h, f.tok, "POST", api.Prefix+"/projects/p_0.gamma/tasks/build/actions/save-sandbox", `{"save":true}`, map[string]string{"If-Match": f.taskETag(t, "p_0.gamma", "build")})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	if res := taskResult(t, rec.Body.Bytes()); !res.Task.Capabilities.SaveSandbox || res.Write.Fields["save_sandbox"] != true {
		t.Errorf("result = %+v", res)
	}
	if tk, _ := task.Load(build); !tk.SaveSandbox {
		t.Error("save_sandbox not written")
	}
	if rec := send(t, h, f.tok, "POST", api.Prefix+"/projects/p_0.gamma/tasks/build/actions/save-sandbox", `{"save":true}`, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"noop": true`) {
		t.Errorf("again: %d %s", rec.Code, rec.Body)
	}
}

// PATCH a task: the edit is loaded with taskgraph.Load first, and one that
// would break the graph is a 422 with the task as it is.
func TestAPIEditTask(t *testing.T) {
	f := newAPIFixture(t)
	h := f.writeServer(t)
	_, build := gammaBlocked(t, f, "x")
	etag := f.taskETag(t, "p_0.gamma", "build")
	rec := send(t, h, f.tok, "PATCH", api.Prefix+"/projects/p_0.gamma/tasks/build", `{"description":"build it, docs first","depends_on":["docs"],"touches":["widgets/**"],"areas":["go-deps"]}`, map[string]string{"If-Match": etag})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body)
	}
	res := taskResult(t, rec.Body.Bytes())
	if res.Task.Description != "build it, docs first" || strings.Join(res.Task.DependsOn, ",") != "docs" || res.Task.Touches == nil || res.Task.Areas[0] != "go-deps" {
		t.Errorf("task = %+v", res.Task)
	}
	tk, _ := task.Load(build)
	if tk.Status != task.StatusFailed || tk.Attempts != 3 {
		t.Errorf("the edit changed status or attempts: %+v", tk)
	}
	etag = res.Task.ETag
	before, _ := os.ReadFile(build)
	rec = send(t, h, f.tok, "PATCH", api.Prefix+"/projects/p_0.gamma/tasks/build", `{"depends_on":["ship"]}`, map[string]string{"If-Match": etag})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "dependency cycle") || !strings.Contains(rec.Body.String(), `"current"`) {
		t.Errorf("cycle: %d %s", rec.Code, rec.Body)
	}
	if after, _ := os.ReadFile(build); string(after) != string(before) {
		t.Error("a refused edit rewrote the task")
	}
	rec = send(t, h, f.tok, "PATCH", api.Prefix+"/projects/p_0.gamma/tasks/runner", `{"description":"x"}`, map[string]string{"If-Match": "*"})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "is running") {
		t.Errorf("running: %d %s", rec.Code, rec.Body)
	}
}

// The host tabs (§A.4.6) call the daemon's Open* functions and open a tab in
// its terminal.
func TestAPIHostTabs(t *testing.T) {
	f := newAPIFixture(t)
	h := f.writeServer(t)
	launcher := &dirTabLauncher{}
	f.d.runner.Launcher = launcher
	f.d.runner.Workspaces = workspace.NewStub(t.TempDir())

	rec := send(t, h, f.tok, "POST", api.Prefix+"/projects/"+alphaID+"/actions/open-shell", `{"where":"orch"}`, nil)
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"target": "orch-alpha"`) {
		t.Fatalf("orch: %d %s", rec.Code, rec.Body)
	}
	if len(launcher.specs) != 1 || launcher.specs[0].Workspace != filepath.Dir(f.alpha) {
		t.Errorf("specs = %+v", launcher.specs)
	}
	// No workspace provisioned: the TUI's error, as a 409.
	rec = send(t, h, f.tok, "POST", api.Prefix+"/projects/"+alphaID+"/actions/open-shell", `{"where":"workspace"}`, nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "does not exist") {
		t.Errorf("workspace: %d %s", rec.Code, rec.Body)
	}
	// A task's shell opens in its worktree.
	rec = send(t, h, f.tok, "POST", api.Prefix+"/projects/"+alphaID+"/tasks/build/actions/open-shell", ``, nil)
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"kind": "worktree"`) {
		t.Fatalf("task shell: %d %s", rec.Code, rec.Body)
	}
	if got := launcher.specs[len(launcher.specs)-1].Workspace; got != f.worktree {
		t.Errorf("task shell cwd = %q, want %q", got, f.worktree)
	}
	// ship has no worktree.
	rec = send(t, h, f.tok, "POST", api.Prefix+"/projects/"+alphaID+"/tasks/ship/actions/open-session", ``, nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "no worktree") {
		t.Errorf("ship session: %d %s", rec.Code, rec.Body)
	}
	if l := lastAudit(f, "api_write"); !strings.Contains(l, "action=task_open-shell") {
		t.Errorf("audit: %q", l)
	}
}
