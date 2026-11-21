package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/task"
)

// writePipelineTasks saves a project's tasks at each stage of the integration
// pipeline (docs/parallel-task-worktrees.md D5) and returns the tasks dir.
func writePipelineTasks(t *testing.T, dir string) string {
	t.Helper()
	tasksDir := filepath.Join(dir, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	at := func(min int) *time.Time {
		v := time.Date(2026, 11, 19, 10, min, 0, 0, time.UTC)
		return &v
	}
	none := []string{}
	for _, tk := range []*task.Task{
		{Name: "base", Status: task.StatusCommitted, Touches: &none, CompletedAt: at(0)},
		{Name: "head", Status: task.StatusSuccess, SucceededAt: at(1), Worktree: "/wt/head", Areas: []string{"x"}},
		{Name: "next", Status: task.StatusSuccess, SucceededAt: at(5), Worktree: "/wt/next", Areas: []string{"y"}},
		{Name: "conflicted", Status: task.StatusReady, Attempts: 1, Worktree: "/wt/conflicted", Areas: []string{"z"},
			Integration: &task.Integration{Result: "conflict", Repo: "gw", Files: []string{"a.go", "b.go"}}},
		{Name: "dependent", Status: task.StatusReady, DependsOn: []string{"next"}, Touches: &none},
		{Name: "legacy", Status: task.StatusReady},
		{Name: "stuck", Status: task.StatusBlocked, Worktree: "/wt/stuck"},
	} {
		if err := task.Save(filepath.Join(tasksDir, tk.Name+".yaml"), tk); err != nil {
			t.Fatal(err)
		}
	}
	return tasksDir
}

func viewsByName(vs []TaskView) map[string]TaskView {
	out := map[string]TaskView{}
	for _, v := range vs {
		out[v.Name] = v
	}
	return out
}

// tasksFor fills the TaskView pipeline fields and returns the commit queue,
// head first.
func TestTasksForPipelineFields(t *testing.T) {
	counts, views, queue := tasksFor(writePipelineTasks(t, t.TempDir()))
	if strings.Join(queue, ",") != "head,next" {
		t.Errorf("queue = %v, want [head next]", queue)
	}
	if counts[task.StatusSuccess] != 2 || counts[task.StatusReady] != 3 {
		t.Errorf("counts = %v", counts)
	}
	got := viewsByName(views)
	if v := got["head"]; v.QueuePos != 1 || v.Worktree != "/wt/head" || v.SucceededAt.IsZero() || v.Exclusive {
		t.Errorf("head = %+v", v)
	}
	if v := got["next"]; v.QueuePos != 2 {
		t.Errorf("next queue pos = %d", v.QueuePos)
	}
	if v := got["dependent"]; strings.Join(v.WaitingOn, ",") != "next" || v.QueuePos != 0 {
		t.Errorf("dependent = %+v", v)
	}
	if v := got["conflicted"]; v.Integration == nil || v.Integration.Result != "conflict" ||
		strings.Join(v.Integration.Files, ",") != "a.go,b.go" || v.Attempts != 1 {
		t.Errorf("conflicted = %+v", v)
	}
	if v := got["legacy"]; !v.Exclusive || v.Worktree != "" {
		t.Errorf("legacy = %+v", v)
	}
}

// success no longer shares ✓ with committed: it is ⧗ queued or ⇡ integrating,
// in the running colour; a conflict re-run waiting for a slot is ↻.
func TestTaskViewGlyphs(t *testing.T) {
	_, views, _ := tasksFor(writePipelineTasks(t, t.TempDir()))
	got := viewsByName(views)
	for name, want := range map[string]string{
		"base":       "✓",
		"head":       "⇡",
		"next":       "⧗",
		"conflicted": "↻",
		"dependent":  "○",
		"stuck":      taskStatusGlyph(task.StatusBlocked),
	} {
		if g := taskViewGlyph(got[name], "head"); g != want {
			t.Errorf("%s glyph = %q, want %q", name, g, want)
		}
	}
	if g := taskViewGlyph(got["head"], ""); g != "⧗" {
		t.Errorf("head with nothing integrating = %q, want ⧗", g)
	}
	if taskStatusGlyph(task.StatusSuccess) == taskStatusGlyph(task.StatusCommitted) {
		t.Error("success and committed share a glyph")
	}
	if taskStatusStyle(task.StatusSuccess).Render("x") != statusRunning.Render("x") {
		t.Error("success is not in the running colour")
	}
	if taskViewStyle(got["conflicted"]).Render("x") != statusRunning.Render("x") {
		t.Error("a conflict re-run is not in the running colour")
	}
}

// Each pipeline stage gets a row hint; other tasks get none.
func TestTaskRowHints(t *testing.T) {
	_, views, _ := tasksFor(writePipelineTasks(t, t.TempDir()))
	got := viewsByName(views)
	for name, want := range map[string]string{
		"head":       "integrating",
		"next":       "queue #2",
		"dependent":  "waiting on next",
		"conflicted": "conflict in a.go, b.go (attempt 2/4)",
		"base":       "",
		"legacy":     "",
	} {
		if h := taskRowHint(got[name], "head"); h != want {
			t.Errorf("%s hint = %q, want %q", name, h, want)
		}
	}
	if h := taskRowHint(got["head"], ""); h != "queue #1" {
		t.Errorf("head with nothing integrating = %q", h)
	}
	if h := taskRowHint(got["dependent"], "next"); h != "waiting on next (integrating)" {
		t.Errorf("dependent while next integrates = %q", h)
	}
}

// The breakdown counts success as queued (Q), not done (D).
func TestTaskBreakdownQueuedCount(t *testing.T) {
	counts, _, _ := tasksFor(writePipelineTasks(t, t.TempDir()))
	line := renderTaskBreakdown(counts, 80)
	for _, want := range []string{"R:3", "W:0", "Q:2", "B:1", "D:1", "F:0"} {
		if !strings.Contains(line, want) {
			t.Errorf("breakdown %q missing %q", line, want)
		}
	}
}

// withIntegrating names each project's commit-agent task from the live
// sessions, without writing to the slice it was given.
func TestWithIntegrating(t *testing.T) {
	views := []ProjectView{{Path: "/a/project.yaml"}, {Path: "/b/project.yaml"}}
	sessions := []SessionView{
		{ID: "/a/project.yaml", AgentName: "commit", TaskName: "head"},
		{ID: "/b/project.yaml", AgentName: "task", TaskName: "other"},
		{ID: "/b/project.yaml#wolf", AgentName: "wolf"},
	}
	out := withIntegrating(views, sessions)
	if out[0].Integrating != "head" || out[1].Integrating != "" {
		t.Errorf("integrating = %q, %q", out[0].Integrating, out[1].Integrating)
	}
	if views[0].Integrating != "" {
		t.Error("withIntegrating wrote to its input")
	}
	if got := withIntegrating(views, []SessionView{{ID: "/a/project.yaml#commit", AgentName: "commit", TaskName: "x"}}); got[0].Integrating != "x" {
		t.Errorf("a #commit key: %q", got[0].Integrating)
	}
}

// The detail pane's parallel line names running, integrating and queued
// tasks, and appears only when there is a limit above 1 or a queue; a failed
// or blocked task lists its worktree.
func TestProjectDetailParallelLineAndWorktree(t *testing.T) {
	dir := t.TempDir()
	_, views, queue := tasksFor(writePipelineTasks(t, dir))
	v := ProjectView{Name: "p", Status: project.StatusWorking, Tasks: views, CommitQueue: queue, MaxParallel: 2, Integrating: "head"}
	line := renderProjectDetailParallel(v, 200)
	if !strings.Contains(line, "parallel: 2 (integrating head · queued next)") {
		t.Errorf("parallel line = %q", line)
	}
	if renderProjectDetailParallel(ProjectView{MaxParallel: 1}, 200) != "" {
		t.Error("a serial project with an empty queue shows a parallel line")
	}
	body := strings.Join(renderProjectDetailTasks(v, 200), "\n")
	if !strings.Contains(body, "worktree: /wt/stuck") {
		t.Errorf("blocked task's worktree not listed:\n%s", body)
	}
	if strings.Contains(body, "worktree: /wt/next") {
		t.Errorf("a queued task's worktree is listed:\n%s", body)
	}
}

// :workspace from the Tasks pane opens the selected task's worktree when it
// has one; from the work-streams pane it keeps opening the shared workspace.
func TestWorkspaceCommandOpensTaskWorktree(t *testing.T) {
	m, projPath := selectProject(t, t.TempDir(), "widget")
	fake := &fakeInteractive{target: "worktree-widget"}
	m.interactive = fake
	_, views, _ := tasksFor(writePipelineTasks(t, filepath.Dir(projPath)))
	m.projects[0].Tasks = views
	stuck := viewsByName(views)["stuck"]
	m.taskSel = stuck.Path
	m.focus = paneTasks

	m.mode = modeCommandPicker
	m.cmdPickerIdx = projectCommandIndex("workspace")
	step, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = step.(model)
	if cmd == nil {
		t.Fatal("no launch command")
	}
	if msg, ok := cmd().(interactiveLaunchedMsg); !ok || msg.kind != "worktree" {
		t.Fatalf("launched = %+v", msg)
	}
	if len(fake.wtCalls) != 1 || fake.wtCalls[0] != projPath+"|"+stuck.Path || len(fake.wsCalls) != 0 {
		t.Errorf("worktree calls = %v, workspace calls = %v", fake.wtCalls, fake.wsCalls)
	}

	// A task with no worktree falls back to the shared workspace.
	m.taskSel = viewsByName(views)["legacy"].Path
	m.mode = modeCommandPicker
	m.cmdPickerIdx = projectCommandIndex("workspace")
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if msg, ok := cmd().(interactiveLaunchedMsg); !ok || msg.kind != "workspace" || len(fake.wsCalls) != 1 {
		t.Errorf("legacy task: launched = %+v, workspace calls = %v", msg, fake.wsCalls)
	}
}

// :session from the Tasks pane opens the interactive session in the selected
// task's worktree when it has one (docs/parallel-task-worktrees.md X3); for a
// task without one, or from the work-streams pane, it opens the shared
// workspace's session.
func TestSessionCommandOpensTaskSession(t *testing.T) {
	m, projPath := selectProject(t, t.TempDir(), "widget")
	fake := &fakeInteractive{target: "session-widget-stuck"}
	m.interactive = fake
	_, views, _ := tasksFor(writePipelineTasks(t, filepath.Dir(projPath)))
	m.projects[0].Tasks = views
	stuck := viewsByName(views)["stuck"]
	m.taskSel = stuck.Path
	m.focus = paneTasks

	m.mode = modeCommandPicker
	m.cmdPickerIdx = projectCommandIndex("session")
	step, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = step.(model)
	if cmd == nil {
		t.Fatal("no launch command")
	}
	if msg, ok := cmd().(interactiveLaunchedMsg); !ok || msg.kind != "task session" {
		t.Fatalf("launched = %+v", msg)
	}
	if len(fake.tsCalls) != 1 || fake.tsCalls[0] != projPath+"|"+stuck.Path || len(fake.sessionCalls) != 0 {
		t.Errorf("task session calls = %v, session calls = %v", fake.tsCalls, fake.sessionCalls)
	}

	m.taskSel = viewsByName(views)["legacy"].Path
	m.mode = modeCommandPicker
	m.cmdPickerIdx = projectCommandIndex("session")
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if msg, ok := cmd().(interactiveLaunchedMsg); !ok || msg.kind != "session" || len(fake.sessionCalls) != 1 {
		t.Errorf("legacy task: launched = %+v, session calls = %v", msg, fake.sessionCalls)
	}
}
