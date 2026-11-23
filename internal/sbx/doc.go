// Package sbx holds the shared helpers for the sbx sandbox CLI: sandbox-name
// rules, the robust runner for sbx's JSON-output commands, and the
// `sbx ls --json` existence check.
//
// Role: shared (daemon, runner, TUI, prompts and acp-wrapper)
//
// Naming: a sandbox name becomes a Docker container name, so sbx rejects
// anything over [MaxNameLen] (63) characters and any underscore. Every
// producer of a sandbox name goes through [SanitizeName], so the name we ask
// for, the name we log and the name sbx registers are the same string.
// [FitNameTo] shortens deterministically (kept prefix plus a 7-hex digest of
// the full name), so a task maps to the same sandbox across daemon restarts
// and long names that share a prefix stay distinct. [NameBudget] says how
// much room a "<prefix>-<suffix>" name leaves for the suffix. The task
// package and the planning prompts use it to limit task names.
//
// JSON calls: [Client] runs every sbx command whose stdout is parsed as JSON
// (`sbx ls --json` via [Client.List], `sbx secret ls --json` via
// [Client.SecretList]/[Client.GatewayConfigured]). It decodes stdout only
// (stderr is quoted in errors), skipping `WARN: ...` lines and JSON log lines
// before and after the document and decoding just the first value of the
// expected shape, so sbx's docker hub refresh warnings under concurrent
// starts never read as the payload or as trailing garbage. A non-zero exit or
// undecodable output is retried with jittered exponential backoff (5
// attempts from 200ms), and each invocation, never a backoff sleep, holds a
// host-wide advisory flock: [LockPath] beside a sessions root, or
// [DefaultLockPath] (~/.workingman/locks/sbx.lock, the default sessions
// root's) for callers without one. A lock that can't be had is logged and
// skipped. Exhaustion names the command (`sbx ls --json failed after 5
// attempts: ...`).
//
// Liveness: [Alive] runs `sbx ls --json` through a [Client] and reports
// whether a named sandbox exists. A non-nil error means the check was
// inconclusive, not that the sandbox is gone. [DecodeList] accepts both the
// old {"sandboxes": [...]} and the newer bare-array output.
//
// LLM gateway secret: [Client.GatewayConfigured] tells acp-wrapper and
// `:session` launches whether to set ANTHROPIC_BASE_URL ([DecodeSecretList]
// finds the secrets document). [GatewaySecretScopes] is the single-attempt,
// unlocked form.
//
// # Key types
//
//   - [Sandbox] — one `sbx ls --json` entry (name and workspaces).
//   - [Client] — the retrying, serialized runner for sbx JSON commands.
//   - [Alive] — existence check used by the TUI's ACP watcher and the
//     daemon's session reconciliation.
//   - [SanitizeName], [FitNameTo], [NameBudget], [CheckName] — the naming
//     rules.
//
// # Collaborators
//
// No internal imports. Used by internal/task (task-name budget),
// internal/prompts (quotes that budget to agents), internal/runner and
// internal/acpwrapper (sandbox names), internal/daemon (reconciling sessions
// against live sandboxes) and internal/tui (ACP watcher). Most other sbx calls
// are still made directly by those callers; docs/sandbox-provider-design.md
// proposes moving them all behind one sandbox.Provider interface.
package sbx
