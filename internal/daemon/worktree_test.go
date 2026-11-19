package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/task"
	"github.com/slimslenderslacks/work/internal/workspace"
	"github.com/slimslenderslacks/work/internal/worktree"
)

// The tests in this file run the per-task worktree path
// (docs/parallel-task-worktrees.md phase 1) end to end: a stub workspace
// manager that creates real git repos, and agents that are shell scripts doing
// real git work, so what lands where is checked on disk rather than mocked.

const wtBranch = "feat/wt"

// gitID makes the scripted agents' commits independent of the host's git
// config (identity, signing).
const gitID = "-c user.name=t -c user.email=t@example.com -c commit.gpgsign=false"

// wtEnv is one worktree-enabled daemon under test.
type wtEnv struct {
	t        *testing.T
	buf      *safeBuf
	root     string // the orch control dir (holds project.yaml, tasks/)
	stub     *workspace.StubManager
	shared   string // the project's shared workspace
	tasksDir string

	mu      sync.Mutex
	scripts map[agent.Kind][]string // per-kind scripts, consumed in launch order
	starts  map[agent.Kind]int
}

func newWtEnv(t *testing.T) *wtEnv {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	stubRoot := t.TempDir()
	e := &wtEnv{
		t:        t,
		buf:      &safeBuf{},
		root:     root,
		stub:     workspace.NewGitStub(stubRoot),
		shared:   filepath.Join(stubRoot, wtBranch),
		tasksDir: filepath.Join(root, "tasks"),
		scripts:  map[agent.Kind][]string{},
		starts:   map[agent.Kind]int{},
	}
	if err := os.MkdirAll(e.tasksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return e
}

// script queues the shell script the next agent of kind runs (in its working
// directory). An agent with no script queued just exits.
func (e *wtEnv) script(kind agent.Kind, sh string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.scripts[kind] = append(e.scripts[kind], sh)
}

func (e *wtEnv) command(kind agent.Kind, _ string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.starts[kind]++
	q := e.scripts[kind]
	if len(q) == 0 {
		return []string{"true"}
	}
	e.scripts[kind] = q[1:]
	return []string{"sh", "-ec", q[0]}
}

// provision creates the shared workspace up front, so a test can seed it
// before the daemon first dispatches (the daemon's own Create is idempotent).
func (e *wtEnv) provision() {
	e.t.Helper()
	if _, err := e.stub.Create(context.Background(), wtBranch, []workspace.Repo{{Identity: "github.com/docker/gateway"}}); err != nil {
		e.t.Fatalf("provision: %v", err)
	}
}

func (e *wtEnv) start() *Daemon {
	e.t.Helper()
	a := audit.New(e.buf)
	r := &runner.Runner{
		Workspaces: e.stub,
		Launcher:   &agent.ProcessLauncher{},
		Audit:      a,
		Command:    e.command,
	}
	d, err := newDaemon(e.t, []string{e.root}, a, WithRunner(r))
	if err != nil {
		e.t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = d.Run(ctx); close(done) }()
	e.t.Cleanup(func() { cancel(); <-done })
	if ok, snap := waitFor(e.t, e.buf, "watch_root"); !ok {
		e.t.Fatalf("daemon never ready: %s", snap)
	}
	return d
}

func (e *wtEnv) saveProject() string {
	e.t.Helper()
	path := filepath.Join(e.root, "project.yaml")
	if err := project.SaveAs(path, &project.Project{
		Description: "worktree test",
		Branch:      wtBranch,
		Status:      project.StatusWorking,
		Repos:       []project.Repo{{Org: "docker", Name: "gateway"}},
	}, project.WriterAgent); err != nil {
		e.t.Fatalf("SaveAs: %v", err)
	}
	return path
}

func (e *wtEnv) taskPath(name string) string { return filepath.Join(e.tasksDir, name+".yaml") }

func (e *wtEnv) loadTask(name string) *task.Task {
	e.t.Helper()
	tk, err := task.Load(e.taskPath(name))
	if err != nil {
		e.t.Fatalf("load task %s: %v", name, err)
	}
	return tk
}

func (e *wtEnv) wait(event string) string {
	e.t.Helper()
	ok, snap := waitForWithin(e.t, e.buf, event, 15*time.Second)
	if !ok {
		e.t.Fatalf("never saw %q.\naudit:\n%s", event, snap)
	}
	return snap
}

func (e *wtEnv) git(dir string, args ...string) string {
	e.t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		e.t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// setStatus is the shell an agent uses to write its exit status into its
// task file (portable: no sed -i).
func setStatus(taskPath, status string) string {
	return fmt.Sprintf("sed 's/^status: .*/status: %s/' %q > %q.tmp && mv %q.tmp %q\n", status, taskPath, taskPath, taskPath, taskPath)
}

// commitWorktreeScript is a commit agent following the worktree branch of
// commit.tmpl: commit leftovers in the task worktree, fast-forward the shared
// branch, record committed with the published hash.
func (e *wtEnv) commitWorktreeScript(taskName string) string {
	wt := worktree.Path(e.shared, taskName, "gateway")
	sharedRepo := filepath.Join(e.shared, "gateway")
	tp := e.taskPath(taskName)
	return fmt.Sprintf(`
if [ -n "$(git -C %[1]q status --porcelain)" ]; then
  git -C %[1]q add -A
  git -C %[1]q %[2]s commit -q -m %[3]q
fi
git -C %[4]q merge -q --ff-only %[5]q
hash=$(git -C %[4]q rev-parse HEAD)
%[6]sprintf 'commits:\n  - repo: gateway\n    hash: %%s\n' "$hash" >> %[7]q
`, wt, gitID, taskName, sharedRepo, worktree.BranchName(wtBranch, taskName), setStatus(tp, "committed"), tp)
}

// TestWorktreeTaskEditsLandInWorktree is the core phase-1 property: a task
// agent works in .worktrees/<task>/ on its own branch, its edits never touch
// the shared tree, and the commit agent lands them on the shared branch, after
// which the daemon verifies and removes the worktree.
func TestWorktreeTaskEditsLandInWorktree(t *testing.T) {
	e := newWtEnv(t)
	mustSaveTask(t, e.taskPath("api"), &task.Task{Name: "api", Status: task.StatusReady})
	wtRoot := worktree.Root(e.shared, "api")
	probe := filepath.Join(t.TempDir(), "probe")

	// The task agent: runs in its worktree root, edits the repo there.
	e.script(agent.TaskAgent, fmt.Sprintf(`
pwd > %[1]q
git -C gateway rev-parse --abbrev-ref HEAD >> %[1]q
echo hello > gateway/feature.txt
%[2]s`, probe, setStatus(e.taskPath("api"), "success")))
	// The commit agent first records what the shared tree looked like while
	// the task's work was still only in its worktree.
	e.script(agent.CommitAgent, fmt.Sprintf(`
git -C %[1]q status --porcelain >> %[2]q
ls %[1]q >> %[2]q
`, filepath.Join(e.shared, "gateway"), probe)+e.commitWorktreeScript("api"))

	e.start()
	e.saveProject()
	snap := e.wait("project_idle")

	p, err := os.ReadFile(probe)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(p)), "\n")
	if len(lines) < 2 || worktreeReal(lines[0]) != worktreeReal(wtRoot) {
		t.Fatalf("task agent cwd = %q, want %q", lines, wtRoot)
	}
	if lines[1] != "wm/feat/wt/api" {
		t.Errorf("task worktree on %q, want the task branch", lines[1])
	}
	for _, l := range lines[2:] {
		if strings.Contains(l, "feature.txt") {
			t.Errorf("the task's edit reached the shared tree before integration: %q", lines)
		}
	}

	// Integrated: the shared branch has the file, the worktree and branch are
	// gone, the task file records the commit and no longer points at a worktree.
	sharedRepo := filepath.Join(e.shared, "gateway")
	if got := e.git(sharedRepo, "show", "HEAD:feature.txt"); got != "hello" {
		t.Errorf("shared HEAD feature.txt = %q", got)
	}
	if _, err := os.Stat(wtRoot); !os.IsNotExist(err) {
		t.Errorf("worktree root still present after committed: %v", err)
	}
	if out := e.git(sharedRepo, "branch", "--list", "wm/*"); out != "" {
		t.Errorf("task branch not deleted: %q", out)
	}
	tk := e.loadTask("api")
	if tk.Status != task.StatusCommitted || tk.Worktree != "" || tk.TaskBranch != "wm/feat/wt/api" || len(tk.Base) != 1 {
		t.Errorf("task = status %s worktree %q branch %q base %v", tk.Status, tk.Worktree, tk.TaskBranch, tk.Base)
	}
	if len(tk.Commits) != 1 || tk.Commits[0].Hash != e.git(sharedRepo, "rev-parse", "HEAD") {
		t.Errorf("commits = %v", tk.Commits)
	}
	for _, ev := range []string{"worktree_created", "worktree_removed"} {
		if !strings.Contains(snap, ev) {
			t.Errorf("audit missing %s:\n%s", ev, snap)
		}
	}
	if _, err := os.Stat(filepath.Join(e.shared, ".orch", "instructions.md")); err != nil {
		t.Errorf("commit agent's handoff should be in the shared workspace: %v", err)
	}
}

// TestWorktreeRetryReusesDirtyWorktree: a failed attempt's worktree — commits,
// uncommitted and untracked edits — is kept as it is, the retry runs in it and
// is told so, and the commit agent integrates what both attempts left.
func TestWorktreeRetryReusesDirtyWorktree(t *testing.T) {
	e := newWtEnv(t)
	mustSaveTask(t, e.taskPath("api"), &task.Task{Name: "api", Status: task.StatusReady})
	probe := filepath.Join(t.TempDir(), "probe")

	// Attempt 1: commits one file, leaves another uncommitted, then fails.
	e.script(agent.TaskAgent, fmt.Sprintf(`
echo one > gateway/committed.txt
git -C gateway add committed.txt
git -C gateway %s commit -q -m attempt-1
echo wip > gateway/wip.txt
%s`, gitID, setStatus(e.taskPath("api"), "failed")))
	// Attempt 2: sees attempt 1's work and its retry instructions.
	e.script(agent.TaskAgent, fmt.Sprintf(`
pwd > %[1]q
cat gateway/wip.txt >> %[1]q
git -C gateway log --format=%%s -1 >> %[1]q
grep -c 'This is attempt 2' .orch/instructions.md >> %[1]q
echo two >> gateway/wip.txt
%[2]s`, probe, setStatus(e.taskPath("api"), "success")))
	e.script(agent.CommitAgent, e.commitWorktreeScript("api"))

	e.start()
	e.saveProject()
	snap := e.wait("project_idle")

	p, err := os.ReadFile(probe)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(p)), "\n")
	want := []string{worktree.Root(e.shared, "api"), "wip", "attempt-1", "1"}
	if len(lines) != len(want) || worktreeReal(lines[0]) != worktreeReal(want[0]) || strings.Join(lines[1:], ",") != strings.Join(want[1:], ",") {
		t.Fatalf("retry saw %q, want %q", lines, want)
	}
	if !strings.Contains(snap, "worktree_reused") || !strings.Contains(snap, "dirty=true") {
		t.Errorf("expected a dirty worktree_reused:\n%s", snap)
	}
	if n := strings.Count(snap, "worktree_created"); n != 1 {
		t.Errorf("worktree_created %d times, want once (the retry reuses it)", n)
	}
	sharedRepo := filepath.Join(e.shared, "gateway")
	if got := e.git(sharedRepo, "show", "HEAD:committed.txt"); got != "one" {
		t.Errorf("attempt 1's commit missing from the shared branch: %q", got)
	}
	if got := e.git(sharedRepo, "show", "HEAD:wip.txt"); got != "wip\ntwo" {
		t.Errorf("wip.txt on the shared branch = %q", got)
	}
	if tk := e.loadTask("api"); tk.Status != task.StatusCommitted || tk.Attempts != 1 {
		t.Errorf("task = %s attempts %d", tk.Status, tk.Attempts)
	}
}

// TestWorktreeLegacyInFlightFinishesInSharedTree: a task an older daemon left
// at success in the shared tree (no task_branch) is committed the legacy way,
// in the shared tree, and only then does the next task get a worktree (D7).
func TestWorktreeLegacyInFlightFinishesInSharedTree(t *testing.T) {
	e := newWtEnv(t)
	e.provision()
	sharedRepo := filepath.Join(e.shared, "gateway")
	// The legacy task's uncommitted work sits in the shared tree.
	if err := os.WriteFile(filepath.Join(sharedRepo, "legacy.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustSaveTask(t, e.taskPath("old"), &task.Task{Name: "old", Status: task.StatusSuccess})
	mustSaveTask(t, e.taskPath("new"), &task.Task{Name: "new", Status: task.StatusReady, DependsOn: []string{"old"}})
	probe := filepath.Join(t.TempDir(), "probe")

	// Legacy commit agent: commits the shared tree, as today.
	e.script(agent.CommitAgent, fmt.Sprintf(`
grep -c 'ff-only' .orch/instructions.md >> %[1]q || true
git -C gateway add -A
git -C gateway %[2]s commit -q -m old
%[3]s`, probe, gitID, setStatus(e.taskPath("old"), "committed")))
	e.script(agent.TaskAgent, fmt.Sprintf("pwd >> %q\necho new > gateway/new.txt\n%s", probe, setStatus(e.taskPath("new"), "success")))
	e.script(agent.CommitAgent, e.commitWorktreeScript("new"))

	e.start()
	e.saveProject()
	e.wait("project_idle")

	p, _ := os.ReadFile(probe)
	lines := strings.Split(strings.TrimSpace(string(p)), "\n")
	if len(lines) != 2 || lines[0] != "0" {
		t.Fatalf("probe = %q; the legacy commit prompt must not carry the worktree branch", lines)
	}
	if worktreeReal(lines[1]) != worktreeReal(worktree.Root(e.shared, "new")) {
		t.Errorf("the task after the legacy one ran in %q, want its worktree", lines[1])
	}
	if old := e.loadTask("old"); old.HasTaskBranch() || old.Worktree != "" {
		t.Errorf("legacy task gained worktree fields: %+v", old)
	}
	if got := e.git(sharedRepo, "log", "--format=%s"); !strings.HasPrefix(got, "new\nold") {
		t.Errorf("shared history = %q, want new on top of old", got)
	}
}

// A dirty shared tree with no task to own it keeps a new task in the shared
// tree rather than cutting a worktree beside the stray edits (D7).
func TestWorktreeSkippedWhileSharedTreeDirty(t *testing.T) {
	e := newWtEnv(t)
	e.provision()
	if err := os.WriteFile(filepath.Join(e.shared, "gateway", "stray.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustSaveTask(t, e.taskPath("api"), &task.Task{Name: "api", Status: task.StatusReady})
	e.script(agent.TaskAgent, setStatus(e.taskPath("api"), "failed"))

	e.start()
	e.saveProject()
	snap := e.wait("worktree_skipped_legacy")
	if strings.Contains(snap, "worktree_created") {
		t.Errorf("cut a worktree beside a dirty shared tree:\n%s", snap)
	}
}

// A commit agent that claims committed without landing the branch on the
// shared branch is caught by the ancestor check: the project blocks and the
// worktree is kept.
func TestWorktreeCommittedWithoutIntegrationBlocks(t *testing.T) {
	e := newWtEnv(t)
	mustSaveTask(t, e.taskPath("api"), &task.Task{Name: "api", Status: task.StatusReady})
	e.script(agent.TaskAgent, fmt.Sprintf(`
echo x > gateway/x.txt
git -C gateway add x.txt
git -C gateway %s commit -q -m x
%s`, gitID, setStatus(e.taskPath("api"), "success")))
	e.script(agent.CommitAgent, setStatus(e.taskPath("api"), "committed"))

	e.start()
	projectPath := e.saveProject()
	snap := e.wait("project_blocked")
	if !strings.Contains(snap, "integration_verify_failed") {
		t.Errorf("expected integration_verify_failed:\n%s", snap)
	}
	if _, err := os.Stat(worktree.Path(e.shared, "api", "gateway")); err != nil {
		t.Errorf("worktree must be kept when integration did not land: %v", err)
	}
	if p, _ := project.Load(projectPath); p == nil || p.Status != project.StatusBlocked {
		t.Errorf("project not blocked")
	}
}

// A worktree that can't be cut is a failed launch: worktree_create_error, the
// task back at ready without spending a retry, the project blocked.
func TestWorktreeCreateErrorBlocks(t *testing.T) {
	e := newWtEnv(t)
	e.provision()
	// A ref under wm/feat/wt/api/ makes the branch wm/feat/wt/api impossible.
	e.git(filepath.Join(e.shared, "gateway"), "branch", "wm/feat/wt/api/child")
	mustSaveTask(t, e.taskPath("api"), &task.Task{Name: "api", Status: task.StatusReady})

	e.start()
	e.saveProject()
	snap := e.wait("project_blocked")
	for _, want := range []string{"worktree_create_error", "repo=gateway", "why=worktree_failed"} {
		if !strings.Contains(snap, want) {
			t.Errorf("audit missing %q:\n%s", want, snap)
		}
	}
	e.mu.Lock()
	n := e.starts[agent.TaskAgent]
	e.mu.Unlock()
	if n != 0 {
		t.Errorf("task agent launched %d times despite the worktree failure", n)
	}
	if tk := e.loadTask("api"); tk.Status != task.StatusReady || tk.Attempts != 0 {
		t.Errorf("task = %s attempts %d, want ready/0", tk.Status, tk.Attempts)
	}
}

// TestWorktreeResumeAfterStop: a stopped task is released to ready with its
// worktree intact, and the next dispatch (`:start`) reuses it as it is,
// telling the agent it is resuming.
func TestWorktreeResumeAfterStop(t *testing.T) {
	e := newWtEnv(t)
	a := audit.New(e.buf)
	d, err := newDaemon(t, []string{e.root}, a, WithRunner(&runner.Runner{Workspaces: e.stub, Audit: a}))
	if err != nil {
		t.Fatal(err)
	}
	d.ctx = context.Background()
	projectPath := e.saveProject()
	p, _ := project.Load(projectPath)
	tp := e.taskPath("api")
	mustSaveTask(t, tp, &task.Task{Name: "api", Status: task.StatusReady})

	first := e.loadTask("api")
	var plan runner.Plan
	if err := d.prepareTaskWorktree(projectPath, p, first, &plan); err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	if plan.TaskWorktree == "" || plan.Resumed || len(plan.WorktreeMounts) == 0 {
		t.Fatalf("first plan = %+v, want a fresh worktree with git-dir mounts", plan)
	}
	gitDir := filepath.Join(e.shared, "gateway", ".git")
	if worktreeReal(plan.WorktreeMounts[0]) != worktreeReal(gitDir) {
		t.Errorf("mounts = %v, want the repo's git common dir first", plan.WorktreeMounts)
	}
	base := e.loadTask("api").Base
	// The interrupted run left an edit; stop released the task to ready.
	edit := filepath.Join(worktree.Path(e.shared, "api", "gateway"), "half.txt")
	if err := os.WriteFile(edit, []byte("half\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	again := e.loadTask("api")
	var plan2 runner.Plan
	if err := d.prepareTaskWorktree(projectPath, p, again, &plan2); err != nil {
		t.Fatalf("second prepare: %v", err)
	}
	if plan2.TaskWorktree != plan.TaskWorktree || !plan2.Resumed {
		t.Errorf("second plan = %+v, want the same worktree, resumed", plan2)
	}
	if b, err := os.ReadFile(edit); err != nil || string(b) != "half\n" {
		t.Errorf("the interrupted run's edit was not kept: %q %v", b, err)
	}
	if got := e.loadTask("api").Base; formatCommits(got) != formatCommits(base) {
		t.Errorf("base changed on reuse: %v → %v", base, got)
	}
	if !strings.Contains(e.buf.String(), "worktree_reused") {
		t.Errorf("expected worktree_reused:\n%s", e.buf.String())
	}
}

func TestMergeBaseKeepsRecordedBaseOnReuse(t *testing.T) {
	cur := []task.Commit{{Repo: "a", Hash: "old"}}
	res := worktree.Result{Repos: []worktree.RepoResult{
		{Repo: "a", Action: worktree.Reused},
		{Repo: "b", Action: worktree.Created, Base: "bb"},
	}}
	got := mergeBase(cur, res)
	if formatCommits(got) != "a=old,b=bb" {
		t.Errorf("mergeBase = %v", got)
	}
	res = worktree.Result{Repos: []worktree.RepoResult{{Repo: "a", Action: worktree.Created, Base: "new"}}}
	if got := mergeBase(cur, res); formatCommits(got) != "a=new" {
		t.Errorf("a re-cut repo must record its new base: %v", got)
	}
}

// A daemon whose workspace manager is not git-backed never cuts worktrees.
func TestWorktreesDisabledWithoutGitBackedManager(t *testing.T) {
	a := audit.New(&safeBuf{})
	d, err := newDaemon(t, []string{t.TempDir()}, a, WithRunner(&runner.Runner{Workspaces: workspace.NewStub(t.TempDir())}))
	if err != nil {
		t.Fatal(err)
	}
	if d.worktreesEnabled() {
		t.Error("plain stub manager must not enable worktrees")
	}
	d2, err := newDaemon(t, []string{t.TempDir()}, a, WithRunner(&runner.Runner{Workspaces: workspace.NewGitStub(t.TempDir())}))
	if err != nil {
		t.Fatal(err)
	}
	if !d2.worktreesEnabled() {
		t.Error("git stub manager must enable worktrees")
	}
}

func TestSkipWatchDir(t *testing.T) {
	if !skipWatchDir(".worktrees") || skipWatchDir("tasks") {
		t.Error("skipWatchDir must skip exactly .worktrees")
	}
}

// worktreeReal resolves symlinks (macOS /var → /private/var) so paths printed
// by `pwd` compare equal to the ones the test built.
func worktreeReal(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}
