// Package runner is the glue between the daemon and the agent process model.
// Given a Plan (which Kind of agent, where, on what), Runner:
//
//  1. Resolves the working directory: for ProjectAgent the plan supplies it
//     directly (the dir holding the empty .project.yaml); for every other
//     Kind it asks workspace.Manager to provision one.
//  2. Renders the kind-specific instructions text via the prompts package.
//  3. Hands off to setup.Apply to drop .orch/context.yaml,
//     .orch/instructions.md, and any skills into the working directory.
//  4. Builds an agent.Spec and asks the agent.Launcher to start the session.
//
// The CommandBuilder seam lets tests swap claude-code for `sleep` without
// changing any of the orchestration logic.
package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/policy"
	"github.com/slimslenderslacks/work/internal/prompts"
	"github.com/slimslenderslacks/work/internal/session"
	"github.com/slimslenderslacks/work/internal/setup"
	"github.com/slimslenderslacks/work/internal/workspace"
)

// Plan is the request shape. Fields that don't apply to a Kind are ignored.
type Plan struct {
	Kind agent.Kind

	// WorkingDir, when set, is used directly — no wsp workspace is created.
	// Set this for agents that run in the project's control directory
	// (project, planning, wolf).
	//
	// Leave WorkingDir empty for agents that need a wsp workspace (task,
	// commit); in that case Branch + Repos are required so workspace.Manager
	// can provision one.
	WorkingDir string
	Branch     string
	Repos      []workspace.Repo

	ProjectPath string
	TasksDir    string
	TaskPath    string
	TaskName    string
	FailedTasks []string

	// StaticMCPs are sbx static-MCP names to attach to this agent's sandbox.
	// Forwarded by the daemon from the task's static_mcps field for task and
	// commit agents (commit shares the task's per-task sandbox). Empty for
	// planning/project/wolf and for tasks that declared none.
	StaticMCPs []string

	// Policies are sbx policy rules applied to the sandbox right after
	// `sbx create` and before any `sbx exec`. Forwarded by the daemon from
	// the task's policies field for task and commit agents (commit shares
	// the task's sandbox; the rules already on it remain in effect).
	Policies []policy.Rule

	// BlockedReason, when set, is the message surfaced to the wolf agent
	// describing why the project entered status:blocked. Ignored for any
	// other Kind. Mirrors the project file's blocked_reason field but is
	// passed in directly so the daemon doesn't depend on the agent
	// re-parsing the YAML.
	BlockedReason string

	Skills []setup.Skill

	// SessionName is the tmux session name. If empty, Runner derives one
	// from Kind and Branch (or Kind and a short hash of WorkingDir).
	SessionName string
}

// CommandBuilder produces the argv that the launcher runs inside the
// workspace. Production builds a claude invocation; tests can return
// `sleep 1` or similar.
type CommandBuilder func(kind agent.Kind, workingDir string) []string

const initialPrompt = "Read .orch/instructions.md and .orch/context.yaml, then follow the instructions."

// DefaultCommandBuilder returns the production command: claude-code, told to
// read the instructions and context the orchestrator just wrote.
//
// Autonomous kinds (planning, task, commit) use --print so claude executes
// one turn — including any tool use needed to complete the task — and exits.
// That exit closes the tmux window and lets the daemon chain to the next
// phase.
//
// Interactive kinds (project, wolf) omit --print: a human attaches via tmux
// and drives the conversation, so claude must remain at the prompt.
//
// Sandbox wrapping (running claude inside an `sbx exec`) is layered on by
// Runner.Start *after* this builder returns — keeping the builder pure of
// sandbox concerns lets tests substitute their own commands without
// reasoning about wrapping.
func DefaultCommandBuilder(kind agent.Kind, _ string) []string {
	cmd := []string{"claude", "--dangerously-skip-permissions"}
	if !kind.Interactive() {
		cmd = append(cmd, "--print")
	}
	cmd = append(cmd, initialPrompt)
	return cmd
}

// SandboxSpec describes the sandbox a SandboxCreator should ensure exists.
// Name is the sbx sandbox name; Workspaces are host paths to bind-mount (the
// first is the primary cwd, the rest are extra mounts); StaticMCPs are sbx
// static-MCP names that become repeated `--static-mcp <name>` flags on
// `sbx create`; Policies are sbx policy rules applied immediately after
// `sbx create` and before any `sbx exec` (one `sbx policy ...` invocation
// per rule, in order).
type SandboxSpec struct {
	Name       string
	Workspaces []string
	StaticMCPs []string
	Policies   []policy.Rule
}

// SandboxCreator ensures a sandbox described by spec exists.
//
// Task/commit agents need two mounts: the worktree (for code) and the
// project's orch dir (for `.project.yaml` and `tasks/*.yaml`, which live
// outside the worktree).
//
// Must be idempotent: the daemon re-dispatches across restarts. The default
// implementation treats "already exists" errors from sbx as success.
type SandboxCreator func(ctx context.Context, spec SandboxSpec) error

// DefaultSandboxCreator ensures a sandbox named spec.Name exists with exactly
// the given workspaces mounted and static MCPs attached. The flow is:
//
//  1. `sbx ls --json` to find the existing sandbox (if any).
//  2. If it exists with the same set of workspaces → no-op. We do NOT inspect
//     MCPs or policies on an existing sandbox because sbx ls doesn't surface
//     those details; for per-task sandboxes the name uniquely encodes the
//     task and its MCP/policy set, so a name collision implies a matching
//     set. (A retry of the same task reuses the same sandbox.)
//  3. If it exists with a different workspace set → `sbx rm --force` then
//     recreate. This self-heals when the daemon's idea of the desired mounts
//     has grown (e.g. task agents added the orch dir as a second mount).
//  4. Otherwise `sbx create claude --name <name> [--static-mcp <m>...] <ws...>`,
//     then one `sbx policy <action> <kind> --sandbox <name> <resource>` per
//     rule in spec.Policies (in declaration order, so deny-all + allow-host
//     compositions evaluate left-to-right as written).
//
// Recreation is safe here because we never use --clone — the sandbox is a
// thin bind-mount wrapper, so rm just stops the container and leaves the
// host paths (worktrees, orch dir) untouched.
func DefaultSandboxCreator(ctx context.Context, spec SandboxSpec) error {
	if len(spec.Workspaces) == 0 {
		return fmt.Errorf("sandbox: at least one workspace is required")
	}
	existing, err := readSandboxWorkspaces(ctx, spec.Name)
	if err != nil {
		return fmt.Errorf("sbx ls: %w", err)
	}
	if existing != nil {
		if sameWorkspaceSet(existing, spec.Workspaces) {
			return nil
		}
		rm := exec.CommandContext(ctx, "sbx", "rm", "--force", spec.Name)
		if out, err := rm.CombinedOutput(); err != nil {
			return fmt.Errorf("sbx rm %s: %w: %s", spec.Name, err, strings.TrimSpace(string(out)))
		}
	}
	args := []string{"create", "claude", "--name", spec.Name}
	for _, m := range spec.StaticMCPs {
		args = append(args, "--static-mcp", m)
	}
	args = append(args, spec.Workspaces...)
	cmd := exec.CommandContext(ctx, "sbx", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sbx create %s %v: %w: %s", spec.Name, spec.Workspaces, err, strings.TrimSpace(string(out)))
	}
	for _, r := range spec.Policies {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("sbx policy %s: %w", spec.Name, err)
		}
		pcmd := exec.CommandContext(ctx, "sbx", r.CLIArgs(spec.Name)...)
		if out, err := pcmd.CombinedOutput(); err != nil {
			return fmt.Errorf("sbx policy %s %s %s %s: %w: %s",
				r.Action, r.Kind, spec.Name, r.Resource, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// readSandboxWorkspaces returns the workspace list for the named sandbox,
// or nil if no sandbox by that name exists. `sbx ls --json` is the only
// stable read interface sbx exposes.
func readSandboxWorkspaces(ctx context.Context, name string) ([]string, error) {
	out, err := exec.CommandContext(ctx, "sbx", "ls", "--json").Output()
	if err != nil {
		return nil, err
	}
	var data struct {
		Sandboxes []struct {
			Name       string   `json:"name"`
			Workspaces []string `json:"workspaces"`
		} `json:"sandboxes"`
	}
	if err := json.Unmarshal(out, &data); err != nil {
		return nil, fmt.Errorf("decode sbx ls output: %w", err)
	}
	for _, s := range data.Sandboxes {
		if s.Name == name {
			return s.Workspaces, nil
		}
	}
	return nil, nil
}

// sameWorkspaceSet reports whether a and b contain the same workspace paths.
// Order is intentionally ignored — sbx doesn't expose an "order" semantics
// for mounts, and we want the comparison to be insensitive to how either
// side happened to enumerate them.
func sameWorkspaceSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]bool, len(a))
	for _, x := range a {
		seen[x] = true
	}
	for _, x := range b {
		if !seen[x] {
			return false
		}
	}
	return true
}

type Runner struct {
	Workspaces workspace.Manager
	Launcher   agent.Launcher
	Audit      *audit.Logger
	Command    CommandBuilder // defaults to DefaultCommandBuilder when nil

	// Sandbox, when non-nil, is called before each non-project agent launch
	// to ensure a sandbox exists; the launch command is then wrapped with
	// `sbx exec -it <name>` so claude runs inside it. Leave nil to skip
	// sandboxing entirely — tests that fake the launch command set this to
	// nil so they don't shell out to sbx.
	//
	// This is the legacy tmux + `sbx exec` path. In production it has been
	// superseded for non-interactive agents by the ACP path (see AcpLauncher):
	// when AcpLauncher is set, planning/task/commit agents launch as
	// acp-wrapper-backed ACP sessions instead and never reach this seam. It is
	// retained for interactive-agent dev/tests that exercise the generic
	// launcher without ACP.
	Sandbox SandboxCreator

	// AcpLauncher, when non-nil, switches non-interactive agents
	// (planning/task/commit) from the tmux + `sbx exec claude -p` path to a
	// per-session acp-wrapper host process that backs an ACP claude session.
	// The wrapper creates the sandbox (with acp-kit layered on), execs the ACP
	// client, and serves <SessionsRoot>/<id>/agent.sock; the TUI watches the
	// stream over that socket. Interactive agents (project/wolf) are never
	// routed here — they keep the tmux path. Leave nil to fall back to the
	// legacy launcher for every kind (dev/tests).
	AcpLauncher agent.Launcher

	// Kit is the acp-kit reference passed to `acp-wrapper --kit` (a local kit
	// dir or a published ref). Required when AcpLauncher is set.
	Kit string

	// SessionsRoot is where per-session directories live; passed to
	// `acp-wrapper --sessions-root` and used to write the initial session.json.
	// Defaults to ~/.workingman/sessions when empty.
	SessionsRoot string

	// AcpWrapperPath is the acp-wrapper binary the AcpLauncher runs. Defaults
	// to "acp-wrapper" resolved on PATH.
	AcpWrapperPath string

	// SbxPath, when set, is forwarded to `acp-wrapper --sbx` so the wrapper
	// uses a specific sbx binary. Empty lets the wrapper resolve sbx on PATH.
	SbxPath string
}

// UsesACP reports whether a Start for kind would take the ACP path (i.e. the
// agent runs inside an acp-wrapper-managed sandbox). The daemon uses this to
// decide whether to surface a session's sandbox name to the TUI: only ACP
// sessions own a stable, user-visible sandbox; the legacy tmux path's sandbox
// is an implementation detail.
func (r *Runner) UsesACP(kind agent.Kind) bool {
	return r.AcpLauncher != nil && !kind.Interactive()
}

// Start is non-blocking: it returns the Session once the launcher accepts it.
// The caller owns Wait/Close on the returned Session.
func (r *Runner) Start(ctx context.Context, p Plan) (agent.Session, error) {
	workingDir, err := r.resolveWorkingDir(ctx, p)
	if err != nil {
		return nil, err
	}

	// Planning runs in the project's orch dir, but it needs to read source
	// from the project's repos to write meaningful task breakdowns. Provision
	// the same wsp worktree the task/commit agents will later use, so the
	// planner can inspect source via a second sandbox mount instead of
	// hitting GitHub from inside a credential-less sandbox. wsp Create is
	// idempotent, so the task agent's later call returns the same dir.
	planningWorktree, err := r.resolvePlanningWorktree(ctx, p)
	if err != nil {
		return nil, err
	}

	data := prompts.Data{
		Kind:          p.Kind,
		Workspace:     workingDir,
		Branch:        p.Branch,
		ProjectPath:   p.ProjectPath,
		TasksDir:      p.TasksDir,
		TaskPath:      p.TaskPath,
		TaskName:      p.TaskName,
		FailedTasks:   p.FailedTasks,
		BlockedReason: p.BlockedReason,
		Worktree:      planningWorktree,
	}
	instructions, err := prompts.Render(p.Kind, data)
	if err != nil {
		return nil, err
	}

	ctxFile := setup.Context{
		Kind:          p.Kind.String(),
		Workspace:     workingDir,
		Branch:        p.Branch,
		ProjectPath:   p.ProjectPath,
		TasksDir:      p.TasksDir,
		TaskPath:      p.TaskPath,
		TaskName:      p.TaskName,
		FailedTasks:   p.FailedTasks,
		BlockedReason: p.BlockedReason,
		Worktree:      planningWorktree,
	}
	if err := setup.Apply(workingDir, ctxFile, instructions, p.Skills); err != nil {
		return nil, err
	}

	// Non-interactive agents back ACP claude sessions via acp-wrapper when the
	// Runner is configured for it (production). The .orch instructions/context
	// written above are read by the sandboxed ACP client from the mounted
	// workspace exactly as before; only the launch mechanism changes — an
	// acp-wrapper host process instead of a tmux window running
	// `sbx exec claude -p`. Interactive agents (project/wolf) always take the
	// legacy tmux path below.
	if !p.Kind.Interactive() && r.AcpLauncher != nil {
		return r.startACP(ctx, p, workingDir, planningWorktree)
	}

	build := r.Command
	if build == nil {
		build = DefaultCommandBuilder
	}
	command := build(p.Kind, workingDir)

	sandboxName := SandboxNameFor(p.Kind, p.ProjectPath, p.TaskName)
	if r.Sandbox != nil && sandboxName != "" {
		workspaces := sandboxWorkspaces(p.Kind, workingDir, p.ProjectPath, planningWorktree)
		if err := r.Sandbox(ctx, SandboxSpec{
			Name:       sandboxName,
			Workspaces: workspaces,
			StaticMCPs: p.StaticMCPs,
			Policies:   p.Policies,
		}); err != nil {
			return nil, fmt.Errorf("runner: sandbox: %w", err)
		}
		if r.Audit != nil {
			r.Audit.Log("sandbox_ensured",
				"name", sandboxName,
				"workspaces", strings.Join(workspaces, ","),
				"kind", p.Kind.String(),
				"static_mcps", strings.Join(p.StaticMCPs, ","),
				"policies", strconv.Itoa(len(p.Policies)),
			)
		}
		// -w pins claude's working directory to the project workspace. The
		// sandbox bind-mounts the host workspace at the same absolute path,
		// but `sbx exec` lands in /home/agent/workspace by default (empty),
		// so the agent's relative `Read .orch/instructions.md` prompt would
		// resolve to nothing without this — claude would start up, find no
		// instructions, and exit, sending the daemon into a relaunch loop.
		command = append([]string{"sbx", "exec", "-it", "-w", workingDir, sandboxName}, command...)
	}

	spec := agent.Spec{
		Kind:      p.Kind,
		Name:      sessionName(p),
		Workspace: workingDir,
		Command:   command,
	}

	sess, err := r.Launcher.Launch(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("runner: launch: %w", err)
	}
	if r.Audit != nil {
		r.Audit.Log("session_started",
			"kind", p.Kind.String(),
			"name", spec.Name,
			"workspace", workingDir,
		)
	}
	return sess, nil
}

// startACP launches a non-interactive agent as an acp-wrapper-backed ACP
// session. It allocates a session id, writes the initial session.json (so the
// daemon and a restarting TUI can discover the session before the wrapper has
// brought the sandbox up), and starts the acp-wrapper host process. The wrapper
// itself creates the sandbox with acp-kit, execs the ACP client, and re-writes
// session.json with the live status once the socket is accepting connections.
//
// Unlike the tmux path, this does NOT call r.Sandbox or wrap the command in
// `sbx exec` — the wrapper owns sandbox creation end-to-end.
func (r *Runner) startACP(ctx context.Context, p Plan, workingDir, planningWorktree string) (agent.Session, error) {
	sandboxName := SandboxNameFor(p.Kind, p.ProjectPath, p.TaskName)
	if sandboxName == "" {
		return nil, fmt.Errorf("runner: acp launch for kind %s needs a ProjectPath to derive a sandbox name", p.Kind)
	}
	if strings.TrimSpace(r.Kit) == "" {
		return nil, fmt.Errorf("runner: acp launch requires Kit (the acp-kit reference)")
	}

	sessionsRoot, err := r.sessionsRoot()
	if err != nil {
		return nil, err
	}
	sessionID := acpSessionID(p)
	workspaces := sandboxWorkspaces(p.Kind, workingDir, p.ProjectPath, planningWorktree)

	// Allocate the session id and write the initial session.json. acp-wrapper
	// will overwrite this record with StatusRunning once its socket is live;
	// writing it here means the session is discoverable in the brief window
	// before the wrapper's own write lands.
	store := session.Store{Root: sessionsRoot}
	now := time.Now()
	rec := session.Session{
		ID:          sessionID,
		SandboxName: sandboxName,
		Status:      session.StatusStarting,
		CreatedAt:   now,
		UpdatedAt:   now,
		SocketPath:  store.SocketPath(sessionID),
		Workspaces:  workspaces,
		Kit:         r.Kit,
	}
	if err := store.Write(rec); err != nil {
		return nil, fmt.Errorf("runner: write initial session.json: %w", err)
	}

	command := r.acpWrapperCommand(sessionID, sandboxName, sessionsRoot, workspaces, p.StaticMCPs, p.Policies)
	spec := agent.Spec{
		Kind:      p.Kind,
		Name:      sessionID,
		Workspace: workingDir,
		Command:   command,
	}
	sess, err := r.AcpLauncher.Launch(ctx, spec)
	if err != nil {
		// The wrapper never started, so no one will clean up the record we just
		// wrote — remove it so a restarting TUI doesn't discover a dead session.
		_ = store.Remove(sessionID)
		return nil, fmt.Errorf("runner: acp launch: %w", err)
	}
	if r.Audit != nil {
		r.Audit.Log("acp_session_started",
			"kind", p.Kind.String(),
			"session_id", sessionID,
			"sandbox", sandboxName,
			"workspaces", strings.Join(workspaces, ","),
		)
	}
	return sess, nil
}

// acpWrapperCommand builds the argv that launches one acp-wrapper host process
// for an ACP session. The wrapper resolves --workspace paths to absolute itself,
// but they already are (workspace.Manager and the orch dir both yield abs paths).
func (r *Runner) acpWrapperCommand(sessionID, sandboxName, sessionsRoot string, workspaces, staticMCPs []string, policies []policy.Rule) []string {
	bin := r.AcpWrapperPath
	if bin == "" {
		bin = "acp-wrapper"
	}
	args := []string{
		bin,
		"--session-id", sessionID,
		"--kit", r.Kit,
		"--sandbox", sandboxName,
		"--sessions-root", sessionsRoot,
		// Orch's planning/task/commit are single-turn: when the TUI's watcher
		// drives its one prompt and disconnects, the wrapper should exit so
		// the daemon's session_ended callback fires and the next stage
		// dispatches. Without this the wrapper would survive the disconnect
		// (designed for long-lived interactive sessions) and the project
		// would stall in `working`.
		"--exit-when-empty",
	}
	if r.SbxPath != "" {
		args = append(args, "--sbx", r.SbxPath)
	}
	for _, m := range staticMCPs {
		args = append(args, "--static-mcp", m)
	}
	for _, p := range policies {
		args = append(args, "--policy", p.Encode())
	}
	for _, w := range workspaces {
		args = append(args, "--workspace", w)
	}
	return args
}

// sessionsRoot resolves the absolute sessions root: the configured value when
// set, otherwise the session package default (~/.workingman/sessions).
func (r *Runner) sessionsRoot() (string, error) {
	if strings.TrimSpace(r.SessionsRoot) != "" {
		abs, err := filepath.Abs(r.SessionsRoot)
		if err != nil {
			return "", fmt.Errorf("runner: sessions root %q: %w", r.SessionsRoot, err)
		}
		return abs, nil
	}
	return session.DefaultRoot()
}

// acpSessionID derives the ACP session id from the Plan. It reuses the tmux
// window-name logic (kind-tail) so the id is stable across daemon restarts —
// the same task relaunch maps to the same session dir and sandbox, making the
// launch idempotent — then sanitizes path separators (a branch tail like
// "feat/x" would otherwise be an illegal multi-segment session id).
func acpSessionID(p Plan) string {
	id := sessionName(p)
	id = strings.ReplaceAll(id, "/", "-")
	id = strings.ReplaceAll(id, `\`, "-")
	return id
}

func (r *Runner) resolveWorkingDir(ctx context.Context, p Plan) (string, error) {
	if p.WorkingDir != "" {
		return p.WorkingDir, nil
	}
	if r.Workspaces == nil {
		return "", fmt.Errorf("runner: kind %s needs either WorkingDir or a workspace manager", p.Kind)
	}
	if p.Branch == "" {
		return "", fmt.Errorf("runner: kind %s requires Branch (or WorkingDir)", p.Kind)
	}
	return r.Workspaces.Create(ctx, p.Branch, p.Repos)
}

// resolvePlanningWorktree provisions the wsp worktree that backs a planning
// agent's second sandbox mount. It returns "" for any kind that isn't planning
// or any planning launch missing the workspace manager / Branch / Repos
// (legitimate in dev/tests with no wsp wired up). A planning launch that DOES
// have the wiring but fails to provision is surfaced as an error so the
// daemon's transitionProjectBlocked path can mark the project blocked rather
// than silently launching without the mount.
func (r *Runner) resolvePlanningWorktree(ctx context.Context, p Plan) (string, error) {
	if p.Kind != agent.PlanningAgent {
		return "", nil
	}
	if r.Workspaces == nil || p.Branch == "" || len(p.Repos) == 0 {
		return "", nil
	}
	dir, err := r.Workspaces.Create(ctx, p.Branch, p.Repos)
	if err != nil {
		return "", fmt.Errorf("runner: provision planning worktree: %w", err)
	}
	return dir, nil
}

// sessionName builds the tmux window name in the form "<kind>-<tail>". The
// tail picks the most specific identifier the Plan carries: a task name
// when the agent is working a specific task (task / commit agents), the
// branch otherwise (project / planning / wolf), and as a last resort a
// hash of WorkingDir so we still produce something unique.
//
// Preferring TaskName for task-bound kinds is the whole point of this
// function — without it, every task on the same branch would land in a
// window called "task-<branch>" and a glance at tmux's status bar wouldn't
// tell you which task is actually running.
func sessionName(p Plan) string {
	if p.SessionName != "" {
		return p.SessionName
	}
	tail := p.TaskName
	if tail == "" {
		tail = p.Branch
	}
	if tail == "" {
		tail = shortID(p.WorkingDir)
	}
	// No "orch-" prefix — the umbrella tmux session carries that brand.
	// Window names show up bare in tmux's status bar.
	return fmt.Sprintf("%s-%s", p.Kind, tail)
}

// SandboxNameFor derives the sbx sandbox name for a given launch:
//
//   - Project agent → "" (no sandbox; the project agent interviews the user
//     in the bare workspace and writes the initial .project.yaml).
//   - Wolf agent → "" (runs outside the sandbox so it can advise on the
//     project from the host, including for sandbox-related blocks).
//   - Planning → basename of the project's control dir (the dir holding
//     .project.yaml). Workspace = control dir.
//   - Task / commit → "<work-stream>-<task-name>" where work-stream is the
//     basename of the project's control dir. Each task gets its OWN sandbox
//     so its `--static-mcp` set can differ from siblings; the commit agent
//     for that task reuses the same sandbox so it sees the task's git work.
//
// sbx rejects sandbox names containing underscores, so any "_" in the derived
// name is rewritten to "-" here — that matches the normalization the
// acp-wrapper does before calling sbx, so the returned name is the one the
// sandbox actually registers under.
//
// Returns "" when there's no ProjectPath to derive a name from, or when a
// Task/Commit launch is missing TaskName (the daemon always supplies it).
//
// Exported so the daemon can surface the name to the TUI's sessions pane
// without duplicating the derivation logic.
func SandboxNameFor(kind agent.Kind, projectPath, taskName string) string {
	if projectPath == "" {
		return ""
	}
	base := filepath.Base(filepath.Dir(projectPath))
	var name string
	switch kind {
	case agent.PlanningAgent:
		name = base
	case agent.TaskAgent, agent.CommitAgent:
		if taskName == "" {
			return ""
		}
		name = base + "-" + taskName
	default:
		return ""
	}
	return strings.ReplaceAll(name, "_", "-")
}

// sandboxWorkspaces returns the host paths to mount into the sandbox. The
// first element is the primary workspace claude `cd`s into; the rest are
// extra mounts.
//
// Task/commit agents need TWO mounts: the worktree (where the code lives
// and where the agent does `git` work) and the project's orch dir (which
// holds `.project.yaml` and `tasks/*.yaml`). Without the second mount, the
// task agent's status writeback to `tasks/<name>.yaml` fails because the
// directory simply doesn't exist inside the sandbox.
//
// Planning's primary mount is the project's orch dir (where .orch/ lives).
// When planningWorktree is non-empty, it is mounted as a second workspace so
// the planner can read source code from the wsp-provisioned worktree without
// trying to clone from GitHub (the sandbox has no auth and prior runs got
// stuck thrashing on auth failures). Empty falls back to one mount.
func sandboxWorkspaces(kind agent.Kind, workingDir, projectPath, planningWorktree string) []string {
	switch kind {
	case agent.TaskAgent, agent.CommitAgent:
		orchDir := filepath.Dir(projectPath)
		if orchDir == "" || orchDir == workingDir {
			return []string{workingDir}
		}
		return []string{workingDir, orchDir}
	case agent.PlanningAgent:
		if planningWorktree == "" || planningWorktree == workingDir {
			return []string{workingDir}
		}
		return []string{workingDir, planningWorktree}
	}
	return []string{workingDir}
}

// shortID hashes a path to a short stable suffix. Used for session names when
// the project agent is launched (no Branch available yet).
func shortID(s string) string {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return fmt.Sprintf("%08x", h)
}
