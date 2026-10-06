package runner

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/policy"
	"github.com/slimslenderslacks/work/internal/session"
)

func TestWorkingmanWorkspaces(t *testing.T) {
	cases := []struct {
		name    string
		scratch string
		ro      []string
		want    []string
	}{
		{
			name:    "scratch first and writable, everything else read-only",
			scratch: "/s/workingman-agent",
			ro:      []string{"/orch", "/s/state", "/s/sessions"},
			want:    []string{"/s/workingman-agent", "/orch:ro", "/s/state:ro", "/s/sessions:ro"},
		},
		{
			name:    "duplicates collapse, first wins",
			scratch: "/s/w",
			ro:      []string{"/orch", "/orch", "/orch/"},
			want:    []string{"/s/w", "/orch:ro"},
		},
		{
			name:    "a mount under another read-only mount is dropped",
			scratch: "/s/w",
			ro:      []string{"/orch/logs", "/orch", "/other"},
			want:    []string{"/s/w", "/orch:ro", "/other:ro"},
		},
		{
			name:    "a read-only mount that would contain the scratch dir is dropped",
			scratch: "/s/w",
			ro:      []string{"/s", "/orch"},
			want:    []string{"/s/w", "/orch:ro"},
		},
		{
			name:    "a read-only mount inside the scratch dir is dropped",
			scratch: "/s/w",
			ro:      []string{"/s/w/inner", "/orch"},
			want:    []string{"/s/w", "/orch:ro"},
		},
		{
			name:    "blank entries ignored",
			scratch: "/s/w",
			ro:      []string{"", "  ", "/orch"},
			want:    []string{"/s/w", "/orch:ro"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := workingmanWorkspaces(tc.scratch, tc.ro)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("workingmanWorkspaces = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWorkingmanSandboxNamesAreProjectIndependent(t *testing.T) {
	if got := ACPSandboxNameFor(agent.WorkingmanAgent, "", ""); got != "workingman-agent" {
		t.Errorf("ACPSandboxNameFor = %q, want workingman-agent", got)
	}
	// A stray project path must not leak into the name.
	if got := ACPSandboxNameFor(agent.WorkingmanAgent, "/orch/myproj/.project.yaml", ""); got != "workingman-agent" {
		t.Errorf("ACPSandboxNameFor with a project path = %q, want workingman-agent", got)
	}
	// The legacy (non-ACP) derivation has no sandbox for it.
	if got := SandboxNameFor(agent.WorkingmanAgent, "/orch/myproj/.project.yaml", ""); got != "" {
		t.Errorf("SandboxNameFor = %q, want \"\"", got)
	}
}

func TestUsesACPWorkingman(t *testing.T) {
	if (&Runner{}).UsesACP(agent.WorkingmanAgent) {
		t.Error("without an AcpLauncher the workingman agent must not claim ACP")
	}
	if !(&Runner{AcpLauncher: &fakeLauncher{}}).UsesACP(agent.WorkingmanAgent) {
		t.Error("with an AcpLauncher the workingman agent runs under ACP")
	}
}

func TestStartWorkingmanAgent(t *testing.T) {
	base := t.TempDir()
	scratch := filepath.Join(base, "workingman-agent") // does not exist yet: Start creates it
	sessionsRoot := filepath.Join(base, "sessions")
	stateDir := filepath.Join(base, "state")
	orch := t.TempDir()
	auditDir := filepath.Join(t.TempDir(), "logs")

	acp := &fakeLauncher{}
	tmux := &fakeLauncher{}
	r := &Runner{
		Launcher:       tmux,
		AcpLauncher:    acp,
		Kit:            "/kits/acp-kit",
		SessionsRoot:   sessionsRoot,
		AcpWrapperPath: "/bin/acp-wrapper",
		// A configured idle timeout must still not apply to the always-on agent.
		PersistentIdleTimeout: 1,
	}
	denyAll := policy.Rule{Action: policy.ActionDeny, Kind: policy.KindFilesystem, Resource: "/orch/**"}
	_, err := r.Start(context.Background(), Plan{
		Kind:           agent.WorkingmanAgent,
		WorkingDir:     scratch,
		ReadOnlyMounts: []string{orch, stateDir, auditDir, sessionsRoot},
		Observe: Observe{
			Roots:        []string{orch},
			SnapshotFile: filepath.Join(stateDir, "snapshot.json"),
			AuditLog:     filepath.Join(auditDir, "audit.log"),
			SessionsRoot: sessionsRoot,
		},
		Policies: []policy.Rule{denyAll},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if tmux.last.Name != "" {
		t.Errorf("the tmux launcher must not be used: %+v", tmux.last)
	}
	cmd := acp.last.Command
	if len(cmd) == 0 || cmd[0] != "/bin/acp-wrapper" {
		t.Fatalf("command = %v", cmd)
	}
	if got := argValue(cmd, "--session-id"); got != "workingman-agent" {
		t.Errorf("--session-id = %q", got)
	}
	if got := argValue(cmd, "--sandbox"); got != "workingman-agent" {
		t.Errorf("--sandbox = %q", got)
	}
	if got := argValue(cmd, "--kind"); got != "workingman" {
		t.Errorf("--kind = %q", got)
	}
	if !hasFlag(cmd, "--persistent") || hasFlag(cmd, "--exit-when-empty") {
		t.Errorf("must be persistent, not one-shot: %v", cmd)
	}
	for _, f := range []string{"--idle-timeout", "--unblock-grace", "--project-path", "--task-path", "--static-mcp"} {
		if hasFlag(cmd, f) {
			t.Errorf("workingman agent must not carry %s: %v", f, cmd)
		}
	}
	// Mounts: scratch writable and FIRST (the ACP client's cwd); the rest :ro.
	want := []string{scratch, orch + ":ro", stateDir + ":ro", auditDir + ":ro", sessionsRoot + ":ro"}
	if got := argValues(cmd, "--workspace"); !reflect.DeepEqual(got, want) {
		t.Errorf("--workspace = %v, want %v", got, want)
	}
	if got := argValues(cmd, "--policy"); !reflect.DeepEqual(got, []string{denyAll.Encode()}) {
		t.Errorf("--policy = %v", got)
	}

	// The scratch dir exists and holds the handoff files, rendered for this kind.
	instr, err := os.ReadFile(filepath.Join(scratch, ".orch", "instructions.md"))
	if err != nil {
		t.Fatalf("instructions.md: %v", err)
	}
	for _, want := range []string{"workingman agent", filepath.Join(stateDir, "snapshot.json"), filepath.Join(auditDir, "audit.log"), orch} {
		if !strings.Contains(string(instr), want) {
			t.Errorf("instructions.md missing %q", want)
		}
	}
	ctxYAML, err := os.ReadFile(filepath.Join(scratch, ".orch", "context.yaml"))
	if err != nil {
		t.Fatalf("context.yaml: %v", err)
	}
	for _, want := range []string{"kind: workingman", "snapshot_file: " + filepath.Join(stateDir, "snapshot.json"), "sessions_root: " + sessionsRoot} {
		if !strings.Contains(string(ctxYAML), want) {
			t.Errorf("context.yaml missing %q:\n%s", want, ctxYAML)
		}
	}

	// The session record has no project and no task, and is marked persistent.
	rec, err := (session.Store{Root: sessionsRoot}).Read("workingman-agent")
	if err != nil {
		t.Fatalf("session.json: %v", err)
	}
	if rec.Kind != "workingman" || rec.ProjectPath != "" || rec.TaskPath != "" || !rec.Persistent || rec.SandboxName != "workingman-agent" {
		t.Errorf("session record = %+v", rec)
	}
}

func TestStartWorkingmanAgentNeedsACP(t *testing.T) {
	r := &Runner{Launcher: &fakeLauncher{}} // no AcpLauncher
	_, err := r.Start(context.Background(), Plan{Kind: agent.WorkingmanAgent, WorkingDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "ACP") {
		t.Fatalf("Start without ACP = %v, want an error naming ACP", err)
	}
}
