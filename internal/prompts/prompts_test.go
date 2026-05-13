package prompts

import (
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/agent"
)

func TestRenderProject(t *testing.T) {
	out, err := Render(agent.ProjectAgent, Data{
		ProjectPath: "/ws/.project.yaml",
		Workspace:   "/ws",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, "/ws/.project.yaml") {
		t.Errorf("project path not substituted:\n%s", out)
	}
	if !strings.Contains(out, "updated_by: agent") {
		t.Errorf("expected updated_by guidance:\n%s", out)
	}
}

func TestRenderTask(t *testing.T) {
	out, err := Render(agent.TaskAgent, Data{
		Workspace: "/ws",
		Branch:    "feat/x",
		TaskPath:  "/ws/tasks/01.yaml",
		TaskName:  "add-healthz-handler",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{"add-healthz-handler", "/ws/tasks/01.yaml", "feat/x"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestRenderWolfWithFailedTasks(t *testing.T) {
	out, err := Render(agent.WolfAgent, Data{
		ProjectPath: "/ws/.project.yaml",
		Workspace:   "/ws",
		FailedTasks: []string{"/ws/tasks/01.yaml", "/ws/tasks/03.yaml"},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, "/ws/tasks/01.yaml") || !strings.Contains(out, "/ws/tasks/03.yaml") {
		t.Errorf("failed task list not substituted:\n%s", out)
	}
}

func TestRenderAllKinds(t *testing.T) {
	// Smoke test — every Kind must have a template; ensures we don't ship
	// a Kind without text.
	for _, k := range []agent.Kind{
		agent.ProjectAgent,
		agent.PlanningAgent,
		agent.TaskAgent,
		agent.WolfAgent,
		agent.CommitAgent,
	} {
		if _, err := Render(k, Data{Workspace: "/ws"}); err != nil {
			t.Errorf("Render(%s): %v", k, err)
		}
	}
}
