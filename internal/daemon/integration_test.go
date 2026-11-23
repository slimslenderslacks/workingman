package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/task"
	"github.com/slimslenderslacks/work/internal/worktree"
)

// The tests in this file run phase 2 of docs/parallel-task-worktrees.md: the
// commit agent integrates a task branch by rebasing it onto the shared tip and
// fast-forwarding the shared branch (I3), and the daemon routes what it
// reports (I4). The commit agents are shell scripts following commit.tmpl's
// worktree procedure, doing real git work against the stub's real repos.

// taskFileShell is the shell helpers a scripted agent uses to read and edit
// its task file (yaml.v3's layout: top-level keys at column 0, lists indented
// by four).
const taskFileShell = `
base_of() { awk -v r="$1" '/^base:/{b=1;next} /^[^ ]/{b=0} b && $0 ~ ("repo: " r "$") {f=1;next} b && f && /hash:/ {print $2; exit}' "$TP"; }
setfield() { awk -v k="$1" -v v="$2" 'BEGIN{d=0} $0 ~ ("^" k ":") {print k ": " v; d=1; next} {print} END{if(!d) print k ": " v}' "$TP" > "$TP.tmp" && mv "$TP.tmp" "$TP"; }
dropblock() { awk -v k="$1" '$0 ~ ("^" k ":") {s=1;next} s && /^[^ ]/ {s=0} !s {print}' "$TP" > "$TP.tmp" && mv "$TP.tmp" "$TP"; }
`

// integrateOpts tunes the scripted commit agent.
type integrateOpts struct {
	// stopAfterFirstPublish exits right after fast-forwarding the first repo,
	// leaving the task at success: a commit agent killed between two repos'
	// fast-forwards.
	stopAfterFirstPublish bool
	// bogusHash records this hash in commits: instead of the real shared tip.
	bogusHash string
}

// integrateScript is a commit agent following the worktree branch of
// commit.tmpl (I3): commit leftovers, skip repos at their base, rebase every
// repo onto the shared tip (aborting all of them on a conflict), run the
// build check after a rebase, fast-forward, record.
func (e *wtEnv) integrateScript(taskName string, o integrateOpts) string {
	tp := e.taskPath(taskName)
	return fmt.Sprintf(`
TP=%[1]q; WT=%[2]q; SH=%[3]q; TB=%[4]q; PB=%[5]q; REPOS=%[6]q; VERIFY=%[7]q
STOP=%[8]q; BOGUS=%[9]q
%[10]s
fail() {
  dropblock integration; setfield status failed; setfield failure_reason "\"$4\""
  { echo "integration:"; echo "    result: $1"; echo "    repo: $2"
    if [ -n "$3" ]; then echo "    files:"; for f in $3; do echo "        - $f"; done; fi
    if [ -n "$5" ]; then echo "    published:"; for f in $5; do echo "        - $f"; done; fi; } >> "$TP"
  exit 0
}
work=""
for r in $REPOS; do
  w="$WT/$r"
  if [ -n "$(git -C "$w" status --porcelain)" ]; then git -C "$w" add -A; git -C "$w" %[11]s commit -q -m %[12]q; fi
  if [ "$(git -C "$w" rev-parse HEAD)" != "$(base_of "$r")" ]; then work="$work $r"; fi
done
rebased=""
for r in $work; do
  w="$WT/$r"
  onto=$(git -C "$SH/$r" rev-parse "refs/heads/$PB")
  eval "saved_$r=$(git -C "$w" rev-parse HEAD)"
  if ! git -C "$w" merge-base --is-ancestor "$onto" HEAD; then
    if ! git -C "$w" %[11]s rebase -q "$onto" >/dev/null 2>&1; then
      files=$(git -C "$w" diff --name-only --diff-filter=U)
      git -C "$w" rebase --abort
      for x in $rebased; do eval "git -C \"$WT/$x\" reset -q --hard \$saved_$x"; done
      fail conflict "$r" "$files" "integration conflict in $r"
    fi
    rebased="$rebased $r"
  fi
done
if [ -n "$rebased" ] && [ -n "$VERIFY" ]; then
  if ! (cd "$WT" && sh -c "$VERIFY") >/dev/null 2>&1; then
    for x in $rebased; do eval "git -C \"$WT/$x\" reset -q --hard \$saved_$x"; done
    set -- $rebased
    fail verify_failed "$1" "" "build check failed after rebase"
  fi
fi
published=""
for r in $work; do
  if ! git -C "$SH/$r" merge -q --ff-only "$TB" >/dev/null 2>&1; then
    if [ -n "$published" ]; then fail partial "$r" "" "fast-forward failed in $r" "$published"; fi
    fail error "$r" "" "fast-forward failed in $r"
  fi
  published="$published $r"
  if [ -n "$STOP" ]; then exit 0; fi
done
dropblock integration; dropblock commits
if [ -n "$work" ]; then
  echo "commits:" >> "$TP"
  for r in $work; do
    h=$(git -C "$SH/$r" rev-parse "refs/heads/$PB")
    if [ -n "$BOGUS" ]; then h="$BOGUS"; fi
    printf '    - repo: %%s\n      hash: %%s\n' "$r" "$h" >> "$TP"
  done
fi
setfield status committed
`, tp, worktree.Root(e.shared, taskName), e.shared, worktree.BranchName(wtBranch, taskName), wtBranch,
		strings.Join(e.repos, " "), e.verify, boolFlag(o.stopAfterFirstPublish), o.bogusHash,
		taskFileShell, gitID, taskName)
}

func boolFlag(b bool) string {
	if b {
		return "1"
	}
	return ""
}

// commitShared is the shell of a human committing directly on the shared
// branch, in repo, outside the queue: the tip moves under an integration.
func (e *wtEnv) commitShared(repo, file, content string) string {
	dir := filepath.Join(e.shared, repo)
	return fmt.Sprintf("printf '%%s\\n' %[1]q > %[2]q/%[3]s\ngit -C %[2]q add %[3]s\ngit -C %[2]q %[4]s commit -q -m 'manual %[3]s'\n",
		content, dir, file, gitID)
}

func (e *wtEnv) count(kind agent.Kind) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.starts[kind]
}

func (e *wtEnv) blockedReason(projectPath string) string {
	e.t.Helper()
	p, err := project.Load(projectPath)
	if err != nil {
		e.t.Fatalf("load project: %v", err)
	}
	if p.Status != project.StatusBlocked {
		e.t.Fatalf("project status = %s, want blocked", p.Status)
	}
	return p.BlockedReason
}

// TestIntegrationConflictReRunsInSameWorktree: a human commits a conflicting
// change on the shared branch while the task runs. The commit agent's rebase
// conflicts and aborts; the daemon re-runs the task (attempts 1) in the same
// worktree with the conflict in its prompt; the re-run rebases and resolves;
// the next integration is a fast path and lands.
func TestIntegrationConflictReRunsInSameWorktree(t *testing.T) {
	e := newWtEnv(t)
	e.verify = "true"
	mustSaveTask(t, e.taskPath("api"), &task.Task{Name: "api", Status: task.StatusReady})
	probe := filepath.Join(t.TempDir(), "probe")
	tp := e.taskPath("api")

	// Attempt 1: commits its version of shared.txt; a human commits another
	// version on the shared branch meanwhile.
	e.script(agent.TaskAgent, fmt.Sprintf(`
pwd > %[1]q
printf 'task\n' > gateway/shared.txt
git -C gateway add shared.txt
git -C gateway %[2]s commit -q -m task-version
%[3]s%[4]s`, probe, gitID, e.commitShared("gateway", "shared.txt", "human"), setStatus(tp, "success")))
	e.script(agent.CommitAgent, e.integrateScript("api", integrateOpts{}))
	// Attempt 2 (the re-run): told about the conflict, rebases its own branch
	// and resolves it by keeping both lines.
	e.script(agent.TaskAgent, fmt.Sprintf(`
pwd >> %[1]q
grep -c 'git rebase refs/heads/%[2]s' .orch/instructions.md >> %[1]q
grep -c 'shared.txt' .orch/instructions.md >> %[1]q
git -C gateway %[3]s rebase -q refs/heads/%[2]s >/dev/null 2>&1 || true
printf 'human\ntask\n' > gateway/shared.txt
git -C gateway add shared.txt
GIT_EDITOR=true git -C gateway %[3]s rebase --continue >/dev/null
%[4]s`, probe, wtBranch, gitID, setStatus(tp, "success")))
	e.script(agent.CommitAgent, e.integrateScript("api", integrateOpts{}))

	e.start()
	e.saveProject()
	snap := e.wait("project_idle")

	for _, want := range []string{"integration_conflicted", "files=shared.txt", "cause=conflict", "integration_succeeded", "fast_forward=true", "why=fast_path"} {
		if !strings.Contains(snap, want) {
			t.Errorf("audit missing %q:\n%s", want, snap)
		}
	}
	if n := strings.Count(snap, "worktree_created"); n != 1 {
		t.Errorf("worktree_created %d times, want once (the re-run reuses it)", n)
	}
	p, _ := os.ReadFile(probe)
	lines := strings.Split(strings.TrimSpace(string(p)), "\n")
	if len(lines) != 4 || lines[0] != lines[1] || lines[2] == "0" || lines[3] == "0" {
		t.Errorf("re-run probe = %q: want the same worktree and the conflict in the prompt", lines)
	}
	sharedRepo := filepath.Join(e.shared, "gateway")
	if got := e.git(sharedRepo, "show", "HEAD:shared.txt"); got != "human\ntask" {
		t.Errorf("shared.txt = %q", got)
	}
	if got := e.git(sharedRepo, "log", "--format=%s", "-2"); got != "task-version\nmanual shared.txt" {
		t.Errorf("shared history = %q, want the task rebased on top of the manual commit", got)
	}
	tk := e.loadTask("api")
	if tk.Status != task.StatusCommitted || tk.Attempts != 1 || tk.Integration != nil {
		t.Errorf("task = %s attempts %d integration %+v", tk.Status, tk.Attempts, tk.Integration)
	}
}

// TestIntegrationConflictExhaustedBlocks: a conflict on the last attempt
// blocks with the I6 reason, naming the task that landed in between, the
// repo and files and the attempt count, and keeps the worktree.
func TestIntegrationConflictExhaustedBlocks(t *testing.T) {
	e := newWtEnv(t)
	tp := e.taskPath("api")
	mustSaveTask(t, tp, &task.Task{Name: "api", Status: task.StatusReady, Attempts: maxTaskAttempts})
	other := e.taskPath("auth")
	// The task agent's run doubles as the other task landing: a commit on the
	// shared branch recorded by a committed task "auth".
	e.script(agent.TaskAgent, fmt.Sprintf(`
printf 'task\n' > gateway/handler.go
%[1]s
h=$(git -C %[2]q rev-parse HEAD)
printf 'name: auth\ndescription: x\nstatus: committed\nattempts: 0\nmodel: default\ncommits:\n    - repo: gateway\n      hash: %%s\n' "$h" > %[3]q
%[4]s`, e.commitShared("gateway", "handler.go", "auth"), filepath.Join(e.shared, "gateway"), other, setStatus(tp, "success")))
	e.script(agent.CommitAgent, e.integrateScript("api", integrateOpts{}))

	e.start()
	projectPath := e.saveProject()
	e.wait("project_blocked")

	want := `task "api" conflicted with ["auth"] in gateway: handler.go after 3 attempts`
	if got := e.blockedReason(projectPath); got != want {
		t.Errorf("blocked_reason = %q, want %q", got, want)
	}
	tk := e.loadTask("api")
	if tk.Status != task.StatusFailed || tk.Integration == nil || strings.Join(tk.Integration.Landed, ",") != "auth" || !tk.SaveSandbox {
		t.Errorf("task = %s integration %+v save_sandbox %v", tk.Status, tk.Integration, tk.SaveSandbox)
	}
	if _, err := os.Stat(worktree.Path(e.shared, "api", "gateway")); err != nil {
		t.Errorf("worktree must be kept for the wolf: %v", err)
	}
	if n := e.count(agent.TaskAgent); n != 1 {
		t.Errorf("task agent ran %d times, want 1 (no budget left)", n)
	}
}

// TestIntegrationTransientRetriesCommitAgentOnly: the shared tip moves under
// the commit agent and its fast-forward fails (result error). The daemon
// relaunches the commit agent only — not the task — which rebases onto the
// new tip, runs the build check because it rebased, and lands.
func TestIntegrationTransientRetriesCommitAgentOnly(t *testing.T) {
	e := newWtEnv(t)
	e.verify = "test -f gateway/feature.txt"
	tp := e.taskPath("api")
	mustSaveTask(t, tp, &task.Task{Name: "api", Status: task.StatusReady})
	e.script(agent.TaskAgent, "echo hello > gateway/feature.txt\n"+setStatus(tp, "success"))
	// First commit agent: snapshots, the tip moves (a human commit), its
	// ff-only fails, it reports an integration error.
	e.script(agent.CommitAgent, fmt.Sprintf(`
TP=%[1]q
%[2]s
git -C %[3]q add -A && git -C %[3]q %[4]s commit -q -m api
%[5]s
git -C %[6]q merge -q --ff-only %[7]q 2>/dev/null && exit 1
setfield status failed
setfield failure_reason '"fast-forward failed in gateway"'
printf 'integration:\n    result: error\n    repo: gateway\n' >> "$TP"
`, tp, taskFileShell, worktree.Path(e.shared, "api", "gateway"), gitID,
		e.commitShared("gateway", "other.txt", "human"), filepath.Join(e.shared, "gateway"), worktree.BranchName(wtBranch, "api")))
	e.script(agent.CommitAgent, e.integrateScript("api", integrateOpts{}))

	e.start()
	e.saveProject()
	snap := e.wait("project_idle")

	for _, want := range []string{"integration_retry", "integration_attempts=1", "integration_verify_check_passed", "fast_forward=false"} {
		if !strings.Contains(snap, want) {
			t.Errorf("audit missing %q:\n%s", want, snap)
		}
	}
	if n := e.count(agent.TaskAgent); n != 1 {
		t.Errorf("task agent ran %d times, want 1: a transient failure must not re-run the task", n)
	}
	if n := e.count(agent.CommitAgent); n != 2 {
		t.Errorf("commit agent ran %d times, want 2", n)
	}
	sharedRepo := filepath.Join(e.shared, "gateway")
	if got := e.git(sharedRepo, "log", "--format=%s", "-2"); got != "api\nmanual other.txt" {
		t.Errorf("shared history = %q", got)
	}
	tk := e.loadTask("api")
	if tk.Status != task.StatusCommitted || tk.Attempts != 0 || tk.Integration != nil {
		t.Errorf("task = %s attempts %d integration %+v", tk.Status, tk.Attempts, tk.Integration)
	}
}

// TestIntegrationTransientExhaustedBlocks: a commit agent that keeps ending
// with the task still at success is relaunched maxIntegrationAttempts times,
// then the project blocks.
func TestIntegrationTransientExhaustedBlocks(t *testing.T) {
	e := newWtEnv(t)
	tp := e.taskPath("api")
	mustSaveTask(t, tp, &task.Task{Name: "api", Status: task.StatusReady})
	e.script(agent.TaskAgent, "echo x > gateway/x.txt\n"+setStatus(tp, "success"))
	// Commit agents with no script just exit.

	e.start()
	projectPath := e.saveProject()
	e.wait("project_blocked")
	if n := e.count(agent.CommitAgent); n != 1+maxIntegrationAttempts {
		t.Errorf("commit agent ran %d times, want %d", n, 1+maxIntegrationAttempts)
	}
	if got := e.blockedReason(projectPath); !strings.Contains(got, `integration of task "api" failed after 2 commit agent retries`) {
		t.Errorf("blocked_reason = %q", got)
	}
}

// TestIntegrationMultiRepoConflictLeavesOtherRepoUnpublished: repo alpha
// rebases cleanly, repo beta conflicts. Nothing is published: alpha's shared
// branch does not have the task's commit, and alpha's task branch is back at
// its pre-rebase tip.
func TestIntegrationMultiRepoConflictLeavesOtherRepoUnpublished(t *testing.T) {
	e := newWtEnv(t)
	e.repos = []string{"alpha", "beta"}
	tp := e.taskPath("api")
	mustSaveTask(t, tp, &task.Task{Name: "api", Status: task.StatusReady, Attempts: maxTaskAttempts})
	e.script(agent.TaskAgent, fmt.Sprintf(`
echo a > alpha/a.txt
echo b > beta/b.txt
git -C alpha add -A && git -C alpha %[1]s commit -q -m task-a
%[2]s%[3]s%[4]s`, gitID,
		e.commitShared("alpha", "unrelated.txt", "human"), e.commitShared("beta", "b.txt", "human"), setStatus(tp, "success")))
	e.script(agent.CommitAgent, e.integrateScript("api", integrateOpts{}))

	e.start()
	projectPath := e.saveProject()
	e.wait("project_blocked")

	if got := e.blockedReason(projectPath); !strings.Contains(got, "in beta: b.txt") {
		t.Errorf("blocked_reason = %q, want the beta conflict", got)
	}
	alpha := filepath.Join(e.shared, "alpha")
	if got := e.git(alpha, "log", "--format=%s", "-1"); got != "manual unrelated.txt" {
		t.Errorf("alpha's shared branch moved: HEAD is %q", got)
	}
	wtAlpha := worktree.Path(e.shared, "api", "alpha")
	base := ""
	for _, b := range e.loadTask("api").Base {
		if b.Repo == "alpha" {
			base = b.Hash
		}
	}
	if parent := e.git(wtAlpha, "rev-parse", "HEAD~1"); parent != base {
		t.Errorf("alpha's task branch was left rebased: parent %s, base %s", parent, base)
	}
	if tk := e.loadTask("api"); tk.Integration == nil || tk.Integration.Repo != "beta" || len(tk.Integration.Published) != 0 {
		t.Errorf("integration = %+v", tk.Integration)
	}
}

// TestIntegrationResumesAfterKillBetweenFastForwards: the first commit agent
// publishes alpha and dies before beta. The relaunch runs the procedure from
// the top: alpha is already published (still counted, because its branch
// differs from base), beta lands, commits: lists both.
func TestIntegrationResumesAfterKillBetweenFastForwards(t *testing.T) {
	e := newWtEnv(t)
	e.repos = []string{"alpha", "beta"}
	tp := e.taskPath("api")
	mustSaveTask(t, tp, &task.Task{Name: "api", Status: task.StatusReady})
	e.script(agent.TaskAgent, "echo a > alpha/a.txt\necho b > beta/b.txt\n"+setStatus(tp, "success"))
	e.script(agent.CommitAgent, e.integrateScript("api", integrateOpts{stopAfterFirstPublish: true}))
	e.script(agent.CommitAgent, e.integrateScript("api", integrateOpts{}))

	e.start()
	e.saveProject()
	snap := e.wait("project_idle")

	if !strings.Contains(snap, "integration_retry") {
		t.Errorf("expected the interrupted integration to be retried:\n%s", snap)
	}
	tk := e.loadTask("api")
	if tk.Status != task.StatusCommitted || len(tk.Commits) != 2 {
		t.Fatalf("task = %s commits %v, want committed with both repos", tk.Status, tk.Commits)
	}
	for _, c := range tk.Commits {
		dir := filepath.Join(e.shared, c.Repo)
		if c.Hash != e.git(dir, "rev-parse", "HEAD") {
			t.Errorf("%s commit %s is not the shared tip", c.Repo, c.Hash)
		}
	}
	if got := e.git(filepath.Join(e.shared, "beta"), "show", "HEAD:b.txt"); got != "b" {
		t.Errorf("beta not published: %q", got)
	}
	if n := e.count(agent.TaskAgent); n != 1 {
		t.Errorf("task agent ran %d times, want 1", n)
	}
}

// TestIntegrationVerifyFailedReRuns: the build check fails after a clean
// rebase. The task re-runs (attempts 1) with the failure in its prompt; its
// base is re-stamped, so the next integration is a fast path that skips the
// check and lands.
func TestIntegrationVerifyFailedReRuns(t *testing.T) {
	e := newWtEnv(t)
	e.verify = "false"
	tp := e.taskPath("api")
	mustSaveTask(t, tp, &task.Task{Name: "api", Status: task.StatusReady})
	probe := filepath.Join(t.TempDir(), "probe")
	e.script(agent.TaskAgent, "echo x > gateway/x.txt\n"+e.commitShared("gateway", "y.txt", "human")+setStatus(tp, "success"))
	e.script(agent.CommitAgent, e.integrateScript("api", integrateOpts{}))
	e.script(agent.TaskAgent, fmt.Sprintf(`
grep -c 'build check' .orch/instructions.md > %[1]q
git -C gateway %[2]s rebase -q refs/heads/%[3]s
%[4]s`, probe, gitID, wtBranch, setStatus(tp, "success")))
	e.script(agent.CommitAgent, e.integrateScript("api", integrateOpts{}))

	e.start()
	e.saveProject()
	snap := e.wait("project_idle")
	for _, want := range []string{"integration_verify_check_failed", "cause=verify", "why=fast_path"} {
		if !strings.Contains(snap, want) {
			t.Errorf("audit missing %q:\n%s", want, snap)
		}
	}
	if p, _ := os.ReadFile(probe); strings.TrimSpace(string(p)) == "0" {
		t.Errorf("the re-run's prompt does not mention the failed build check")
	}
	if tk := e.loadTask("api"); tk.Status != task.StatusCommitted || tk.Attempts != 1 {
		t.Errorf("task = %s attempts %d", tk.Status, tk.Attempts)
	}
}

// TestIntegrationVerifyFailureBlocks: the commit agent publishes, but records
// a commit that is not on the shared branch. The daemon's ancestor check
// catches it and blocks, keeping the worktree.
func TestIntegrationVerifyFailureBlocks(t *testing.T) {
	e := newWtEnv(t)
	e.provision()
	sharedRepo := filepath.Join(e.shared, "gateway")
	e.git(sharedRepo, "checkout", "-q", "-b", "elsewhere")
	e.git(sharedRepo, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false",
		"commit", "-q", "--allow-empty", "-m", "not on the shared branch")
	bogus := e.git(sharedRepo, "rev-parse", "HEAD")
	e.git(sharedRepo, "checkout", "-q", wtBranch)

	tp := e.taskPath("api")
	mustSaveTask(t, tp, &task.Task{Name: "api", Status: task.StatusReady})
	e.script(agent.TaskAgent, "echo x > gateway/x.txt\n"+setStatus(tp, "success"))
	e.script(agent.CommitAgent, e.integrateScript("api", integrateOpts{bogusHash: bogus}))

	e.start()
	projectPath := e.saveProject()
	snap := e.wait("project_blocked")
	if !strings.Contains(snap, "integration_verify_failed") || !strings.Contains(snap, "hash="+bogus) {
		t.Errorf("expected integration_verify_failed for %s:\n%s", bogus, snap)
	}
	if got := e.blockedReason(projectPath); !strings.Contains(got, bogus) {
		t.Errorf("blocked_reason = %q", got)
	}
	if _, err := os.Stat(worktree.Path(e.shared, "api", "gateway")); err != nil {
		t.Errorf("worktree must be kept: %v", err)
	}
}

// TestIntegrationDirtySharedTreeBlocks: prepareIntegration refuses to launch
// the commit agent over uncommitted changes in the shared tree.
func TestIntegrationDirtySharedTreeBlocks(t *testing.T) {
	e := newWtEnv(t)
	tp := e.taskPath("api")
	mustSaveTask(t, tp, &task.Task{Name: "api", Status: task.StatusReady})
	e.script(agent.TaskAgent, fmt.Sprintf("echo x > gateway/x.txt\necho stray > %q\n%s",
		filepath.Join(e.shared, "gateway", "stray.txt"), setStatus(tp, "success")))

	e.start()
	projectPath := e.saveProject()
	e.wait("project_blocked")
	if got := e.blockedReason(projectPath); !strings.Contains(got, "shared workspace gateway has uncommitted changes") {
		t.Errorf("blocked_reason = %q", got)
	}
	if n := e.count(agent.CommitAgent); n != 0 {
		t.Errorf("commit agent launched %d times over a dirty shared tree", n)
	}
}

// TestPrepareIntegrationRecoversInterruptedRebase: a rebase left in progress
// in the task worktree is aborted (the branch is back at its pre-rebase tip)
// and stale index.lock files are cleared, before the commit agent runs again.
func TestPrepareIntegrationRecoversInterruptedRebase(t *testing.T) {
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
	var plan runner.Plan
	if err := d.prepareTaskWorktree(projectPath, p, e.loadTask("api"), &plan); err != nil {
		t.Fatal(err)
	}
	wt := worktree.Path(e.shared, "api", "gateway")
	sharedRepo := filepath.Join(e.shared, "gateway")
	run := func(dir, sh string) {
		t.Helper()
		if out, err := execSh(dir, sh); err != nil {
			t.Fatalf("%s: %v: %s", sh, err, out)
		}
	}
	run(wt, "echo task > f.txt && git add f.txt && git "+gitID+" commit -q -m task")
	run(sharedRepo, "echo human > f.txt && git add f.txt && git "+gitID+" commit -q -m human")
	saved := e.git(wt, "rev-parse", "HEAD")
	if out, err := execSh(wt, "git "+gitID+" rebase -q "+wtBranch); err == nil {
		t.Fatalf("expected the rebase to conflict: %s", out)
	}
	lock := filepath.Join(sharedRepo, ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	tk := e.loadTask("api")
	onto, err := d.prepareIntegration(projectPath, p, tk, worktree.Root(e.shared, "api"))
	if err != nil {
		t.Fatalf("prepareIntegration: %v", err)
	}
	if got := e.git(wt, "rev-parse", "HEAD"); got != saved {
		t.Errorf("task branch at %s after abort, want the pre-rebase tip %s", got, saved)
	}
	if busy, _ := d.worktrees.RebaseInProgress(d.ctx, wt); busy {
		t.Error("rebase still in progress")
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("stale index.lock not removed: %v", err)
	}
	if onto["gateway"] != e.git(sharedRepo, "rev-parse", "HEAD") {
		t.Errorf("onto = %v", onto)
	}
	for _, ev := range []string{"integration_rebase_aborted", "integration_lock_cleared"} {
		if !strings.Contains(e.buf.String(), ev) {
			t.Errorf("audit missing %s:\n%s", ev, e.buf.String())
		}
	}
}

// TestDocsOnlyVerdict: the build check is skipped only when the declared
// touches and the actual diff are both documentation.
func TestDocsOnlyVerdict(t *testing.T) {
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
	docs := []string{"gateway/docs/**", "gateway/README.md"}
	mustSaveTask(t, tp, &task.Task{Name: "api", Status: task.StatusReady, Touches: &docs})
	var plan runner.Plan
	if err := d.prepareTaskWorktree(projectPath, p, e.loadTask("api"), &plan); err != nil {
		t.Fatal(err)
	}
	root := worktree.Root(e.shared, "api")
	wt := filepath.Join(root, "gateway")
	if err := os.WriteFile(filepath.Join(wt, "README.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !d.docsOnly(e.loadTask("api"), root) {
		t.Error("a README edit with docs-only touches should be docs-only")
	}
	if err := os.WriteFile(filepath.Join(wt, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if d.docsOnly(e.loadTask("api"), root) {
		t.Error("a code edit outside the declared touches must run the check")
	}
	noHints := e.loadTask("api")
	noHints.Touches = nil
	if d.docsOnly(noHints, root) {
		t.Error("a task with no hints must run the check")
	}
	for p, want := range map[string]bool{"a/docs/x.go": true, "README.MD": true, "a/b.go": false, "documentation/x": false} {
		if isDocPath(p) != want {
			t.Errorf("isDocPath(%q) = %v", p, !want)
		}
	}
}

// execSh runs sh -c script in dir.
func execSh(dir, script string) ([]byte, error) {
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}
