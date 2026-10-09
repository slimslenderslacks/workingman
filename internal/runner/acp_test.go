package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/policy"
	"github.com/slimslenderslacks/work/internal/session"
	"github.com/slimslenderslacks/work/internal/workspace"
)

// argValue returns the token following the first occurrence of flag in args, or
// "" if the flag isn't present (or has no following token).
func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// hasFlag reports whether flag appears anywhere in args. For boolean flags like
// --exit-when-empty that take no following token.
func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// argValues returns every token following an occurrence of flag — used for
// repeatable flags like --workspace.
func argValues(args []string, flag string) []string {
	var out []string
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}

func TestACPLaunchPlanningAgent(t *testing.T) {
	workingDir := t.TempDir()
	projectPath := filepath.Join(workingDir, ".project.yaml")
	if err := os.WriteFile(projectPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sessionsRoot := t.TempDir()

	acp := &fakeLauncher{}
	tmux := &fakeLauncher{}
	r := &Runner{
		Launcher:       tmux,
		AcpLauncher:    acp,
		Kit:            "/kits/acp-kit",
		SessionsRoot:   sessionsRoot,
		AcpWrapperPath: "/bin/acp-wrapper",
		SbxPath:        "/bin/sbx",
	}

	if _, err := r.Start(context.Background(), Plan{
		Kind:        agent.PlanningAgent,
		WorkingDir:  workingDir,
		ProjectPath: projectPath,
		Branch:      "feat-x",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The tmux launcher must not be used for a non-interactive ACP launch.
	if tmux.last.Name != "" {
		t.Errorf("tmux launcher was used for an ACP agent: %+v", tmux.last)
	}

	cmd := acp.last.Command
	if len(cmd) == 0 || cmd[0] != "/bin/acp-wrapper" {
		t.Fatalf("command[0] = %v, want /bin/acp-wrapper (full: %v)", cmd, cmd)
	}
	wantID := "planning-feat-x"
	if got := argValue(cmd, "--session-id"); got != wantID {
		t.Errorf("--session-id = %q, want %q", got, wantID)
	}
	if got := argValue(cmd, "--kit"); got != "/kits/acp-kit" {
		t.Errorf("--kit = %q, want /kits/acp-kit", got)
	}
	if got := argValue(cmd, "--sandbox"); got != filepath.Base(workingDir) {
		t.Errorf("--sandbox = %q, want %q", got, filepath.Base(workingDir))
	}
	if got := argValue(cmd, "--sessions-root"); got != sessionsRoot {
		t.Errorf("--sessions-root = %q, want %q", got, sessionsRoot)
	}
	if got := argValue(cmd, "--sbx"); got != "/bin/sbx" {
		t.Errorf("--sbx = %q, want /bin/sbx", got)
	}
	if ws := argValues(cmd, "--workspace"); len(ws) != 1 || ws[0] != workingDir {
		t.Errorf("--workspace = %v, want [%q]", ws, workingDir)
	}
	if !hasFlag(cmd, "--exit-when-empty") {
		t.Errorf("expected --exit-when-empty in argv, got %v", cmd)
	}
	if acp.last.Name != wantID {
		t.Errorf("spec.Name = %q, want %q", acp.last.Name, wantID)
	}

	// The initial session.json must be on disk for a restarting TUI to find.
	store := session.Store{Root: sessionsRoot}
	rec, err := store.Read(wantID)
	if err != nil {
		t.Fatalf("read session.json: %v", err)
	}
	if rec.Status != session.StatusStarting {
		t.Errorf("status = %q, want %q", rec.Status, session.StatusStarting)
	}
	if rec.SandboxName != filepath.Base(workingDir) {
		t.Errorf("sandbox = %q, want %q", rec.SandboxName, filepath.Base(workingDir))
	}
	if rec.Kit != "/kits/acp-kit" {
		t.Errorf("kit = %q, want /kits/acp-kit", rec.Kit)
	}
	if rec.SocketPath != store.SocketPath(wantID) {
		t.Errorf("socket_path = %q, want %q", rec.SocketPath, store.SocketPath(wantID))
	}
	if rec.CreatedAt.IsZero() {
		t.Error("created_at is zero")
	}
}

func TestACPLaunchTaskAgentMountsOrchDir(t *testing.T) {
	wsRoot := t.TempDir()
	orchDir := t.TempDir()
	projectPath := filepath.Join(orchDir, ".project.yaml")
	if err := os.WriteFile(projectPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sessionsRoot := t.TempDir()

	acp := &fakeLauncher{}
	r := &Runner{
		Workspaces:   workspace.NewStub(wsRoot),
		Launcher:     &fakeLauncher{},
		AcpLauncher:  acp,
		Kit:          "kit-ref",
		SessionsRoot: sessionsRoot,
	}

	if _, err := r.Start(context.Background(), Plan{
		Kind:        agent.TaskAgent,
		Branch:      "feat-x",
		ProjectPath: projectPath,
		TaskPath:    filepath.Join(orchDir, "tasks", "first.yaml"),
		TaskName:    "first",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	cmd := acp.last.Command
	if cmd[0] != "acp-wrapper" {
		t.Errorf("default binary = %q, want acp-wrapper", cmd[0])
	}
	wantWorktree := filepath.Join(wsRoot, "feat-x")
	ws := argValues(cmd, "--workspace")
	if len(ws) != 2 || ws[0] != wantWorktree || ws[1] != orchDir {
		t.Errorf("--workspace = %v, want [%q %q]", ws, wantWorktree, orchDir)
	}
	if got := argValue(cmd, "--sandbox"); got != filepath.Base(orchDir)+"-first" {
		t.Errorf("--sandbox = %q, want %q", got, filepath.Base(orchDir)+"-first")
	}
	if got := argValue(cmd, "--session-id"); got != "task-first" {
		t.Errorf("--session-id = %q, want task-first", got)
	}
	// --sbx omitted when SbxPath is unset.
	if got := argValue(cmd, "--sbx"); got != "" {
		t.Errorf("--sbx = %q, want empty (unset)", got)
	}
}

func TestACPLaunchPlanningAgentMountsWorktree(t *testing.T) {
	wsRoot := t.TempDir()
	orchDir := t.TempDir()
	projectPath := filepath.Join(orchDir, ".project.yaml")
	if err := os.WriteFile(projectPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sessionsRoot := t.TempDir()

	acp := &fakeLauncher{}
	r := &Runner{
		Workspaces:   workspace.NewStub(wsRoot),
		Launcher:     &fakeLauncher{},
		AcpLauncher:  acp,
		Kit:          "kit-ref",
		SessionsRoot: sessionsRoot,
	}

	if _, err := r.Start(context.Background(), Plan{
		Kind:        agent.PlanningAgent,
		WorkingDir:  orchDir,
		ProjectPath: projectPath,
		Branch:      "feat-x",
		Repos:       []workspace.Repo{{Identity: "github.com/example/repo"}},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	wantWorktree := filepath.Join(wsRoot, "feat-x")
	cmd := acp.last.Command
	ws := argValues(cmd, "--workspace")
	if len(ws) != 2 || ws[0] != orchDir || ws[1] != wantWorktree {
		t.Errorf("--workspace = %v, want [%q %q]", ws, orchDir, wantWorktree)
	}
}

func TestACPLaunchPlanningAgentNoRepoSingleMount(t *testing.T) {
	// Planning with a Workspaces manager but no Repos must NOT call Create
	// (no Repos means no source to mount); the planner falls back to the
	// single-mount layout.
	wsRoot := t.TempDir()
	orchDir := t.TempDir()
	projectPath := filepath.Join(orchDir, ".project.yaml")
	if err := os.WriteFile(projectPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	acp := &fakeLauncher{}
	r := &Runner{
		Workspaces:   workspace.NewStub(wsRoot),
		Launcher:     &fakeLauncher{},
		AcpLauncher:  acp,
		Kit:          "kit-ref",
		SessionsRoot: t.TempDir(),
	}

	if _, err := r.Start(context.Background(), Plan{
		Kind:        agent.PlanningAgent,
		WorkingDir:  orchDir,
		ProjectPath: projectPath,
		Branch:      "feat-x",
		// no Repos
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ws := argValues(acp.last.Command, "--workspace")
	if len(ws) != 1 || ws[0] != orchDir {
		t.Errorf("--workspace = %v, want [%q]", ws, orchDir)
	}
}

func TestACPLaunchTaskAgentForwardsStaticMCPs(t *testing.T) {
	wsRoot := t.TempDir()
	orchDir := t.TempDir()
	projectPath := filepath.Join(orchDir, ".project.yaml")
	if err := os.WriteFile(projectPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	acp := &fakeLauncher{}
	r := &Runner{
		Workspaces:   workspace.NewStub(wsRoot),
		Launcher:     &fakeLauncher{},
		AcpLauncher:  acp,
		Kit:          "kit-ref",
		SessionsRoot: t.TempDir(),
	}

	if _, err := r.Start(context.Background(), Plan{
		Kind:        agent.TaskAgent,
		Branch:      "feat-x",
		ProjectPath: projectPath,
		TaskPath:    filepath.Join(orchDir, "tasks", "first.yaml"),
		TaskName:    "first",
		StaticMCPs:  []string{"github", "web-search"},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	mcps := argValues(acp.last.Command, "--static-mcp")
	if len(mcps) != 2 || mcps[0] != "github" || mcps[1] != "web-search" {
		t.Errorf("--static-mcp = %v, want [github web-search]", mcps)
	}
}

// Runner.Image reaches acp-wrapper as --template for EVERY ACP kind: task and
// commit share one Kit and one sandbox shape, so a prebuilt image that removes
// the kit's per-create npm install must not be kind-conditional. Unset, the
// flag is absent entirely so hosts without a prebuilt image keep sbx's stock
// template.
func TestACPLaunchForwardsTemplateImageForEveryKind(t *testing.T) {
	const image = "slimslenderslacks/claude-code-acp:0.88.0"
	for _, kind := range []agent.Kind{agent.TaskAgent, agent.CommitAgent} {
		t.Run(kind.String(), func(t *testing.T) {
			orchDir := t.TempDir()
			projectPath := filepath.Join(orchDir, ".project.yaml")
			if err := os.WriteFile(projectPath, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			plan := Plan{
				Kind:        kind,
				Branch:      "feat-x",
				ProjectPath: projectPath,
				TaskPath:    filepath.Join(orchDir, "tasks", "first.yaml"),
				TaskName:    "first",
			}

			acp := &fakeLauncher{}
			r := &Runner{
				Workspaces:   workspace.NewStub(t.TempDir()),
				Launcher:     &fakeLauncher{},
				AcpLauncher:  acp,
				Kit:          "kit-ref",
				Image:        image,
				SessionsRoot: t.TempDir(),
			}
			if _, err := r.Start(context.Background(), plan); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if got := argValue(acp.last.Command, "--template"); got != image {
				t.Errorf("--template = %q, want %q", got, image)
			}
			// The kit is still layered on: the image removes the install cost,
			// it does not replace the mixin.
			if got := argValue(acp.last.Command, "--kit"); got != "kit-ref" {
				t.Errorf("--kit = %q, want kit-ref", got)
			}

			bare := &fakeLauncher{}
			rNoImage := &Runner{
				Workspaces:   workspace.NewStub(t.TempDir()),
				Launcher:     &fakeLauncher{},
				AcpLauncher:  bare,
				Kit:          "kit-ref",
				SessionsRoot: t.TempDir(),
			}
			if _, err := rNoImage.Start(context.Background(), plan); err != nil {
				t.Fatalf("Start (no image): %v", err)
			}
			if hasFlag(bare.last.Command, "--template") {
				t.Errorf("--template present with no Runner.Image: %v", bare.last.Command)
			}
		})
	}
}

func TestACPLaunchTaskAgentForwardsPolicies(t *testing.T) {
	wsRoot := t.TempDir()
	orchDir := t.TempDir()
	projectPath := filepath.Join(orchDir, ".project.yaml")
	if err := os.WriteFile(projectPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	acp := &fakeLauncher{}
	r := &Runner{
		Workspaces:   workspace.NewStub(wsRoot),
		Launcher:     &fakeLauncher{},
		AcpLauncher:  acp,
		Kit:          "kit-ref",
		SessionsRoot: t.TempDir(),
	}

	if _, err := r.Start(context.Background(), Plan{
		Kind:        agent.TaskAgent,
		Branch:      "feat-x",
		ProjectPath: projectPath,
		TaskPath:    filepath.Join(orchDir, "tasks", "first.yaml"),
		TaskName:    "first",
		Policies: []policy.Rule{
			{Action: policy.ActionDeny, Kind: policy.KindNetwork, Resource: "**"},
			{Action: policy.ActionAllow, Kind: policy.KindNetwork, Resource: "api.github.com"},
		},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := argValues(acp.last.Command, "--policy")
	want := []string{"deny:network:**", "allow:network:api.github.com"}
	if len(got) != len(want) {
		t.Fatalf("--policy = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("--policy[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestACPLaunchTaskAgentForwardsSandboxCleanupFlags(t *testing.T) {
	wsRoot := t.TempDir()
	orchDir := t.TempDir()
	projectPath := filepath.Join(orchDir, ".project.yaml")
	if err := os.WriteFile(projectPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	taskPath := filepath.Join(orchDir, "tasks", "first.yaml")

	// A task without save_sandbox: --task-path is forwarded (so the wrapper can
	// re-read the final status) but --save-sandbox is absent.
	acp := &fakeLauncher{}
	r := &Runner{
		Workspaces:   workspace.NewStub(wsRoot),
		Launcher:     &fakeLauncher{},
		AcpLauncher:  acp,
		Kit:          "kit-ref",
		SessionsRoot: t.TempDir(),
	}
	if _, err := r.Start(context.Background(), Plan{
		Kind:        agent.TaskAgent,
		Branch:      "feat-x",
		ProjectPath: projectPath,
		TaskPath:    taskPath,
		TaskName:    "first",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := argValue(acp.last.Command, "--task-path"); got != taskPath {
		t.Errorf("--task-path = %q, want %q", got, taskPath)
	}
	if hasFlag(acp.last.Command, "--save-sandbox") {
		t.Errorf("--save-sandbox present without SaveSandbox set: %v", acp.last.Command)
	}

	// A task with SaveSandbox=true forwards --save-sandbox.
	acp2 := &fakeLauncher{}
	r.AcpLauncher = acp2
	if _, err := r.Start(context.Background(), Plan{
		Kind:        agent.TaskAgent,
		Branch:      "feat-x",
		ProjectPath: projectPath,
		TaskPath:    taskPath,
		TaskName:    "first",
		SaveSandbox: true,
	}); err != nil {
		t.Fatalf("Start (save): %v", err)
	}
	if !hasFlag(acp2.last.Command, "--save-sandbox") {
		t.Errorf("--save-sandbox missing with SaveSandbox set: %v", acp2.last.Command)
	}
}

// TestArchiveAgentNeverUsesACP: the archive agent is interactive and has no
// persistent-ACP mode, so even a fully ACP-wired runner keeps it on tmux.
func TestArchiveAgentNeverUsesACP(t *testing.T) {
	workingDir := t.TempDir()
	projectPath := filepath.Join(workingDir, ".project.yaml")
	if err := os.WriteFile(projectPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	tmux := &fakeLauncher{}
	r := &Runner{
		Launcher:     tmux,
		AcpLauncher:  &failLauncher{t: t},
		Kit:          "kit",
		SessionsRoot: t.TempDir(),
		Command:      func(_ agent.Kind, _ string) []string { return []string{"claude", "hi"} },
	}
	if _, err := r.Start(context.Background(), Plan{
		Kind:        agent.ArchiveAgent,
		WorkingDir:  workingDir,
		ProjectPath: projectPath,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if tmux.last.Command[0] != "claude" {
		t.Errorf("archive agent should use the tmux launcher with the built command, got %v", tmux.last.Command)
	}
}

// TestWolfOnHostUsesTmux: the --wolf-host escape hatch keeps the wolf on the
// legacy host/tmux path, unsandboxed, even when an AcpLauncher is configured.
func TestWolfOnHostUsesTmux(t *testing.T) {
	workingDir := t.TempDir()
	projectPath := filepath.Join(workingDir, ".project.yaml")
	if err := os.WriteFile(projectPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	tmux := &fakeLauncher{}
	r := &Runner{
		Launcher:     tmux,
		AcpLauncher:  &failLauncher{t: t},
		WolfOnHost:   true,
		Kit:          "kit",
		SessionsRoot: t.TempDir(),
		Command:      func(_ agent.Kind, _ string) []string { return []string{"claude", "hi"} },
	}
	if _, err := r.Start(context.Background(), Plan{
		Kind:        agent.WolfAgent,
		WorkingDir:  workingDir,
		ProjectPath: projectPath,
		Persistent:  true, // meaningless on the tmux path; must be ignored
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if tmux.last.Command[0] != "claude" {
		t.Errorf("host wolf should use the tmux launcher with the built command, got %v", tmux.last.Command)
	}
	instructions, err := os.ReadFile(filepath.Join(workingDir, ".orch", "instructions.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(instructions), "you are sandboxed") {
		t.Errorf("host wolf must not be told it is sandboxed:\n%s", instructions)
	}
}

func TestUsesACPWolf(t *testing.T) {
	cases := []struct {
		name string
		r    Runner
		kind agent.Kind
		want bool
	}{
		{"wolf under ACP", Runner{AcpLauncher: &fakeLauncher{}}, agent.WolfAgent, true},
		{"wolf on host", Runner{AcpLauncher: &fakeLauncher{}, WolfOnHost: true}, agent.WolfAgent, false},
		{"wolf without ACP", Runner{}, agent.WolfAgent, false},
		{"archive stays on tmux", Runner{AcpLauncher: &fakeLauncher{}}, agent.ArchiveAgent, false},
		{"planning under ACP unaffected by WolfOnHost", Runner{AcpLauncher: &fakeLauncher{}, WolfOnHost: true}, agent.PlanningAgent, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.UsesACP(tc.kind); got != tc.want {
				t.Errorf("UsesACP(%s) = %v, want %v", tc.kind, got, tc.want)
			}
		})
	}
}

func TestACPSandboxNameFor(t *testing.T) {
	projectPath := "/orch/my_proj/.project.yaml"
	if got := ACPSandboxNameFor(agent.WolfAgent, projectPath, ""); got != "my-proj-wolf" {
		t.Errorf("wolf sandbox = %q, want my-proj-wolf", got)
	}
	if got := ACPSandboxNameFor(agent.WolfAgent, "", ""); got != "" {
		t.Errorf("wolf sandbox without a project = %q, want empty", got)
	}
	// Every other kind is exactly SandboxNameFor.
	if got, want := ACPSandboxNameFor(agent.TaskAgent, projectPath, "t1"), SandboxNameFor(agent.TaskAgent, projectPath, "t1"); got != want {
		t.Errorf("task sandbox = %q, want %q", got, want)
	}
	// The host wolf contract is unchanged: no sandbox.
	if got := SandboxNameFor(agent.WolfAgent, projectPath, ""); got != "" {
		t.Errorf("SandboxNameFor(wolf) = %q, want empty (host path)", got)
	}
}

// TestACPLaunchWolfPersistent: the wolf launches through acp-wrapper as a
// persistent session — --persistent instead of --exit-when-empty, with its own
// sandbox, the worktree as a second mount, the unblock/idle end conditions, and
// a session.json marked Persistent so watchers keep the tab open and typeable.
func TestACPLaunchWolfPersistent(t *testing.T) {
	wsRoot := t.TempDir()
	orchDir := t.TempDir()
	projectPath := filepath.Join(orchDir, ".project.yaml")
	if err := os.WriteFile(projectPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sessionsRoot := t.TempDir()

	acp := &fakeLauncher{}
	tmux := &fakeLauncher{}
	r := &Runner{
		Workspaces:   workspace.NewStub(wsRoot),
		Launcher:     tmux,
		AcpLauncher:  acp,
		Kit:          "kit-ref",
		SessionsRoot: sessionsRoot,
	}
	if _, err := r.Start(context.Background(), Plan{
		Kind:          agent.WolfAgent,
		WorkingDir:    orchDir,
		ProjectPath:   projectPath,
		Branch:        "feat-x",
		Repos:         []workspace.Repo{{Identity: "github.com/acme/widgets", Shortname: "widgets"}},
		BlockedReason: "boom",
		Persistent:    true,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if tmux.last.Name != "" {
		t.Errorf("tmux launcher was used for the ACP wolf: %+v", tmux.last)
	}

	cmd := acp.last.Command
	if !hasFlag(cmd, "--persistent") {
		t.Errorf("expected --persistent in argv, got %v", cmd)
	}
	if hasFlag(cmd, "--exit-when-empty") {
		t.Errorf("a persistent session must not be one-shot: %v", cmd)
	}
	if got := argValue(cmd, "--unblock-grace"); got != DefaultUnblockGrace.String() {
		t.Errorf("--unblock-grace = %q, want %q", got, DefaultUnblockGrace)
	}
	if got := argValue(cmd, "--idle-timeout"); got != DefaultPersistentIdleTimeout.String() {
		t.Errorf("--idle-timeout = %q, want %q", got, DefaultPersistentIdleTimeout)
	}
	if got := argValue(cmd, "--kind"); got != "wolf" {
		t.Errorf("--kind = %q, want wolf", got)
	}
	if got := argValue(cmd, "--project-path"); got != projectPath {
		t.Errorf("--project-path = %q, want %q", got, projectPath)
	}
	if got, want := argValue(cmd, "--sandbox"), filepath.Base(orchDir)+"-wolf"; got != want {
		t.Errorf("--sandbox = %q, want %q", got, want)
	}
	// Control dir first (the agent's cwd), source worktree second; no extra orch
	// mount since the control dir already is the orch dir.
	wantWorktree := filepath.Join(wsRoot, "feat-x")
	if ws := argValues(cmd, "--workspace"); len(ws) != 2 || ws[0] != orchDir || ws[1] != wantWorktree {
		t.Errorf("--workspace = %v, want [%q %q]", ws, orchDir, wantWorktree)
	}

	store := session.Store{Root: sessionsRoot}
	rec, err := store.Read(acp.last.Name)
	if err != nil {
		t.Fatalf("read session.json: %v", err)
	}
	if !rec.Persistent || rec.Kind != "wolf" {
		t.Errorf("session.json = %+v, want persistent wolf", rec)
	}

	instructions, err := os.ReadFile(filepath.Join(orchDir, ".orch", "instructions.md"))
	if err != nil {
		t.Fatal(err)
	}
	out := string(instructions)
	if !strings.Contains(out, "you are sandboxed") || !strings.Contains(out, wantWorktree) {
		t.Errorf("sandboxed wolf instructions should say so and name the worktree:\n%s", out)
	}
	if strings.Contains(out, "send a macOS") {
		t.Errorf("sandboxed wolf can't osascript; instructions still say to:\n%s", out)
	}
	ctxYAML, err := os.ReadFile(filepath.Join(orchDir, ".orch", "context.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ctxYAML), "sandboxed: true") {
		t.Errorf("context.yaml should record that the wolf is sandboxed:\n%s", ctxYAML)
	}
}

// TestACPLaunchWolfKnobs: zero uses the defaults, negative disables the
// corresponding end condition, positive is passed through.
func TestACPLaunchWolfKnobs(t *testing.T) {
	orchDir := t.TempDir()
	projectPath := filepath.Join(orchDir, ".project.yaml")
	if err := os.WriteFile(projectPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	acp := &fakeLauncher{}
	r := &Runner{
		Launcher:               &fakeLauncher{},
		AcpLauncher:            acp,
		Kit:                    "kit-ref",
		SessionsRoot:           t.TempDir(),
		PersistentUnblockGrace: -1,
		PersistentIdleTimeout:  90 * time.Minute,
	}
	if _, err := r.Start(context.Background(), Plan{
		Kind:        agent.WolfAgent,
		WorkingDir:  orchDir,
		ProjectPath: projectPath,
		Persistent:  true,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	cmd := acp.last.Command
	if hasFlag(cmd, "--unblock-grace") {
		t.Errorf("negative grace should disable the flag: %v", cmd)
	}
	if got := argValue(cmd, "--idle-timeout"); got != (90 * time.Minute).String() {
		t.Errorf("--idle-timeout = %q, want %q", got, 90*time.Minute)
	}
}

// brokenWorkspaces is a workspace.Manager whose Create always fails.
type brokenWorkspaces struct{ workspace.Manager }

func (brokenWorkspaces) Create(context.Context, string, []workspace.Repo) (string, error) {
	return "", errors.New("wsp is broken")
}

// TestACPLaunchWolfSurvivesWorktreeFailure: the wolf exists to diagnose broken
// projects, so a worktree it can't provision must degrade to "no source mount",
// not fail the launch.
func TestACPLaunchWolfSurvivesWorktreeFailure(t *testing.T) {
	orchDir := t.TempDir()
	projectPath := filepath.Join(orchDir, ".project.yaml")
	if err := os.WriteFile(projectPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	acp := &fakeLauncher{}
	r := &Runner{
		Workspaces:   brokenWorkspaces{},
		Launcher:     &fakeLauncher{},
		AcpLauncher:  acp,
		Kit:          "kit-ref",
		SessionsRoot: t.TempDir(),
	}
	if _, err := r.Start(context.Background(), Plan{
		Kind:        agent.WolfAgent,
		WorkingDir:  orchDir,
		ProjectPath: projectPath,
		Branch:      "feat-x",
		Repos:       []workspace.Repo{{Identity: "github.com/acme/widgets", Shortname: "widgets"}},
		Persistent:  true,
	}); err != nil {
		t.Fatalf("Start should tolerate a worktree failure for the wolf: %v", err)
	}
	if ws := argValues(acp.last.Command, "--workspace"); len(ws) != 1 || ws[0] != orchDir {
		t.Errorf("--workspace = %v, want just the control dir", ws)
	}
}

// A one-shot agent is unchanged by the new knobs: exit-when-empty, no
// persistent-only flags.
func TestACPLaunchTaskStaysOneShot(t *testing.T) {
	orchDir := t.TempDir()
	projectPath := filepath.Join(orchDir, ".project.yaml")
	if err := os.WriteFile(projectPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	acp := &fakeLauncher{}
	r := &Runner{
		Workspaces:   workspace.NewStub(t.TempDir()),
		Launcher:     &fakeLauncher{},
		AcpLauncher:  acp,
		Kit:          "kit-ref",
		SessionsRoot: t.TempDir(),
	}
	if _, err := r.Start(context.Background(), Plan{
		Kind:        agent.TaskAgent,
		Branch:      "feat-x",
		ProjectPath: projectPath,
		TaskName:    "first",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	cmd := acp.last.Command
	if !hasFlag(cmd, "--exit-when-empty") || hasFlag(cmd, "--persistent") || hasFlag(cmd, "--unblock-grace") || hasFlag(cmd, "--idle-timeout") {
		t.Errorf("task agent should stay one-shot: %v", cmd)
	}
}

func TestACPRequiresKit(t *testing.T) {
	workingDir := t.TempDir()
	projectPath := filepath.Join(workingDir, ".project.yaml")
	if err := os.WriteFile(projectPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := &Runner{
		Launcher:     &fakeLauncher{},
		AcpLauncher:  &fakeLauncher{},
		SessionsRoot: t.TempDir(),
		// Kit deliberately empty.
	}
	_, err := r.Start(context.Background(), Plan{
		Kind:        agent.PlanningAgent,
		WorkingDir:  workingDir,
		ProjectPath: projectPath,
		Branch:      "feat-x",
	})
	if err == nil || !strings.Contains(err.Error(), "Kit") {
		t.Errorf("expected a Kit-required error, got %v", err)
	}
}

// failLauncher fails the test if its Launch is ever called. Used to prove the
// ACP path is not taken for interactive agents.
type failLauncher struct{ t *testing.T }

func (f *failLauncher) Launch(context.Context, agent.Spec) (agent.Session, error) {
	f.t.Fatal("AcpLauncher must not be used for interactive agents")
	return nil, nil
}
