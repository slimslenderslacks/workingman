package taskgraph

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/sbx"
	"github.com/slimslenderslacks/work/internal/task"
)

// testWorkStream is the work-stream name every tasksDir below is created under.
// Load derives the per-task name budget from it (MaxNameLenFor), so tests must
// use the real "<work-stream>/tasks" layout rather than a bare t.TempDir() —
// otherwise the budget comes from Go's temp-dir name, which is both long and
// test-name dependent.
const testWorkStream = "proj"

// tasksDir returns a fresh "<tmp>/proj/tasks" directory, mirroring the on-disk
// layout the daemon always passes to Load.
func tasksDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), testWorkStream, "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	return dir
}

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
	dir := tasksDir(t)
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
	dir := tasksDir(t)
	writeTask(t, dir, "a", task.StatusRunning)
	g, _ := Load(dir)
	if len(g.Ready()) != 0 {
		t.Errorf("running task should not be in Ready, got %v", names(g.Ready()))
	}
}

func TestAllCommitted(t *testing.T) {
	dir := tasksDir(t)
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
	dir := tasksDir(t)
	writeTask(t, dir, "a", task.StatusReady, "b")
	writeTask(t, dir, "b", task.StatusReady, "a")
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("expected cycle error, got %v", err)
	}
}

func TestSelfCycle(t *testing.T) {
	dir := tasksDir(t)
	writeTask(t, dir, "a", task.StatusReady, "a")
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("expected cycle error, got %v", err)
	}
}

func TestUnknownDependency(t *testing.T) {
	dir := tasksDir(t)
	writeTask(t, dir, "a", task.StatusReady, "nonexistent")
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "unknown task") {
		t.Errorf("expected unknown-task error, got %v", err)
	}
}

func TestDuplicateTaskNamesRejected(t *testing.T) {
	dir := tasksDir(t)
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

// budget is the per-task name limit for the work stream the test fixtures use.
func budget() int { return task.MaxNameLenFor(testWorkStream) }

func TestOverlongNameRepaired(t *testing.T) {
	dir := tasksDir(t)
	// Exactly at the project's budget loads untouched, with no warning.
	writeTask(t, dir, strings.Repeat("a", budget()), task.StatusReady)
	g, err := Load(dir)
	if err != nil {
		t.Fatalf("name of exactly %d chars should load, got %v", budget(), err)
	}
	if len(g.Warnings()) != 0 {
		t.Errorf("expected no warnings for a name at the limit, got %v", g.Warnings())
	}

	// One char over the budget no longer aborts the whole graph: the name is
	// shortened to fit and the repair is recorded as a loud warning.
	dir2 := tasksDir(t)
	overlong := strings.Repeat("a", budget()+1)
	writeRawTask(t, dir2, overlong)
	g2, err := Load(dir2)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(g2.Tasks()) != 1 {
		t.Fatalf("got %d tasks, want 1", len(g2.Tasks()))
	}
	got := g2.Tasks()[0]
	if len(got.Name) > budget() {
		t.Errorf("repaired name %q is %d chars, want <= %d", got.Name, len(got.Name), budget())
	}
	if len(g2.Warnings()) != 1 {
		t.Errorf("expected exactly 1 warning for the repaired overlong name, got %v", g2.Warnings())
	}
}

// writeRawTask writes a task file as raw YAML. task.Save refuses an over-length
// name by design, so this is how a test simulates the file a planning or review
// agent authored directly — the only way such a name reaches disk.
func writeRawTask(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"),
		[]byte("name: "+name+"\nstatus: ready\n"), 0o644); err != nil {
		t.Fatalf("write raw task: %v", err)
	}
}

// TestNameWithinMaxNameLenButOverProjectBudgetRepaired is the regression test
// for the bug this budget exists to prevent: a task name that is individually
// legal (<= MaxNameLen) but, once prefixed with the work stream, produces a
// sandbox name longer than sbx accepts. That combination used to load clean and
// then fail `sbx create` on every single relaunch — the task could never run,
// and nothing in the task's own validation could see why.
func TestNameWithinMaxNameLenButOverProjectBudgetRepaired(t *testing.T) {
	dir := tasksDir(t)
	// Legal on its own, over budget once "proj-" is prepended.
	name := strings.Repeat("a", MaxNameLen)
	if !task.ValidName(name) {
		t.Fatalf("precondition: %q should pass the context-free ValidName", name)
	}
	writeRawTask(t, dir, name)

	g, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(g.Warnings()) != 1 {
		t.Errorf("expected 1 repair warning, got %v", g.Warnings())
	}
	got := g.Tasks()[0].Name
	if sandbox := testWorkStream + "-" + got; len(sandbox) > sbx.MaxNameLen {
		t.Errorf("sandbox name %q is %d chars, want <= %d", sandbox, len(sandbox), sbx.MaxNameLen)
	}
}

// TestOverlongNamesSharingAPrefixStayDistinct guards the other half of the
// repair: shortening must not merge two tasks into one identity. A plain
// truncation would give both of these the same name, at which point two tasks
// collide on one sandbox.
func TestOverlongNamesSharingAPrefixStayDistinct(t *testing.T) {
	dir := tasksDir(t)
	prefix := strings.Repeat("a", budget())
	writeRawTask(t, dir, prefix+"-one")
	writeRawTask(t, dir, prefix+"-two")

	g, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(g.Tasks()) != 2 {
		t.Fatalf("got %d tasks, want 2 (names collapsed onto each other?): %v", len(g.Tasks()), names(g.Tasks()))
	}
	for _, tk := range g.Tasks() {
		if len(tk.Name) > budget() {
			t.Errorf("repaired name %q is %d chars, want <= %d", tk.Name, len(tk.Name), budget())
		}
	}
}

func TestBlankNameWithoutDescriptionRepairedFromFilename(t *testing.T) {
	dir := tasksDir(t)
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
	dir := tasksDir(t)
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
	dir := tasksDir(t)
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
	dir := tasksDir(t)
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

// TestDependencyRepairedWhenNameSlugged is the regression for the opencode-dmr
// wolf-summon: the planning agent emitted underscore task names (invalid
// kebab-case) and referenced them, underscores and all, in depends_on. Repairing
// the node name to hyphens without repairing the edge left the dependent
// pointing at a name that no longer existed, so validate() failed with "depends
// on unknown task" and the project was stranded to the wolf. Load must carry the
// repair through the edges too.
func TestDependencyRepairedWhenNameSlugged(t *testing.T) {
	dir := tasksDir(t)
	if err := os.WriteFile(filepath.Join(dir, "a.yaml"),
		[]byte("name: select_pull_models\nstatus: committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.yaml"),
		[]byte("name: validate_integration\nstatus: ready\ndepends_on:\n  - select_pull_models\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	g, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Both nodes repaired to kebab-case.
	if g.Task("validate-integration") == nil || g.Task("select-pull-models") == nil {
		t.Fatalf("names not repaired: %v", names(g.Tasks()))
	}
	// The edge was carried along: the dep now names the repaired task, on both
	// the graph's edge list (via Ready/depsCommitted) and the task's own field.
	if got := g.Task("validate-integration").DependsOn; len(got) != 1 || got[0] != "select-pull-models" {
		t.Errorf("DependsOn = %v, want [select-pull-models]", got)
	}
	// Its committed dependency resolves, so the dependent is Ready.
	if ready := names(g.Ready()); len(ready) != 1 || ready[0] != "validate-integration" {
		t.Errorf("Ready = %v, want [validate-integration]", ready)
	}
}

func TestNonYamlFilesIgnored(t *testing.T) {
	dir := tasksDir(t)
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
	dir := tasksDir(t)
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
