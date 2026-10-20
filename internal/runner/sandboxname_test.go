package runner

import (
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/sbx"
	"github.com/slimslenderslacks/work/internal/task"
)

// allKinds is every agent kind the orchestrator can launch. A new kind added to
// internal/agent without being added here is a hole in the invariant below, so
// keep it exhaustive.
var allKinds = []agent.Kind{
	agent.ProjectAgent,
	agent.PlanningAgent,
	agent.TaskAgent,
	agent.WolfAgent,
	agent.CommitAgent,
	agent.ArchiveAgent,
	agent.ReviewAgent,
	agent.WorkingmanAgent,
}

// TestSandboxNameNeverExceedsSbxLimit is the regression test for the failure
// this whole budget exists to prevent:
//
//	error: sandbox name cannot exceed 63 characters:
//	server-namespace-fix-testifylint-go-require-create-mcp-prefix-test-1019
//
// `sbx create` rejected that name on every relaunch, so the task never ran and
// the daemon kept retrying forever. Neither half was individually illegal: the
// work stream was 16 chars, the task name 53 (under task.MaxNameLen) — it was
// the composite that overflowed, and nothing checked the composite.
//
// The derivation is now total: for ANY kind, ANY project path and ANY task
// name, the returned sandbox name fits. No agent-authored string can reopen
// this.
func TestSandboxNameNeverExceedsSbxLimit(t *testing.T) {
	projectPaths := []string{
		"/orch/proj/.project.yaml",
		"/orch/server-namespace/.project.yaml",
		"/orch/" + strings.Repeat("work-stream-", 10) + "/.project.yaml",
		"/orch/under_scored_work_stream/.project.yaml",
	}
	taskNames := []string{
		"",
		"short",
		"fix-testifylint-go-require-create-mcp-prefix-test-1019",
		strings.Repeat("a", task.MaxNameLen),
		strings.Repeat("very-long-task-name-", 20),
		"has_underscores_in_it",
	}

	for _, kind := range allKinds {
		for _, projectPath := range projectPaths {
			for _, taskName := range taskNames {
				for _, fn := range []struct {
					label  string
					derive func(agent.Kind, string, string) string
				}{
					{"SandboxNameFor", SandboxNameFor},
					{"ACPSandboxNameFor", ACPSandboxNameFor},
				} {
					got := fn.derive(kind, projectPath, taskName)
					if err := sbx.CheckName(got); err != nil {
						t.Errorf("%s(%v, %q, %q): %v", fn.label, kind, projectPath, taskName, err)
					}
					if strings.Contains(got, "_") {
						t.Errorf("%s(%v, %q, %q) = %q, contains an underscore (sbx rejects them)",
							fn.label, kind, projectPath, taskName, got)
					}
				}
			}
		}
	}
}

// TestSandboxNameIsStableAcrossCalls guards the property the launch path relies
// on: the sandbox name is the session's identity across daemon restarts
// (ensureSandbox looks the sandbox up by name to decide reuse vs. recreate), so
// shortening must not introduce any per-call variation.
func TestSandboxNameIsStableAcrossCalls(t *testing.T) {
	projectPath := "/orch/server-namespace/.project.yaml"
	taskName := strings.Repeat("long-task-", 10)

	want := SandboxNameFor(agent.TaskAgent, projectPath, taskName)
	for i := 0; i < 50; i++ {
		if got := SandboxNameFor(agent.TaskAgent, projectPath, taskName); got != want {
			t.Fatalf("SandboxNameFor is not stable: %q then %q", want, got)
		}
	}
	// The commit agent must land on the task agent's sandbox — it reuses it to
	// see the task's git work — so the two derivations have to agree even when
	// the name had to be shortened.
	if got := SandboxNameFor(agent.CommitAgent, projectPath, taskName); got != want {
		t.Errorf("commit agent sandbox = %q, want the task agent's %q", got, want)
	}
}

// TestSandboxNameUnchangedWhenItFits pins the ordinary case: names within
// budget are passed through verbatim, so the fix is invisible to every project
// that was already naming tasks sensibly.
func TestSandboxNameUnchangedWhenItFits(t *testing.T) {
	cases := []struct {
		kind     agent.Kind
		taskName string
		want     string
	}{
		{agent.TaskAgent, "add-healthz-handler", "myproj-add-healthz-handler"},
		{agent.CommitAgent, "add-healthz-handler", "myproj-add-healthz-handler"},
		{agent.PlanningAgent, "", "myproj"},
		{agent.ProjectAgent, "", "myproj-project"},
		{agent.ArchiveAgent, "", "myproj-archive"},
		{agent.ReviewAgent, "", "myproj-review"},
	}
	for _, c := range cases {
		if got := SandboxNameFor(c.kind, "/orch/myproj/.project.yaml", c.taskName); got != c.want {
			t.Errorf("SandboxNameFor(%v) = %q, want %q", c.kind, got, c.want)
		}
	}
}

// TestTaskNameBudgetMatchesSandboxDerivation ties the number the prompts quote
// to an agent (task.MaxNameLenFor, rendered via the `nameBudget` template func)
// to what the sandbox derivation actually accepts. If these ever drift, agents
// get told a limit that is either needlessly tight or — worse — too loose, and
// their names get silently repaired out from under them.
func TestTaskNameBudgetMatchesSandboxDerivation(t *testing.T) {
	for _, workStream := range []string{"proj", "server-namespace", "shared-gateway", "a"} {
		projectPath := "/orch/" + workStream + "/.project.yaml"
		budget := task.MaxNameLenFor(workStream)

		atLimit := strings.Repeat("a", budget)
		got := SandboxNameFor(agent.TaskAgent, projectPath, atLimit)
		if want := workStream + "-" + atLimit; got != want {
			t.Errorf("a name at the advertised budget was altered: got %q, want %q", got, want)
		}

		// One over, and the derivation shortens rather than passing it through.
		overBudget := strings.Repeat("a", budget+1)
		if got := SandboxNameFor(agent.TaskAgent, projectPath, overBudget); got == workStream+"-"+overBudget {
			t.Errorf("a name one over the budget was passed through unshortened: %q", got)
		}
	}
}
