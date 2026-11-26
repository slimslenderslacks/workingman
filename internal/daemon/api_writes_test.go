package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/api"
	"github.com/slimslenderslacks/work/internal/project"
)

// writeServer replaces the fixture's API with one whose log is the daemon's
// audit log, as WithAPI wires it, so api_write lines land there.
func (f *apiFixture) writeServer(t *testing.T) http.Handler {
	t.Helper()
	tokPath := filepath.Join(t.TempDir(), "token")
	tok, _, err := api.EnsureToken(tokPath, false)
	if err != nil {
		t.Fatal(err)
	}
	srv := api.New(api.Config{Mode: api.ModeUnix, TokenPath: tokPath}, apiSource{f.d})
	srv.Log = f.d.audit.Log
	f.tok, f.h = tok, srv.Handler("")
	return f.h
}

func send(t *testing.T, h http.Handler, tok, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "localhost"
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func lastAudit(f *apiFixture, event string) string {
	lines := f.d.audit.Recent(50)
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], " "+event+" ") {
			return lines[i]
		}
	}
	return ""
}

func TestAPINewProjectSeedsProjectMD(t *testing.T) {
	f := newAPIFixture(t)
	h := f.writeServer(t)
	rec := send(t, h, f.tok, "POST", api.Prefix+"/projects", `{"root":0,"name":"gui-wireframes","description":"Draw SVG wireframes"}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST /projects: %d %s", rec.Code, rec.Body)
	}
	var res NewProjectResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	md := filepath.Join(f.root, "gui-wireframes", "project.md")
	if res.ID != "p_0.gui-wireframes" || res.Path != md || res.State != "seeded" || res.Write.Path != md {
		t.Errorf("result = %+v", res)
	}
	if b, _ := os.ReadFile(md); string(b) != "Draw SVG wireframes\n" {
		t.Errorf("project.md = %q", b)
	}
	if l := lastAudit(f, "api_write"); !strings.Contains(l, "action=new_project") || !strings.Contains(l, "client=bearer") || !strings.Contains(l, "path="+md) {
		t.Errorf("audit: %q", l)
	}
	// The daemon takes it from there as for a hand-made seed.
	f.d.handleProjectSeed(md)
	p, err := project.Load(filepath.Join(f.root, "gui-wireframes", project.FileName))
	if err != nil || p.Description != "Draw SVG wireframes" || !p.Unpopulated() {
		t.Errorf("seeded project.yaml = %+v, %v", p, err)
	}

	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"exists":    {`{"root":0,"name":"alpha","description":"x"}`, http.StatusConflict},
		"bad name":  {`{"root":0,"name":"../evil","description":"x"}`, http.StatusBadRequest},
		"worktrees": {`{"root":0,"name":".worktrees","description":"x"}`, http.StatusBadRequest},
		"no desc":   {`{"root":0,"name":"fresh","description":"  "}`, http.StatusBadRequest},
		"bad root":  {`{"root":3,"name":"fresh","description":"x"}`, http.StatusBadRequest},
		"populated": {`{"root":0,"name":"fresh","description":"x","status":"ready"}`, http.StatusBadRequest},
	} {
		if rec := send(t, h, f.tok, "POST", api.Prefix+"/projects", tc.body, nil); rec.Code != tc.want {
			t.Errorf("%s: %d %s, want %d", name, rec.Code, rec.Body, tc.want)
		}
	}
	if _, err := os.Stat(filepath.Join(f.root, "fresh")); !os.IsNotExist(err) {
		t.Errorf("a refused create left a directory: %v", err)
	}
}

func TestAPIQueueAndWithdrawIntake(t *testing.T) {
	f := newAPIFixture(t)
	h := f.writeServer(t)
	path := api.Prefix + "/projects/" + alphaID + "/intake"
	rec := send(t, h, f.tok, "POST", path, `{"name":"add-card","body":"Add a sandbox card"}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST intake: %d %s", rec.Code, rec.Body)
	}
	var res IntakeWriteResult
	json.Unmarshal(rec.Body.Bytes(), &res)
	file := filepath.Join(filepath.Dir(f.alpha), "intake", "add-card.md")
	if res.Intake.Name != "add-card" || res.Intake.State != api.IntakePending || res.Write.Path != file || !strings.HasPrefix(res.Intake.ETag, `"in-`) {
		t.Errorf("result = %+v", res)
	}
	if rec.Header().Get("ETag") != res.Intake.ETag {
		t.Errorf("ETag header %q, body %q", rec.Header().Get("ETag"), res.Intake.ETag)
	}
	// The read endpoint agrees on the etag.
	var got IntakeRequest
	f.get(t, path+"/add-card", &got)
	if got.ETag != res.Intake.ETag || got.State != api.IntakePending || got.Body == nil || *got.Body != "Add a sandbox card\n" {
		t.Errorf("GET intake = %+v", got)
	}
	if l := lastAudit(f, "api_write"); !strings.Contains(l, "action=queue_intake") || !strings.Contains(l, "etag_after="+strings.Trim(res.Intake.ETag, `"`)) {
		t.Errorf("audit: %q", l)
	}

	// Refusals.
	for name, tc := range map[string]struct {
		path, body string
		hdr        map[string]string
		want       int
	}{
		"dup":         {path, `{"name":"add-card","body":"again"}`, nil, http.StatusConflict},
		"processed":   {path, `{"name":"old","body":"again"}`, nil, http.StatusConflict},
		"blank":       {path, `{"name":"x","body":" \n"}`, nil, http.StatusBadRequest},
		"bad name":    {path, `{"name":"x.md","body":"b"}`, nil, http.StatusBadRequest},
		"no project":  {api.Prefix + "/projects/p_0.nope/intake", `{"body":"b"}`, nil, http.StatusNotFound},
		"if-match":    {path, `{"name":"y","body":"b"}`, map[string]string{"If-Match": `"in-0"`}, http.StatusPreconditionFailed},
		"wrong field": {path, `{"name":"y","text":"b"}`, nil, http.StatusBadRequest},
	} {
		if rec := send(t, h, f.tok, "POST", tc.path, tc.body, tc.hdr); rec.Code != tc.want {
			t.Errorf("%s: %d %s, want %d", name, rec.Code, rec.Body, tc.want)
		}
	}

	// No name: the daemon picks one.
	rec = send(t, h, f.tok, "POST", path, `{"body":"anonymous"}`, nil)
	json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != http.StatusAccepted || !strings.HasPrefix(res.Intake.Name, "gui-") {
		t.Errorf("default name: %d %+v", rec.Code, res.Intake)
	}

	// Withdraw: a stale etag is a 412 carrying the request as it is now; a
	// concurrent hand edit is what makes it stale.
	if err := os.WriteFile(file, []byte("Add a sandbox card, and a list\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := send(t, h, f.tok, "DELETE", path+"/add-card", "", map[string]string{"If-Match": got.ETag})
	if stale.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale withdraw: %d %s", stale.Code, stale.Body)
	}
	var eb struct {
		Error struct {
			Code    string        `json:"code"`
			Current IntakeRequest `json:"current"`
		} `json:"error"`
	}
	json.Unmarshal(stale.Body.Bytes(), &eb)
	if eb.Error.Code != api.CodePreconditionFailed || eb.Error.Current.Name != "add-card" || eb.Error.Current.ETag == got.ETag || eb.Error.Current.ETag == "" {
		t.Errorf("412 body: %s", stale.Body)
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("a refused withdraw moved the file: %v", err)
	}
	if l := lastAudit(f, "api_write_refused"); !strings.Contains(l, "action=withdraw_intake") || !strings.Contains(l, "code=precondition_failed") {
		t.Errorf("audit: %q", l)
	}
	// With the current etag it goes through.
	rec = send(t, h, f.tok, "DELETE", path+"/add-card", "", map[string]string{"If-Match": eb.Error.Current.ETag})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("withdraw: %d %s", rec.Code, rec.Body)
	}
	json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Intake.State != IntakeWithdrawn || res.Intake.File != "add-card.withdrawn" || res.Noop {
		t.Errorf("withdraw result = %+v", res)
	}
	if _, err := os.Stat(strings.TrimSuffix(file, ".md") + ".withdrawn"); err != nil {
		t.Errorf("no .withdrawn file: %v", err)
	}
	// It leaves the read endpoints, and the planner never sees it.
	if rec := f.get(t, path+"/add-card", nil); rec.Code != http.StatusNotFound {
		t.Errorf("GET withdrawn: %d", rec.Code)
	}
	for _, p := range pendingIntakeFiles(filepath.Dir(f.alpha)) {
		if strings.Contains(p, "add-card") {
			t.Errorf("withdrawn request still pending: %s", p)
		}
	}
	// Again: 200 noop. A processed request: 409.
	if rec := send(t, h, f.tok, "DELETE", path+"/add-card", "", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"noop": true`) {
		t.Errorf("withdraw again: %d %s", rec.Code, rec.Body)
	}
	if rec := send(t, h, f.tok, "DELETE", path+"/old", "", nil); rec.Code != http.StatusConflict {
		t.Errorf("withdraw processed: %d %s", rec.Code, rec.Body)
	}
	if rec := send(t, h, f.tok, "DELETE", path+"/nope", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("withdraw missing: %d %s", rec.Code, rec.Body)
	}
}

func TestAPIQueuedIntakeIsHandledByTheDaemon(t *testing.T) {
	f := newAPIFixture(t)
	h := f.writeServer(t)
	// bravo is a resting project: intake flips it to ready.
	bravo := filepath.Join(f.root, "bravo", project.FileName)
	before, err := project.Load(bravo)
	if err != nil {
		t.Fatal(err)
	}
	if before.Status == project.StatusWorking || before.Status == project.StatusStopped {
		t.Skipf("fixture bravo is %s", before.Status)
	}
	rec := send(t, h, f.tok, "POST", api.Prefix+"/projects/p_0.bravo/intake", `{"name":"more","body":"one more thing"}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST intake: %d %s", rec.Code, rec.Body)
	}
	f.d.handleIntakeFile(filepath.Join(f.root, "bravo", "intake", "more.md"))
	after, err := project.Load(bravo)
	if err != nil || after.Status != project.StatusReady || after.UpdatedBy != project.WriterAgent {
		t.Errorf("bravo after intake = %+v, %v", after, err)
	}
}

// Intake on a stopped project is refused: handleIntakeFile would flip it to
// ready, ending the stop.
func TestAPIQueueIntakeRefusesStoppedProject(t *testing.T) {
	f := newAPIFixture(t)
	h := f.writeServer(t)
	bravo := filepath.Join(f.root, "bravo", project.FileName)
	if err := project.Update(bravo, project.WriterAgent, func(p *project.Project) bool {
		p.StoppedFrom, p.Status = p.Status, project.StatusStopped
		return true
	}); err != nil {
		t.Fatal(err)
	}
	rec := send(t, h, f.tok, "POST", api.Prefix+"/projects/p_0.bravo/intake", `{"name":"more","body":"x"}`, nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"invalid_state"`) || !strings.Contains(rec.Body.String(), ":start it first") {
		t.Errorf("stopped: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(f.root, "bravo", "intake", "more.md")); !os.IsNotExist(err) {
		t.Errorf("a refused queue wrote the file: %v", err)
	}
}
