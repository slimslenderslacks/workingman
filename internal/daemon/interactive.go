package daemon

import (
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/slimslenderslacks/work/internal/gitsign"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/sbx"
	"github.com/slimslenderslacks/work/internal/task"
	"github.com/slimslenderslacks/work/internal/terminal"
	"github.com/slimslenderslacks/work/internal/worktree"
)

// OpenOrchDirTab opens a host shell in a new terminal tab whose working
// directory is the work stream's orch directory (the project.md's parent).
// Backs the TUI's `:orch` command. It fails, without opening a tab, when the
// directory does not exist.
func (d *Daemon) OpenOrchDirTab(ctx context.Context, projectPath string) (string, error) {
	orchDir := filepath.Dir(projectPath)
	return d.openDirTab(ctx, "orch", orchDir, orchDir)
}

// OpenWorkspaceDirTab opens a host shell in a new terminal tab whose working
// directory is the root of the work stream's wsp workspace. Backs the TUI's
// `:workspace` command. Unlike `:session` it never provisions: it fails, without
// opening a tab, when the workspace does not exist.
func (d *Daemon) OpenWorkspaceDirTab(ctx context.Context, projectPath string) (string, error) {
	if d.runner == nil || d.runner.Workspaces == nil {
		return "", fmt.Errorf("workspace tabs need a workspace manager (none configured)")
	}
	p, err := project.Load(projectPath)
	if err != nil {
		return "", fmt.Errorf("load project: %w", err)
	}
	if p.Branch == "" {
		return "", fmt.Errorf("project %q has no branch to resolve a workspace from", projectPath)
	}
	dir, err := d.runner.Workspaces.Path(p.Branch)
	if err != nil {
		return "", fmt.Errorf("resolve workspace for %q: %w", p.Branch, err)
	}
	return d.openDirTab(ctx, "workspace", filepath.Dir(projectPath), dir)
}

// OpenTaskWorktreeDirTab opens a host shell in a new terminal tab whose working
// directory is the task's own worktree (its `worktree:` field,
// docs/parallel-task-worktrees.md D5). Backs `:workspace` when the TUI's Tasks
// pane has a task with a worktree selected. It fails, without opening a tab,
// when the task has no worktree or the directory is gone.
func (d *Daemon) OpenTaskWorktreeDirTab(ctx context.Context, projectPath, taskPath string) (string, error) {
	t, err := task.Load(taskPath)
	if err != nil {
		return "", fmt.Errorf("load task: %w", err)
	}
	if t.Worktree == "" {
		return "", fmt.Errorf("task %q has no worktree", t.Name)
	}
	return d.openDirTabNamed(ctx, "worktree", filepath.Dir(projectPath), t.Worktree, t.Name)
}

// openDirTab verifies dir exists and opens a host bash there in a terminal tab
// named "<kind>-<work-stream>", titled after the work stream. An already-open
// tab is focused and reported with a *terminal.AlreadyOpenError.
func (d *Daemon) openDirTab(ctx context.Context, kind, orchDir, dir string) (string, error) {
	return d.openDirTabNamed(ctx, kind, orchDir, dir, "")
}

// openDirTabNamed is openDirTab with an optional suffix, so that one work
// stream can have a tab per task: "<kind>-<work-stream>-<suffix>".
func (d *Daemon) openDirTabNamed(ctx context.Context, kind, orchDir, dir, suffix string) (string, error) {
	if d.runner == nil {
		return "", fmt.Errorf("terminal tabs need a runner (none configured)")
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%s directory %s does not exist", kind, dir)
	}
	workStream := filepath.Base(orchDir)
	name, title := kind+"-"+workStream, workStream
	if suffix != "" {
		name += "-" + suffix
		title += " " + suffix
	}
	target, err := d.runner.LaunchInteractive(ctx, runner.InteractiveSpec{
		Cwd:   dir,
		Inner: []string{"bash"},
		Name:  name,
		Title: title,
	})
	var already *terminal.AlreadyOpenError
	if errors.As(err, &already) {
		return target, err
	}
	if err != nil {
		return "", err
	}
	d.audit.Log("interactive_open", "work_stream", workStream, "kind", kind, "target", target)
	return target, nil
}

// OpenInteractiveSession opens (or resumes) the work stream's persistent
// interactive claude session in its `session-<work-stream>` sandbox — both the
// workspace worktree and the orch control dir mounted, cwd the worktree's root,
// no prompt injected. It is not an ACP session: the sandbox is set up the way
// sbx-create.sh does it (a dynamic MCP gateway with no servers preselected,
// acp-kit's image and kit, the claude env) and attached to the tab's pty with
// `sbx run`. The claude session id is derived deterministically from the work
// stream name, so closing the tab and re-running the command resumes the same
// conversation. The tab first prints sharedSessionBanner: with per-task
// worktrees, edits left in the shared tree block integration
// (docs/parallel-task-worktrees.md X3).
// It returns the session name the TUI can focus the tab by. Backs the TUI's
// `:session` command. If the work stream's session tab is already open it is
// focused instead, and a *terminal.AlreadyOpenError is returned with the name.
func (d *Daemon) OpenInteractiveSession(ctx context.Context, projectPath string) (string, error) {
	p, err := d.loadInteractiveProject(projectPath)
	if err != nil {
		return "", err
	}
	orchDir := filepath.Dir(projectPath)
	workStream := filepath.Base(orchDir)

	// Provision (idempotent) or resolve the workspace worktree — the same call
	// the task/commit agents make, so an already-provisioned work stream is a
	// cheap no-op and a fresh one gets set up on first use.
	workspaceDir, err := d.runner.Workspaces.Create(ctx, p.Branch, workspaceReposFor(p))
	if err != nil {
		return "", fmt.Errorf("provision workspace for %q: %w", p.Branch, err)
	}
	// Mount both the worktree and the orch control dir, like the task
	// agents, so the session can see the code and the project.yaml / tasks.
	// cwd is the worktree.
	return d.launchSession(ctx, workStream, "", []string{workspaceDir, orchDir}, workspaceDir,
		d.sharedSessionBanner(projectPath, p))
}

// OpenTaskSession is `:session` for a task that has its own worktree (its
// `worktree:` field), opened from the TUI's Tasks pane: the interactive claude
// session runs in that worktree, the useful place to poke at a failed or
// blocked task (docs/parallel-task-worktrees.md X3). It gets a sandbox, tab
// and conversation of its own (`session-<work-stream>-<task>`), mounted the
// way the task agent's is (W5): the worktree first (the cwd), every repo's
// git common dir, and the orch control dir. The shared tree is not mounted,
// so the session cannot leave it dirty. The tab first prints
// taskSessionBanner. It fails, without opening a tab, when the task has no
// worktree or the directory is gone.
func (d *Daemon) OpenTaskSession(ctx context.Context, projectPath, taskPath string) (string, error) {
	p, err := d.loadInteractiveProject(projectPath)
	if err != nil {
		return "", err
	}
	t, err := task.Load(taskPath)
	if err != nil {
		return "", fmt.Errorf("load task: %w", err)
	}
	if t.Worktree == "" {
		return "", fmt.Errorf("task %q has no worktree", t.Name)
	}
	if fi, err := os.Stat(t.Worktree); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("worktree directory %s does not exist", t.Worktree)
	}
	shared, err := d.runner.Workspaces.Path(p.Branch)
	if err != nil {
		return "", fmt.Errorf("resolve workspace for %q: %w", p.Branch, err)
	}
	mounts, err := d.worktreeMounts(shared, repoDirNames(p))
	if err != nil {
		return "", err
	}
	orchDir := filepath.Dir(projectPath)
	workspaces := append(append([]string{t.Worktree}, mounts...), orchDir)
	return d.launchSession(ctx, filepath.Base(orchDir), t.Name, workspaces, t.Worktree,
		taskSessionBanner(p, t))
}

// loadInteractiveProject checks the daemon can open a `:session` and loads
// the project, which must have a branch to resolve a workspace from.
func (d *Daemon) loadInteractiveProject(projectPath string) (*project.Project, error) {
	if d.runner == nil {
		return nil, fmt.Errorf("interactive sessions need a runner (none configured)")
	}
	if d.runner.Workspaces == nil {
		return nil, fmt.Errorf("interactive sessions need a workspace manager (none configured)")
	}
	p, err := project.Load(projectPath)
	if err != nil {
		return nil, fmt.Errorf("load project: %w", err)
	}
	if p.Branch == "" {
		return nil, fmt.Errorf("project %q has no branch to resolve a workspace from", projectPath)
	}
	return p, nil
}

// launchSession opens the `session-<work-stream>[-<suffix>]` sandbox's
// interactive claude in a tab, with the given mounts (first is primary), cwd
// and banner. The suffix (a task name) gives a task's session a sandbox, tab
// and conversation of its own.
func (d *Daemon) launchSession(ctx context.Context, workStream, suffix string, workspaces []string, cwd, banner string) (string, error) {
	name, title, convo := "session-"+workStream, workStream, workStream
	if suffix != "" {
		name += "-" + suffix
		title += " " + suffix
		convo += "/" + suffix
	}
	// Resolve the host git identity and SSH-signing config so a `:session`
	// tab commits as the user and SSH-signs, just like the ACP commit
	// agent. Both read from the host git config; Key() is empty unless the
	// host is fully set up for SSH signing, and the runner preflights the
	// forwarded agent before actually forcing commit.gpgsign on.
	gitName, gitEmail := gitsign.Identity()
	spec := runner.InteractiveSpec{
		SandboxName: sanitizeSandboxName(name),
		Workspaces:  workspaces,
		Cwd:         cwd,
		// Open (or resume) a stable per-session claude conversation. No
		// prompt is injected — claude opens at its prompt awaiting the user.
		ClaudeSessionID: sessionUUID(convo),
		Name:            name,
		Title:           title,
		GitName:         gitName,
		GitEmail:        gitEmail,
		SigningKey:      gitsign.Resolve().Key(),
		Banner:          banner,
	}

	target, err := d.runner.LaunchInteractive(ctx, spec)
	var already *terminal.AlreadyOpenError
	if errors.As(err, &already) {
		d.audit.Log("interactive_already_open",
			"work_stream", workStream,
			"kind", "session",
			"target", target,
		)
		return target, err
	}
	if err != nil {
		return "", err
	}
	kv := []string{"work_stream", workStream, "kind", "session", "sandbox", spec.SandboxName, "target", target}
	if suffix != "" {
		kv = append(kv, "task", suffix)
	}
	d.audit.Log("interactive_open", kv...)
	return target, nil
}

// sharedSessionBanner is what a `:session` tab on the shared workspace prints
// before claude starts (docs/parallel-task-worktrees.md X3, R4, R8). With
// per-task worktrees, the shared tree belongs to the commit queue: uncommitted
// edits left in it block the next integration, and a commit on the shared
// branch while one is running races it. Without worktrees there is no queue
// to disturb, and no banner.
func (d *Daemon) sharedSessionBanner(projectPath string, p *project.Project) string {
	if !d.worktreesEnabled() {
		return ""
	}
	lines := []string{
		"⚠ This is the shared workspace of " + p.Branch + ". Tasks work in their own",
		"  worktrees (.worktrees/<task>/) and the commit queue integrates them here.",
		"  - Never leave uncommitted changes in this tree: they block the next integration.",
		"  - Don't commit on " + p.Branch + " while an integration is in flight (a task at",
		"    success, or live.integrating in the state snapshot). Route changes",
		"    through a task instead, or wait for the queue to drain.",
		"  - To work on one task, open :session from the Tasks pane on that task.",
	}
	if d.commitReserved(projectPath) {
		now := "  An integration is in flight right now"
		if name := d.integratingTask(projectPath); name != "" {
			now += " (task " + name + ")"
		}
		lines = append(lines, now+".")
	}
	return strings.Join(lines, "\n")
}

// integratingTask is the task the project's commit agent is integrating, ""
// when none is running.
func (d *Daemon) integratingTask(projectPath string) string {
	d.sessionsMu.Lock()
	defer d.sessionsMu.Unlock()
	return d.sessions[commitSessionKey(projectPath)].taskName
}

// taskSessionBanner is what a task's `:session` tab prints before claude
// starts: where it is, and the rules the task agent works under.
func taskSessionBanner(p *project.Project, t *task.Task) string {
	branch := t.TaskBranch
	if branch == "" {
		branch = worktree.BranchName(p.Branch, t.Name)
	}
	return strings.Join([]string{
		"This is task " + t.Name + "'s worktree (status " + string(t.Status) + "), one git worktree",
		"per repo, each on " + branch + ".",
		"  - Commit here, on " + branch + " only. Never move " + p.Branch + " or any other",
		"    branch: the commit queue integrates this branch when the task is at success.",
		"  - Whatever you leave here is what the task's next run starts from.",
	}, "\n")
}

// sanitizeSandboxName rewrites characters sbx rejects (underscores) to hyphens
// and shortens an over-long name, mirroring runner.SandboxNameFor so the name we
// ask for is the one sbx registers. Both share sbx.SanitizeName so the two paths
// can't drift.
func sanitizeSandboxName(name string) string {
	return sbx.SanitizeName(name)
}

// sessionUUID derives a stable RFC-4122 v5 UUID from the work stream name so the
// `:session` claude invocation can pass the same --session-id on every launch
// and resume the same conversation. The namespace prefix keeps it distinct from
// any other UUIDv5 the caller might derive from the same names.
func sessionUUID(workStream string) string {
	sum := sha1.Sum([]byte("orch-session:" + workStream))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50 // version 5
	b[8] = (b[8] & 0x3f) | 0x80 // RFC-4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
