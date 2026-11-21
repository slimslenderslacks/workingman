package daemon

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/task"
)

// The tests in this file cover the hint audit after an integration
// (docs/parallel-task-worktrees.md H3): changed paths outside a task's
// touches and areas are logged as touches_exceeded and recorded in its
// touches_actual, and nothing blocks on it.

// hintedRun runs one task that declares hints and writes files (paths
// relative to the gateway repo) in its worktree, through a real integration,
// and returns the audit and the committed task.
func hintedRun(t *testing.T, tk *task.Task, files ...string) (string, *task.Task) {
	t.Helper()
	e := newWtEnv(t)
	mustSaveTask(t, e.taskPath(tk.Name), tk)
	var sh strings.Builder
	for _, f := range files {
		fmt.Fprintf(&sh, "mkdir -p \"$(dirname gateway/%[1]s)\" && echo x > gateway/%[1]s\n", f)
	}
	sh.WriteString(setStatus(e.taskPath(tk.Name), "success"))
	e.script(agent.TaskAgent, sh.String())
	e.script(agent.CommitAgent, e.integrateScript(tk.Name, integrateOpts{}))
	e.start()
	e.saveProject()
	snap := e.wait("project_idle")
	got := e.loadTask(tk.Name)
	if got.Status != task.StatusCommitted {
		t.Fatalf("task = %s:\n%s", got.Status, snap)
	}
	return snap, got
}

func TestTouchesExceededRecordsActual(t *testing.T) {
	snap, tk := hintedRun(t, &task.Task{
		Name: "api", Status: task.StatusReady,
		Touches: &[]string{"gateway/internal/api/**"}, Areas: []string{"go-deps"},
	}, "internal/api/h.go", "go.mod", "internal/db/x.go", "README.md")

	want := []string{"gateway/README.md", "gateway/internal/db/x.go"}
	if !strings.Contains(snap, "touches_exceeded") || !strings.Contains(snap, "paths="+strings.Join(want, ",")) {
		t.Errorf("audit missing touches_exceeded paths=%v:\n%s", want, snap)
	}
	if !reflect.DeepEqual(tk.TouchesActual, want) {
		t.Errorf("touches_actual = %v, want %v", tk.TouchesActual, want)
	}
	// The declared hints are left as they were.
	if tk.Touches == nil || !reflect.DeepEqual(*tk.Touches, []string{"gateway/internal/api/**"}) {
		t.Errorf("touches = %v", tk.Touches)
	}
}

func TestTouchesWithinHintsNotReported(t *testing.T) {
	snap, tk := hintedRun(t, &task.Task{
		Name: "api", Status: task.StatusReady, Touches: &[]string{"gateway/internal/api/"},
	}, "internal/api/h.go", "internal/api/sub/y.go")
	if strings.Contains(snap, "touches_exceeded") {
		t.Errorf("touches_exceeded logged for a task within its hints:\n%s", snap)
	}
	if tk.TouchesActual != nil {
		t.Errorf("touches_actual = %v, want none", tk.TouchesActual)
	}
}

// A task with no hints ran alone and declared nothing to exceed.
func TestTouchesExclusiveTaskNotAudited(t *testing.T) {
	snap, tk := hintedRun(t, &task.Task{Name: "api", Status: task.StatusReady}, "anything.go")
	if strings.Contains(snap, "touches_exceeded") || tk.TouchesActual != nil {
		t.Errorf("exclusive task audited: touches_actual %v:\n%s", tk.TouchesActual, snap)
	}
}
