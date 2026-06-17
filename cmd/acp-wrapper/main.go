// Command acp-wrapper launches a sandboxed Claude Code agent as an ACP client
// and exposes it to the workingman TUI over a per-session unix socket.
//
// It creates (idempotently) an sbx claude sandbox with the acp-kit ACP bridge
// layered on, execs acp-kit's claude-acp-client entrypoint inside it — which
// speaks the Agent Client Protocol over stdio — and serves a
// <sessions-root>/<session-id>/agent.sock that the TUI connects to. The socket
// bridges the TUI to the sandboxed ACP client's stdio: prompts in, streamed ACP
// responses out.
//
// Usage:
//
//	acp-wrapper \
//	  --session-id <id> \
//	  --kit /path/to/acp-kit \
//	  --workspace /host/path/to/repo
//
// One acp-wrapper process backs one ACP session; the daemon spawns one per
// non-interactive claude session.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/slimslenderslacks/work/internal/acpwrapper"
	"github.com/slimslenderslacks/work/internal/policy"
)

// workspacesFlag collects repeatable --workspace values, resolving each to an
// absolute host path (the sandbox bind-mounts host paths at their native path).
type workspacesFlag []string

func (w *workspacesFlag) String() string { return strings.Join(*w, ",") }
func (w *workspacesFlag) Set(s string) error {
	abs, err := filepath.Abs(s)
	if err != nil {
		return err
	}
	*w = append(*w, abs)
	return nil
}

// staticMCPsFlag collects repeatable --static-mcp values, forwarded verbatim
// as `--static-mcp <name>` to `sbx create`.
type staticMCPsFlag []string

func (m *staticMCPsFlag) String() string { return strings.Join(*m, ",") }
func (m *staticMCPsFlag) Set(s string) error {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	*m = append(*m, s)
	return nil
}

// policiesFlag collects repeatable --policy values. Each value is the
// encoded form "<action>:<type>:<resource>" (see policy.Rule.Encode); we
// decode here so a typo surfaces at flag parse time, not deep inside
// ensureSandbox after `sbx create`.
type policiesFlag []policy.Rule

func (p *policiesFlag) String() string {
	parts := make([]string, len(*p))
	for i, r := range *p {
		parts[i] = r.Encode()
	}
	return strings.Join(parts, ",")
}

func (p *policiesFlag) Set(s string) error {
	r, err := policy.Decode(s)
	if err != nil {
		return err
	}
	*p = append(*p, r)
	return nil
}

func main() {
	fs := flag.NewFlagSet("acp-wrapper", flag.ExitOnError)
	sessionID := fs.String("session-id", "", "unique session id; names the session dir and (by default) the sandbox (required)")
	sessionsRoot := fs.String("sessions-root", "", "root dir holding per-session dirs (default ~/.workingman/sessions)")
	sandboxName := fs.String("sandbox", "", "sbx sandbox name (default acp-<session-id>)")
	kitPath := fs.String("kit", "", "acp-kit reference to layer onto the claude sandbox: a local kit dir or published ref (required)")
	sbxPath := fs.String("sbx", "", "path to the sbx binary (default: sbx on PATH)")
	exitWhenEmpty := fs.Bool("exit-when-empty", false, "shut down once the last connected TUI disconnects (after at least one has connected); used by orch's autonomous single-turn flow")
	var workspaces workspacesFlag
	fs.Var(&workspaces, "workspace", "host path to mount into the sandbox; the first is the ACP client cwd (repeatable, at least one required)")
	var staticMCPs staticMCPsFlag
	fs.Var(&staticMCPs, "static-mcp", "static-MCP name to attach when creating the sandbox; passed verbatim to `sbx create --static-mcp` (repeatable)")
	var policies policiesFlag
	fs.Var(&policies, "policy", "sbx policy rule applied after `sbx create`, encoded as \"<allow|deny>:<network|filesystem>:<resource>\" (repeatable)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}

	cfg := acpwrapper.Config{
		SessionID:     *sessionID,
		SessionsRoot:  *sessionsRoot,
		SandboxName:   *sandboxName,
		KitPath:       *kitPath,
		SbxPath:       *sbxPath,
		Workspaces:    workspaces,
		ExitWhenEmpty: *exitWhenEmpty,
		StaticMCPs:    staticMCPs,
		Policies:      policies,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := acpwrapper.Run(ctx, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "acp-wrapper:", err)
		os.Exit(1)
	}
}
