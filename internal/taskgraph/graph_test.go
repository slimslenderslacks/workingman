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

func TestOverlongNameRepaired(t *testing.T) {
	dir := t.TempDir()
	// Exactly at the limit loads untouched, with no warning.
	writeTask(t, dir, strings.Repeat("a", MaxNameLen), task.StatusReady)
	g, err := Load(dir)
	if err != nil {
		t.Fatalf("name of exactly %d chars should load, got %v", MaxNameLen, err)
	}
	if len(g.Warnings()) != 0 {
		t.Errorf("expected no warnings for a name at the limit, got %v", g.Warnings())
	}

	// One char over the limit no longer aborts the whole graph: the name is
	// truncated to fit and the repair is recorded as a loud warning.
	dir2 := t.TempDir()
	overlong := strings.Repeat("a", MaxNameLen+1)
	// task.Save refuses an over-length name by design, so write the YAML raw to
	// simulate a file the planning agent authored directly (which is the only
	// way an over-length name reaches disk) and confirm Load repairs it.
	if err := os.WriteFile(filepath.Join(dir2, overlong+".yaml"),
		[]byte("name: "+overlong+"\nstatus: ready\n"), 0o644); err != nil {
		t.Fatalf("write raw task: %v", err)
	}
	g2, err := Load(dir2)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(g2.Tasks()) != 1 {
		t.Fatalf("got %d tasks, want 1", len(g2.Tasks()))
	}
	got := g2.Tasks()[0]
	if len(got.Name) > MaxNameLen {
		t.Errorf("repaired name %q is %d chars, want <= %d", got.Name, len(got.Name), MaxNameLen)
	}
	if len(g2.Warnings()) != 1 {
		t.Errorf("expected exactly 1 warning for the repaired overlong name, got %v", g2.Warnings())
	}
}

func TestBlankNameWithoutDescriptionRepairedFromFilename(t *testing.T) {
	dir := t.TempDir()
	tk := &task.Task{Status: task.StatusReady} // no Name, no Description
	path := filepath.Join(dir, "00-mystery-task.yaml")
	if err := task.Save(path, tk); err != nil {
		t.Fatalf("Save: %v", err)
	}
	g, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(g.Tasks()) != 1 {
		t.Fatalf("got %d tasks, want 1", len(g.Tasks()))
	}
	if got := g.Tasks()[0].Name; got != "00-mystery-task" {
		t.Errorf("Name = %q, want %q (derived from filename)", got, "00-mystery-task")
	}
	if len(g.Warnings()) != 1 {
		t.Errorf("expected exactly 1 warning, got %v", g.Warnings())
	}
}

func TestBlankNameWithDescriptionSkippedAsSeed(t *testing.T) {
	dir := t.TempDir()
	seed := &task.Task{Description: "do the thing a human asked for", Status: task.StatusReady}
	if err := task.Save(filepath.Join(dir, "seed.yaml"), seed); err != nil {
		t.Fatalf("Save: %v", err)
	}
	writeTask(t, dir, "existing", task.StatusReady)

	g, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(g.Tasks()) != 1 || g.Task("existing") == nil {
		t.Errorf("expected only the named task, got %v", names(g.Tasks()))
	}
	if len(g.Warnings()) != 0 {
		t.Errorf("a pending seed is not a repair and should not warn, got %v", g.Warnings())
	}
}

func TestNonSlugNameRepaired(t *testing.T) {
	dir := t.TempDir()
	tk := &task.Task{Name: "Add Healthz Handler!", Status: task.StatusReady}
	if err := task.Save(filepath.Join(dir, "task.yaml"), tk); err != nil {
		t.Fatalf("Save: %v", err)
	}
	g, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(g.Tasks()) != 1 {
		t.Fatalf("got %d tasks, want 1", len(g.Tasks()))
	}
	if got := g.Tasks()[0].Name; got != "add-healthz-handler" {
		t.Errorf("Name = %q, want %q", got, "add-healthz-handler")
	}
	if len(g.Warnings()) != 1 {
		t.Errorf("expected exactly 1 warning, got %v", g.Warnings())
	}
}

func TestRepairedNameDedupedAgainstExisting(t *testing.T) {
	dir := t.TempDir()
	writeTask(t, dir, "add-healthz-handler", task.StatusReady)
	tk := &task.Task{Name: "Add Healthz Handler!!", Status: task.StatusReady}
	if err := task.Save(filepath.Join(dir, "dup-seed.yaml"), tk); err != nil {
		t.Fatalf("Save: %v", err)
	}
	g, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(g.Tasks()) != 2 {
		t.Fatalf("got %d tasks, want 2", len(g.Tasks()))
	}
	if g.Task("add-healthz-handler") == nil || g.Task("add-healthz-handler-2") == nil {
		t.Errorf("expected deduped names, got %v", names(g.Tasks()))
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
