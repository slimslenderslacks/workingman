package daemon

import (
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/slimslenderslacks/work/internal/gitsign"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/sbx"
	"github.com/slimslenderslacks/work/internal/terminal"
)

// OpenInteractiveShell opens a plain bash shell in a new terminal tab on the host
// (no sandbox) with the working directory set to the work stream's workspace
// worktree. It returns the session name the TUI can focus the tab by. Backs the
// TUI's `:dir` command. If the work stream's shell tab is already open it is
// focused instead, and a *terminal.AlreadyOpenError is returned with the name.
func (d *Daemon) OpenInteractiveShell(ctx context.Context, projectPath string) (string, error) {
	return d.openInteractive(ctx, projectPath, false)
}

// OpenInteractiveSession opens (or resumes) the work stream's persistent
// interactive claude session in its `session-<work-stream>` sandbox — both the
// workspace worktree and the orch control dir mounted, no prompt injected. The
// claude session id is derived deterministically from the work stream name, so
// closing the tab and re-running the command resumes the same conversation.
// It returns the session name the TUI can focus the tab by. Backs the TUI's
// `:session` command. Like OpenInteractiveShell, an already-open tab is focused
// and reported with a *terminal.AlreadyOpenError.
func (d *Daemon) OpenInteractiveSession(ctx context.Context, projectPath string) (string, error) {
	return d.openInteractive(ctx, projectPath, true)
}

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
// `:workspace` command. Unlike `:dir` it never provisions: it fails, without
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

// openDirTab verifies dir exists and opens a host bash there in a terminal tab
// named "<kind>-<work-stream>", titled after the work stream. An already-open
// tab is focused and reported with a *terminal.AlreadyOpenError.
func (d *Daemon) openDirTab(ctx context.Context, kind, orchDir, dir string) (string, error) {
	if d.runner == nil {
		return "", fmt.Errorf("terminal tabs need a runner (none configured)")
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%s directory %s does not exist", kind, dir)
	}
	workStream := filepath.Base(orchDir)
	target, err := d.runner.LaunchInteractive(ctx, runner.InteractiveSpec{
		Cwd:   dir,
		Inner: []string{"bash"},
		Name:  kind + "-" + workStream,
		Title: workStream,
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

// openInteractive is the shared body of OpenInteractiveShell/Session. It loads
// the project to learn its branch + repos, provisions (idempotently) or
// resolves the workspace worktree, derives the sandbox/session names and the
// inner command, and delegates the sandbox+tab launch to the runner.
func (d *Daemon) openInteractive(ctx context.Context, projectPath string, claudeSession bool) (string, error) {
	if d.runner == nil {
		return "", fmt.Errorf("interactive sessions need a runner (none configured)")
	}
	if d.runner.Workspaces == nil {
		return "", fmt.Errorf("interactive sessions need a workspace manager (none configured)")
	}
	p, err := project.Load(projectPath)
	if err != nil {
		return "", fmt.Errorf("load project: %w", err)
	}
	if p.Branch == "" {
		return "", fmt.Errorf("project %q has no branch to resolve a workspace from", projectPath)
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

	var spec runner.InteractiveSpec
	if claudeSession {
		name := sanitizeSandboxName("session-" + workStream)
		// Resolve the host git identity and SSH-signing config so a `:session`
		// tab commits as the user and SSH-signs, just like the ACP commit
		// agent. Both read from the host git config; Key() is empty unless the
		// host is fully set up for SSH signing, and the runner preflights the
		// forwarded agent before actually forcing commit.gpgsign on.
		gitName, gitEmail := gitsign.Identity()
		spec = runner.InteractiveSpec{
			SandboxName: name,
			// Mount both the worktree and the orch control dir, like the task
			// agents, so the session can see the code and the project.yaml /
			// tasks. cwd is the worktree.
			Workspaces: []string{workspaceDir, orchDir},
			Cwd:        workspaceDir,
			// Open (or resume) a stable per-work-stream claude conversation. No
			// prompt is injected — claude opens at its prompt awaiting the user.
			Inner:      sessionCommand(sessionUUID(workStream)),
			Name:       "session-" + workStream,
			Title:      workStream,
			GitName:    gitName,
			GitEmail:   gitEmail,
			SigningKey: gitsign.Resolve().Key(),
		}
	} else {
		// `:dir` is a plain host shell (no sandbox) cd'd to the workspace dir —
		// SandboxName empty tells the runner to run bash directly on the host.
		spec = runner.InteractiveSpec{
			Cwd:   workspaceDir,
			Inner: []string{"bash"},
			Name:  "shell-" + workStream,
			Title: workStream,
		}
	}

	target, err := d.runner.LaunchInteractive(ctx, spec)
	var already *terminal.AlreadyOpenError
	if errors.As(err, &already) {
		d.audit.Log("interactive_already_open",
			"work_stream", workStream,
			"kind", interactiveKindName(claudeSession),
			"target", target,
		)
		return target, err
	}
	if err != nil {
		return "", err
	}
	d.audit.Log("interactive_open",
		"work_stream", workStream,
		"kind", interactiveKindName(claudeSession),
		"sandbox", spec.SandboxName,
		"target", target,
	)
	return target, nil
}

func interactiveKindName(claudeSession bool) string {
	if claudeSession {
		return "session"
	}
	return "shell"
}

// sanitizeSandboxName rewrites characters sbx rejects (underscores) to hyphens
// and shortens an over-long name, mirroring runner.SandboxNameFor so the name we
// ask for is the one sbx registers. Both share sbx.SanitizeName so the two paths
// can't drift.
func sanitizeSandboxName(name string) string {
	return sbx.SanitizeName(name)
}

// sessionCommand builds the claude invocation for a `:session` window. It runs
// inside the sandbox as a bash wrapper that decides, at launch time, whether to
// resume or create the conversation with the given stable id.
//
// This matters because `claude --session-id <uuid>` only *creates* a
// conversation with that id — it errors ("Session ID <uuid> is already in
// use") and exits if one already exists. Passing it on every open therefore
// worked the first time but crashed instantly on every reopen, closing the
// tab. The sandbox's ~/.claude/projects is a persistent volume that
// survives sandbox restarts, so the wrapper checks it: `--resume` when the
// transcript already exists, `--session-id` (to create with the stable id)
// when it doesn't. `exec` replaces the shell with claude so the tab's
// process is claude itself, keeping lifecycle and signals identical to running
// it directly. The uuid is a v5 UUID (hex + hyphens only), so it's safe to
// interpolate unquoted into the shell.
func sessionCommand(uuid string) []string {
	script := fmt.Sprintf(
		`uuid=%s; `+
			`if ls ~/.claude/projects/*/"$uuid".jsonl >/dev/null 2>&1; then `+
			`exec claude --dangerously-skip-permissions --resume "$uuid"; `+
			`else exec claude --dangerously-skip-permissions --session-id "$uuid"; fi`,
		uuid)
	return []string{"bash", "-lc", script}
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
