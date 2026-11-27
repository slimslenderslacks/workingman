package control

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/task"
)

// writeTask drops raw task YAML into the project's tasks/ directory.
func writeTask(t *testing.T, projectPath, file, yaml string) string {
	t.Helper()
	dir := filepath.Join(filepath.Dir(projectPath), "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, file)
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func loadTask(t *testing.T, path string) *task.Task {
	t.Helper()
	tk, err := task.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

const failedBuild = "name: build\ndescription: build it\nstatus: failed\nattempts: 3\nfailure_reason: go test failed\nworktree: /wt/build\n"

func TestRetryUnblocks(t *testing.T) {
	for reason, want := range map[string]bool{
		`task "build" failed after 3 attempts: go test failed`:                                 true,
		`task "build" reported blocked: needs a token`:                                         true,
		`task "build" conflicted with "lint" in widgets: go.mod after 3 attempts`:              true,
		`no ready tasks remain but project is not fully committed: build (failed)`:             true,
		`no ready tasks remain but project is not fully committed: build (failed), x (failed)`: false,
		`task "build" failed after 3 attempts; also: task "ship" failed after 3 attempts`:      false,
		`task "builder" failed after 3 attempts`:                                               false,
		`summoned manually by the user via the TUI (:wolf)`:                                    false,
		``: false,
	} {
		if got := RetryUnblocks(reason, "build"); got != want {
			t.Errorf("RetryUnblocks(%q) = %v, want %v", reason, got, want)
		}
	}
}

func TestRetryTaskOnWorkingNudges(t *testing.T) {
	root := t.TempDir()
	pp := writeYAML(t, root, "widget", "description: x\nbranch: widget\nstatus: working\nupdated_by: daemon\n")
	tp := writeTask(t, pp, "build.yaml", failedBuild)
	res, err := RetryTask(pp, tp, Retry{ResetAttempts: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Dispatch != DispatchNudged {
		t.Errorf("dispatch = %+v", res)
	}
	tk := loadTask(t, tp)
	if tk.Status != task.StatusReady || tk.Attempts != 0 || tk.FailureReason != "" || tk.Worktree != "/wt/build" {
		t.Errorf("task after retry = %+v", tk)
	}
	if p := load(t, pp); p.Status != project.StatusWorking || p.UpdatedBy != project.WriterAgent {
		t.Errorf("project after the nudge = %+v", p)
	}

	// Without reset_attempts the budget is kept.
	writeTask(t, pp, "build.yaml", failedBuild)
	if _, err := RetryTask(pp, tp, Retry{}); err != nil {
		t.Fatal(err)
	}
	if tk := loadTask(t, tp); tk.Attempts != 3 || tk.Status != task.StatusReady {
		t.Errorf("task = %+v, want attempts kept", tk)
	}
}

func TestRetryTaskUnblocksOnlyItsOwnBlock(t *testing.T) {
	root := t.TempDir()
	pp := writeYAML(t, root, "widget", "description: x\nbranch: widget\nstatus: blocked\nblocked_reason: 'task \"build\" failed after 3 attempts: go test failed'\nupdated_by: daemon\n")
	tp := writeTask(t, pp, "build.yaml", failedBuild)
	res, err := RetryTask(pp, tp, Retry{ResetAttempts: true})
	if err != nil || res.Dispatch != DispatchUnblocked {
		t.Fatalf("retry = %+v, %v", res, err)
	}
	if p := load(t, pp); p.Status != project.StatusWorking || p.BlockedReason != "" || p.UpdatedBy != project.WriterAgent {
		t.Errorf("project = %+v", p)
	}

	// A block that covers a second failure is the wolf's: refused, and
	// nothing written, the task file included.
	pp = writeYAML(t, root, "gadget", "description: x\nbranch: gadget\nstatus: blocked\nblocked_reason: 'task \"build\" failed after 3 attempts; also: task \"ship\" reported blocked: x'\nupdated_by: daemon\n")
	tp = writeTask(t, pp, "build.yaml", failedBuild)
	beforeP, beforeT := readFile(t, pp), readFile(t, tp)
	_, err = RetryTask(pp, tp, Retry{ResetAttempts: true})
	assertRefused(t, err, "summon the wolf", pp, beforeP)
	if !errors.Is(err, ErrState) || readFile(t, tp) != beforeT {
		t.Errorf("err = %v; task rewritten: %v", err, readFile(t, tp) != beforeT)
	}
}

func TestRetryTaskRefusals(t *testing.T) {
	root := t.TempDir()
	stopped := writeYAML(t, root, "stopped", "description: x\nbranch: s\nstatus: stopped\nstopped_from: working\n")
	tp := writeTask(t, stopped, "build.yaml", failedBuild)
	before := readFile(t, tp)
	_, err := RetryTask(stopped, tp, Retry{})
	assertRefused(t, err, "is stopped", tp, before)

	working := writeYAML(t, root, "working", "description: x\nbranch: w\nstatus: working\n")
	for file, yaml := range map[string]string{
		"running.yaml":   "name: running\ndescription: d\nstatus: running\n",
		"committed.yaml": "name: committed\ndescription: d\nstatus: committed\n",
		"ready.yaml":     "name: ready\ndescription: d\nstatus: ready\n",
		"seed.yaml":      "name: \"\"\ndescription: please add a thing\nstatus: ready\n",
	} {
		tp := writeTask(t, working, file, yaml)
		before := readFile(t, tp)
		_, err := RetryTask(working, tp, Retry{})
		if !errors.Is(err, ErrState) {
			t.Errorf("%s: err = %v, want ErrState", file, err)
		}
		if readFile(t, tp) != before {
			t.Errorf("%s: rewritten", file)
		}
	}
	// A ready task stuck after a conflict is retried.
	tp = writeTask(t, working, "conflicted.yaml", "name: conflicted\ndescription: d\nstatus: ready\nattempts: 3\nintegration: {result: conflict, repo: widgets}\n")
	if _, err := RetryTask(working, tp, Retry{ResetAttempts: true}); err != nil {
		t.Errorf("conflict re-run: %v", err)
	}

	// The check (If-Match) refuses before anything is read or written.
	tp = writeTask(t, working, "build.yaml", failedBuild)
	before = readFile(t, tp)
	stale := errors.New("stale")
	if _, err := RetryTask(working, tp, Retry{Check: func([]byte) error { return stale }}); err != stale {
		t.Errorf("check err = %v", err)
	}
	if readFile(t, tp) != before {
		t.Error("a failed check rewrote the task")
	}
}

func TestRetryTaskDefersOnRestingProject(t *testing.T) {
	root := t.TempDir()
	pp := writeYAML(t, root, "widget", "description: x\nbranch: widget\nstatus: idle\nupdated_by: daemon\n")
	tp := writeTask(t, pp, "build.yaml", failedBuild)
	before := readFile(t, pp)
	res, err := RetryTask(pp, tp, Retry{})
	if err != nil || res.Dispatch != DispatchDeferred || !strings.Contains(res.Reason, "idle") {
		t.Fatalf("retry = %+v, %v", res, err)
	}
	if readFile(t, pp) != before {
		t.Error("a deferred retry wrote project.yaml")
	}
	if tk := loadTask(t, tp); tk.Status != task.StatusReady {
		t.Errorf("task = %+v", tk)
	}
}

func TestSaveSandbox(t *testing.T) {
	pp := writeYAML(t, t.TempDir(), "widget", "description: x\nstatus: working\n")
	tp := writeTask(t, pp, "build.yaml", failedBuild)
	if noop, err := SaveSandbox(tp, true, nil); err != nil || noop {
		t.Fatalf("save = %v, %v", noop, err)
	}
	if !loadTask(t, tp).SaveSandbox {
		t.Error("save_sandbox not set")
	}
	before := readFile(t, tp)
	if noop, err := SaveSandbox(tp, true, nil); err != nil || !noop || readFile(t, tp) != before {
		t.Errorf("second save = %v, %v", noop, err)
	}
	if _, err := SaveSandbox(tp, false, nil); err != nil || loadTask(t, tp).SaveSandbox {
		t.Errorf("unsave: %v", err)
	}
}

func TestEditTask(t *testing.T) {
	pp := writeYAML(t, t.TempDir(), "widget", "description: x\nstatus: working\n")
	build := writeTask(t, pp, "build.yaml", "name: build\ndescription: build it\nstatus: failed\nattempts: 3\nstatic_mcps: [github]\n")
	writeTask(t, pp, "lint.yaml", "name: lint\ndescription: lint it\nstatus: committed\n")
	ship := writeTask(t, pp, "ship.yaml", "name: ship\ndescription: ship it\ndepends_on: [build]\nstatus: ready\n")
	writeTask(t, pp, "run.yaml", "name: run\ndescription: running\nstatus: running\n")

	desc, deps, touches, areas := "build it, with lint first", []string{"lint"}, []string{"widgets/**"}, []string{"go-deps"}
	if noop, err := EditTask(build, TaskEdit{Description: &desc, DependsOn: &deps, SetTouches: true, Touches: &touches, Areas: &areas}, nil); err != nil || noop {
		t.Fatalf("edit = %v, %v", noop, err)
	}
	tk := loadTask(t, build)
	if tk.Description != desc || strings.Join(tk.DependsOn, ",") != "lint" || tk.Touches == nil || (*tk.Touches)[0] != "widgets/**" || tk.Areas[0] != "go-deps" {
		t.Errorf("task = %+v", tk)
	}
	if tk.Status != task.StatusFailed || tk.Attempts != 3 || tk.StaticMCPs[0] != "github" {
		t.Errorf("the edit touched fields it does not own: %+v", tk)
	}
	// The same edit again writes nothing.
	before := readFile(t, build)
	if noop, err := EditTask(build, TaskEdit{Description: &desc}, nil); err != nil || !noop || readFile(t, build) != before {
		t.Errorf("same edit = %v, %v", noop, err)
	}
	// touches: null removes the hint.
	if _, err := EditTask(build, TaskEdit{SetTouches: true}, nil); err != nil || loadTask(t, build).Touches != nil {
		t.Errorf("clear touches: %v", err)
	}

	// Edits taskgraph.Load would refuse are refused, the file untouched.
	cycle, unknown := []string{"ship"}, []string{"nope"}
	badGlob, badArea := []string{"/etc/passwd"}, []string{"Not_Kebab"}
	blank := "  "
	for name, tc := range map[string]struct {
		e    TaskEdit
		kind error
		want string
	}{
		"cycle":       {TaskEdit{DependsOn: &cycle}, ErrGraph, "dependency cycle"},
		"unknown dep": {TaskEdit{DependsOn: &unknown}, ErrGraph, `unknown task "nope"`},
		"bad glob":    {TaskEdit{SetTouches: true, Touches: &badGlob}, ErrGraph, "is absolute"},
		"bad area":    {TaskEdit{Areas: &badArea}, ErrGraph, "not kebab-case"},
		"blank desc":  {TaskEdit{Description: &blank}, ErrInvalid, "must not be blank"},
	} {
		before := readFile(t, build)
		_, err := EditTask(build, tc.e, nil)
		if !errors.Is(err, tc.kind) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %v containing %q", name, err, tc.kind, tc.want)
		}
		if strings.Contains(err.Error(), os.TempDir()+"/wm-taskgraph") {
			t.Errorf("%s: the error names the temp copy: %v", name, err)
		}
		if readFile(t, build) != before {
			t.Errorf("%s: the task was rewritten", name)
		}
	}
	// The cycle is caught from the other side too: ship already depends on
	// build, so build may not depend on ship, but ship's own edit is fine.
	ok := "ship it now"
	if _, err := EditTask(ship, TaskEdit{Description: &ok}, nil); err != nil {
		t.Errorf("edit ship: %v", err)
	}
	// Not while it runs, nor once done.
	for _, f := range []string{"run.yaml", "lint.yaml"} {
		p := filepath.Join(filepath.Dir(build), f)
		if _, err := EditTask(p, TaskEdit{Description: &ok}, nil); !errors.Is(err, ErrState) {
			t.Errorf("%s: err = %v, want ErrState", f, err)
		}
	}
}
