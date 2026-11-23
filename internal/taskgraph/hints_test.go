package taskgraph

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/task"
)

// saveTasks writes tasks into a fresh tasks dir and loads the graph.
func saveTasks(t *testing.T, tasks ...*task.Task) *Graph {
	t.Helper()
	dir := tasksDir(t)
	for _, tk := range tasks {
		if err := task.Save(filepath.Join(dir, tk.Name+".yaml"), tk); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}
	g, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return g
}

func TestRunnablePriority(t *testing.T) {
	// Disjoint hints, so the order is all priority: the re-run first, then
	// the task with the most transitive dependents (hub: d1 and, through it,
	// d2), then one dependent (mid), then name.
	g := saveTasks(t,
		&task.Task{Name: "alpha", Status: task.StatusReady, Touches: touches("wm/alpha/**")},
		&task.Task{Name: "hub", Status: task.StatusReady, Touches: touches("wm/hub/**")},
		&task.Task{Name: "d1", Status: task.StatusReady, Touches: touches("wm/d1/**"), DependsOn: []string{"hub"}},
		&task.Task{Name: "d2", Status: task.StatusReady, Touches: touches("wm/d2/**"), DependsOn: []string{"d1"}},
		&task.Task{Name: "mid", Status: task.StatusReady, Touches: touches("wm/mid/**")},
		&task.Task{Name: "m1", Status: task.StatusReady, Touches: touches("wm/m1/**"), DependsOn: []string{"mid"}},
		&task.Task{Name: "zulu", Status: task.StatusReady, Touches: touches("wm/zulu/**"), Attempts: 1},
	)
	if got := g.Dependents("hub"); got != 2 {
		t.Errorf("Dependents(hub) = %d, want 2", got)
	}
	adm := g.Runnable(nil, 10)
	if got, want := names(adm.Admit), []string{"zulu", "hub", "mid", "alpha"}; !reflect.DeepEqual(got, want) {
		t.Errorf("admitted %v, want %v", got, want)
	}
	// Ready() itself stays name-sorted.
	if got, want := names(g.Ready()), []string{"alpha", "hub", "mid", "zulu"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Ready = %v, want %v", got, want)
	}
	// One slot, nothing in flight: the re-run, whatever the names say.
	if got := names(g.Runnable(nil, 1).Admit); !reflect.DeepEqual(got, []string{"zulu"}) {
		t.Errorf("1 slot: admitted %v, want [zulu]", got)
	}
}

// A re-run that sorts first is still a barrier when it overlaps a running
// task: tasks behind it that overlap it wait, unrelated ones start.
func TestRunnablePriorityKeepsBarrier(t *testing.T) {
	g := saveTasks(t,
		&task.Task{Name: "run", Status: task.StatusRunning, Touches: touches("wm/x/**")},
		&task.Task{Name: "rerun", Status: task.StatusReady, Touches: touches("wm/x/a.go", "wm/y/**"), Attempts: 2},
		&task.Task{Name: "a", Status: task.StatusReady, Touches: touches("wm/y/b.go")},
		&task.Task{Name: "b", Status: task.StatusReady, Touches: touches("wm/z/**")},
	)
	adm := g.Runnable(g.InFlight(), 3)
	if got := names(adm.Admit); !reflect.DeepEqual(got, []string{"b"}) {
		t.Errorf("admitted %v, want [b]", got)
	}
	want := map[string][]string{"rerun": {"run"}, "a": {"rerun"}}
	if got := deferredNames(adm); !reflect.DeepEqual(got, want) {
		t.Errorf("deferred %v, want %v", got, want)
	}
}

// An exclusive task is never starved. While anything is in flight it is
// deferred and holds back every candidate behind it in priority order, so the
// in-flight work drains; then it starts, alone, ahead of tasks that sort
// after it and are disjoint from everything else.
func TestRunnableExclusiveNotStarved(t *testing.T) {
	tasks := []*task.Task{
		{Name: "run", Status: task.StatusRunning, Touches: touches("wm/run/**")},
		{Name: "big", Status: task.StatusReady}, // exclusive; dependents put it first
		{Name: "after", Status: task.StatusReady, Touches: touches("wm/after/**"), DependsOn: []string{"big"}},
		{Name: "a", Status: task.StatusReady, Touches: touches("wm/a/**")},
		{Name: "b", Status: task.StatusReady, Touches: touches("wm/b/**")},
	}
	g := saveTasks(t, tasks...)
	adm := g.Runnable(g.InFlight(), 3)
	if len(adm.Admit) != 0 {
		t.Fatalf("admitted %v beside an exclusive task waiting on run", names(adm.Admit))
	}
	if got, want := deferredNames(adm), map[string][]string{"big": {"run"}, "a": {"big"}, "b": {"big"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("deferred %v, want %v", got, want)
	}

	// run lands: big starts, and only big.
	tasks[0].Status = task.StatusCommitted
	g = saveTasks(t, tasks...)
	adm = g.Runnable(g.InFlight(), 4)
	if got := names(adm.Admit); !reflect.DeepEqual(got, []string{"big"}) {
		t.Errorf("after drain: admitted %v, want [big]", got)
	}
}

// Conflict feedback (H3): a re-run carrying integration.files also touches
// those files, so it is not co-scheduled with a task declaring them, though
// its declared hints are disjoint. Without the report the two run together.
func TestRunnableConflictFeedback(t *testing.T) {
	rerun := &task.Task{
		Name: "api", Status: task.StatusReady, Attempts: 1,
		Touches: touches("wm/internal/api/**"),
		Integration: &task.Integration{
			Result: "conflict", Repo: "wm", Files: []string{"go.mod", "internal/router.go"},
		},
	}
	router := &task.Task{Name: "router", Status: task.StatusRunning, Touches: touches("wm/internal/router.go")}
	other := &task.Task{Name: "other", Status: task.StatusReady, Touches: touches("wm/docs/**")}

	g := saveTasks(t, rerun, router, other)
	if got := *g.EffectiveTouches(g.Task("api")); !reflect.DeepEqual(got, []string{"wm/internal/api/**", "wm/go.mod", "wm/internal/router.go"}) {
		t.Errorf("effective touches = %v", got)
	}
	adm := g.Runnable(g.InFlight(), 2)
	if got := names(adm.Admit); !reflect.DeepEqual(got, []string{"other"}) {
		t.Errorf("admitted %v, want [other]", got)
	}
	if got := deferredNames(adm); !reflect.DeepEqual(got, map[string][]string{"api": {"router"}}) {
		t.Errorf("deferred %v", got)
	}
	// The file is not rewritten.
	if got := *g.Task("api").Touches; !reflect.DeepEqual(got, []string{"wm/internal/api/**"}) {
		t.Errorf("declared touches changed: %v", got)
	}

	// Same files, no integration report: no overlap.
	rerun.Integration = nil
	g = saveTasks(t, rerun, router, other)
	if got := names(g.Runnable(g.InFlight(), 2).Admit); !reflect.DeepEqual(got, []string{"api", "other"}) {
		t.Errorf("without feedback: admitted %v, want [api other]", got)
	}

	// The widening goes both ways: a ready task declaring a file the running
	// re-run conflicted in waits for it.
	rerun.Integration = &task.Integration{Result: "conflict", Repo: "wm", Files: []string{"internal/router.go"}}
	rerun.Status = task.StatusRunning
	router.Status = task.StatusReady
	g = saveTasks(t, rerun, router)
	if got := deferredNames(g.Runnable(g.InFlight(), 2)); !reflect.DeepEqual(got, map[string][]string{"router": {"api"}}) {
		t.Errorf("reverse: deferred %v", got)
	}
}

func TestLoadRepairsBadHints(t *testing.T) {
	g := saveTasks(t,
		&task.Task{Name: "bad-glob", Status: task.StatusReady, Touches: touches("wm/[oops/x.go")},
		&task.Task{Name: "abs", Status: task.StatusReady, Touches: touches("/etc/passwd")},
		&task.Task{Name: "dotdot", Status: task.StatusReady, Touches: touches("wm/../other/x")},
		&task.Task{Name: "blank", Status: task.StatusReady, Touches: touches("  ")},
		&task.Task{Name: "bad-tag", Status: task.StatusReady, Areas: []string{"Go Deps"}},
		&task.Task{Name: "good", Status: task.StatusReady, Touches: touches("wm/a/**"), Areas: []string{"go-deps"}},
		&task.Task{Name: "nothing", Status: task.StatusReady, Touches: touches()},
	)
	w := strings.Join(g.Warnings(), "\n")
	for _, name := range []string{"bad-glob", "abs", "dotdot", "blank", "bad-tag"} {
		if !strings.Contains(w, `task "`+name+`" has an invalid hint`) {
			t.Errorf("no repair warning for %s:\n%s", name, w)
		}
		if g.EffectiveTouches(g.Task(name)) != nil || !g.Overlap(g.Task(name), g.Task("nothing")) {
			t.Errorf("%s: not scheduled as exclusive", name)
		}
	}
	if strings.Contains(w, `"good"`) || strings.Contains(w, `"nothing"`) {
		t.Errorf("valid hints warned about:\n%s", w)
	}
	if g.Overlap(g.Task("good"), g.Task("nothing")) {
		t.Error("good and nothing overlap")
	}
	// An exclusive task runs alone: one admitted per pass from the front.
	if got := names(g.Runnable(nil, 7).Admit); !reflect.DeepEqual(got, []string{"abs"}) {
		t.Errorf("admitted %v, want [abs]", got)
	}
}

func TestPathCovered(t *testing.T) {
	cases := []struct {
		glob, path string
		want       bool
	}{
		{"wm/internal/api/router.go", "wm/internal/api/router.go", true},
		{"wm/internal/api/healthz*.go", "wm/internal/api/healthz_test.go", true},
		{"wm/internal/api/healthz*.go", "wm/internal/api/router.go", false},
		{"wm/internal/**", "wm/internal/api/router.go", true},
		{"wm/internal", "wm/internal/api/router.go", true}, // directory prefix
		{"wm/internal/", "wm/internal/x.go", true},
		{"wm/go.mod", "wm/go.mod", true},
		{"wm/go.mod", "wm/go.sum", false},
		{"wm/internal/api/*.go", "wm/internal/api", false}, // glob is deeper than the path
		{"*/go.mod", "other/go.mod", true},
		{"wm/go.mod", "other/go.mod", false},
		{"wm/[bad", "wm/x", false},
		{"", "wm/x", false},
	}
	for _, c := range cases {
		if got := PathCovered(c.glob, c.path); got != c.want {
			t.Errorf("PathCovered(%q, %q) = %v, want %v", c.glob, c.path, got, c.want)
		}
	}
}

func TestUncovered(t *testing.T) {
	tk := &task.Task{
		Name:    "api",
		Touches: touches("wm/internal/api/**", "wm/docs/api.md"),
		Areas:   []string{"go-deps", "ci", "custom-tag"},
	}
	paths := []string{
		"wm/internal/api/router.go",
		"wm/docs/api.md",
		"wm/go.mod", "wm/go.sum", // go-deps
		"wm/.github/workflows/ci.yml", // ci
		"wm/internal/db/x.go",         // outside
		"other/README.md",             // outside
	}
	if got, want := Uncovered(tk, paths), []string{"other/README.md", "wm/internal/db/x.go"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Uncovered = %v, want %v", got, want)
	}
	if got := Uncovered(&task.Task{Name: "excl"}, paths); got != nil {
		t.Errorf("exclusive task: Uncovered = %v, want nil", got)
	}
	if got := Uncovered(&task.Task{Name: "none", Touches: touches()}, []string{"wm/x"}); !reflect.DeepEqual(got, []string{"wm/x"}) {
		t.Errorf("touches []: Uncovered = %v", got)
	}
}
