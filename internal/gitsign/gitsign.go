// Package gitsign is the single source of truth for teaching a sandboxed agent
// to commit as the host user and to SSH-sign those commits. Both the ACP commit
// path (internal/acpwrapper) and the interactive `:session` path (the daemon's
// runner) need the exact same three things — the host git identity, the SSH
// signing config, and a runtime preflight that the forwarded agent actually
// holds a key — so they live here rather than being copied into each caller.
//
// The env injection is delivered as `sbx exec -e` flags: git identity as
// GIT_AUTHOR_*/GIT_COMMITTER_*, and signing as GIT_CONFIG_COUNT/KEY_n/VALUE_n
// so it applies to every git invocation without writing a .git/config.
package gitsign

import (
	"context"
	"os"
	"os/exec"
	"strings"
)

// Identity reads the host user's git identity (the same `git config` the user
// commits under) so the sandboxed agent can commit as them. Returns empty
// strings when git or either value is unavailable — callers then omit the env
// and the sandbox's default identity applies. We deliberately do NOT fail hard
// on a missing identity: a planning/wolf/session window has no need for it, and
// nothing should refuse to run because git isn't configured.
func Identity() (name, email string) {
	get := func(key string) string {
		out, err := exec.Command("git", "config", "--get", key).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	return get("user.name"), get("user.email")
}

// Config is the host's SSH commit-signing configuration, read once so callers
// can both decide whether to inject signing and explain, in their logs, why
// they did or didn't. The three fields are exactly what SSH signing requires.
type Config struct {
	SigningKey string // user.signingkey
	Format     string // gpg.format
	GpgSign    string // commit.gpgsign
}

// Resolve reads the host's SSH-signing settings in a way that does NOT depend on
// the caller's launch directory. It runs `git config --get` from a non-repo
// directory (a scratch temp dir), so resolution sees the user-level scopes —
// system, ~/.gitconfig, AND the XDG path ~/.config/git/config — but is blind to
// any per-repo local config.
//
// Both properties matter, and each rules out a simpler approach:
//   - Not the plain cwd-relative `git config --get`: that layers in the local
//     config of whatever repo the process was started in, so a repo overriding
//     (or lacking) a signing setting would silently turn signing off.
//   - Not `--global`: that reads only ~/.gitconfig and misses the XDG global
//     ~/.config/git/config, where signing actually lives on some hosts — which
//     would silently disable signing for everyone using that layout.
func Resolve() Config {
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
	return Config{
		SigningKey: get("user.signingkey"),
		Format:     get("gpg.format"),
		GpgSign:    get("commit.gpgsign"),
	}
}

// Enabled reports whether the host is set up for SSH commit signing: a signing
// key, gpg.format=ssh, and commit.gpgsign=true must ALL be present. We activate
// only for SSH signing — openpgp/GPG needs a keyring the sandbox doesn't have,
// and a user who doesn't sign by default shouldn't have it forced on inside the
// sandbox (which, lacking a working signer, would make `git commit` fail).
func (c Config) Enabled() bool {
	return c.SigningKey != "" &&
		strings.EqualFold(c.Format, "ssh") &&
		strings.EqualFold(c.GpgSign, "true")
}

// Key returns the signing key to inject, or "" when signing isn't fully
// configured (so callers leave commits unsigned rather than half-configured).
func (c Config) Key() string {
	if c.Enabled() {
		return c.SigningKey
	}
	return ""
}

// IdentityEnvArgs returns the `sbx exec -e` flags that make git commit as the
// host user — author AND committer. Passing the identity as environment
// overrides any git config (including the base image's baked identity) for every
// git command in the session. Both name and email must be present: a half-set
// identity is worse than the sandbox default, so an empty either returns nil.
func IdentityEnvArgs(name, email string) []string {
	if name == "" || email == "" {
		return nil
	}
	return []string{
		"-e", "GIT_AUTHOR_NAME=" + name,
		"-e", "GIT_AUTHOR_EMAIL=" + email,
		"-e", "GIT_COMMITTER_NAME=" + name,
		"-e", "GIT_COMMITTER_EMAIL=" + email,
	}
}

// SigningEnvArgs returns the `sbx exec -e` flags that make git SSH-sign commits.
// Delivered via GIT_CONFIG_COUNT/KEY_n/VALUE_n so it applies to every git
// invocation without writing a .git/config. gpg.ssh.program is forced to
// ssh-keygen: the host's configured signer may be a macOS/1Password binary
// (op-ssh-sign) absent in the Linux sandbox, so signing must run through
// ssh-keygen against the SSH agent sbx exposes inside the sandbox
// (SSH_AUTH_SOCK=/run/ssh-agent.sock, which callers deliberately leave
// untouched). An empty signingKey returns nil (signing off).
func SigningEnvArgs(signingKey string) []string {
	if signingKey == "" {
		return nil
	}
	return []string{
		"-e", "GIT_CONFIG_COUNT=4",
		"-e", "GIT_CONFIG_KEY_0=user.signingkey",
		"-e", "GIT_CONFIG_VALUE_0=" + signingKey,
		"-e", "GIT_CONFIG_KEY_1=gpg.format",
		"-e", "GIT_CONFIG_VALUE_1=ssh",
		"-e", "GIT_CONFIG_KEY_2=gpg.ssh.program",
		"-e", "GIT_CONFIG_VALUE_2=ssh-keygen",
		"-e", "GIT_CONFIG_KEY_3=commit.gpgsign",
		"-e", "GIT_CONFIG_VALUE_3=true",
	}
}

// RunFunc runs a command and returns its combined/stdout output, matching the
// exec seam callers already use. It exists so Preflight can be unit-tested
// without shelling out to a real sbx.
type RunFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRun is the production RunFunc: it runs the command for real and returns
// its stdout. `ssh-add -l` writes the fingerprint to stdout, so stdout is what
// Preflight inspects.
func ExecRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// Preflight verifies, once the sandbox exists, that the SSH agent sbx forwards
// into it actually holds a key — the runtime precondition ssh-keygen needs to
// sign. Config resolution proves signing is *configured*; this proves the key is
// *reachable*, a separate, runtime-varying fact: 1Password can be locked, or
// sandboxd can have been restarted since orch launched and lost the 1Password
// agent. Without this the gap only surfaces at commit time as a signing failure
// — and worse, with commit.gpgsign forced on, `git commit` hard-fails rather
// than landing an unsigned commit.
//
// Returns checked=false when signing isn't configured (nothing to verify), and
// otherwise ok=true iff the forwarded agent lists at least one identity.
// `ssh-add -l` exits non-zero (so run returns an error) when the agent has no
// identities or is unreachable, so a good result is a nil error whose output
// carries a key fingerprint.
func Preflight(ctx context.Context, run RunFunc, sbxPath, sandbox, signingKey string) (checked, ok bool) {
	if signingKey == "" {
		return false, false
	}
	if sbxPath == "" {
		sbxPath = "sbx"
	}
	out, err := run(ctx, sbxPath, "exec", sandbox, "--", "ssh-add", "-l")
	return true, err == nil && strings.Contains(string(out), "SHA256:")
}
