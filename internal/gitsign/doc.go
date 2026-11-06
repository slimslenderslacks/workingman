// Package gitsign makes a sandboxed agent commit as the host user, SSH-sign
// those commits, and check that the SSH agent forwarded into the sandbox can
// sign and push.
//
// Role: shared (daemon/runner and acp-wrapper)
//
// Two callers need the same setup: the ACP commit path (internal/acpwrapper)
// and the interactive `:session` path (internal/runner, driven by the daemon).
// Both need the host git identity ([Identity]), the host's SSH signing config
// ([Resolve], read from user-level git config only, never from the launch
// directory's repo), and checks run once the sandbox exists ([Preflight] for
// a signing key, [PushPreflight] for ssh push access to github.com).
//
// The setup is passed as `sbx exec -e` flags rather than written into a
// .git/config: [IdentityEnvArgs] sets GIT_AUTHOR_* and GIT_COMMITTER_*, and
// [SigningEnvArgs] sets GIT_CONFIG_COUNT/KEY_n/VALUE_n with gpg.ssh.program
// forced to ssh-keygen. SSH_AUTH_SOCK is deliberately left alone, so signing
// and push both use the agent sbx exposes at /run/ssh-agent.sock.
//
// # Key types
//
//   - [Config] — the host's signing settings. [Config.Enabled] is true only
//     for complete SSH signing, and [Config.Key] is the key to inject or "".
//   - [RunFunc] — the exec seam the preflights call; [ExecRun] is the
//     production implementation and tests pass a fake.
//   - [PushResult] — the PushPreflight result, with an actionable Reason on
//     failure.
//
// # Collaborators
//
// No internal imports. Used by internal/acpwrapper and cmd/acp-wrapper
// (identity, signing and both preflights for ACP sessions), internal/runner
// (identity, signing and Preflight for interactive sessions) and
// internal/daemon (resolves identity and signing key, and allows [PushHost]
// in the network policy of pushing agents). See docs/ssh-agent.md for how
// the agent socket reaches the sandbox and why push can fail.
package gitsign
