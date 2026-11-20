package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/setup"
	"github.com/slimslenderslacks/work/internal/task"
	"github.com/slimslenderslacks/work/internal/worktree"
	"gopkg.in/yaml.v3"
)

// Before the archive agent launches (docs/parallel-task-worktrees.md W6, X6
// step 4), the daemon removes the worktrees of committed tasks and hands the
// agent every task worktree still holding unintegrated work: in its
// .orch/context.yaml and in its prompt.
func TestArchiveAgentPrepCleansCommittedAndListsUnintegrated(t *testing.T) {
	e := newWtEnv(t)
	e.seedWorktreeTask("done", task.StatusCommitted, map[string]string{"gateway": "done"})
	e.integrate("gateway", "done")
	e.seedWorktreeTask("stuck", task.StatusFailed, map[string]string{"gateway": "stuck"})
	e.seedWorktreeTask("dirty", task.StatusBlocked, nil)
	if err := os.WriteFile(filepath.Join(worktree.Path(e.shared, "dirty", "gateway"), "half.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.seedWorktreeTask("clean", task.StatusReady, nil)
	e.seedWorktreeTask("gone", task.StatusSuccess, map[string]string{"gateway": "gone"})
	if err := os.Remove(e.taskPath("gone")); err != nil {
		t.Fatal(err)
	}
	path := e.saveProject()
	p, err := project.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p.Cleanup = true
	if err := project.SaveAs(path, p, project.WriterAgent); err != nil {
		t.Fatal(err)
	}

	a := audit.New(e.buf)
	lch := &recordingLauncher{}
	d, err := newDaemon(t, []string{e.root}, a, WithRunner(&runner.Runner{
		Workspaces: e.stub,
		Launcher:   lch,
		Audit:      a,
		Command:    func(agent.Kind, string) []string { return []string{"true"} },
	}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.ctx = ctx
	t.Cleanup(func() { cancel(); lch.closeAll(); _ = d.watcher.Close() })

	d.launchArchiveAgent(path, p)
	if lch.countKind(agent.ArchiveAgent) != 1 {
		t.Fatalf("archive agent not launched:\n%s", e.buf.String())
	}

	if e.exists(worktree.Root(e.shared, "done")) || e.branchExists("gateway", "done") {
		t.Error("committed task's worktree or branch survived the pre-archive cleanup")
	}
	for _, name := range []string{"stuck", "dirty", "gone", "clean"} {
		if !e.exists(worktree.Root(e.shared, name)) {
			t.Errorf("%s's worktree removed before the archive", name)
		}
	}

	raw, err := os.ReadFile(filepath.Join(e.shared, ".orch", "context.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var c setup.Context
	if err := yaml.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	got := map[string]setup.UnintegratedTask{}
	for _, u := range c.UnintegratedTasks {
		got[u.Name] = u
	}
	if len(got) != 3 {
		t.Errorf("unintegrated_tasks = %+v, want stuck, dirty and gone", c.UnintegratedTasks)
	}
	if u := got["stuck"]; u.Status != "failed" || u.Branch != worktree.BranchName(wtBranch, "stuck") ||
		u.Worktree != worktree.Root(e.shared, "stuck") || !strings.Contains(u.Why, "is not on") {
		t.Errorf("stuck = %+v", u)
	}
	if u := got["dirty"]; u.Status != "blocked" || !strings.Contains(u.Why, "uncommitted") {
		t.Errorf("dirty = %+v", u)
	}
	if u := got["gone"]; u.Status != "orphaned" {
		t.Errorf("gone = %+v", u)
	}

	instr, err := os.ReadFile(filepath.Join(e.shared, ".orch", "instructions.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"WORK NOT CARRIED INTO THE BRANCH", "- stuck (failed)", "- gone (orphaned)"} {
		if !strings.Contains(string(instr), want) {
			t.Errorf("archive prompt missing %q:\n%s", want, instr)
		}
	}
	if s := e.buf.String(); !strings.Contains(s, "archive_unintegrated") {
		t.Errorf("no archive_unintegrated audit:\n%s", s)
	}
}
