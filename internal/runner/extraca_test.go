package runner

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/mcpgateway"
	"github.com/slimslenderslacks/work/internal/policy"
	"github.com/slimslenderslacks/work/internal/sbx"
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

// writeRunnerTestCA writes a valid self-signed root as PEM and returns its
// path and the normalized bundle LoadExtraCA yields for it.
func writeRunnerTestCA(t *testing.T) (string, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Runner Test Root"},
		NotBefore: time.Now().AddDate(-1, 0, 0), NotAfter: time.Now().AddDate(1, 0, 0),
		IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "ca.pem")
	data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p, data
}

// fakeSbxOnPath puts a fake `sbx` on PATH that logs each invocation to the
// returned log and answers `sbx ls --json` with ls.
func fakeSbxOnPath(t *testing.T, ls string) string {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "sbx.log")
	bin := t.TempDir()
	script := "#!/bin/sh\necho \"sbx $*\" >> '" + logPath + "'\n" +
		"[ \"$1\" = ls ] && echo '" + ls + "'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "sbx"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir()) // sbx.DefaultLockPath
	return logPath
}

// stubSandboxRun replaces the sandboxRun seam with one that records its argv
// and returns err.
func stubSandboxRun(t *testing.T, err error) *[][]string {
	t.Helper()
	var calls [][]string
	prev := sandboxRun
	sandboxRun = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		if err != nil {
			return []byte("sudo: a password is required"), err
		}
		return []byte("workingman-extra-ca: installed"), nil
	}
	t.Cleanup(func() { sandboxRun = prev })
	return &calls
}

// DefaultSandboxCreator installs the extra CA once per call — after `sbx
// create` and the policies, or on a reused sandbox — and makes no install
// call (the sbx calls are unchanged) when the setting is off.
func TestDefaultSandboxCreatorInstallsExtraCA(t *testing.T) {
	caPath, caPEM := writeRunnerTestCA(t)
	rule := policy.Rule{Action: policy.ActionAllow, Kind: policy.KindNetwork, Resource: "api.github.com"}
	for _, tc := range []struct {
		name, ls string
		sbxLog   string
	}{
		{"create", `{"sandboxes":[]}`, "sbx ls --json\nsbx create claude --name task-x /ws\nsbx policy allow network --sandbox task-x api.github.com\n"},
		{"repair", `{"sandboxes":[{"name":"task-x","workspaces":["/old"]}]}`, "sbx ls --json\nsbx rm --force task-x\nsbx create claude --name task-x /ws\nsbx policy allow network --sandbox task-x api.github.com\n"},
		{"reuse", `{"sandboxes":[{"name":"task-x","workspaces":["/ws"]}]}`, "sbx ls --json\nsbx policy allow network --sandbox task-x api.github.com\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, ca := range []string{"", caPath} {
				logPath := fakeSbxOnPath(t, tc.ls)
				calls := stubSandboxRun(t, nil)
				if err := DefaultSandboxCreator(context.Background(), SandboxSpec{
					Name: "task-x", Workspaces: []string{"/ws"}, Policies: []policy.Rule{rule}, ExtraCACerts: ca,
				}); err != nil {
					t.Fatalf("ca=%q: %v", ca, err)
				}
				if data, _ := os.ReadFile(logPath); string(data) != tc.sbxLog {
					t.Errorf("ca=%q: sbx calls =\n%s\nwant\n%s", ca, data, tc.sbxLog)
				}
				var want [][]string
				if ca != "" {
					want = [][]string{append([]string{"sbx"}, sbx.ExtraCAInstallArgs("task-x", caPEM)...)}
				}
				if !reflect.DeepEqual(*calls, want) {
					t.Errorf("ca=%q: install calls = %q, want %q", ca, *calls, want)
				}
			}
		})
	}
}

// A failed install on create fails DefaultSandboxCreator and DELETEs the
// gateway minted for it.
func TestDefaultSandboxCreatorExtraCAFailureDiscardsGateway(t *testing.T) {
	caPath, _ := writeRunnerTestCA(t)
	gw := newFakeGateway(t)
	fakeSbxOnPath(t, `{"sandboxes":[]}`)
	stubSandboxRun(t, errors.New("exit status 1"))
	store := mcpgateway.Store{Dir: t.TempDir()}
	m := mcpgateway.Manager{Client: mcpgateway.Client{BaseURL: gw.srv.URL}, Store: store, Servers: []string{"github"}}
	err := DefaultSandboxCreator(context.Background(), SandboxSpec{Name: "task-x", Workspaces: []string{"/ws"}, MCPGateway: m, ExtraCACerts: caPath})
	if err == nil || !strings.Contains(err.Error(), `install extra CA (`+caPath+`) into sandbox "task-x"`) ||
		!strings.Contains(err.Error(), "sudo: a password is required") {
		t.Fatalf("err = %v, want the install failure", err)
	}
	if len(gw.deleted) != 1 {
		t.Errorf("deleted = %v, want the minted gateway", gw.deleted)
	}
	if _, ok, _ := store.Load("task-x"); ok {
		t.Error("gateway record left behind")
	}
}

// The `:session` path installs the extra CA after create's policies (before
// the gateway is recorded), and on reuse; off, there is no install exec.
func TestSessionSandboxInstallsExtraCA(t *testing.T) {
	caPath, caPEM := writeRunnerTestCA(t)
	for _, tc := range []struct{ name, ls string }{
		{"create", ""},
		{"reuse", `[{"name":"session-widget","workspaces":["/ws/widget"]}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, ca := range []string{"", caPath} {
				gwSrv, _ := newGatewayServer(t)
				fs := &fakeSbx{ls: tc.ls}
				r := &Runner{
					Launcher: &fakeLauncher{}, SbxPath: "/bin/sbx",
					SessionsRoot: filepath.Join(t.TempDir(), "sessions"), MCPGatewayURL: gwSrv.URL,
					ExtraCACerts: ca, sbxRun: fs.run,
				}
				if _, err := r.LaunchInteractive(context.Background(), InteractiveSpec{
					SandboxName: "session-widget", Workspaces: []string{"/ws/widget"}, Cwd: "/ws/widget", Name: "session-widget",
				}); err != nil {
					t.Fatalf("ca=%q: %v", ca, err)
				}
				var installs [][]string
				lastPolicy, install := -1, -1
				for i, c := range fs.cmds {
					if len(c) > 1 && c[1] == "policy" {
						lastPolicy = i
					}
					if len(c) > 6 && c[1] == "exec" && c[6] == sbx.ExtraCAScript {
						installs = append(installs, c)
						install = i
					}
				}
				if ca == "" {
					if installs != nil {
						t.Errorf("off: install calls %q", installs)
					}
					continue
				}
				want := append([]string{"/bin/sbx"}, sbx.ExtraCAInstallArgs("session-widget", caPEM)...)
				if len(installs) != 1 || !reflect.DeepEqual(installs[0], want) {
					t.Errorf("install calls = %q, want [%q]", installs, want)
				}
				if install < lastPolicy {
					t.Errorf("install (#%d) ran before the gateway policies (#%d): %q", install, lastPolicy, fs.cmds)
				}
			}
		})
	}
}
