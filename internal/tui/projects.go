package tui

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/task"
	"github.com/slimslenderslacks/work/internal/taskgraph"
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
	// Tasks is the full list of tasks for the project, in the order
	// taskgraph.Tasks returns them (alphabetical by name). Populated in
	// the same scan pass that computes TaskCounts so the Tasks pane
	// doesn't have to re-walk disk.
	Tasks []TaskView
	// LastUpdate is the mtime of the .project.yaml file at scan time. Diff
	// logic uses it to detect changes even if the project's structured
	// fields are unchanged.
	LastUpdate time.Time
}

// TaskView is the minimal snapshot the Tasks pane renders: a task's name
// and its current status. Kept narrow so the diff logic in projectViewEqual
// stays cheap and a future "skipped" status (see workingman#1) plugs in
// without churn.
type TaskView struct {
	Name   string
	Status task.Status
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

	sort.Slice(views, func(i, j int) bool { return views[i].Path < views[j].Path })
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
	}, true
}

// tasksFor loads the taskgraph and returns both the per-status counts and
// the ordered list of task views. Both are derived from the same scan so a
// project that has 12 tasks doesn't pay for the YAML decode twice.
func tasksFor(tasksDir string) (map[task.Status]int, []TaskView) {
	counts := map[task.Status]int{}
	g, err := taskgraph.Load(tasksDir)
	if err != nil || g.Empty() {
		return counts, nil
	}
	gTasks := g.Tasks()
	tasks := make([]TaskView, 0, len(gTasks))
	for _, t := range gTasks {
		counts[t.Status]++
		tasks = append(tasks, TaskView{Name: t.Name, Status: t.Status})
	}
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
		if a.Tasks[i] != b.Tasks[i] {
			return false
		}
	}
	return true
}
