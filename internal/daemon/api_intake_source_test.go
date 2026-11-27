package daemon

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/api"
)

// TestAPIIntakeSourceNotAPRFix: a task carrying an intake back-reference is
// served with its source as written, and is never counted among a PR's fix
// tasks — even when a confused planner also wrote the PR's number on it.
func TestAPIIntakeSourceNotAPRFix(t *testing.T) {
	f := newAPIFixture(t)
	path := filepath.Join(filepath.Dir(f.alpha), "tasks", "ship.yaml")
	body := "name: ship\ndescription: ship it\ndepends_on: [build]\nstatus: ready\n" +
		"source: {kind: intake, ref: add-svg, repo: acme/widgets, pr: 7}\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	var one TaskResource
	f.get(t, api.Prefix+"/projects/"+alphaID+"/tasks/ship", &one)
	if one.Source == nil || one.Source.Kind != "intake" || one.Source.Ref != "add-svg" {
		t.Errorf("ship source = %+v, want intake add-svg", one.Source)
	}

	var l listOf[PullRequest]
	f.get(t, api.Prefix+"/projects/"+alphaID+"/pulls", &l)
	if len(l.Items) != 1 {
		t.Fatalf("pulls = %+v", l.Items)
	}
	if got := strings.Join(l.Items[0].FixTasks, ","); got != "build" {
		t.Errorf("fix_tasks = %q, want only the pr-comment task build", got)
	}
}

// planRun simulates one planning run on the fixture's alpha project: it is
// offered the intake files named in offered (created when missing), writes
// the task files in tasks (file stem -> YAML) and renames each offered file
// to .processed, between beginPlanning and endPlanning as a real run would.
func (f *apiFixture) planRun(t *testing.T, offered []string, tasks map[string]string) {
	t.Helper()
	dir := filepath.Dir(f.alpha)
	var files []string
	for _, name := range offered {
		p := filepath.Join(dir, "intake", name+".md")
		if _, err := os.Stat(p); err != nil {
			if err := os.WriteFile(p, []byte("please do "+name+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		files = append(files, p)
	}
	if !f.d.beginPlanning(f.alpha, files) {
		t.Fatal("beginPlanning: a planner is already in flight")
	}
	for stem, body := range tasks {
		if err := os.WriteFile(filepath.Join(dir, "tasks", stem+".yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range files {
		if err := os.Rename(p, strings.TrimSuffix(p, ".md")+".processed"); err != nil {
			t.Fatal(err)
		}
	}
	f.d.endPlanning(f.alpha)
}

func intakeTasksOf(t *testing.T, f *apiFixture, name string) string {
	t.Helper()
	var r IntakeRequest
	f.get(t, api.Prefix+"/projects/"+alphaID+"/intake/"+name, &r)
	if r.Tasks == nil {
		t.Errorf("%s: tasks is null, want a list", name)
	}
	var out []string
	for _, tk := range r.Tasks {
		out = append(out, tk.Name+":"+tk.Mapping)
	}
	return strings.Join(out, ",")
}

// TestAPIIntakeTasks: an intake request lists the tasks that record it, and
// only those, when any does; one no task records falls back to the tasks the
// planning run that processed it created, marked inferred, on both the
// request and the task resource; tasks with a source of their own (another
// request, a PR signal) are never reattributed, and PR sources are served
// exactly as before.
func TestAPIIntakeTasks(t *testing.T) {
	f := newAPIFixture(t)
	// Run 1 records the back-reference on one of its tasks: the recorded
	// mapping wins, and the untagged sibling is not guessed in.
	f.planRun(t, []string{"add-svg"}, map[string]string{
		"svg-card": "name: svg-card\ndescription: d\nstatus: ready\nsource: {kind: intake, ref: add-svg}\n",
		"svg-docs": "name: svg-docs\ndescription: d\nstatus: ready\n",
	})
	// Run 2 records nothing: its untagged tasks are inferred, its PR-sourced
	// one is not.
	f.planRun(t, []string{"add-relay"}, map[string]string{
		"relay-api": "name: relay-api\ndescription: d\nstatus: ready\n",
		"relay-gui": "name: relay-gui\ndescription: d\nstatus: ready\ndepends_on: [relay-api]\n",
		"relay-fix": "name: relay-fix\ndescription: d\nstatus: ready\nsource: {kind: pr-check, ref: build, repo: acme/widgets, pr: 7}\n",
	})
	// Run 3 processes two requests and records nothing: its task is listed
	// under both, but its own resource names neither.
	f.planRun(t, []string{"req-a", "req-b"}, map[string]string{
		"both": "name: both\ndescription: d\nstatus: ready\n",
	})

	for name, want := range map[string]string{
		"add-svg":   "svg-card:recorded",
		"add-relay": "relay-api:inferred,relay-gui:inferred",
		"req-a":     "both:inferred",
		"req-b":     "both:inferred",
		"old":       "", // processed before this daemon: nothing known
	} {
		if got := intakeTasksOf(t, f, name); got != want {
			t.Errorf("intake %s tasks = %q, want %q", name, got, want)
		}
	}
	var l listOf[IntakeRequest]
	f.get(t, api.Prefix+"/projects/"+alphaID+"/intake?state=processed", &l)
	for _, r := range l.Items {
		if r.Name == "add-relay" && len(r.Tasks) != 2 {
			t.Errorf("list: add-relay tasks = %+v", r.Tasks)
		}
	}

	src := func(name string) *APITaskSource {
		t.Helper()
		var one TaskResource
		f.get(t, api.Prefix+"/projects/"+alphaID+"/tasks/"+name, &one)
		return one.Source
	}
	if s := src("svg-card"); s == nil || s.Kind != "intake" || s.Ref != "add-svg" || s.Inferred {
		t.Errorf("svg-card source = %+v, want recorded intake add-svg", s)
	}
	if s := src("relay-api"); s == nil || s.Kind != "intake" || s.Ref != "add-relay" || !s.Inferred || s.PR != 0 {
		t.Errorf("relay-api source = %+v, want inferred intake add-relay", s)
	}
	for _, name := range []string{"svg-docs", "both", "ship"} {
		if s := src(name); s != nil {
			t.Errorf("%s source = %+v, want none", name, s)
		}
	}
	if s := src("relay-fix"); s == nil || s.Kind != "pr-check" || s.Inferred {
		t.Errorf("relay-fix source = %+v, want its pr-check", s)
	}

	// A PR-sourced task's source is byte-for-byte what it was.
	var raw struct {
		Source json.RawMessage `json:"source"`
	}
	f.get(t, api.Prefix+"/projects/"+alphaID+"/tasks/build", &raw)
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw.Source); err != nil {
		t.Fatal(err)
	}
	if want := `{"kind":"pr-comment","ref":"PRRT_1","repo":"acme/widgets","pr":7}`; compact.String() != want {
		t.Errorf("build source = %s, want %s", compact.String(), want)
	}
	var pulls listOf[PullRequest]
	f.get(t, api.Prefix+"/projects/"+alphaID+"/pulls", &pulls)
	if len(pulls.Items) != 1 || strings.Join(pulls.Items[0].FixTasks, ",") != "build,relay-fix" {
		t.Errorf("pulls = %+v", pulls.Items)
	}
}

// TestPlanningInferenceSkipsUnprocessed: a request a run was handed but left
// pending, or that was withdrawn mid-run, gets no inferred tasks.
func TestPlanningInferenceSkipsUnprocessed(t *testing.T) {
	f := newAPIFixture(t)
	dir := filepath.Dir(f.alpha)
	pending := filepath.Join(dir, "intake", "add-svg.md")
	gone := filepath.Join(dir, "intake", "gone.md")
	if err := os.WriteFile(gone, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !f.d.beginPlanning(f.alpha, []string{pending, gone}) {
		t.Fatal("beginPlanning")
	}
	if err := os.WriteFile(filepath.Join(dir, "tasks", "new.yaml"), []byte("name: new\nstatus: ready\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(gone, filepath.Join(dir, "intake", "gone.withdrawn")); err != nil {
		t.Fatal(err)
	}
	if left := f.d.endPlanning(f.alpha); len(left) != 1 || left[0] != pending {
		t.Errorf("leftover = %v", left)
	}
	if got := f.d.inferredIntake(f.alpha); len(got) != 0 {
		t.Errorf("inferredIntake = %v, want none", got)
	}
	if got := intakeTasksOf(t, f, "add-svg"); got != "" {
		t.Errorf("add-svg tasks = %q", got)
	}
}
