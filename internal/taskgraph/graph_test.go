package taskgraph

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/task"
)

// writeTask is a convenience to seed a tasks/ directory in tests.
func writeTask(t *testing.T, dir, name string, status task.Status, deps ...string) {
	t.Helper()
	tk := &task.Task{
		Name:      name,
		Status:    status,
		DependsOn: deps,
	}
	if err := task.Save(filepath.Join(dir, name+".yaml"), tk); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

func TestLoadMissingDirReturnsEmptyGraph(t *testing.T) {
	g, err := Load(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !g.Empty() {
		t.Errorf("expected empty graph, got %d tasks", len(g.Tasks()))
	}
	if g.AllCommitted() {
		t.Errorf("empty graph must not be AllCommitted")
	}
}

func TestLoadExamples(t *testing.T) {
	g, err := Load("../../examples/tasks")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(g.Tasks()) != 2 {
		t.Fatalf("got %d tasks, want 2", len(g.Tasks()))
	}
	if g.Task("add-healthz-handler") == nil || g.Task("add-readiness-probe") == nil {
		t.Errorf("expected both task names: %v", g.Tasks())
	}
	// add-healthz-handler has no deps and is ready → in Ready().
	// add-readiness-probe depends on add-healthz-handler, which is NOT committed.
	ready := g.Ready()
	if len(ready) != 1 || ready[0].Name != "add-healthz-handler" {
		t.Errorf("Ready = %v, want only add-healthz-handler", names(ready))
	}
}

func TestReadyAdvancesWhenDepCommitted(t *testing.T) {
	dir := t.TempDir()
	writeTask(t, dir, "a", task.StatusCommitted)
	writeTask(t, dir, "b", task.StatusReady, "a")

	g, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ready := g.Ready()
	if len(ready) != 1 || ready[0].Name != "b" {
		t.Errorf("Ready = %v, want [b]", names(ready))
	}
}

func TestRunningTaskNotInReady(t *testing.T) {
	dir := t.TempDir()
	writeTask(t, dir, "a", task.StatusRunning)
	g, _ := Load(dir)
	if len(g.Ready()) != 0 {
		t.Errorf("running task should not be in Ready, got %v", names(g.Ready()))
	}
}

func TestAllCommitted(t *testing.T) {
	dir := t.TempDir()
	writeTask(t, dir, "a", task.StatusCommitted)
	writeTask(t, dir, "b", task.StatusCommitted, "a")
	g, _ := Load(dir)
	if !g.AllCommitted() {
		t.Errorf("expected AllCommitted = true")
	}

	// Flip one back to ready and reload — no longer all-committed.
	writeTask(t, dir, "b", task.StatusReady, "a")
	g, _ = Load(dir)
	if g.AllCommitted() {
		t.Errorf("expected AllCommitted = false after status flip")
	}
}

func TestCycleDetection(t *testing.T) {
	dir := t.TempDir()
	writeTask(t, dir, "a", task.StatusReady, "b")
	writeTask(t, dir, "b", task.StatusReady, "a")
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("expected cycle error, got %v", err)
	}
}

func TestSelfCycle(t *testing.T) {
	dir := t.TempDir()
	writeTask(t, dir, "a", task.StatusReady, "a")
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("expected cycle error, got %v", err)
	}
}

func TestUnknownDependency(t *testing.T) {
	dir := t.TempDir()
	writeTask(t, dir, "a", task.StatusReady, "nonexistent")
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "unknown task") {
		t.Errorf("expected unknown-task error, got %v", err)
	}
}

func TestDuplicateTaskNamesRejected(t *testing.T) {
	dir := t.TempDir()
	// Two files, same `name:` field.
	tk := &task.Task{Name: "shared", Status: task.StatusReady}
	if err := task.Save(filepath.Join(dir, "01.yaml"), tk); err != nil {
		t.Fatal(err)
	}
	if err := task.Save(filepath.Join(dir, "02.yaml"), tk); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("expected duplicate-name error, got %v", err)
	}
}

func TestNonYamlFilesIgnored(t *testing.T) {
	dir := t.TempDir()
	writeTask(t, dir, "a", task.StatusReady)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	g, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(g.Tasks()) != 1 {
		t.Errorf("expected 1 task, got %d", len(g.Tasks()))
	}
}

func TestMalformedYamlReturnsError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "broken.yaml"), []byte("name: x\nstatus: bogus\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Errorf("expected error for invalid status")
	}
}

func names(ts []*task.Task) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name
	}
	return out
}
