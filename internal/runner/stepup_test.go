package runner

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/session"
)

func TestStepUpSandboxNameIsProjectIndependent(t *testing.T) {
	for _, p := range []string{"", "/orch/myproj/project.yaml"} {
		if got := ACPSandboxNameFor(agent.StepUpAgent, p, ""); got != "step-up" {
			t.Errorf("ACPSandboxNameFor(%q) = %q, want step-up", p, got)
		}
	}
}

// The Step-Up agent gets its fixed names, a persistent wrapper with no end
// condition, the okta-gateway requirement, post-office as its only and static
// MCP server (even with a dynamic default elsewhere), its own mounts and no
// orch roots.
func TestStartStepUpAgent(t *testing.T) {
	base := t.TempDir()
	scratch := filepath.Join(base, "step-up-agent")
	sessionsRoot := filepath.Join(base, "sessions")
	stateDir := filepath.Join(base, "state")
	stepUpDir := filepath.Join(base, "step-up")
	requests := filepath.Join(stepUpDir, "requests")
	results := filepath.Join(stepUpDir, "results")

	acp := &fakeLauncher{}
	r := &Runner{
		Launcher:          &fakeLauncher{},
		AcpLauncher:       acp,
		Kit:               "/kits/acp-kit",
		SessionsRoot:      sessionsRoot,
		AcpWrapperPath:    "/bin/acp-wrapper",
		MCPGatewayURL:     "http://localhost:3333",
		MCPGatewayServers: []string{"github"},
		// Must not apply to the always-on agent.
		PersistentIdleTimeout: 1,
	}
	_, err := r.Start(context.Background(), Plan{
		Kind:           agent.StepUpAgent,
		WorkingDir:     scratch,
		WritableMounts: []string{requests},
		ReadOnlyMounts: []string{stateDir, sessionsRoot, results},
		Observe: Observe{
			SnapshotFile: filepath.Join(stateDir, "snapshot.json"),
			SessionsRoot: sessionsRoot,
			StepUpDir:    stepUpDir,
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	cmd := acp.last.Command
	for flag, want := range map[string]string{"--session-id": "step-up-agent", "--sandbox": "step-up", "--kind": "step-up"} {
		if got := argValue(cmd, flag); got != want {
			t.Errorf("%s = %q, want %q", flag, got, want)
		}
	}
	for _, f := range []string{"--persistent", "--require-okta-gateway"} {
		if !hasFlag(cmd, f) {
			t.Errorf("missing %s: %v", f, cmd)
		}
	}
	for _, f := range []string{"--exit-when-empty", "--idle-timeout", "--unblock-grace", "--mcp-dynamic", "--project-path", "--task-path"} {
		if hasFlag(cmd, f) {
			t.Errorf("must not carry %s: %v", f, cmd)
		}
	}
	if got := argValues(cmd, "--mcp-server"); !reflect.DeepEqual(got, []string{"post-office"}) {
		t.Errorf("--mcp-server = %v, want [post-office]", got)
	}
	want := []string{scratch, requests, stateDir + ":ro", sessionsRoot + ":ro", results + ":ro"}
	if got := argValues(cmd, "--workspace"); !reflect.DeepEqual(got, want) {
		t.Errorf("--workspace = %v, want %v", got, want)
	}

	instr, err := os.ReadFile(filepath.Join(scratch, ".orch", "instructions.md"))
	if err != nil {
		t.Fatalf("instructions.md: %v", err)
	}
	for _, want := range []string{"Step-Up agent", stepUpDir, "next_message"} {
		if !strings.Contains(string(instr), want) {
			t.Errorf("instructions.md missing %q", want)
		}
	}
	ctxYAML, err := os.ReadFile(filepath.Join(scratch, ".orch", "context.yaml"))
	if err != nil {
		t.Fatalf("context.yaml: %v", err)
	}
	if !strings.Contains(string(ctxYAML), "kind: step-up") || !strings.Contains(string(ctxYAML), "step_up_dir: "+stepUpDir) {
		t.Errorf("context.yaml:\n%s", ctxYAML)
	}
	rec, err := (session.Store{Root: sessionsRoot}).Read("step-up-agent")
	if err != nil {
		t.Fatalf("session.json: %v", err)
	}
	if rec.Kind != "step-up" || rec.SandboxName != "step-up" || !rec.Persistent || rec.ProjectPath != "" {
		t.Errorf("session record = %+v", rec)
	}
}

func TestStepUpAgentGatewayIsNeverDynamic(t *testing.T) {
	r := &Runner{AcpLauncher: &fakeLauncher{}}
	if r.MCPGatewayDynamic(agent.StepUpAgent) {
		t.Error("the Step-Up agent's gateway must be static")
	}
}

func TestStartStepUpAgentNeedsACP(t *testing.T) {
	r := &Runner{Launcher: &fakeLauncher{}}
	_, err := r.Start(context.Background(), Plan{Kind: agent.StepUpAgent, WorkingDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "ACP") {
		t.Fatalf("Start without ACP = %v, want an error naming ACP", err)
	}
}
