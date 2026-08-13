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

// TestRenderPlanningModesAreExclusive checks the planning template's two
// re-invocation modes don't bleed into each other. They give directly opposing
// instructions about the same task files — a re-plan may delete them, an
// incremental run must preserve them — so a render that showed both would leave
// the agent to guess which one applies.
func TestRenderPlanningModesAreExclusive(t *testing.T) {
	const (
		replanMarker      = "RE-PLANNING cycle"
		incrementalMarker = "Preserve every existing task"
	)
	cases := []struct {
		name          string
		replan        bool
		want, notWant string
	}{
		{"cron re-plan", true, replanMarker, incrementalMarker},
		{"first plan or human-added task", false, incrementalMarker, replanMarker},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Render(agent.PlanningAgent, Data{
				Workspace:   "/ws",
				ProjectPath: "/ws/.project.yaml",
				TasksDir:    "/ws/tasks",
				Replan:      tc.replan,
			})
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("missing %q in:\n%s", tc.want, out)
			}
			if strings.Contains(out, tc.notWant) {
				t.Errorf("the other mode's instructions leaked in (%q):\n%s", tc.notWant, out)
			}
		})
	}
}

// TestRenderPlanningReplanSpellsOutTheResetRules pins the details a re-plan gets
// wrong silently. A carried-forward task that keeps last cycle's `summary:` or
// `completed_at:` misreports what happened, and deleting a task without pruning
// the `depends_on` entries naming it fails the whole graph load — which strands
// the project rather than erroring on that one task.
func TestRenderPlanningReplanSpellsOutTheResetRules(t *testing.T) {
	out, err := Render(agent.PlanningAgent, Data{
		Workspace:   "/ws",
		ProjectPath: "/ws/.project.yaml",
		TasksDir:    "/ws/tasks",
		Replan:      true,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{
		"status: ready",     // survivors go back to ready to be picked up
		"attempts: 0",       // and lose the previous cycle's retry count
		"completed_at:",     // stale completion records must be dropped
		"summary:",          //  "
		"depends_on",        // dangling deps after a delete
		"status: committed", // the keep-it-settled option
		"updated_by: agent", // how the cycle ends
	} {
		if !strings.Contains(out, want) {
			t.Errorf("re-plan instructions never mention %q:\n%s", want, out)
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

func TestRenderArchive(t *testing.T) {
	out, err := Render(agent.ArchiveAgent, Data{
		ProjectPath: "/orch/myproj/.project.yaml",
		Workspace:   "/ws",
		TasksDir:    "/orch/myproj/tasks",
		Branch:      "feat/x",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{
		"/orch/myproj/.project.yaml",  // where archive: true is written
		"/ws",                         // the wsp workspace it works in
		"git status",                  // step 1: find uncommitted work
		".gitignore",                  // step 2: the proposal
		"WAIT",                        // ...and that it must not apply it alone
		"notification",                // approval arrives via the interactive session
		"rev-parse --abbrev-ref HEAD", // step 4: branch is read, not assumed
		"@{upstream}..HEAD",           // ...and so is how far ahead it is
		"archive: true",               // step 5: success marker
		"updated_by: agent",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// The branch is whatever is checked out per repo; the template must not
	// hardcode one or interpolate Data.Branch as the push target.
	if strings.Contains(out, "feat/x") {
		t.Errorf("archive template should not name a branch, got:\n%s", out)
	}
	// No pre-commit busywork: the agent is told to skip tests and lint.
	if !strings.Contains(out, "No tests, no lint") {
		t.Errorf("expected the no-extra-work instruction in:\n%s", out)
	}
	// A repo that is already clean and already up to date must be left alone:
	// no empty commit, no no-op push — and that is still a success.
	for _, want := range []string{
		"Commit ONLY if there is something to commit",
		"--allow-empty",
		"Push ONLY if the repo has commits the upstream does not",
		"run a no-op push",
		"needed neither a commit nor a push is a success",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// The push has to be gated from the framing down, not just in step 4: the
	// agent reads the top of the prompt first, so nothing above may promise a
	// push the conditional then withholds.
	for _, unwanted := range []string{
		"committed and pushed",
		"commit-and-push are the whole job",
	} {
		if strings.Contains(out, unwanted) {
			t.Errorf("push described as unconditional (%q) in:\n%s", unwanted, out)
		}
	}
	if !strings.Contains(out, "ONLY what actually needs it") {
		t.Errorf("expected the conditional framing of the job in:\n%s", out)
	}
	// Whether an upstream exists is settled by rev-parse, whose failure mode is
	// "do not push" — not by an empty `ahead` from a rev-list that errored.
	for _, want := range []string{
		"rev-parse --abbrev-ref --symbolic-full-name @{upstream}",
		"ONLY if $upstream is non-empty",
		"never read it as \"no upstream, therefore push\"",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing upstream-detection guidance %q in:\n%s", want, out)
		}
	}
	// The already-level repo is an explicit success with an explicit report,
	// and no workaround may be used to manufacture a push.
	for _, want := range []string{
		`"already clean, nothing to push"`,
		"--force",
		"--set-upstream",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing already-clean success wording %q in:\n%s", want, out)
		}
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
		agent.ArchiveAgent,
	} {
		if _, err := Render(k, Data{Workspace: "/ws"}); err != nil {
			t.Errorf("Render(%s): %v", k, err)
		}
	}
}
