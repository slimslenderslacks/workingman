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

// Graph is an immutable snapshot of a project's task DAG.
type Graph struct {
	dir   string
	tasks map[string]*task.Task // keyed by task name
	deps  map[string][]string   // adjacency: name → names it depends on
}

// Dir returns the directory the Graph was loaded from.
func (g *Graph) Dir() string { return g.dir }

// Load reads every *.yaml file in dir into a Graph and validates the result.
// If dir does not exist, Load returns an empty Graph and no error — the
// daemon treats a missing tasks/ as "planning hasn't produced anything yet."
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

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		t, err := task.Load(path)
		if err != nil {
			return nil, fmt.Errorf("taskgraph: load %s: %w", path, err)
		}
		if t.Name == "" {
			return nil, fmt.Errorf("taskgraph: %s has no name", path)
		}
		if _, dup := g.tasks[t.Name]; dup {
			return nil, fmt.Errorf("taskgraph: duplicate task name %q in %s", t.Name, dir)
		}
		g.tasks[t.Name] = t
		g.deps[t.Name] = append([]string(nil), t.DependsOn...)
	}

	if err := g.validate(); err != nil {
		return nil, err
	}
	return g, nil
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
