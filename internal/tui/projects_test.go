package tui

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/task"
)

func TestScanProjectsReturnsViews(t *testing.T) {
	root := t.TempDir()

	alphaDir := filepath.Join(root, "alpha")
	if err := os.MkdirAll(filepath.Join(alphaDir, "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	alphaPath := filepath.Join(alphaDir, ".project.yaml")
	if err := project.SaveAs(alphaPath, &project.Project{
		Description: "alpha description",
		Branch:      "feat/alpha",
		Status:      project.StatusWorking,
		Repos:       []project.Repo{{Org: "acme", Name: "alpha"}},
	}, project.WriterAgent); err != nil {
		t.Fatal(err)
	}
	writeTask(t, filepath.Join(alphaDir, "tasks", "01-foo.yaml"),
		&task.Task{Name: "foo", Status: task.StatusCommitted})
	writeTask(t, filepath.Join(alphaDir, "tasks", "02-bar.yaml"),
		&task.Task{Name: "bar", DependsOn: []string{"foo"}, Status: task.StatusReady})
	writeTask(t, filepath.Join(alphaDir, "tasks", "03-baz.yaml"),
		&task.Task{Name: "baz", Status: task.StatusRunning})

	bravoDir := filepath.Join(root, "bravo")
	if err := os.MkdirAll(bravoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bravoPath := filepath.Join(bravoDir, ".project.yaml")
	if err := project.SaveAs(bravoPath, &project.Project{
		Description: "bravo description",
		Branch:      "feat/bravo",
		Status:      project.StatusBlocked,
	}, project.WriterAgent); err != nil {
		t.Fatal(err)
	}

	views, err := ScanProjects([]string{root})
	if err != nil {
		t.Fatalf("ScanProjects: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("want 2 views, got %d: %+v", len(views), views)
	}

	alpha, bravo := views[0], views[1]
	if alpha.Name != "alpha" {
		t.Errorf("alpha.Name = %q, want %q", alpha.Name, "alpha")
	}
	if alpha.Path != alphaPath {
		t.Errorf("alpha.Path = %q, want %q", alpha.Path, alphaPath)
	}
	if alpha.Status != project.StatusWorking {
		t.Errorf("alpha.Status = %q, want %q", alpha.Status, project.StatusWorking)
	}
	if alpha.Description != "alpha description" {
		t.Errorf("alpha.Description = %q", alpha.Description)
	}
	if alpha.Branch != "feat/alpha" {
		t.Errorf("alpha.Branch = %q", alpha.Branch)
	}
	if len(alpha.Repos) != 1 || alpha.Repos[0] != (project.Repo{Org: "acme", Name: "alpha"}) {
		t.Errorf("alpha.Repos = %+v", alpha.Repos)
	}
	wantCounts := map[task.Status]int{
		task.StatusCommitted: 1,
		task.StatusReady:     1,
		task.StatusRunning:   1,
	}
	if !taskCountsEqual(alpha.TaskCounts, wantCounts) {
		t.Errorf("alpha.TaskCounts = %v, want %v", alpha.TaskCounts, wantCounts)
	}
	info, err := os.Stat(alphaPath)
	if err != nil {
		t.Fatal(err)
	}
	if !alpha.LastUpdate.Equal(info.ModTime()) {
		t.Errorf("alpha.LastUpdate = %v, want %v", alpha.LastUpdate, info.ModTime())
	}

	if bravo.Name != "bravo" {
		t.Errorf("bravo.Name = %q", bravo.Name)
	}
	if bravo.Status != project.StatusBlocked {
		t.Errorf("bravo.Status = %q", bravo.Status)
	}
	if len(bravo.TaskCounts) != 0 {
		t.Errorf("bravo.TaskCounts non-empty: %v", bravo.TaskCounts)
	}
}

func TestScanProjectsSkipsBrokenAndDedupes(t *testing.T) {
	root := t.TempDir()

	goodDir := filepath.Join(root, "good")
	if err := os.MkdirAll(goodDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := project.SaveAs(filepath.Join(goodDir, ".project.yaml"), &project.Project{
		Description: "good",
		Status:      project.StatusReady,
	}, project.WriterAgent); err != nil {
		t.Fatal(err)
	}

	badDir := filepath.Join(root, "bad")
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, ".project.yaml"),
		[]byte("status: not-a-real-status\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Passing the same root twice must not double-report the good project.
	views, err := ScanProjects([]string{root, root})
	if err != nil {
		t.Fatalf("ScanProjects: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("want 1 view (bad skipped, dup elided), got %d: %+v", len(views), views)
	}
	if views[0].Name != "good" {
		t.Errorf("Name = %q, want %q", views[0].Name, "good")
	}
}

func TestWatchProjectsEmitsUpdates(t *testing.T) {
	root := t.TempDir()
	pDir := filepath.Join(root, "alpha")
	if err := os.MkdirAll(pDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pPath := filepath.Join(pDir, ".project.yaml")
	if err := project.SaveAs(pPath, &project.Project{
		Description: "alpha",
		Status:      project.StatusReady,
	}, project.WriterAgent); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := WatchProjects(ctx, []string{root}, 10*time.Millisecond)

	select {
	case snap := <-ch:
		if len(snap) != 1 || snap[0].Status != project.StatusReady {
			t.Fatalf("initial snapshot: %+v", snap)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no initial snapshot")
	}

	if err := project.SaveAs(pPath, &project.Project{
		Description: "alpha",
		Status:      project.StatusWorking,
	}, project.WriterAgent); err != nil {
		t.Fatal(err)
	}
	// Force a later mtime so the diff fires regardless of filesystem
	// timestamp resolution (HFS+/APFS round to the second on some setups).
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(pPath, later, later); err != nil {
		t.Fatal(err)
	}

	select {
	case snap := <-ch:
		if len(snap) != 1 || snap[0].Status != project.StatusWorking {
			t.Fatalf("update snapshot: %+v", snap)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no update snapshot")
	}
}

func TestWatchProjectsClosesOnCancel(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	ch := WatchProjects(ctx, []string{root}, 10*time.Millisecond)
	// Drain the initial empty snapshot so the goroutine reaches its select.
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("no initial snapshot")
	}
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			// A late snapshot is allowed, but the channel must close shortly.
			select {
			case _, ok := <-ch:
				if ok {
					t.Fatal("channel did not close after cancel")
				}
			case <-time.After(time.Second):
				t.Fatal("channel did not close after cancel")
			}
		}
	case <-time.After(time.Second):
		t.Fatal("channel did not close after cancel")
	}
}

func writeTask(t *testing.T, path string, tk *task.Task) {
	t.Helper()
	if err := task.Save(path, tk); err != nil {
		t.Fatal(err)
	}
}

func taskCountsEqual(a, b map[task.Status]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
