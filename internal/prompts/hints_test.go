package prompts

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/agent"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files under testdata/")

// planningGoldenCases cover each branch of planning.tmpl. Their output at the
// default limit is pinned by testdata/planning_*.golden, captured before the
// parallelism hints were added (docs/parallel-task-worktrees.md H4: at limit
// 1 the planning prompt stays exactly as it was).
var planningGoldenCases = []struct {
	name string
	data Data
}{
	{"first-plan", Data{}},
	{"intake", Data{IntakeFiles: []string{"/orch/p/intake/a.md", "/orch/p/intake/b.md"}}},
	{"replan", Data{Replan: true}},
	{"project-changes", Data{ProjectChanges: []string{"description changed"}}},
}

func planningData(d Data) Data {
	d.Workspace = "/orch/p"
	d.ProjectPath = "/orch/p/project.yaml"
	d.ProjectName = "p"
	d.TasksDir = "/orch/p/tasks"
	d.Worktree = "/ws/feat-x"
	d.Repos = []Repo{{DirName: "workingman"}, {DirName: "docs"}}
	return d
}

// At max_parallel_tasks 0 or 1 the planning prompt is byte-for-byte what it
// was before the hints existed.
func TestRenderPlanningLimitOneUnchanged(t *testing.T) {
	for _, tc := range planningGoldenCases {
		golden := filepath.Join("testdata", "planning_"+tc.name+".golden")
		for _, limit := range []int{1, 0} {
			d := planningData(tc.data)
			d.MaxParallelTasks = limit
			out, err := Render(agent.PlanningAgent, d)
			if err != nil {
				t.Fatalf("%s: Render: %v", tc.name, err)
			}
			if *updateGolden && limit == 1 {
				if err := os.WriteFile(golden, []byte(out), 0o644); err != nil {
					t.Fatal(err)
				}
				continue
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run go test ./internal/prompts -run LimitOne -update)", err)
			}
			if out != string(want) {
				t.Errorf("%s at limit %d differs from %s:\n%s", tc.name, limit, golden, out)
			}
		}
	}
}

// Above limit 1 the planner is asked for touches/areas hints, with the
// project's repo dir names, the standard area tags, the extra result fields
// to clear on a re-plan, and the rule against editing in-flight hints.
func TestRenderPlanningLimitTwoHasHints(t *testing.T) {
	d := planningData(Data{})
	d.MaxParallelTasks = 2
	out, err := Render(agent.PlanningAgent, d)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"IMPORTANT — parallelism hints",
		"(up to 2 for this project)",
		"directory name (`workingman`, `docs`)",
		"`touches: []` means the task changes no files",
		"leave both `touches:` and\n    `areas:` out",
		"`go-deps`, `npm-deps`, `db-schema`, `ci`, `generated-docs`, `changelog`,\n    `release-version`",
		"    - workingman/internal/api/router.go",
		"Do not change `max_parallel_tasks`",
		"Never change the `touches:`, `areas:` or\n`depends_on` of a task that is `running` or `success`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("planning prompt missing %q:\n%s", want, out)
		}
	}
	// The hints block sits after the name rules and before static_mcps (H4).
	hints, names, mcps := strings.Index(out, "parallelism hints"), strings.Index(out, "Get it\nright the first time"), strings.Index(out, "`static_mcps:` — a YAML list")
	if !(names < hints && hints < mcps) {
		t.Errorf("hints block out of place: names=%d hints=%d static_mcps=%d", names, hints, mcps)
	}

	d = planningData(Data{Replan: true})
	d.MaxParallelTasks = 2
	out, err = Render(agent.PlanningAgent, d)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"`succeeded_at:`, `integration:`, `touches_actual:`, `base:`, `worktree:`,\n    `task_branch:`",
		"Keep its `touches:` and `areas:` hints",
		"IMPORTANT — parallelism hints",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("re-plan prompt missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Never change the `touches:`") {
		t.Errorf("the incremental-intake rule leaked into the re-plan prompt:\n%s", out)
	}
}

// The task agent sees its declared hints and is asked to stay within them;
// an explicit `touches: []` reads as "none"; no hints, no paragraph.
func TestRenderTaskHintLine(t *testing.T) {
	base := Data{Workspace: "/ws/.worktrees/api", TaskName: "api", Branch: "feat/x",
		TaskWorktree: "/ws/.worktrees/api", TaskBranch: "wm/feat/x/api"}

	with := base
	with.HasTouches = true
	with.Touches = []string{"gateway/internal/api/**", "gateway/go.mod"}
	with.Areas = []string{"go-deps"}
	out, err := Render(agent.TaskAgent, with)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Your task declares the files and shared areas it expects to change",
		"  touches:\n    - `gateway/internal/api/**`\n    - `gateway/go.mod`",
		"  areas:\n    - `go-deps`",
		"Stay within them where you reasonably can",
		"say which ones in `failure_reason` or your summary",
		"Other tasks may be\nrunning beside you",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("task prompt missing %q:\n%s", want, out)
		}
	}

	empty := base
	empty.HasTouches = true
	out, err = Render(agent.TaskAgent, empty)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "touches: none (no files in any repo)") || strings.Contains(out, "areas:") {
		t.Errorf("an explicit touches: [] should read as none:\n%s", out)
	}

	areasOnly := base
	areasOnly.Areas = []string{"ci"}
	out, err = Render(agent.TaskAgent, areasOnly)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "  areas:\n    - `ci`") || strings.Contains(out, "touches:") {
		t.Errorf("areas-only hints rendered wrong:\n%s", out)
	}

	out, err = Render(agent.TaskAgent, base)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "Your task declares") {
		t.Errorf("a task with no hints must not carry the hint paragraph:\n%s", out)
	}
}

// The wolf may tighten a conflicted task's hints when it resets it (I6, H4).
func TestRenderWolfHintTightening(t *testing.T) {
	out, err := Render(agent.WolfAgent, Data{Workspace: "/orch/p", ProjectPath: "/orch/p/project.yaml", Branch: "feat/x"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "tighten its `touches:`/`areas:` hints") {
		t.Errorf("wolf prompt missing the hint-tightening sentence:\n%s", out)
	}
}
