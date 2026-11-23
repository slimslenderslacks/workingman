package taskgraph

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/slimslenderslacks/work/internal/task"
)

func touches(globs ...string) *[]string { return &globs }

func TestGlobsOverlap(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"wm/internal/api/router.go", "wm/internal/api/router.go", true},
		{"wm/internal/api/router.go", "wm/internal/api/healthz.go", false},
		{"wm/internal/api/healthz*.go", "wm/internal/api/healthz_test.go", true},
		{"wm/internal/api/healthz*.go", "wm/internal/api/router.go", false},
		{"wm/internal/*/x.go", "wm/internal/*/y.go", false},
		{"wm/internal/a*/x.go", "wm/internal/b*/x.go", true}, // two wildcards assumed to match
		{"wm/internal/**", "wm/internal/api/router.go", true},
		{"wm/**/router.go", "wm/docs/api.md", true},        // ** matches the rest
		{"wm/internal", "wm/internal/api/router.go", true}, // directory prefix
		{"wm/go.mod", "wm/go.mod/x", false},                // file, not a dir
		{"wm/internal/api/*.go", "wm/internal/api", true},  // dir prefix on the other side
		{"wm/internal/api/*.go", "wm/internal/api/**", true},
		{"*/go.mod", "other/go.mod", true}, // repo wildcard
		{"wm/go.mod", "other/go.mod", false},
		{"wm/[bad", "wm/x", true},            // malformed pattern is conservative
		{"", "wm/x", true},                   // empty glob is conservative
		{"wm/docs/", "wm/docs/api.md", true}, // trailing slash cleaned
	}
	for _, c := range cases {
		if got := GlobsOverlap(c.a, c.b); got != c.want {
			t.Errorf("GlobsOverlap(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
		if got := GlobsOverlap(c.b, c.a); got != c.want {
			t.Errorf("GlobsOverlap(%q, %q) = %v, want %v (symmetry)", c.b, c.a, got, c.want)
		}
	}
}

func TestOverlap(t *testing.T) {
	api := &task.Task{Name: "api", Touches: touches("wm/internal/api/**")}
	docs := &task.Task{Name: "docs", Touches: touches("wm/docs/**")}
	none := &task.Task{Name: "none", Touches: touches()}
	excl := &task.Task{Name: "excl"}
	depsA := &task.Task{Name: "deps-a", Touches: touches("wm/a/**"), Areas: []string{"go-deps"}}
	depsB := &task.Task{Name: "deps-b", Areas: []string{"go-deps"}}
	other := &task.Task{Name: "other", Areas: []string{"db-schema"}}

	cases := []struct {
		a, b *task.Task
		want bool
	}{
		{api, docs, false},
		{api, api, true},
		{api, none, false},  // touches: [] touches nothing
		{none, none, false}, // and two of them never overlap
		{api, excl, true},   // no hints: exclusive
		{none, excl, true},
		{depsA, depsB, true}, // shared area
		{depsB, other, false},
		{depsA, api, false},
	}
	for _, c := range cases {
		if got := Overlap(c.a, c.b); got != c.want {
			t.Errorf("Overlap(%s, %s) = %v, want %v", c.a.Name, c.b.Name, got, c.want)
		}
		if got := Overlap(c.b, c.a); got != c.want {
			t.Errorf("Overlap(%s, %s) = %v, want %v (symmetry)", c.b.Name, c.a.Name, got, c.want)
		}
	}
}

func TestInFlight(t *testing.T) {
	tasks := []*task.Task{
		{Name: "z", Status: task.StatusSuccess},
		{Name: "a", Status: task.StatusRunning},
		{Name: "b", Status: task.StatusReady},
		{Name: "c", Status: task.StatusCommitted},
		{Name: "d", Status: task.StatusFailed},
		{Name: "e", Status: task.StatusBlocked},
	}
	if got, want := names(InFlight(tasks)), []string{"a", "z"}; !reflect.DeepEqual(got, want) {
		t.Errorf("InFlight = %v, want %v", got, want)
	}
}

// hinted describes one task file for loadHinted.
type hinted struct {
	name    string
	status  task.Status
	touches *[]string
	areas   []string
	deps    []string
	source  *task.Source
}

func loadHinted(t *testing.T, specs ...hinted) *Graph {
	t.Helper()
	dir := tasksDir(t)
	for _, s := range specs {
		tk := &task.Task{
			Name: s.name, Status: s.status, Touches: s.touches,
			Areas: s.areas, DependsOn: s.deps, Source: s.source,
		}
		if err := task.Save(filepath.Join(dir, s.name+".yaml"), tk); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}
	g, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return g
}

// slotsFor is what schedule will compute: limit minus what is in flight.
func slotsFor(g *Graph, limit int) int { return limit - len(g.InFlight()) }

func deferredNames(adm Admission) map[string][]string {
	out := map[string][]string{}
	for _, d := range adm.Deferred {
		out[d.Task.Name] = d.Overlaps
	}
	return out
}

func TestRunnableLimitTwoVsOne(t *testing.T) {
	g := loadHinted(t,
		hinted{name: "a", status: task.StatusReady, touches: touches("wm/a/**")},
		hinted{name: "b", status: task.StatusReady, touches: touches("wm/b/**")},
		hinted{name: "c", status: task.StatusReady, touches: touches("wm/c/**")},
	)
	if got := names(g.Runnable(g.InFlight(), slotsFor(g, 2)).Admit); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("limit 2: admitted %v, want [a b]", got)
	}
	if got := names(g.Runnable(g.InFlight(), slotsFor(g, 1)).Admit); !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("limit 1: admitted %v, want [a]", got)
	}
	if got := g.Runnable(g.InFlight(), 0).Admit; len(got) != 0 {
		t.Errorf("no slots: admitted %v", names(got))
	}
	if got := g.Runnable(g.InFlight(), -1).Admit; len(got) != 0 {
		t.Errorf("negative slots: admitted %v", names(got))
	}
}

// At limit 1, a running task fills the only slot even if its hints are
// disjoint from every candidate's.
func TestRunnableLimitOneWithTaskInFlight(t *testing.T) {
	g := loadHinted(t,
		hinted{name: "a", status: task.StatusRunning, touches: touches("wm/a/**")},
		hinted{name: "b", status: task.StatusReady, touches: touches("wm/b/**")},
	)
	if got := g.Runnable(g.InFlight(), slotsFor(g, 1)).Admit; len(got) != 0 {
		t.Errorf("limit 1 with a in flight: admitted %v", names(got))
	}
	if got := names(g.Runnable(g.InFlight(), slotsFor(g, 2)).Admit); !reflect.DeepEqual(got, []string{"b"}) {
		t.Errorf("limit 2 with a in flight: admitted %v, want [b]", got)
	}
}

// With one slot and nothing in flight, Runnable is ready[0] whatever the
// hints: today's serial dispatch.
func TestRunnableLimitOneIsReadyHead(t *testing.T) {
	g := loadHinted(t,
		hinted{name: "b", status: task.StatusReady},
		hinted{name: "a", status: task.StatusReady},
	)
	adm := g.Runnable(nil, 1)
	if got := names(adm.Admit); !reflect.DeepEqual(got, []string{g.Ready()[0].Name}) || got[0] != "a" {
		t.Errorf("admitted %v, want [a]", got)
	}
}

func TestRunnableExclusiveTasks(t *testing.T) {
	// Nothing in flight: the first exclusive task is admitted, and then
	// everything behind it overlaps it.
	g := loadHinted(t,
		hinted{name: "a", status: task.StatusReady},
		hinted{name: "b", status: task.StatusReady, touches: touches("wm/b/**")},
		hinted{name: "c", status: task.StatusReady},
	)
	adm := g.Runnable(nil, 3)
	if got := names(adm.Admit); !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("admitted %v, want [a]", got)
	}
	want := map[string][]string{"b": {"a"}, "c": {"a", "b"}}
	if got := deferredNames(adm); !reflect.DeepEqual(got, want) {
		t.Errorf("deferred %v, want %v", got, want)
	}

	// An exclusive task in flight blocks every candidate.
	g = loadHinted(t,
		hinted{name: "a", status: task.StatusRunning},
		hinted{name: "b", status: task.StatusReady, touches: touches()},
	)
	adm = g.Runnable(g.InFlight(), slotsFor(g, 4))
	if len(adm.Admit) != 0 {
		t.Errorf("exclusive in flight: admitted %v", names(adm.Admit))
	}
	if got := deferredNames(adm); !reflect.DeepEqual(got, map[string][]string{"b": {"a"}}) {
		t.Errorf("deferred %v", got)
	}
}

func TestRunnableOverlappingVsDisjointHints(t *testing.T) {
	g := loadHinted(t,
		hinted{name: "api", status: task.StatusRunning, touches: touches("wm/internal/api/**")},
		hinted{name: "queued", status: task.StatusSuccess, areas: []string{"go-deps"}},
		hinted{name: "docs", status: task.StatusReady, touches: touches("wm/docs/**")},
		hinted{name: "router", status: task.StatusReady, touches: touches("wm/internal/api/router.go")},
		hinted{name: "bump", status: task.StatusReady, touches: touches("wm/tools/**"), areas: []string{"go-deps"}},
		hinted{name: "tui", status: task.StatusReady, touches: touches("wm/internal/tui")},
	)
	adm := g.Runnable(g.InFlight(), slotsFor(g, 5))
	if got := names(adm.Admit); !reflect.DeepEqual(got, []string{"docs", "tui"}) {
		t.Errorf("admitted %v, want [docs tui]", got)
	}
	// A queued (success) task overlaps like a running one: its work has not
	// landed yet.
	want := map[string][]string{"router": {"api"}, "bump": {"queued"}}
	if got := deferredNames(adm); !reflect.DeepEqual(got, want) {
		t.Errorf("deferred %v, want %v", got, want)
	}
}

// Two disjoint ready tasks that overlap each other are not admitted together.
func TestRunnableAdmittedInSamePassDoNotOverlap(t *testing.T) {
	g := loadHinted(t,
		hinted{name: "a", status: task.StatusReady, touches: touches("wm/go.mod")},
		hinted{name: "b", status: task.StatusReady, touches: touches("wm/go.mod", "wm/b/**")},
		hinted{name: "c", status: task.StatusReady, touches: touches("wm/c/**")},
	)
	adm := g.Runnable(nil, 3)
	if got := names(adm.Admit); !reflect.DeepEqual(got, []string{"a", "c"}) {
		t.Errorf("admitted %v, want [a c]", got)
	}
	if got := deferredNames(adm); !reflect.DeepEqual(got, map[string][]string{"b": {"a"}}) {
		t.Errorf("deferred %v", got)
	}
}

func TestRunnableDependentsWaitOnIntegration(t *testing.T) {
	g := loadHinted(t,
		hinted{name: "a", status: task.StatusSuccess, touches: touches("wm/a/**")},
		hinted{name: "b", status: task.StatusReady, touches: touches("wm/b/**"), deps: []string{"a"}},
		hinted{name: "c", status: task.StatusReady, touches: touches("wm/c/**")},
	)
	adm := g.Runnable(g.InFlight(), slotsFor(g, 3))
	if got := names(adm.Admit); !reflect.DeepEqual(got, []string{"c"}) {
		t.Errorf("admitted %v, want [c]: b's dependency a is at success, not committed", got)
	}
	if len(adm.Deferred) != 0 {
		t.Errorf("b is not Ready, so not deferred for overlap: %v", deferredNames(adm))
	}
	if got := WaitingOnIntegration(g.Task("b"), g.Tasks()); !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("WaitingOnIntegration(b) = %v, want [a]", got)
	}

	// Once a is committed, b is admitted.
	g = loadHinted(t,
		hinted{name: "a", status: task.StatusCommitted, touches: touches("wm/a/**")},
		hinted{name: "b", status: task.StatusReady, touches: touches("wm/b/**"), deps: []string{"a"}},
	)
	if got := names(g.Runnable(g.InFlight(), slotsFor(g, 3)).Admit); !reflect.DeepEqual(got, []string{"b"}) {
		t.Errorf("after commit: admitted %v, want [b]", got)
	}
}

// A deferred task becomes a barrier: a later candidate that overlaps it is
// deferred too, even though it overlaps nothing in flight.
func TestRunnableBarrier(t *testing.T) {
	g := loadHinted(t,
		hinted{name: "run", status: task.StatusRunning, touches: touches("wm/x/**")},
		hinted{name: "a", status: task.StatusReady, touches: touches("wm/x/one.go", "wm/y/**")},
		hinted{name: "b", status: task.StatusReady, touches: touches("wm/y/two.go")},
		hinted{name: "c", status: task.StatusReady, touches: touches("wm/z/**")},
	)
	adm := g.Runnable(g.InFlight(), slotsFor(g, 4))
	if got := names(adm.Admit); !reflect.DeepEqual(got, []string{"c"}) {
		t.Errorf("admitted %v, want [c]", got)
	}
	want := map[string][]string{"a": {"run"}, "b": {"a"}}
	if got := deferredNames(adm); !reflect.DeepEqual(got, want) {
		t.Errorf("deferred %v, want %v", got, want)
	}

	// An exclusive task at the front is a full barrier while anything is in
	// flight.
	g = loadHinted(t,
		hinted{name: "run", status: task.StatusRunning, touches: touches("wm/x/**")},
		hinted{name: "a", status: task.StatusReady},
		hinted{name: "b", status: task.StatusReady, touches: touches("wm/b/**")},
	)
	adm = g.Runnable(g.InFlight(), slotsFor(g, 4))
	if len(adm.Admit) != 0 {
		t.Errorf("exclusive barrier: admitted %v", names(adm.Admit))
	}
	want = map[string][]string{"a": {"run"}, "b": {"a"}}
	if got := deferredNames(adm); !reflect.DeepEqual(got, want) {
		t.Errorf("deferred %v, want %v", got, want)
	}
}

func TestRunnableNameOrderDeterministic(t *testing.T) {
	specs := []hinted{
		{name: "delta", status: task.StatusReady, touches: touches("wm/d/**")},
		{name: "alpha", status: task.StatusReady, touches: touches("wm/shared.go")},
		{name: "charlie", status: task.StatusReady, touches: touches("wm/shared.go")},
		{name: "bravo", status: task.StatusReady, touches: touches("wm/b/**")},
	}
	for i := 0; i < 5; i++ {
		g := loadHinted(t, specs...)
		adm := g.Runnable(nil, 3)
		if got := names(adm.Admit); !reflect.DeepEqual(got, []string{"alpha", "bravo", "delta"}) {
			t.Fatalf("run %d: admitted %v, want [alpha bravo delta]", i, got)
		}
		if got := deferredNames(adm); !reflect.DeepEqual(got, map[string][]string{"charlie": {"alpha"}}) {
			t.Fatalf("run %d: deferred %v", i, got)
		}
		// The slot cap stops the walk in name order, before later candidates.
		if got := names(g.Runnable(nil, 2).Admit); !reflect.DeepEqual(got, []string{"alpha", "bravo"}) {
			t.Fatalf("run %d: 2 slots admitted %v", i, got)
		}
	}
}

// Push tasks join the commit queue directly and never take a task slot.
func TestRunnableSkipsPushTasks(t *testing.T) {
	g := loadHinted(t,
		hinted{name: "a-push", status: task.StatusReady, source: &task.Source{Kind: task.SourceKindPush}},
		hinted{name: "b", status: task.StatusReady, touches: touches("wm/b/**")},
	)
	adm := g.Runnable(nil, 2)
	if got := names(adm.Admit); !reflect.DeepEqual(got, []string{"b"}) {
		t.Errorf("admitted %v, want [b]", got)
	}
	if len(adm.Deferred) != 0 {
		t.Errorf("deferred %v", deferredNames(adm))
	}
}
