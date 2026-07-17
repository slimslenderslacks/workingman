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
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/slimslenderslacks/work/internal/acpwrapper"
	"github.com/slimslenderslacks/work/internal/policy"
)

// resolveGitIdentity reads the host user's git identity (the same `git config`
// the user commits under) so the sandboxed agent can commit as them. Returns
// empty strings when git or either value is unavailable — the wrapper then
// omits the env and the sandbox's default identity applies. We deliberately do
// NOT fail hard on a missing identity: a planning/wolf session has no need for
// it, and the daemon shouldn't refuse to run because git isn't configured.
func resolveGitIdentity() (name, email string) {
	get := func(key string) string {
		out, err := exec.Command("git", "config", "--get", key).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	return get("user.name"), get("user.email")
}

// gitSigningConfig is the host's SSH commit-signing configuration, read once so
// the wrapper can both decide whether to inject signing and explain, in the
// session log, why it did or didn't. The three fields are exactly what SSH
// signing requires.
type gitSigningConfig struct {
	signingKey string // user.signingkey
	format     string // gpg.format
	gpgSign    string // commit.gpgsign
}

// readGitSigningConfig reads the host's SSH-signing settings in a way that does
// NOT depend on the wrapper's launch directory. It runs `git config --get` from
// a non-repo directory (a scratch temp dir), so resolution sees the user-level
// scopes — system, ~/.gitconfig, AND the XDG path ~/.config/git/config — but is
// blind to any per-repo local config.
//
// Both properties matter, and each rules out a simpler approach:
//   - Not the plain cwd-relative `git config --get`: that layers in the local
//     config of whatever repo orch was started in, so a repo overriding (or
//     lacking) a signing setting would silently turn signing off — the field
//     failure that produced unsigned commits days later.
//   - Not `--global`: that reads only ~/.gitconfig and misses the XDG global
//     ~/.config/git/config, where signing actually lives on some hosts (this
//     one included) — which would silently disable signing for everyone.
func readGitSigningConfig() gitSigningConfig {
	// Any non-repo dir works; TempDir is guaranteed to exist and (barring an
	// exotic TMPDIR) is not inside a git repo, so no repo-local config leaks in.
	dir := os.TempDir()
	get := func(key string) string {
		cmd := exec.Command("git", "config", "--get", key)
		cmd.Dir = dir
		out, err := cmd.Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	return gitSigningConfig{
		signingKey: get("user.signingkey"),
		format:     get("gpg.format"),
		gpgSign:    get("commit.gpgsign"),
	}
}

// enabled reports whether the host is set up for SSH commit signing: a signing
// key, gpg.format=ssh, and commit.gpgsign=true must ALL be present. We activate
// only for SSH signing — openpgp/GPG needs a keyring the sandbox doesn't have,
// and a user who doesn't sign by default shouldn't have it (which, lacking a
// working signer, would make `git commit` fail) forced on inside the sandbox.
func (c gitSigningConfig) enabled() bool {
	return c.signingKey != "" &&
		strings.EqualFold(c.format, "ssh") &&
		strings.EqualFold(c.gpgSign, "true")
}

// key returns the signing key to inject, or "" when signing isn't fully
// configured (so the wrapper leaves commits unsigned rather than half-configured).
func (c gitSigningConfig) key() string {
	if c.enabled() {
		return c.signingKey
	}
	return ""
}

// logSigningStatus records, in this session's acp-wrapper log, whether the
// sandboxed agent will SSH-sign commits. It exists because a signing drop is
// otherwise silent — `git commit` still succeeds, just unsigned — so the
// symptom (unsigned commits) shows up long after the cause. The line is prefixed
// with the session id so it's greppable per session. The DISABLED case names
// which of the three settings is missing, and flags the specific asymmetry we
// hit in the field: a resolvable identity but unresolved signing.
func logSigningStatus(sessionID string, haveIdentity bool, c gitSigningConfig) {
	if c.enabled() {
		fmt.Fprintf(os.Stderr, "acp-wrapper: session %s: commit signing ENABLED (ssh)\n", sessionID)
		return
	}
	msg := fmt.Sprintf(
		"acp-wrapper: session %s: commit signing DISABLED — commits will be UNSIGNED "+
			"(user.signingkey=%s gpg.format=%q commit.gpgsign=%q, read from global git config)",
		sessionID, presence(c.signingKey), c.format, c.gpgSign)
	if haveIdentity {
		msg += " — note: git identity DID resolve but signing did not"
	}
	fmt.Fprintln(os.Stderr, msg)
}

func presence(s string) string {
	if s == "" {
		return "unset"
	}
	return "set"
}

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

	gitName, gitEmail := resolveGitIdentity()
	signing := readGitSigningConfig()
	logSigningStatus(*sessionID, gitName != "" && gitEmail != "", signing)
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
		GitName:       gitName,
		GitEmail:      gitEmail,
		SigningKey:    signing.key(),
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := acpwrapper.Run(ctx, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "acp-wrapper:", err)
		os.Exit(1)
	}
}
