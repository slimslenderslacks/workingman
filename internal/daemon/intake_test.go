package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/task"
)

// TestIntakeFileFlipsProjectReady is the primary intake bootstrap path:
// dropping a markdown file into an already-watched project's intake/ dir
// flips the project back to status:ready (clearing any blocked_reason) so
// the daemon relaunches the planning agent — the same effect the old `:task`
// TUI command had, but with no on-disk task-seed conversion: the planning
// agent reads the intake file itself (see the runner_test.go-level test for
// that half of the contract).
func TestIntakeFileFlipsProjectReady(t *testing.T) {
	root := t.TempDir()
	buf, _ := startDaemon(t, root)

	sub := filepath.Join(root, "widget")
	if err := mkdirAll(sub); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	projectPath := filepath.Join(sub, "project.yaml")
	existing := &project.Project{
		Description: "ship the widget", Branch: "feat/widget",
		Status: project.StatusBlocked, BlockedReason: "waiting on something unrelated",
	}
	if err := project.SaveAs(projectPath, existing, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	// Let the project's own observation (project_updated, plus whatever the
	// blocked-status switch does) fully settle on its own event chain before
	// the intake dir even exists — otherwise creating intake/ races that
	// chain's in-flight write to the same project.yaml on a separate
	// directory-keyed goroutine (see dispatchEvent's per-directory chaining).
	if ok, snap := waitFor(t, buf, "project_updated"); !ok {
		t.Fatalf("project never observed.\naudit:\n%s", snap)
	}
	if err := mkdirAll(filepath.Join(sub, "intake")); err != nil {
		t.Fatalf("mkdir intake: %v", err)
	}
	if ok, snap := waitFor(t, buf, "watch_added"); !ok {
		t.Fatalf("daemon never watched the intake dir.\naudit:\n%s", snap)
	}

	mdPath := filepath.Join(sub, "intake", "add-healthz.md")
	if err := writeFile(mdPath, []byte("add a /healthz endpoint\n")); err != nil {
		t.Fatalf("writeFile: %v", err)
	}

	if ok, snap := waitFor(t, buf, "intake_queued"); !ok {
		t.Fatalf("daemon never queued the intake file.\naudit:\n%s", snap)
	}

	p, err := project.Load(projectPath)
	if err != nil {
		t.Fatalf("Load project: %v", err)
	}
	if p.Status != project.StatusReady {
		t.Errorf("status = %q, want ready", p.Status)
	}
	if p.BlockedReason != "" {
		t.Errorf("blocked_reason = %q, want cleared", p.BlockedReason)
	}

	// No seed conversion: nothing is written to tasks/, and the intake file
	// itself is left in place — the planning agent reads and renames it.
	if _, err := os.Stat(filepath.Join(sub, "tasks")); err == nil {
		t.Errorf("tasks dir should not have been created by handleIntakeFile")
	}
	if _, err := os.Stat(mdPath); err != nil {
		t.Errorf("intake file should still exist: %v", err)
	}
}

// TestIntakeFileInNewDirIsPickedUp mirrors TestNewDirWithFileIsPickedUp for
// the intake trigger: mkdir + write project.yaml + mkdir intake + write the
// markdown file, all before the daemon's watch on the new subtree is
// installed. Without maybeWatchNewDir's post-watch scan the file's Create
// event would be missed.
func TestIntakeFileInNewDirIsPickedUp(t *testing.T) {
	root := t.TempDir()
	buf, _ := startDaemon(t, root)

	sub := filepath.Join(root, "gadget")
	if err := mkdirAll(filepath.Join(sub, "intake")); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	projectPath := filepath.Join(sub, "project.yaml")
	if err := project.SaveAs(projectPath, &project.Project{
		Description: "x", Branch: "b", Status: project.StatusIdle,
	}, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	if err := writeFile(filepath.Join(sub, "intake", "more-work.md"), []byte("do more work\n")); err != nil {
		t.Fatalf("writeFile: %v", err)
	}

	if ok, snap := waitFor(t, buf, "intake_queued"); !ok {
		t.Fatalf("daemon did not bootstrap the intake file in a new dir.\naudit:\n%s", snap)
	}
}

// TestIntakeFileWithoutProjectIsIgnored guards against an intake/ dir that
// doesn't sit next to a project.yaml — there is no project to queue work
// against, so the file must be left alone rather than crashing or creating
// a project out of thin air (that's project.md's job, not intake's).
func TestIntakeFileWithoutProjectIsIgnored(t *testing.T) {
	root := t.TempDir()
	buf, _ := startDaemon(t, root)

	sub := filepath.Join(root, "orphan", "intake")
	if err := mkdirAll(sub); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mdPath := filepath.Join(sub, "work.md")
	if err := writeFile(mdPath, []byte("do some work\n")); err != nil {
		t.Fatalf("writeFile: %v", err)
	}

	if ok, snap := waitFor(t, buf, "intake_no_project"); !ok {
		t.Fatalf("expected intake_no_project in audit.\naudit:\n%s", snap)
	}
	if _, err := os.Stat(mdPath); err != nil {
		t.Errorf("intake file should be left untouched: %v", err)
	}
}

// TestIntakeFileEmptyIsIgnored guards against a blank intake file triggering
// a planning cycle with nothing for it to read.
func TestIntakeFileEmptyIsIgnored(t *testing.T) {
	root := t.TempDir()
	buf, _ := startDaemon(t, root)

	sub := filepath.Join(root, "widget")
	if err := mkdirAll(filepath.Join(sub, "intake")); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	projectPath := filepath.Join(sub, "project.yaml")
	if err := project.SaveAs(projectPath, &project.Project{
		Description: "x", Branch: "b", Status: project.StatusIdle,
	}, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	if err := writeFile(filepath.Join(sub, "intake", "blank.md"), []byte("   \n")); err != nil {
		t.Fatalf("writeFile: %v", err)
	}

	if ok, snap := waitFor(t, buf, "intake_empty"); !ok {
		t.Fatalf("blank intake file should be logged and skipped.\naudit:\n%s", snap)
	}
	reloaded, err := project.Load(projectPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reloaded.Status != project.StatusIdle {
		t.Errorf("status = %q, want untouched (idle)", reloaded.Status)
	}
}

// TestPendingIntakeFilesScansAndSorts is a direct unit test of the helper
// launchProjectRootAgent uses to populate runner.Plan.IntakeFiles: it must
// find every *.md file in intake/, ignore non-markdown and subdirectories,
// and return them in a stable (sorted) order so the rendered prompt doesn't
// shuffle between runs.
func TestPendingIntakeFilesScansAndSorts(t *testing.T) {
	root := t.TempDir()
	intakeDir := filepath.Join(root, "intake")
	if err := mkdirAll(intakeDir); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := writeFile(filepath.Join(intakeDir, "z-later.md"), []byte("z\n")); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	if err := writeFile(filepath.Join(intakeDir, "a-first.md"), []byte("a\n")); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	if err := writeFile(filepath.Join(intakeDir, "already-done.processed"), []byte("done\n")); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	if err := mkdirAll(filepath.Join(intakeDir, "not-a-file.md")); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	got := pendingIntakeFiles(root)
	want := []string{
		filepath.Join(intakeDir, "a-first.md"),
		filepath.Join(intakeDir, "z-later.md"),
	}
	if len(got) != len(want) {
		t.Fatalf("pendingIntakeFiles = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pendingIntakeFiles[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestPendingIntakeFilesNoIntakeDir guards the common case (no intake/ dir at
// all) returning an empty, non-panicking result.
func TestPendingIntakeFilesNoIntakeDir(t *testing.T) {
	if got := pendingIntakeFiles(t.TempDir()); got != nil {
		t.Errorf("pendingIntakeFiles with no intake dir = %v, want nil", got)
	}
}

// TestHasPendingIntakeReArmsRestingProject covers the orphan-recovery half of
// the contract: an idle project with an un-renamed intake/*.md file left over
// — its status flip skipped or clobbered while another agent held the
// project slot — must be re-armed back to ready the same way a stranded
// task-graph seed already is. Covers both idle sub-cases (see
// Project.WatchingPR): plain idle, and idle-while-watching a PR — the
// re-arm applies the same either way, exactly as it did for the two
// separate statuses (done/reviewing) this collapsed from.
func TestHasPendingIntakeReArmsRestingProject(t *testing.T) {
	cases := []struct {
		name string
		p    project.Project
	}{
		{"idle", project.Project{}},
		{"idle watching a PR", project.Project{Review: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			buf := &safeBuf{}
			d, err := New([]string{root}, audit.New(buf))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			projectPath := filepath.Join(root, "project.yaml")
			if err := mkdirAll(filepath.Join(root, "intake")); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := writeFile(filepath.Join(root, "intake", "stranded.md"), []byte("stranded request\n")); err != nil {
				t.Fatalf("writeFile: %v", err)
			}

			p := tc.p
			p.Description = "x"
			p.Branch = "b"
			p.Status = project.StatusIdle
			p.Repos = []project.Repo{{Org: "docker", Name: "gateway"}}
			d.dispatchProject(projectPath, &p)

			got, err := project.Load(projectPath)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got.Status != project.StatusReady {
				t.Errorf("status = %q, want ready (a pending intake file must re-arm planning)", got.Status)
			}
			if !strings.Contains(buf.String(), "seed_replan") {
				t.Errorf("expected seed_replan in audit:\n%s", buf.String())
			}
		})
	}
}

// intakeWorkingHarness is a working project with one ready task whose task
// agent is already running in the project's main slot — the situation the
// original bug stranded intake in. The daemon is never Run: tests drive
// handleIntakeFile directly, and the recording launcher stands in for agents.
type intakeWorkingHarness struct {
	d           *Daemon
	lch         *recordingLauncher
	buf         *safeBuf
	projectPath string
	taskSess    *stubSession
}

func newIntakeWorkingHarness(t *testing.T) *intakeWorkingHarness {
	t.Helper()
	d, lch, buf, _ := archiveTestDaemon(t)
	dir := filepath.Join(t.TempDir(), "widget")
	if err := mkdirAll(filepath.Join(dir, "intake")); err != nil {
		t.Fatalf("mkdir intake: %v", err)
	}
	projectPath := filepath.Join(dir, "project.yaml")
	writeIntakeTestProject(t, projectPath, project.StatusWorking)
	writeIntakeTestTask(t, dir, "scaffold", task.StatusRunning)
	writeIntakeTestTask(t, dir, "follow-on", task.StatusReady)

	taskSess := newStubSession("task-scaffold")
	if !d.trackSession(projectPath, taskSess, agent.TaskAgent, "scaffold", nil) {
		t.Fatal("could not track the running task session")
	}
	return &intakeWorkingHarness{d: d, lch: lch, buf: buf, projectPath: projectPath, taskSess: taskSess}
}

func writeIntakeTestProject(t *testing.T, projectPath string, status project.Status) {
	t.Helper()
	if err := project.SaveAs(projectPath, &project.Project{
		Description: "ship the widget", Branch: "feat/widget", Status: status,
		Repos: []project.Repo{{Org: "docker", Name: "gateway"}},
	}, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
}

func writeIntakeTestTask(t *testing.T, dir, name string, status task.Status) {
	t.Helper()
	if err := mkdirAll(filepath.Join(dir, "tasks")); err != nil {
		t.Fatalf("mkdir tasks: %v", err)
	}
	if err := task.Save(filepath.Join(dir, "tasks", name+".yaml"), &task.Task{
		Name: name, Description: name, Status: status, Model: task.ModelDefault,
	}); err != nil {
		t.Fatalf("task.Save: %v", err)
	}
}

// dropIntake writes an intake file and delivers it the way the watcher would.
func (h *intakeWorkingHarness) dropIntake(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(filepath.Dir(h.projectPath), "intake", name)
	if err := writeFile(path, []byte(body)); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	h.d.handleIntakeFile(path)
	return path
}

func (h *intakeWorkingHarness) instructions(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(filepath.Dir(h.projectPath), ".orch", "instructions.md"))
	if err != nil {
		t.Fatalf("read instructions.md: %v", err)
	}
	return string(data)
}

func (h *intakeWorkingHarness) assertTaskSessionUntouched(t *testing.T) {
	t.Helper()
	select {
	case <-h.taskSess.done:
		t.Error("the running task session was closed by intake/planning")
	default:
	}
	if kind, ok := h.d.sessionKind(h.projectPath); !ok || kind != agent.TaskAgent {
		t.Errorf("main slot = (%v, %v), want the running task agent", kind, ok)
	}
}

// TestIntakeWhileWorkingLaunchesPlanningAlongsideTasks is the regression for the
// reported bug: an intake file landing while the project is `working` was never
// planned, because the planning launch shared the main session slot the running
// task agent held and was dedup-skipped. Now it starts a planner at once, in its
// own slot, leaves the task agent running, and keeps the project `working`.
func TestIntakeWhileWorkingLaunchesPlanningAlongsideTasks(t *testing.T) {
	h := newIntakeWorkingHarness(t)

	mdPath := h.dropIntake(t, "add-healthz.md", "add a /healthz endpoint\n")

	if got := h.lch.countKind(agent.PlanningAgent); got != 1 {
		t.Fatalf("planning launches = %d, want 1.\naudit:\n%s", got, h.buf.String())
	}
	if !h.d.hasSession(planningSessionKey(h.projectPath)) {
		t.Errorf("planner not tracked under its own slot %q", planningSessionKey(h.projectPath))
	}
	if !strings.Contains(h.instructions(t), mdPath) {
		t.Errorf("planner was not handed the intake file %q", mdPath)
	}
	h.assertTaskSessionUntouched(t)
	if got := h.lch.countKind(agent.TaskAgent); got != 0 {
		t.Errorf("task launches = %d, want 0 (main slot is busy; nothing was relaunched)", got)
	}
	p, err := project.Load(h.projectPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Status != project.StatusWorking {
		t.Errorf("status = %q, want working — intake must not pull a working project out of task dispatch", p.Status)
	}
}

// TestIntakeDuringPlanningRunsFollowUpNotSecondPlanner covers the concurrency
// rule: intake that lands while a planner is already running never starts a
// second one, but does get its own planning run as soon as the first ends.
func TestIntakeDuringPlanningRunsFollowUpNotSecondPlanner(t *testing.T) {
	h := newIntakeWorkingHarness(t)
	first := h.dropIntake(t, "first.md", "first request\n")
	if got := h.lch.countKind(agent.PlanningAgent); got != 1 {
		t.Fatalf("planning launches = %d, want 1", got)
	}

	second := h.dropIntake(t, "second.md", "second request\n")
	// Any other observation of the project in the meantime must dedup too.
	h.d.revisitProject(h.projectPath)
	if got := h.lch.countKind(agent.PlanningAgent); got != 1 {
		t.Fatalf("planning launches = %d while a planner was running, want 1 (no concurrent planners)", got)
	}

	// The first planner does its job: plans and renames the file it was handed,
	// and leaves the project working.
	if err := os.Rename(first, strings.TrimSuffix(first, ".md")+".processed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	writeIntakeTestTask(t, filepath.Dir(h.projectPath), "first-request", task.StatusReady)
	writeIntakeTestProject(t, h.projectPath, project.StatusWorking)
	h.lch.closeKind(t, agent.PlanningAgent)

	eventually(t, "follow-up planning run", func() bool {
		return h.lch.countKind(agent.PlanningAgent) == 2 && h.d.hasSession(planningSessionKey(h.projectPath))
	})
	instructions := h.instructions(t)
	if !strings.Contains(instructions, second) {
		t.Errorf("follow-up planner was not handed %q:\n%s", second, instructions)
	}
	if strings.Contains(instructions, first) {
		t.Errorf("follow-up planner was re-handed the already-processed %q", first)
	}
	h.assertTaskSessionUntouched(t)
}

// TestIntakeLeftUnprocessedIsNotRelaunchedWhileWorking keeps the planning
// crash-loop guard intact on the new path: a planner that ends without
// processing the intake it was handed must not be relaunched for it on every
// observation of the working project.
func TestIntakeLeftUnprocessedIsNotRelaunchedWhileWorking(t *testing.T) {
	h := newIntakeWorkingHarness(t)
	h.dropIntake(t, "stuck.md", "a request the planner ignores\n")
	h.lch.closeKind(t, agent.PlanningAgent)

	eventually(t, "planner released", func() bool {
		return !h.d.planningInFlight(h.projectPath) && strings.Contains(h.buf.String(), "planning_intake_unprocessed")
	})
	h.d.revisitProject(h.projectPath)
	h.d.revisitProject(h.projectPath)
	if got := h.lch.countKind(agent.PlanningAgent); got != 1 {
		t.Errorf("planning launches = %d, want 1 — an unprocessed file must not loop planning", got)
	}
}

// TestIntakeOnIdleProjectStillFlipsReadyAndPlans pins the idle path: intake on a
// resting project still goes through status:ready (not the working-project
// side launch), and that routes to a planner in the planning slot.
func TestIntakeOnIdleProjectStillFlipsReadyAndPlans(t *testing.T) {
	d, lch, _, _ := archiveTestDaemon(t)
	dir := filepath.Join(t.TempDir(), "widget")
	if err := mkdirAll(filepath.Join(dir, "intake")); err != nil {
		t.Fatalf("mkdir intake: %v", err)
	}
	projectPath := filepath.Join(dir, "project.yaml")
	writeIntakeTestProject(t, projectPath, project.StatusIdle)
	writeIntakeTestTask(t, dir, "scaffold", task.StatusCommitted)

	mdPath := filepath.Join(dir, "intake", "more.md")
	if err := writeFile(mdPath, []byte("more work\n")); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	d.handleIntakeFile(mdPath)

	p, err := project.Load(projectPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Status != project.StatusReady {
		t.Fatalf("status = %q, want ready", p.Status)
	}
	// The ready write's own fsnotify event drives the launch in production.
	d.handleProject(projectPath)
	if got := lch.countKind(agent.PlanningAgent); got != 1 {
		t.Fatalf("planning launches = %d, want 1", got)
	}
	if !d.hasSession(planningSessionKey(projectPath)) || d.hasSession(projectPath) {
		t.Errorf("planner should hold the planning slot only")
	}
}

// TestPlanningInFlightDefersProjectCompletion: while a planner works alongside
// tasks, the last task committing must not complete the project out from under
// it — the planner may be about to add tasks. afterPlanningSession makes the
// call once it is done.
func TestPlanningInFlightDefersProjectCompletion(t *testing.T) {
	d, _, buf, _ := archiveTestDaemon(t)
	dir := filepath.Join(t.TempDir(), "widget")
	projectPath := filepath.Join(dir, "project.yaml")
	if err := mkdirAll(dir); err != nil {
		t.Fatal(err)
	}
	writeIntakeTestProject(t, projectPath, project.StatusWorking)
	writeIntakeTestTask(t, dir, "scaffold", task.StatusCommitted)

	if !d.beginPlanning(projectPath, nil) {
		t.Fatal("beginPlanning")
	}
	p, _ := project.Load(projectPath)
	d.dispatchNextTask(projectPath, p)
	if got, _ := project.Load(projectPath); got.Status != project.StatusWorking {
		t.Fatalf("status = %q while planning in flight, want working", got.Status)
	}
	if !strings.Contains(buf.String(), "dispatch_deferred_planning") {
		t.Errorf("expected dispatch_deferred_planning in audit:\n%s", buf.String())
	}

	d.afterPlanningSession(projectPath, nil)
	if got, _ := project.Load(projectPath); got.Status != project.StatusIdle {
		t.Errorf("status = %q after planning ended, want idle", got.Status)
	}
}

// TestCronReplanWaitsForMainSlot: a re-plan may delete or rewrite tasks, so it
// must not run beside a live task agent the way an additive intake run does.
func TestCronReplanWaitsForMainSlot(t *testing.T) {
	h := newIntakeWorkingHarness(t)
	p, err := project.Load(h.projectPath)
	if err != nil {
		t.Fatal(err)
	}
	p.Status = project.StatusReady
	p.Replan = true
	h.d.launchPlanningAgent(h.projectPath, p)
	if got := h.lch.countKind(agent.PlanningAgent); got != 0 {
		t.Errorf("re-plan launched beside a running task agent (%d launches)", got)
	}
	if h.d.planningInFlight(h.projectPath) {
		t.Error("a skipped launch must not hold the planning reservation")
	}
}
