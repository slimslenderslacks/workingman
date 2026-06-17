package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/task"
)

// TestTaskFileWatching verifies that edits to tasks/*.yaml files surface as
// audit entries. No Runner is configured: this is observation-only.
func TestTaskFileWatching(t *testing.T) {
	root := t.TempDir()
	buf, _ := startDaemon(t, root)

	// Create the tasks/ dir first and wait for the watch to install — same
	// race-avoidance pattern as TestNewDirectoryIsPickedUp.
	tasksDir := filepath.Join(root, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatalf("mkdir tasks: %v", err)
	}
	if ok, snap := waitFor(t, buf, "watch_added"); !ok {
		t.Fatalf("daemon never watched tasks dir.\naudit:\n%s", snap)
	}

	taskPath := filepath.Join(tasksDir, "first.yaml")
	if err := task.Save(taskPath, &task.Task{
		Name:   "first",
		Status: task.StatusReady,
	}); err != nil {
		t.Fatalf("Save initial: %v", err)
	}
	if ok, snap := waitFor(t, buf, "task_file_updated"); !ok {
		t.Fatalf("no task_file_updated for initial save.\naudit:\n%s", snap)
	}
	if !strings.Contains(buf.String(), "name=first") || !strings.Contains(buf.String(), "status=ready") {
		t.Errorf("audit missing fields:\n%s", buf.String())
	}

	// Subsequent status change should fire a second entry.
	if err := task.Save(taskPath, &task.Task{
		Name:     "first",
		Status:   task.StatusSuccess,
		Attempts: 2,
	}); err != nil {
		t.Fatalf("Save updated: %v", err)
	}
	// Wait for the distinguishing content of the second save rather than a count
	// of task_file_updated events: the initial save can emit two events (fsnotify
	// CREATE + WRITE for a newly created file), which would satisfy a count of 2
	// before the second save is ever processed and leave the assertions below
	// reading only the status=ready entries.
	if ok, snap := waitForWithin(t, buf, "status=success", 2*time.Second); !ok {
		t.Fatalf("no task_file_updated reflecting the status=success save.\naudit:\n%s", snap)
	}
	if !strings.Contains(buf.String(), "attempts=2") {
		t.Errorf("expected attempts=2 in audit:\n%s", buf.String())
	}
}

func TestNonTaskYamlInProjectRootIgnored(t *testing.T) {
	root := t.TempDir()
	buf, _ := startDaemon(t, root)

	// A yaml file in the project root that isn't .project.yaml and isn't in
	// tasks/ should not match handlerFor and should not appear in audit.
	if err := os.WriteFile(filepath.Join(root, "notes.yaml"), []byte("anything: here"), 0o644); err != nil {
		t.Fatalf("write notes: %v", err)
	}
	time.Sleep(250 * time.Millisecond)
	if strings.Contains(buf.String(), "notes.yaml") {
		t.Errorf("unrelated yaml triggered audit entry:\n%s", buf.String())
	}
}
