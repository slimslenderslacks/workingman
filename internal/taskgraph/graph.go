// Package taskgraph turns a directory of tasks/*.yaml files into a queryable
// DAG. It is a pure-function snapshot of disk state: Load reads everything
// in, validates it, and returns an immutable Graph. The daemon calls Load
// each time it wants a fresh view (after a tasks/*.yaml fsnotify event) and
// uses Ready() / AllCommitted() to decide what to do next.
//
// Validation is strict: any unknown dependency, dependency cycle, or
// duplicate task name aborts Load with an error. The daemon should surface
// those errors via the audit log and (probably) move the project to blocked.
package taskgraph

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/slimslenderslacks/work/internal/task"
)

// MaxNameLen is the maximum length of a task name. Re-exported from the task
// package (the canonical source) so existing callers of taskgraph.MaxNameLen
// keep working.
const MaxNameLen = task.MaxNameLen

// Graph is an immutable snapshot of a project's task DAG.
type Graph struct {
	dir      string
	tasks    map[string]*task.Task // keyed by task name
	deps     map[string][]string   // adjacency: name → names it depends on
	warnings []string              // loud, non-fatal repairs made during Load
	seeds    int                   // pending seeds (blank name + description) awaiting planning
}

// Dir returns the directory the Graph was loaded from.
func (g *Graph) Dir() string { return g.dir }

// Warnings returns a loud, human-readable record of every non-fatal repair
// Load made — e.g. a blank or malformed task name that was rewritten to a
// derived slug. Callers should surface these (audit log, TUI banner, etc.)
// even though they didn't prevent the graph from loading: a repaired name
// means an upstream writer (usually the planning agent) produced something
// invalid, and that's worth someone's attention. Empty when Load made no
// repairs.
func (g *Graph) Warnings() []string { return g.warnings }

// Load reads every *.yaml file in dir into a Graph and validates the result.
// If dir does not exist, Load returns an empty Graph and no error — the
// daemon treats a missing tasks/ as "planning hasn't produced anything yet."
//
// A task file's name is repaired rather than rejected when it's blank or
// malformed (not a lowercase kebab-case slug, or over MaxNameLen): Load
// derives a slug from the name itself (or, if it was blank, from the
// filename) and records the repair in Warnings(). This is deliberate — a
// single planning-agent mistake in one task file must not abort loading
// every other task in the project and strand it. Structural problems that
// span the whole graph (an unknown dependency, a cycle, or two files
// genuinely claiming the same valid name) remain fatal: there's no safe
// single-file repair for those.
//
// A blank name paired with a non-blank description is treated as a pending
// seed (see the tui package's queueTaskForPlanning) rather than a malformed
// task — it's skipped from the graph entirely so it neither errors nor
// occupies a slot until the planning agent gives it a real name.
func Load(dir string) (*Graph, error) {
	g := &Graph{
		dir:   dir,
		tasks: map[string]*task.Task{},
		deps:  map[string][]string{},
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return g, nil
		}
		return nil, fmt.Errorf("taskgraph: read %s: %w", dir, err)
	}

	// renamed records original-name → repaired-name for every task whose name we
	// slugged. depends_on entries are written against the ORIGINAL names, so once
	// the loop below has repaired the nodes we rewrite the edges through this map
	// — otherwise a single invalid name (e.g. `select_pull_models`, which slugs to
	// `select-pull-models`) leaves every dependent pointing at a name that no
	// longer exists and validate() fails with "depends on unknown task". Repairing
	// the node but not the edge is exactly the stranding the repair exists to
	// prevent. Only non-blank originals are recorded: a blank name repaired from
	// its filename was never a valid dependency target, so nothing references it.
	renamed := map[string]string{}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		t, err := task.Load(path)
		if err != nil {
			return nil, fmt.Errorf("taskgraph: load %s: %w", path, err)
		}

		if t.Name == "" && strings.TrimSpace(t.Description) != "" {
			// A pending seed awaiting the planning agent, not yet a task.
			g.seeds++
			continue
		}

		name := t.Name
		if !task.ValidName(name) {
			var repaired string
			if name == "" {
				repaired = task.Slugify(strings.TrimSuffix(e.Name(), ".yaml"))
			} else {
				repaired = task.Slugify(name)
			}
			repaired = dedupeName(repaired, g.tasks)
			g.warnings = append(g.warnings, fmt.Sprintf(
				"taskgraph: %s had invalid name %q; repaired to %q", path, name, repaired))
			if name != "" {
				renamed[name] = repaired
			}
			name = repaired
			t.Name = repaired
		}

		if _, dup := g.tasks[name]; dup {
			return nil, fmt.Errorf("taskgraph: duplicate task name %q in %s", name, dir)
		}
		g.tasks[name] = t
		g.deps[name] = append([]string(nil), t.DependsOn...)
	}

	// Carry the name repairs into the dependency edges. A dep naming a repaired
	// task is rewritten to that task's repaired name; a dep already naming a valid
	// task is left untouched. Both the graph's edge list and the task's own
	// DependsOn are updated so downstream reads (Ready/depsCommitted and any
	// consumer of Task.DependsOn) agree with the repaired node names.
	if len(renamed) > 0 {
		for name, deps := range g.deps {
			for i, dep := range deps {
				if to, ok := renamed[dep]; ok {
					deps[i] = to
				}
			}
			if t := g.tasks[name]; t != nil {
				t.DependsOn = append([]string(nil), deps...)
			}
		}
	}

	if err := g.validate(); err != nil {
		return nil, err
	}
	return g, nil
}

// dedupeName returns name, or name-2 / name-3 / ... — the first candidate
// not already present in tasks. Only invoked for a name Load just repaired,
// so a repair that happens to collide with an existing task gets a stable,
// distinct identity instead of silently colliding; two files that both
// carry the same already-valid name are a separate, fatal condition (see
// the duplicate check in Load).
func dedupeName(name string, tasks map[string]*task.Task) string {
	if _, exists := tasks[name]; !exists {
		return name
	}
	for i := 2; ; i++ {
		suffix := fmt.Sprintf("-%d", i)
		base := name
		if over := len(base) + len(suffix) - task.MaxNameLen; over > 0 {
			base = base[:len(base)-over]
		}
		candidate := base + suffix
		if _, exists := tasks[candidate]; !exists {
			return candidate
		}
	}
}

// Tasks returns every task in the graph, sorted by name. Stable order makes
// audit logs and tests reproducible.
func (g *Graph) Tasks() []*task.Task {
	names := make([]string, 0, len(g.tasks))
	for n := range g.tasks {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]*task.Task, len(names))
	for i, n := range names {
		out[i] = g.tasks[n]
	}
	return out
}

// Task returns the task with the given name, or nil.
func (g *Graph) Task(name string) *task.Task { return g.tasks[name] }

// Ready returns tasks whose status is `ready` AND every dependency is
// `committed`. Order is by name for determinism.
//
// A task that has already started running or has finished does not appear in
// Ready, regardless of its dependencies — the daemon's job is to advance it
// through its own state machine, not to relaunch it.
func (g *Graph) Ready() []*task.Task {
	out := []*task.Task{}
	for _, t := range g.Tasks() {
		if t.Status != task.StatusReady {
			continue
		}
		if g.depsCommitted(t.Name) {
			out = append(out, t)
		}
	}
	return out
}

// AllCommitted is true when the graph has at least one task and every task
// is in StatusCommitted. The daemon uses this signal to transition the
// project to done. The "at least one" guard prevents an empty tasks/ dir
// (planning still pending) from spuriously satisfying the predicate.
func (g *Graph) AllCommitted() bool {
	if len(g.tasks) == 0 {
		return false
	}
	for _, t := range g.tasks {
		if t.Status != task.StatusCommitted {
			return false
		}
	}
	return true
}

// Empty reports whether the graph contains no tasks. Useful for the daemon
// to distinguish "planning hasn't run yet" from "tasks exist".
func (g *Graph) Empty() bool { return len(g.tasks) == 0 }

// HasPendingSeed reports whether the tasks dir holds at least one pending seed:
// a task file with a blank name and a non-blank description, written by the
// daemon's intake handler as the signal for the planning agent to flesh it
// into a real task. Seeds are deliberately excluded from the graph itself
// (they have no name and no place in the DAG yet), so this is the only way to
// tell the daemon "there is unplanned work here." The daemon uses it to
// re-arm planning for a project whose seed was orphaned — its status flip to
// `ready` was skipped or clobbered while another agent held the project slot.
func (g *Graph) HasPendingSeed() bool { return g.seeds > 0 }

func (g *Graph) depsCommitted(name string) bool {
	for _, dep := range g.deps[name] {
		if g.tasks[dep].Status != task.StatusCommitted {
			return false
		}
	}
	return true
}

func (g *Graph) validate() error {
	// Every named dependency must exist.
	for name, deps := range g.deps {
		for _, dep := range deps {
			if _, ok := g.tasks[dep]; !ok {
				return fmt.Errorf("taskgraph: task %q depends on unknown task %q", name, dep)
			}
		}
	}

	// Cycle detection via DFS with three colours: white (unvisited), gray
	// (on current path), black (fully explored).
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(g.tasks))
	var visit func(name string, path []string) error
	visit = func(name string, path []string) error {
		switch color[name] {
		case gray:
			return fmt.Errorf("taskgraph: dependency cycle: %s", formatCycle(path, name))
		case black:
			return nil
		}
		color[name] = gray
		path = append(path, name)
		for _, dep := range g.deps[name] {
			if err := visit(dep, path); err != nil {
				return err
			}
		}
		color[name] = black
		return nil
	}
	for name := range g.tasks {
		if color[name] == white {
			if err := visit(name, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

func formatCycle(path []string, closer string) string {
	for i, n := range path {
		if n == closer {
			cycle := append(path[i:], closer)
			return strings.Join(cycle, " -> ")
		}
	}
	return closer
}
