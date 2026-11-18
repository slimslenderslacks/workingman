package runner

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/task"
	"github.com/slimslenderslacks/work/internal/workspace"
)

// StartConfigured reports, for an ACP launch, exactly what it handed
// acp-wrapper: the harness's template, the model, the --workspace mounts, the
// --mcp-server set and --mcp-dynamic.
func TestStartConfiguredACPReportsLaunchConfig(t *testing.T) {
	for _, tc := range []struct {
		name        string
		kind        agent.Kind
		image       string
		model       string
		staticMCPs  []string
		url         string
		wantHarness string
		wantModel   string
		wantMCPs    []string
		wantDynamic bool
	}{
		{name: "task dynamic gateway", kind: agent.TaskAgent, image: "img:1", model: "claude-opus-5-5", url: "http://gw", wantHarness: "img:1", wantModel: "claude-opus-5-5", wantMCPs: []string{"gmail"}, wantDynamic: true},
		{name: "task static_mcps", kind: agent.TaskAgent, staticMCPs: []string{"linear"}, url: "http://gw", wantHarness: "claude", wantModel: task.ModelDefault, wantMCPs: []string{"linear"}, wantDynamic: true},
		{name: "commit static gateway", kind: agent.CommitAgent, url: "http://gw", wantHarness: "claude", wantModel: task.ModelDefault, wantMCPs: []string{"gmail"}},
		{name: "planning", kind: agent.PlanningAgent, url: "http://gw", wantHarness: "claude", wantModel: task.ModelDefault, wantMCPs: []string{"gmail"}},
		{name: "wolf", kind: agent.WolfAgent, url: "http://gw", wantHarness: "claude", wantModel: task.ModelDefault, wantMCPs: []string{"gmail"}, wantDynamic: true},
		{name: "no gateway url", kind: agent.TaskAgent, wantHarness: "claude", wantModel: task.ModelDefault},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wsRoot := t.TempDir()
			orchDir := t.TempDir()
			projectPath := filepath.Join(orchDir, "project.yaml")
			if err := os.WriteFile(projectPath, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			acp := &fakeLauncher{}
			r := &Runner{
				Workspaces:        workspace.NewStub(wsRoot),
				Launcher:          &fakeLauncher{},
				AcpLauncher:       acp,
				Kit:               "kit-ref",
				Image:             tc.image,
				SessionsRoot:      t.TempDir(),
				MCPGatewayURL:     tc.url,
				MCPGatewayServers: []string{"gmail"},
			}
			p := Plan{
				Kind:        tc.kind,
				Branch:      "feat-x",
				ProjectPath: projectPath,
				Model:       tc.model,
				StaticMCPs:  tc.staticMCPs,
			}
			if tc.kind == agent.TaskAgent || tc.kind == agent.CommitAgent {
				p.TaskPath = filepath.Join(orchDir, "tasks", "first.yaml")
				p.TaskName = "first"
			}
			_, cfg, err := r.StartConfigured(context.Background(), p)
			if err != nil {
				t.Fatalf("StartConfigured: %v", err)
			}
			cmd := acp.last.Command
			if cfg.Harness != tc.wantHarness {
				t.Errorf("Harness = %q, want %q", cfg.Harness, tc.wantHarness)
			}
			if cfg.Model != tc.wantModel {
				t.Errorf("Model = %q, want %q", cfg.Model, tc.wantModel)
			}
			if want := argValues(cmd, "--workspace"); len(want) == 0 || !reflect.DeepEqual(cfg.Mounts, want) {
				t.Errorf("Mounts = %v, want the --workspace args %v", cfg.Mounts, want)
			}
			if !reflect.DeepEqual(cfg.MCPs, tc.wantMCPs) || !reflect.DeepEqual(cfg.MCPs, argValues(cmd, "--mcp-server")) {
				t.Errorf("MCPs = %v, want %v (--mcp-server %v)", cfg.MCPs, tc.wantMCPs, argValues(cmd, "--mcp-server"))
			}
			if cfg.MCPDynamic != tc.wantDynamic || cfg.MCPDynamic != hasFlag(cmd, "--mcp-dynamic") {
				t.Errorf("MCPDynamic = %v, want %v (--mcp-dynamic %v)", cfg.MCPDynamic, tc.wantDynamic, hasFlag(cmd, "--mcp-dynamic"))
			}
		})
	}
}

// The workingman agent with no default servers still gets a dynamic gateway;
// its config must say so rather than report the empty request it started from.
// Its read-only mounts keep their ":ro" suffix.
func TestStartConfiguredWorkingmanFallbackGateway(t *testing.T) {
	ro := t.TempDir()
	r := &Runner{
		AcpLauncher:   &fakeLauncher{},
		Kit:           "/kits/acp",
		SessionsRoot:  filepath.Join(t.TempDir(), "sessions"),
		MCPGatewayURL: "http://gw",
	}
	scratch := filepath.Join(t.TempDir(), "workingman-agent")
	_, cfg, err := r.StartConfigured(context.Background(), Plan{
		Kind:           agent.WorkingmanAgent,
		WorkingDir:     scratch,
		ReadOnlyMounts: []string{ro},
	})
	if err != nil {
		t.Fatalf("StartConfigured: %v", err)
	}
	if !cfg.MCPDynamic || len(cfg.MCPs) != 0 {
		t.Errorf("MCPs = %v, MCPDynamic = %v; want none, dynamic", cfg.MCPs, cfg.MCPDynamic)
	}
	if want := []string{scratch, ro + ReadOnlySuffix}; !reflect.DeepEqual(cfg.Mounts, want) {
		t.Errorf("Mounts = %v, want %v", cfg.Mounts, want)
	}
}

// A terminal-tab launch into a sandbox reports the stock template and the
// sandbox's mounts; one that runs on the host reports the zero config.
func TestStartConfiguredTerminalTab(t *testing.T) {
	workingDir := t.TempDir()
	projectPath := filepath.Join(workingDir, "project.yaml")
	_ = os.WriteFile(projectPath, nil, 0o644)
	plan := Plan{Kind: agent.PlanningAgent, WorkingDir: workingDir, ProjectPath: projectPath, Branch: "feat/x", Model: "m1"}
	command := func(agent.Kind, string) []string { return []string{"claude"} }

	r := &Runner{
		Launcher: &fakeLauncher{},
		Command:  command,
		Sandbox:  func(context.Context, SandboxSpec) error { return nil },
	}
	_, cfg, err := r.StartConfigured(context.Background(), plan)
	if err != nil {
		t.Fatalf("StartConfigured: %v", err)
	}
	want := SessionConfig{Harness: "claude", Model: "m1", Mounts: []string{workingDir}}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("sandboxed config = %+v, want %+v", cfg, want)
	}

	host := &Runner{Launcher: &fakeLauncher{}, Command: command}
	_, cfg, err = host.StartConfigured(context.Background(), plan)
	if err != nil {
		t.Fatalf("StartConfigured (host): %v", err)
	}
	if !reflect.DeepEqual(cfg, SessionConfig{}) {
		t.Errorf("host config = %+v, want zero", cfg)
	}
}
