package runner

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/workspace"
)

// withoutFlag returns args with every "flag value" pair removed.
func withoutFlag(args []string, flag string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if args[i] == flag && i+1 < len(args) {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}

// Runner.ExtraCACerts reaches acp-wrapper as --extra-ca-certs for every ACP
// kind, and is the ONLY difference in the argv: with it unset the argv is
// byte-identical to a Runner that has never heard of the setting.
func TestACPLaunchForwardsExtraCACertsOnlyWhenSet(t *testing.T) {
	const caPath = "/Users/me/.workingman/extra-ca.pem"
	for _, kind := range []agent.Kind{agent.TaskAgent, agent.CommitAgent, agent.PlanningAgent} {
		t.Run(kind.String(), func(t *testing.T) {
			orchDir := t.TempDir()
			projectPath := filepath.Join(orchDir, "project.yaml")
			if err := os.WriteFile(projectPath, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			plan := Plan{
				Kind:        kind,
				Branch:      "feat-x",
				ProjectPath: projectPath,
				TaskName:    "first",
			}
			if kind != agent.PlanningAgent {
				plan.TaskPath = filepath.Join(orchDir, "tasks", "first.yaml")
			}
			wsRoot, sessionsRoot := t.TempDir(), t.TempDir()
			launch := func(ca string) []string {
				t.Helper()
				acp := &fakeLauncher{}
				r := &Runner{
					Workspaces:   workspace.NewStub(wsRoot),
					Launcher:     &fakeLauncher{},
					AcpLauncher:  acp,
					Kit:          "kit-ref",
					Image:        "acp:1",
					SessionsRoot: sessionsRoot,
					ExtraCACerts: ca,
				}
				if _, err := r.Start(context.Background(), plan); err != nil {
					t.Fatalf("Start (ca=%q): %v", ca, err)
				}
				return acp.last.Command
			}

			off := launch("")
			if hasFlag(off, "--extra-ca-certs") {
				t.Errorf("--extra-ca-certs present with the setting off: %v", off)
			}
			on := launch(caPath)
			if got := argValue(on, "--extra-ca-certs"); got != caPath {
				t.Errorf("--extra-ca-certs = %q, want %q (argv %v)", got, caPath, on)
			}
			if got := withoutFlag(on, "--extra-ca-certs"); !reflect.DeepEqual(got, off) {
				t.Errorf("argv differs beyond --extra-ca-certs:\n on  %v\n off %v", on, off)
			}
		})
	}
}

// The legacy terminal-tab path hands Runner.ExtraCACerts to the
// SandboxCreator, and leaves the spec's field empty when it is unset.
func TestSandboxSpecCarriesExtraCACerts(t *testing.T) {
	for _, ca := range []string{"", "/Users/me/.workingman/extra-ca.pem"} {
		var got []SandboxSpec
		r := &Runner{
			Launcher: &fakeLauncher{},
			Command:  func(_ agent.Kind, _ string) []string { return []string{"claude", "hi"} },
			Sandbox: func(_ context.Context, spec SandboxSpec) error {
				got = append(got, spec)
				return nil
			},
			ExtraCACerts: ca,
		}
		projectPath := filepath.Join(t.TempDir(), "project.yaml")
		if err := os.WriteFile(projectPath, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Start(context.Background(), Plan{
			Kind:        agent.ReviewAgent,
			WorkingDir:  t.TempDir(),
			ProjectPath: projectPath,
			Branch:      "feat/x",
		}); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if len(got) != 1 || got[0].ExtraCACerts != ca {
			t.Errorf("ca=%q: specs = %+v, want one with ExtraCACerts %q", ca, got, ca)
		}
	}
}
