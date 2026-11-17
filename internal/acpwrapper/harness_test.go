package acpwrapper

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeKit(t *testing.T, spec string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "spec.yaml"), []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func harnessConfig(t *testing.T, kit string) Config {
	return Config{
		SessionID:          "s",
		SessionsRoot:       filepath.Join(t.TempDir(), "sessions"),
		SandboxName:        "workingman-agent",
		KitPath:            kit,
		TemplateImage:      "slimslenderslacks/claude-code-acp:0.90.0",
		SbxPath:            "sbx",
		Workspaces:         []string{"/repo"},
		RequireOktaGateway: true,
	}
}

func subcommands(calls [][]string) []string {
	var out []string
	for _, c := range calls {
		if len(c) > 1 {
			out = append(out, c[1])
		}
	}
	return out
}

// A sandbox this wrapper created and recorded is reused while the kit and
// image are unchanged.
func TestEnsureSandboxReusesRecordedHarness(t *testing.T) {
	c := harnessConfig(t, writeKit(t, "v1"))
	c.saveHarness(c.SandboxName)
	f := &fakeSbx{lsOutput: `[{"name":"workingman-agent","workspaces":["/repo"]}]`}
	if _, err := ensureSandbox(context.Background(), f.run, c); err != nil {
		t.Fatalf("ensureSandbox: %v", err)
	}
	if got := subcommands(f.calls); strings.Join(got, ",") != "ls" {
		t.Errorf("sbx calls = %v, want only ls (reuse)", got)
	}
}

// The persistent workingman sandbox is recreated when it was built from a
// different kit or image, or when there is no record of what it was built
// from: a reused sandbox keeps its creating kit's claude-acp-client, which is
// how an old kit's mcp-gateway outlives the fix.
func TestEnsureSandboxRecreatesOnHarnessDrift(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, c *Config)
	}{
		{name: "no record"},
		{name: "kit path changed", mutate: func(t *testing.T, c *Config) {
			c.saveHarness(c.SandboxName)
			c.KitPath = writeKit(t, "v1")
		}},
		{name: "kit contents changed", mutate: func(t *testing.T, c *Config) {
			c.saveHarness(c.SandboxName)
			if err := os.WriteFile(filepath.Join(c.KitPath, "spec.yaml"), []byte("v2"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "image changed", mutate: func(t *testing.T, c *Config) {
			c.TemplateImage = "slimslenderslacks/claude-code-acp:0.89.0"
			c.saveHarness(c.SandboxName)
			c.TemplateImage = "slimslenderslacks/claude-code-acp:0.90.0"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := harnessConfig(t, writeKit(t, "v1"))
			if tc.mutate != nil {
				tc.mutate(t, &c)
			}
			f := &fakeSbx{lsOutput: `[{"name":"workingman-agent","workspaces":["/repo"]}]`}
			if _, err := ensureSandbox(context.Background(), f.run, c); err != nil {
				t.Fatalf("ensureSandbox: %v", err)
			}
			if got := subcommands(f.calls); strings.Join(got, ",") != "ls,rm,create" {
				t.Errorf("sbx calls = %v, want ls, rm, create", got)
			}
			if drift := c.harnessDrift(c.SandboxName); drift != "" {
				t.Errorf("after recreate harnessDrift = %q, want none (create must record the harness)", drift)
			}
		})
	}
}

// Without RequireOktaGateway an unrecorded sandbox is reused as before.
func TestEnsureSandboxIgnoresHarnessWhenNotRequired(t *testing.T) {
	c := harnessConfig(t, writeKit(t, "v1"))
	c.RequireOktaGateway = false
	f := &fakeSbx{lsOutput: `[{"name":"workingman-agent","workspaces":["/repo"]}]`}
	if _, err := ensureSandbox(context.Background(), f.run, c); err != nil {
		t.Fatalf("ensureSandbox: %v", err)
	}
	if got := subcommands(f.calls); strings.Join(got, ",") != "ls" {
		t.Errorf("sbx calls = %v, want only ls", got)
	}
}

// preflightSbx simulates `sbx exec` inside a sandbox for oktaGatewayPreflight.
type preflightSbx struct {
	calls        [][]string
	staleClient  bool
	oktaURL      string
	mcpGatewayOn bool
}

func (f *preflightSbx) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	joined := strings.Join(args, " ")
	switch {
	case args[0] == "rm":
		return nil, nil
	case strings.Contains(joined, "grep -q "+entrypointMarker):
		if f.staleClient {
			return nil, errors.New("exit status 1")
		}
	case strings.Contains(joined, "mcp get okta-gateway"):
		if f.oktaURL == "" {
			return []byte("No MCP server found"), errors.New("exit status 1")
		}
		return []byte("okta-gateway:\n  URL: " + f.oktaURL + "\n"), nil
	case strings.Contains(joined, "mcp get mcp-gateway"):
		if !f.mcpGatewayOn {
			return nil, errors.New("exit status 1")
		}
	}
	return nil, nil
}

func TestRequireOktaGateway(t *testing.T) {
	const url = "http://host.docker.internal:3333/gateways/abc"
	for _, tc := range []struct {
		name    string
		mcpURL  string
		sbx     preflightSbx
		wantErr bool
		wantRm  bool
	}{
		{name: "ok", mcpURL: url, sbx: preflightSbx{oktaURL: url}},
		{name: "no MCP_URL fails without removing", sbx: preflightSbx{oktaURL: url}, wantErr: true},
		{name: "stale claude-acp-client recreates", mcpURL: url, sbx: preflightSbx{staleClient: true}, wantErr: true, wantRm: true},
		{name: "okta-gateway not registered recreates", mcpURL: url, sbx: preflightSbx{}, wantErr: true, wantRm: true},
		{name: "okta-gateway at another URL recreates", mcpURL: url, sbx: preflightSbx{oktaURL: "http://old"}, wantErr: true, wantRm: true},
		{name: "project-scope mcp-gateway only warns", mcpURL: url, sbx: preflightSbx{oktaURL: url, mcpGatewayOn: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := harnessConfig(t, writeKit(t, "v1"))
			c.mcpURL = tc.mcpURL
			f := tc.sbx
			err := requireOktaGateway(context.Background(), f.run, c)
			if (err != nil) != tc.wantErr {
				t.Fatalf("requireOktaGateway err = %v, wantErr %v", err, tc.wantErr)
			}
			rm := false
			for _, call := range f.calls {
				if call[1] == "rm" {
					rm = true
				}
				if call[1] == "exec" && !strings.Contains(strings.Join(call, " "), "-w /repo") {
					t.Errorf("preflight exec must run in the agent's cwd: %v", call)
				}
			}
			if rm != tc.wantRm {
				t.Errorf("sandbox removed = %v, want %v: %v", rm, tc.wantRm, f.calls)
			}
		})
	}
}

// The register-only run gets MCP_URL itself: `sbx exec` does not inherit it.
func TestOktaGatewayPreflightPassesMCPURL(t *testing.T) {
	const url = "http://host.docker.internal:3333/gateways/abc"
	c := harnessConfig(t, writeKit(t, "v1"))
	c.mcpURL = url
	f := &preflightSbx{oktaURL: url}
	if problem, _ := oktaGatewayPreflight(context.Background(), f.run, c); problem != "" {
		t.Fatalf("preflight: %s", problem)
	}
	found := false
	for _, call := range f.calls {
		j := strings.Join(call, " ")
		if strings.HasSuffix(j, "claude-acp-client") {
			found = strings.Contains(j, "-e MCP_URL="+url) && strings.Contains(j, "-e CLAUDE_ACP_REGISTER_ONLY=1")
		}
	}
	if !found {
		t.Errorf("register-only call missing MCP_URL / CLAUDE_ACP_REGISTER_ONLY: %v", f.calls)
	}
}

func TestAbsKit(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "acp-kit"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	got := AbsKit("acp-kit")
	if !filepath.IsAbs(got) || filepath.Base(got) != "acp-kit" {
		t.Errorf("AbsKit(acp-kit) = %q, want an absolute path to it", got)
	}
	if got := AbsKit("ghcr.io/x/acp-kit:1"); got != "ghcr.io/x/acp-kit:1" {
		t.Errorf("published ref changed: %q", got)
	}
}
