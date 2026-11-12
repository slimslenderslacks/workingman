package runner

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/sbx"
	"github.com/slimslenderslacks/work/internal/terminal"
)

var errFakeNoKey = errors.New("exit status 1")

// fakeSbx scripts the sbx commands a `:session` launch runs and records them.
type fakeSbx struct {
	ls      string // `sbx ls --json` output
	secrets string // `sbx secret ls --json` output
	resume  bool   // the transcript probe finds an existing conversation
	cmds    [][]string
}

func (f *fakeSbx) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.cmds = append(f.cmds, append([]string{name}, args...))
	switch {
	case len(args) >= 2 && args[0] == "ls":
		if f.ls == "" {
			return []byte("[]"), nil
		}
		return []byte(f.ls), nil
	case len(args) >= 2 && args[0] == "secret":
		if f.secrets == "" {
			return []byte(`{"custom_secrets":[]}`), nil
		}
		return []byte(f.secrets), nil
	case len(args) >= 1 && args[0] == "exec" && strings.Contains(strings.Join(args, " "), ".jsonl"):
		if f.resume {
			return nil, nil
		}
		return nil, errors.New("exit status 2")
	}
	return nil, nil
}

func (f *fakeSbx) find(sub string) []string {
	for _, c := range f.cmds {
		if len(c) > 1 && c[1] == sub {
			return c
		}
	}
	return nil
}

// newGatewayServer answers POST /gateways and records the request bodies.
func newGatewayServer(t *testing.T) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/gateways" {
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			bodies = append(bodies, b)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gw1", "url": "http://localhost:3333/gateways/gw1"})
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed) // GET /gateways/gw1: alive
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

// TestLaunchInteractiveSessionFollowsSbxCreate: a new `:session` sandbox is set
// up the way sbx-create.sh does it — a dynamic gateway with no servers, `sbx
// create` from the acp-kit image and kit with MCP_URL and the claude env, the
// gateway's port allowed — and the tab attaches with `sbx run`, not the ACP
// bridge.
func TestLaunchInteractiveSessionFollowsSbxCreate(t *testing.T) {
	gwSrv, bodies := newGatewayServer(t)
	launcher := &fakeLauncher{}
	fs := &fakeSbx{secrets: `{"custom_secrets":[{"scope":"global","targets":["llm.es.atko.services"]}]}`}
	r := &Runner{
		Launcher:      launcher,
		SbxPath:       "/bin/sbx",
		Kit:           "/kits/acp-kit",
		Image:         "acp:1",
		SessionsRoot:  filepath.Join(t.TempDir(), "sessions"),
		MCPGatewayURL: gwSrv.URL,
		// The task default must not leak into :session.
		MCPGatewayServers: []string{"github"},
		sbxRun:            fs.run,
	}

	target, err := r.LaunchInteractive(context.Background(), InteractiveSpec{
		SandboxName:     "session-widget",
		Workspaces:      []string{"/ws/widget", "/orch/widget"},
		Cwd:             "/ws/widget",
		ClaudeSessionID: "abc",
		Name:            "session-widget",
	})
	if err != nil {
		t.Fatalf("LaunchInteractive: %v", err)
	}
	if target != "session-widget" {
		t.Errorf("target = %q", target)
	}

	if len(*bodies) != 1 {
		t.Fatalf("POST /gateways bodies = %v, want one", *bodies)
	}
	if b := (*bodies)[0]; b["dynamic"] != true || len(b["servers"].([]any)) != 0 {
		t.Errorf("gateway request = %v, want dynamic with no servers", b)
	}

	wantEnv := []string{"-e", "MCP_URL=http://host.docker.internal:3333/gateways/gw1"}
	for _, kv := range sbx.ClaudeEnv {
		wantEnv = append(wantEnv, "-e", kv)
	}
	wantEnv = append(wantEnv, "-e", sbx.GatewayBaseURLEnv)

	create := fs.find("create")
	wantCreate := append([]string{"/bin/sbx", "create", "claude", "--name", "session-widget"}, wantEnv...)
	wantCreate = append(wantCreate, "--kit", "/kits/acp-kit", "-t", "acp:1", "--pull", "missing", "/ws/widget", "/orch/widget")
	if !reflect.DeepEqual(create, wantCreate) {
		t.Errorf("create = %v\nwant     %v", create, wantCreate)
	}
	if pol := fs.find("policy"); pol == nil || !strings.Contains(strings.Join(pol, " "), "session-widget") {
		t.Errorf("no gateway policy applied: %v", fs.cmds)
	}

	wantRun := append([]string{"/bin/sbx", "run", "--name", "session-widget"}, wantEnv...)
	wantRun = append(wantRun, "--", "--session-id", "abc")
	if !reflect.DeepEqual(launcher.last.Command, wantRun) {
		t.Errorf("command = %v\nwant      %v", launcher.last.Command, wantRun)
	}
	if launcher.last.Workspace != "/ws/widget" {
		t.Errorf("tab workspace = %q", launcher.last.Workspace)
	}
	for _, c := range fs.cmds {
		if strings.Contains(strings.Join(c, " "), "claude-acp-client --") || (len(c) > 2 && c[1] == "exec" && c[2] == "-it") {
			t.Errorf("must not go through sbx exec -it / the ACP bridge: %v", c)
		}
	}
}

// Re-opening reuses the sandbox (no create), resumes the conversation, and
// still hands the session MCP_URL.
func TestLaunchInteractiveSessionReusesAndResumes(t *testing.T) {
	gwSrv, _ := newGatewayServer(t)
	launcher := &fakeLauncher{}
	fs := &fakeSbx{ls: `[{"name":"session-widget","workspaces":["/orch/widget","/ws/widget"]}]`, resume: true}
	r := &Runner{
		Launcher:      launcher,
		SessionsRoot:  filepath.Join(t.TempDir(), "sessions"),
		MCPGatewayURL: gwSrv.URL,
		sbxRun:        fs.run,
	}
	if _, err := r.LaunchInteractive(context.Background(), InteractiveSpec{
		SandboxName: "session-widget", Workspaces: []string{"/ws/widget", "/orch/widget"},
		Cwd: "/ws/widget", ClaudeSessionID: "abc", Name: "session-widget",
	}); err != nil {
		t.Fatal(err)
	}
	if c := fs.find("create"); c != nil {
		t.Errorf("reused sandbox was recreated: %v", c)
	}
	if c := fs.find("rm"); c != nil {
		t.Errorf("reused sandbox was removed: %v", c)
	}
	cmd := strings.Join(launcher.last.Command, " ")
	if !strings.HasPrefix(cmd, "sbx run --name session-widget -e MCP_URL=http://host.docker.internal:3333/gateways/gw1") ||
		!strings.HasSuffix(cmd, "-- --resume abc") {
		t.Errorf("command = %s", cmd)
	}
	if strings.Contains(cmd, "ANTHROPIC_BASE_URL") {
		t.Errorf("no sbx secret for the LLM gateway, so no ANTHROPIC_BASE_URL: %s", cmd)
	}
}

// TestLaunchInteractiveHostShell covers the `:dir` path: an empty SandboxName
// runs the command directly on the host — no sandbox ensured, no `sbx exec`
// wrapping — in the given cwd.
func TestLaunchInteractiveHostShell(t *testing.T) {
	launcher := &fakeLauncher{}
	ensured := false
	r := &Runner{
		Launcher: launcher,
		SbxPath:  "/bin/sbx",
		Sandbox:  func(context.Context, SandboxSpec) error { ensured = true; return nil },
	}

	target, err := r.LaunchInteractive(context.Background(), InteractiveSpec{
		Cwd:   "/ws/widget",
		Inner: []string{"bash"},
		Name:  "shell-widget",
	})
	if err != nil {
		t.Fatalf("LaunchInteractive: %v", err)
	}
	if ensured {
		t.Error("host shell must not create a sandbox")
	}
	if want := []string{"bash"}; !reflect.DeepEqual(launcher.last.Command, want) {
		t.Errorf("command = %v, want %v (no sbx wrapping)", launcher.last.Command, want)
	}
	if launcher.last.Workspace != "/ws/widget" {
		t.Errorf("window cwd = %q, want /ws/widget", launcher.last.Workspace)
	}
	if target != "shell-widget" {
		t.Errorf("target = %q, want shell-widget", target)
	}
}

// TestLaunchInteractiveInjectsSigning covers the `:session` fix: with a git
// identity and a signing key set — and a preflight that finds a key in the
// forwarded agent — the wrapped `sbx exec` carries the GIT_AUTHOR_*/COMMITTER_*
// identity env AND the GIT_CONFIG_* signing env, so commits in the window are
// attributed and SSH-signed just like the ACP commit agent.
func TestLaunchInteractiveInjectsSigning(t *testing.T) {
	launcher := &fakeLauncher{}
	fs := &fakeSbx{}
	r := &Runner{
		Launcher: launcher,
		SbxPath:  "/bin/sbx",
		sbxRun:   fs.run,
		// Preflight finds a key → signing stays on.
		preflightRun: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("256 SHA256:abc key (ED25519)\n"), nil
		},
	}

	_, err := r.LaunchInteractive(context.Background(), InteractiveSpec{
		SandboxName: "session-widget",
		Workspaces:  []string{"/ws/widget", "/orch/widget"},
		Cwd:         "/ws/widget",
		Name:        "session-widget",
		GitName:     "Jim Clark",
		GitEmail:    "jim@example.com",
		SigningKey:  "ssh-ed25519 AAAAKEY",
	})
	if err != nil {
		t.Fatalf("LaunchInteractive: %v", err)
	}

	want := []string{"/bin/sbx", "run", "--name", "session-widget"}
	for _, kv := range sbx.ClaudeEnv {
		want = append(want, "-e", kv)
	}
	want = append(want,
		"-e", "GIT_AUTHOR_NAME=Jim Clark",
		"-e", "GIT_AUTHOR_EMAIL=jim@example.com",
		"-e", "GIT_COMMITTER_NAME=Jim Clark",
		"-e", "GIT_COMMITTER_EMAIL=jim@example.com",
		"-e", "GIT_CONFIG_COUNT=4",
		"-e", "GIT_CONFIG_KEY_0=user.signingkey",
		"-e", "GIT_CONFIG_VALUE_0=ssh-ed25519 AAAAKEY",
		"-e", "GIT_CONFIG_KEY_1=gpg.format",
		"-e", "GIT_CONFIG_VALUE_1=ssh",
		"-e", "GIT_CONFIG_KEY_2=gpg.ssh.program",
		"-e", "GIT_CONFIG_VALUE_2=ssh-keygen",
		"-e", "GIT_CONFIG_KEY_3=commit.gpgsign",
		"-e", "GIT_CONFIG_VALUE_3=true",
	)
	if !reflect.DeepEqual(launcher.last.Command, want) {
		t.Errorf("command = %v\nwant %v", launcher.last.Command, want)
	}
}

// TestLaunchInteractiveSigningDegradesWhenAgentEmpty covers the safety valve:
// when the forwarded agent holds no key, forcing commit.gpgsign on would make
// every `git commit` hard-fail, so signing is dropped (no GIT_CONFIG_* env)
// while the git identity is still injected.
func TestLaunchInteractiveSigningDegradesWhenAgentEmpty(t *testing.T) {
	launcher := &fakeLauncher{}
	fs := &fakeSbx{}
	r := &Runner{
		Launcher: launcher,
		SbxPath:  "/bin/sbx",
		sbxRun:   fs.run,
		// `ssh-add -l` exits non-zero when the agent has no identities.
		preflightRun: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("The agent has no identities."), errFakeNoKey
		},
	}

	_, err := r.LaunchInteractive(context.Background(), InteractiveSpec{
		SandboxName: "session-widget",
		Workspaces:  []string{"/ws/widget"},
		Cwd:         "/ws/widget",
		Name:        "session-widget",
		GitName:     "Jim Clark",
		GitEmail:    "jim@example.com",
		SigningKey:  "ssh-ed25519 AAAAKEY",
	})
	if err != nil {
		t.Fatalf("LaunchInteractive: %v", err)
	}

	for _, arg := range launcher.last.Command {
		if strings.HasPrefix(arg, "GIT_CONFIG_") {
			t.Fatalf("signing env %q injected despite an empty agent; command: %v", arg, launcher.last.Command)
		}
	}
	// Identity must still be present — only signing degrades.
	if !containsArg(launcher.last.Command, "GIT_AUTHOR_NAME=Jim Clark") {
		t.Errorf("identity env dropped along with signing; command: %v", launcher.last.Command)
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestLaunchInteractiveValidates(t *testing.T) {
	r := &Runner{Launcher: &fakeLauncher{}, Sandbox: func(context.Context, SandboxSpec) error { return nil }}
	// A sandboxed session (SandboxName set) needs at least one workspace mount.
	if _, err := r.LaunchInteractive(context.Background(), InteractiveSpec{Name: "w", SandboxName: "s"}); err == nil {
		t.Error("expected an error with no workspaces for a sandboxed session")
	}
	// Every launch needs a command to run.
	if _, err := r.LaunchInteractive(context.Background(), InteractiveSpec{Name: "w", Cwd: "/ws"}); err == nil {
		t.Error("expected an error with no inner command")
	}
	noLauncher := &Runner{Sandbox: func(context.Context, SandboxSpec) error { return nil }}
	if _, err := noLauncher.LaunchInteractive(context.Background(), InteractiveSpec{Name: "w", Cwd: "/ws", Inner: []string{"bash"}}); err == nil {
		t.Error("expected an error with no launcher")
	}
}

// TestLaunchInteractivePassesTitle: the tab is titled after the work stream.
func TestLaunchInteractivePassesTitle(t *testing.T) {
	launcher := &fakeLauncher{}
	r := &Runner{Launcher: launcher}
	if _, err := r.LaunchInteractive(context.Background(), InteractiveSpec{
		Cwd:   "/ws/widget",
		Inner: []string{"bash"},
		Name:  "shell-widget",
		Title: "widget",
	}); err != nil {
		t.Fatalf("LaunchInteractive: %v", err)
	}
	if launcher.last.Name != "shell-widget" || launcher.last.Title != "widget" {
		t.Errorf("spec name/title = %q/%q, want shell-widget/widget", launcher.last.Name, launcher.last.Title)
	}
}

// fakeTabOpener is a launcher that reports the session's tab is already open,
// the way terminal.TabLauncher.Open does.
type fakeTabOpener struct {
	fakeLauncher
	opened []agent.Spec
}

func (f *fakeTabOpener) Open(_ context.Context, spec agent.Spec) (*terminal.TabSession, error) {
	f.opened = append(f.opened, spec)
	return nil, &terminal.AlreadyOpenError{Name: spec.Name, Title: spec.Title}
}

// TestLaunchInteractiveReportsAlreadyOpen: when the launcher focused an
// already-open tab instead of opening a new one, LaunchInteractive passes the
// *terminal.AlreadyOpenError through unwrapped (so the TUI can warn) and still
// ensures nothing else was launched.
func TestLaunchInteractiveReportsAlreadyOpen(t *testing.T) {
	opener := &fakeTabOpener{}
	r := &Runner{Launcher: opener}
	_, err := r.LaunchInteractive(context.Background(), InteractiveSpec{
		Cwd:   "/ws/widget",
		Inner: []string{"bash"},
		Name:  "shell-widget",
		Title: "widget",
	})
	var already *terminal.AlreadyOpenError
	if !errors.As(err, &already) || already.Title != "widget" {
		t.Fatalf("err = %v, want an AlreadyOpenError for widget", err)
	}
	if len(opener.opened) != 1 || opener.last.Name != "" {
		t.Errorf("Open calls = %v, Launch spec = %+v; want exactly one Open and no Launch", opener.opened, opener.last)
	}
}
