package tui

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/slimslenderslacks/work/internal/policy"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/task"
)

// ProjectView is the read-only snapshot of a single project that the TUI's
// projects gallery renders. Construction goes through ScanProjects; callers
// never build this struct directly from a .project.yaml.
type ProjectView struct {
	// Name is the basename of the directory containing the .project.yaml.
	// Projects don't carry a name field on disk, so the directory name is
	// the closest stable identifier the user already recognises.
	Name string
	// Path is the absolute path to the .project.yaml file. Used as the
	// stable key when diffing snapshots.
	Path        string
	Description string
	Branch      string
	Status      project.Status
	Repos       []project.Repo
	// TaskCounts holds the number of tasks observed in each task.Status. A
	// status with zero tasks is omitted from the map.
	TaskCounts map[task.Status]int
	// Tasks is the full list of tasks for the project, ordered by run order:
	// completed tasks first (earliest completion at the top), then tasks that
	// haven't completed in taskgraph (alphabetical) order. Populated in the
	// same scan pass that computes TaskCounts so the Tasks pane doesn't have
	// to re-walk disk.
	Tasks []TaskView
	// LastUpdate is the mtime of the .project.yaml file at scan time. Diff
	// logic uses it to detect changes even if the project's structured
	// fields are unchanged.
	LastUpdate time.Time
	// CreatedAt mirrors the project file's `created_at` field, stamped by
	// the daemon the first time it observed the populated project. Zero
	// for projects created before the field existed; those sort last.
	CreatedAt time.Time
}

// TaskView is the snapshot the Tasks pane renders for one task: name, model,
// per-task sandbox MCPs, policy rules, and current status, plus the path to
// the source YAML so the viewer can render it when the pane is focused. The
// slice fields make TaskView not directly comparable (==), so projectViewEqual
// compares them element-wise.
type TaskView struct {
	Name       string
	Model      string
	StaticMCPs []string
	Policies   []policy.Rule
	Status     task.Status
	Path       string
	// CompletedAt mirrors the task file's `completed_at` — when the daemon
	// observed the task reach `committed`. Zero for tasks that haven't
	// committed (or committed before the field existed). The Tasks pane sorts
	// by it so completed tasks appear in the order they actually ran.
	CompletedAt time.Time
}

// ScanProjects walks each root for .project.yaml files and returns a snapshot
// of every project it can load, sorted by path for determinism. Individual
// project- or task-file load failures are skipped rather than aborting the
// whole scan: a half-written file on disk shouldn't blank the gallery.
// A walk error on a root (e.g. root does not exist) is surfaced.
func ScanProjects(roots []string) ([]ProjectView, error) {
	var views []ProjectView
	seen := map[string]struct{}{}

	for _, root := range roots {
		err := filepath.WalkDir(root, func(p string, entry fs.DirEntry, err error) error {
			if err != nil {
				if entry != nil && entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if entry.IsDir() || filepath.Base(p) != ".project.yaml" {
				return nil
			}
			abs, absErr := filepath.Abs(p)
			if absErr != nil {
				abs = p
			}
			if _, dup := seen[abs]; dup {
				return nil
			}
			seen[abs] = struct{}{}
			if v, ok := loadProjectView(abs); ok {
				views = append(views, v)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	sort.Slice(views, func(i, j int) bool {
		ai, aj := views[i].CreatedAt, views[j].CreatedAt
		if !ai.IsZero() && !aj.IsZero() {
			if !ai.Equal(aj) {
				return ai.After(aj) // most recently created first
			}
		} else if !ai.IsZero() {
			return true
		} else if !aj.IsZero() {
			return false
		}
		// Tie-break (and stable fallback for un-stamped projects) on path.
		return views[i].Path < views[j].Path
	})
	return views, nil
}

func loadProjectView(path string) (ProjectView, bool) {
	pr, err := project.Load(path)
	if err != nil {
		return ProjectView{}, false
	}
	var mtime time.Time
	if info, err := os.Stat(path); err == nil {
		mtime = info.ModTime()
	}
	counts, tasks := tasksFor(filepath.Join(filepath.Dir(path), "tasks"))
	var createdAt time.Time
	if pr.CreatedAt != nil {
		createdAt = *pr.CreatedAt
	}
	return ProjectView{
		Name:        filepath.Base(filepath.Dir(path)),
		Path:        path,
		Description: pr.Description,
		Branch:      pr.Branch,
		Status:      pr.Status,
		Repos:       append([]project.Repo(nil), pr.Repos...),
		TaskCounts:  counts,
		Tasks:       tasks,
		LastUpdate:  mtime,
		CreatedAt:   createdAt,
	}, true
}

// tasksFor reads a project's tasks/ directory and returns both the per-status
// counts and the ordered list of task views. Both are derived from the same
// scan so a project that has 12 tasks doesn't pay for the YAML decode twice.
//
// This is deliberately NOT taskgraph.Load: that loader is strict (it aborts on
// the first task with an empty name, a duplicate name, or a bad dependency),
// which is right for the daemon but wrong for display. A single offending file
// must not blank the whole pane. In particular `:task` seeds a task with an
// empty name — the signal the planning agent keys off — and the pane must keep
// showing the project's existing tasks (plus the new seed) while planning runs
// to fill the name in, not wipe every task until it does. So we load each file
// independently, skip any that fail to parse (a half-written file on disk), and
// give an unnamed seed a display name from its filename stem so it shows up
// immediately as a pending row. Mirrors ScanProjects' per-file resilience.
func tasksFor(tasksDir string) (map[task.Status]int, []TaskView) {
	counts := map[task.Status]int{}
	entries, err := os.ReadDir(tasksDir)
	if err != nil {
		return counts, nil
	}
	tasks := make([]TaskView, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		path := filepath.Join(tasksDir, e.Name())
		t, err := task.Load(path)
		if err != nil {
			continue
		}
		counts[t.Status]++
		// task.Load() backfills Model to "default", but mirror the defaulting
		// here for display safety in case a task reaches us another way.
		model := t.Model
		if model == "" {
			model = task.ModelDefault
		}
		// An unnamed seed (just written by `:task`, not yet named by planning)
		// still gets a row — under its filename stem — so the user sees their
		// new task land instead of the pane appearing to lose everything.
		name := t.Name
		if name == "" {
			name = strings.TrimSuffix(e.Name(), ".yaml")
		}
		var completedAt time.Time
		if t.CompletedAt != nil {
			completedAt = *t.CompletedAt
		}
		tasks = append(tasks, TaskView{
			Name:        name,
			Model:       model,
			StaticMCPs:  append([]string(nil), t.StaticMCPs...),
			Policies:    append([]policy.Rule(nil), t.Policies...),
			Status:      t.Status,
			Path:        t.Path,
			CompletedAt: completedAt,
		})
	}
	if len(tasks) == 0 {
		return counts, nil
	}
	// ReadDir returns entries sorted by name, so tasks start in stable
	// alphabetical order. Order the pane by run/completion order on top of that:
	// tasks that have completed come first, earliest completion at the top;
	// tasks that haven't completed keep the alphabetical order below, via the
	// stable sort.
	sort.SliceStable(tasks, func(i, j int) bool {
		ci, cj := tasks[i].CompletedAt, tasks[j].CompletedAt
		switch {
		case !ci.IsZero() && !cj.IsZero():
			return ci.Before(cj)
		case !ci.IsZero():
			return true
		case !cj.IsZero():
			return false
		default:
			return false
		}
	})
	return counts, tasks
}

// WatchProjects polls roots on interval and emits a snapshot whenever the
// result differs from the previous one. The first snapshot is emitted
// immediately. The channel closes when ctx is cancelled. interval <= 0 falls
// back to one second.
//
// The channel is unbuffered: a slow consumer applies backpressure to the
// poller, which is the behaviour we want — a TUI that can't keep up has
// bigger problems than missed snapshots.
func WatchProjects(ctx context.Context, roots []string, interval time.Duration) <-chan []ProjectView {
	if interval <= 0 {
		interval = time.Second
	}
	out := make(chan []ProjectView)
	go func() {
		defer close(out)
		snap, err := ScanProjects(roots)
		if err != nil {
			snap = nil
		}
		select {
		case out <- snap:
		case <-ctx.Done():
			return
		}
		prev := snap
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				snap, err := ScanProjects(roots)
				if err != nil {
					continue
				}
				if projectViewsEqual(prev, snap) {
					continue
				}
				prev = snap
				select {
				case out <- snap:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

func projectViewsEqual(a, b []ProjectView) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !projectViewEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

// reconcileProjectSelection returns the project path the gallery should keep
// highlighted after a snapshot refresh. Mirrors reconcileSelection (sessions)
// so the projects pane stays stable across live updates.
func reconcileProjectSelection(views []ProjectView, prevPath string) string {
	if len(views) == 0 {
		return ""
	}
	if prevPath != "" {
		for _, v := range views {
			if v.Path == prevPath {
				return prevPath
			}
		}
	}
	return views[0].Path
}

// moveProjectSelection shifts the project selection by delta cards, clamped
// to the list bounds.
func moveProjectSelection(views []ProjectView, currentPath string, delta int) string {
	if len(views) == 0 {
		return ""
	}
	idx := -1
	for i, v := range views {
		if v.Path == currentPath {
			idx = i
			break
		}
	}
	if idx < 0 {
		if delta >= 0 {
			return views[0].Path
		}
		return views[len(views)-1].Path
	}
	idx += delta
	if idx < 0 {
		idx = 0
	}
	if idx >= len(views) {
		idx = len(views) - 1
	}
	return views[idx].Path
}

// reconcileTaskSelection mirrors reconcileProjectSelection for the Tasks
// pane: it keeps prevPath highlighted across snapshot refreshes when the
// task still exists, falls back to the first task when it doesn't, and
// returns "" when the list is empty.
func reconcileTaskSelection(tasks []TaskView, prevPath string) string {
	if len(tasks) == 0 {
		return ""
	}
	if prevPath != "" {
		for _, t := range tasks {
			if t.Path == prevPath {
				return prevPath
			}
		}
	}
	return tasks[0].Path
}

// moveTaskSelection shifts the task selection by delta rows, clamped to the
// list bounds. Mirrors moveProjectSelection.
func moveTaskSelection(tasks []TaskView, currentPath string, delta int) string {
	if len(tasks) == 0 {
		return ""
	}
	idx := -1
	for i, t := range tasks {
		if t.Path == currentPath {
			idx = i
			break
		}
	}
	if idx < 0 {
		if delta >= 0 {
			return tasks[0].Path
		}
		return tasks[len(tasks)-1].Path
	}
	idx += delta
	if idx < 0 {
		idx = 0
	}
	if idx >= len(tasks) {
		idx = len(tasks) - 1
	}
	return tasks[idx].Path
}

func projectViewEqual(a, b ProjectView) bool {
	if a.Name != b.Name || a.Path != b.Path ||
		a.Description != b.Description || a.Branch != b.Branch ||
		a.Status != b.Status || !a.LastUpdate.Equal(b.LastUpdate) {
		return false
	}
	if len(a.Repos) != len(b.Repos) {
		return false
	}
	for i := range a.Repos {
		if a.Repos[i] != b.Repos[i] {
			return false
		}
	}
	if len(a.TaskCounts) != len(b.TaskCounts) {
		return false
	}
	for k, v := range a.TaskCounts {
		if b.TaskCounts[k] != v {
			return false
		}
	}
	if len(a.Tasks) != len(b.Tasks) {
		return false
	}
	for i := range a.Tasks {
		if !taskViewEqual(a.Tasks[i], b.Tasks[i]) {
			return false
		}
	}
	return true
}

// taskViewEqual compares two TaskViews including their slice fields. Used by
// projectViewEqual now that TaskView holds StaticMCPs/Policies and is no
// longer directly comparable with ==. Order matters for both slices since
// rule order is meaningful and MCP order is what the planner wrote.
func taskViewEqual(a, b TaskView) bool {
	if a.Name != b.Name || a.Model != b.Model || a.Status != b.Status || a.Path != b.Path ||
		!a.CompletedAt.Equal(b.CompletedAt) {
		return false
	}
	if len(a.StaticMCPs) != len(b.StaticMCPs) {
		return false
	}
	for i := range a.StaticMCPs {
		if a.StaticMCPs[i] != b.StaticMCPs[i] {
			return false
		}
	}
	if len(a.Policies) != len(b.Policies) {
		return false
	}
	for i := range a.Policies {
		if a.Policies[i] != b.Policies[i] {
			return false
		}
	}
	return true
}
