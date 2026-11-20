package daemon

import (
	"testing"

	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/workspace"
)

// The limit the planning prompt quotes is the project's, lowered to the
// global cap, and 1 when tasks can't get worktrees of their own: at 1 the
// prompt leaves the parallelism hints out (docs/parallel-task-worktrees.md
// H4).
func TestPlannerParallelism(t *testing.T) {
	git := &runner.Runner{Workspaces: workspace.NewGitStub(t.TempDir())}
	plain := &runner.Runner{Workspaces: workspace.NewStub(t.TempDir())}
	cases := []struct {
		name  string
		r     *runner.Runner
		cap   int
		limit int
		want  int
	}{
		{"default", git, 0, 0, 1},
		{"project limit", git, 0, 3, 3},
		{"global cap lowers it", git, 2, 3, 2},
		{"global cap above it", git, 5, 3, 3},
		{"no worktrees", plain, 0, 3, 1},
		{"no runner", nil, 0, 3, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &Daemon{runner: tc.r, maxParallelTasks: tc.cap}
			if got := d.plannerParallelism(&project.Project{MaxParallelTasks: tc.limit}); got != tc.want {
				t.Errorf("plannerParallelism = %d, want %d", got, tc.want)
			}
		})
	}
}
