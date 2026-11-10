# Sandbox provider interface — design (for review)

Status: **proposal, not implemented.** No Go code has changed. This document
is meant to be reviewed before work starts.

## 1. Goal

workingman calls the `sbx` CLI directly from about a dozen places. Some of
those calls go through a wrapper and some don't, and the CLI's argv spelling
leaks into packages that otherwise have nothing to do with sandboxes (for
example, `gitsign` returns `-e` flags). The goal is a small Go interface,
**`sandbox.Provider`**, that:

1. puts every sandbox operation behind one seam, so `sbx` becomes *one
   implementation* rather than an assumption spread across the code;
2. lets another backend replace sbx: plain Docker, a no-isolation "local"
   provider for development, or a future VM tool;
3. makes the backend's missing features explicit (capabilities) instead of
   silent, so a backend that cannot enforce a deny rule fails closed;
4. lets the daemon, runner, wrapper and TUI tests use one in-memory fake
   instead of today's three ad-hoc seams (`commandFunc`, `gitsign.RunFunc`,
   `sandboxProbe`/`reconciledSandboxProbe`).

Non-goals: changing *what* sandboxes workingman creates (names, mounts,
lifetimes) or the ACP or terminal-tab launch mechanics. `agent.Launcher`
(process/tab) stays orthogonal: a provider produces argv, and launchers run
argv.

---

## 2. Inventory of sbx touchpoints

### 2.1 sbx subcommands in use

| Subcommand | Flags / shape | Who | Parsed output | Failure handling |
|---|---|---|---|---|
| `sbx ls --json` | — | `internal/sbx.Alive`, `runner.readSandboxWorkspaces`, `acpwrapper.listSandboxes` | `sbx.DecodeList`: either `[{name, workspaces[]}]` (v0.50+) or `{"sandboxes":[...]}` (older) | `Alive` returns a **tri-state**: `(false, nil)` means definitely gone, and `err != nil` means inconclusive and must never be read as gone. Ensure paths wrap the error and abort the launch. |
| `sbx create claude` | `--name <n>`, `--kit <kit>` (ACP only), `-t <image> --pull missing` (when `--acp-image` is set), then positional workspaces (no `--static-mcp` any more: MCP servers go through the per-sandbox gateway, see `internal/mcpgateway`), where the first is primary and a `<path>:ro` suffix means read-only | `acpwrapper.createSandbox`, `runner.DefaultSandboxCreator` (no kit/template), `check-signing.sh` (probe sandbox) | none (CombinedOutput is captured for error text) | The error is wrapped with the trimmed output and the launch fails. The wrapper logs the create duration to stderr. |
| `sbx rm --force <n>` | — | `acpwrapper.ensureSandbox` (mount drift), `acpwrapper.removeSandboxOnExit`, `runner.DefaultSandboxCreator` (drift), `check-signing.sh` cleanup | none | Drift path: a fatal error. Exit path: best-effort, logged and never returned. Script: `|| true`. |
| `sbx policy <allow\|deny> <network\|filesystem> --sandbox <n> <resource>` | one invocation per rule, in declaration order, after create and before any exec | `policy.Rule.CLIArgs` (used by `acpwrapper.createSandbox` and `runner.DefaultSandboxCreator`), `check-signing.sh probe_push` | none | Validated first (`Rule.Validate`), then a failure is fatal to create. Rules are **not** re-applied to a reused sandbox. |
| `sbx exec` (ACP) | `-e K=V`… `-w <primary-ws> <n> -- claude-acp-client`, **no `-it`** (raw stdio JSON-RPC) | `acpwrapper.Run` / `Config.execArgs` | stdout and stdin are the ACP stream, stderr goes to the wrapper log | A start failure is fatal. An exit makes the wrapper tear down. |
| `sbx exec -it` (tab) | `-it -w <cwd> <n> <claude …>` | `runner.Start` legacy path (no `--acp-kit`) | n/a (runs in a terminal tab) | n/a |
| `sbx exec -it` (`:session`) | `-it -w <cwd> -e GIT_*… <n> <bash -c …>` | `runner.LaunchInteractive` | n/a | n/a. Relies on **exec auto-starting a stopped sandbox**. |
| `sbx exec <n> -- ssh-add -l` | — | `gitsign.Preflight`, `gitsign.PushPreflight`, `check-signing.sh` | looks for `SHA256:` in stdout | A missing key degrades signing to unsigned (`SigningBroken` in session.json). For push it is fatal (`PushBroken`). |
| `sbx exec <n> -- sh -c 'ssh -T … git@github.com 2>&1'` | — | `gitsign.PushPreflight`, `check-signing.sh` | text classification: "successfully authenticated", "permission denied", "host key verification failed", or unreachable | Fatal for pushing sessions, with an actionable reason that names `sbx policy log` / `sbx policy allow network`. |
| `sbx secret ls --json` | — | `acpwrapper.gatewaySecretConfigured` | `custom_secrets[].{scope, targets[]}`. Matches when scope is `global` or the sandbox name and `targets` contains `llm.es.atko.services` | An error is logged as a WARNING and the gateway is not used (fail-open to the default API). |
| `sbx daemon stop` / `sbx daemon start -d` | env `SSH_AUTH_SOCK=<private agent>` | `start.sh`, `check-signing.sh restart_sandboxd` | none | `start.sh`: a failed start is fatal. Stop is `|| true`. |
| `sbx template load` / `sbx template ls` | — | **manual**, documented in `start.sh` comments and `docs/mcp-gateway.md` (`ACP_IMAGE` default `slimslenderslacks/claude-code-acp:0.89.0`) | — | — |
| `sbx mcp ls` / `sbx mcp auth` | — | **text only**: `daemon/review.go` block reason, `wolf.tmpl` | — | — |
| `sbx policy log` / `sbx policy allow network` | — | **text only**: `gitsign.classifyPushProbe` reason, `check-signing.sh` messages | — | — |

### 2.2 Per-package touchpoints

- **`internal/sbx/sbx.go`**: `Sandbox{Name, Workspaces}`, `DecodeList`, and
  `Alive(ctx, name)`. Hard-codes the `"sbx"` binary and ignores `--sbx`.
- **`internal/sbx/name.go`**: the naming rules sbx imposes, which come from
  Docker container names (RFC 1123 labels): `MaxNameLen = 63`, no underscores,
  `SanitizeName`, `FitName[To]` (deterministic `<prefix>-<7 hex sha256>`
  shortening), `NameBudget(prefix)`, `CheckName`. Consumers:
  `runner.SandboxNameFor`/`ACPSandboxNameFor`, `acpwrapper.normalize`,
  `daemon.sanitizeSandboxName` (`:session`), `task.MaxNameLen`/`MaxNameLenFor`/
  slug fitting, `taskgraph` (dedupe suffix), and `prompts` (`nameBudget`
  template func, which tells planning/review agents the name budget).
- **`internal/runner/runner.go`**:
  - `SandboxSpec{Name, Workspaces, Policies, MCPGateway}` and the
    `SandboxCreator` func type, which is already a proto-interface for "ensure".
  - `DefaultSandboxCreator`: ls, then compare the workspace set ignoring order,
    then `rm --force` on drift, then create (no kit), then apply policies.
    Hard-codes `"sbx"`.
  - The legacy tab path in `Start`: wraps the command as `sbx exec -it -w`.
  - `LaunchInteractive`: ensure, then signing preflight, then
    `sbx exec -it -w … -e …`. Uses `Runner.SbxPath`, but its ensure fallback
    (`DefaultSandboxCreator`) does not.
  - `startACP`/`acpWrapperCommand`: no direct sbx call. It forwards the
    sandbox name, workspaces, `--mcp-gateway-url`/`--mcp-server`, `--policy`, `--template`,
    `--sbx`, and `--save-sandbox` to acp-wrapper. It also writes the initial
    `session.json` with `SandboxName`.
  - Names: `SandboxNameFor`, `ACPSandboxNameFor`, `WorkingmanAgentSandbox`.
    Mount shapes: `sandboxWorkspaces`, `workingmanWorkspaces`, and
    `ReadOnlySuffix = ":ro"`.
  - `preflightRun gitsign.RunFunc`, the test seam.
- **`internal/acpwrapper/acpwrapper.go`**: the heaviest user, and the only
  one that layers the kit and template on.
  - `commandFunc`/`execCommand`, the single seam for sbx in the wrapper.
  - `ensureSandbox`, `createSandbox`, `listSandboxes`, `sameWorkspaceSet`, and
    `SplitMount`/`JoinMount` (`:ro`).
  - `gatewaySecretConfigured` (`sbx secret ls`), which drives
    `ANTHROPIC_BASE_URL`.
  - `ensureMCPGateway` / `sweepMCPGateways` (via `internal/mcpgateway`):
    the per-sandbox standalone-codemode gateway → `MCP_URL`, plus one
    `sbx exec <n> -- sh -c …` that records it in
    `/etc/sandbox-persistent.sh`.
  - `execArgs`: env (`MCP_URL`, claude model aliases, gateway, model, git identity,
    `.gomodcache` → `GOMODCACHE`/`GOPROXY=off`, signing), `-w`, the sandbox
    name, and `-- claude-acp-client`.
  - `signingPreflight` / `pushPreflightError` (via `gitsign`).
  - `removeSandboxOnExit`: keeps the sandbox on a signal shutdown, on
    `SaveSandbox`, or when the task status is failed/blocked/running.
    Otherwise it runs `rm --force`.
  - **Implicit sbx contracts** it relies on: mounts appear **at the same
    absolute path** inside the sandbox; the SSH agent is exposed at
    `/run/ssh-agent.sock` (proxied from sandboxd's `SSH_AUTH_SOCK`); network
    is **default-deny**; the credential proxy swaps placeholder tokens for
    hosts with a secret.
- **`cmd/acp-wrapper/main.go`**: flags `--sandbox`, `--kit`, `--template`,
  `--sbx`, `--mcp-gateway-url`/`--mcp-server`/`--mcp-gateway-state`,
  `--policy` (`policy.Encode`), `--workspace` (`:ro`), and `--save-sandbox`.
  `--static-mcp` is still accepted but ignored.
- **`cmd/orch/main.go`**: `--acp-kit`, `--acp-image` (documented as
  `sbx create -t`), and `--wolf-host`. Without `--acp-kit` it wires
  `r.Sandbox = runner.DefaultSandboxCreator`.
- **`internal/gitsign/gitsign.go`**: `IdentityEnvArgs` and `SigningEnvArgs`
  return **`sbx exec -e` argv fragments**. `Preflight` and `PushPreflight`
  build `sbx exec <n> -- …` themselves from `sbxPath`. The probe-failure
  reason text names sbx. `PushHost = "github.com"`.
- **`internal/policy/policy.go`**: `Rule{Action, Kind, Resource}` is a
  backend-neutral data model, which is good. `CLIArgs(sandbox)` is
  sbx-specific. `Encode`/`Decode` is the acp-wrapper flag wire format.
- **`internal/daemon`**:
  - `dispatch_lifecycle.go`: `archivePolicies()` (allow network
    `github.com`). The commit agent reuses the task's sandbox name, MCPs and
    policies. The comment states the assumption that "create is skipped when
    it exists".
  - `review.go`: `StaticMCPs: ["github"]`, `reviewNetworkPolicies()`
    (`api.github.com`, `api.githubcopilot.com`), and the block reason text
    that mentions `sbx mcp ls/auth`.
  - `workingman_agent.go`: `Policies` config, `SandboxName` in
    `WorkingmanAgentSession`, and a **read-only mount** dependency (the
    security property of the observer agent).
  - `reconcile.go`: `reconciledSandboxProbe` defaults to `sbx.Alive`. It
    adopts sessions from disk and ends them when the sandbox is confirmed gone.
  - `sessions.go`: `SessionInfo.SandboxName`, which matches the name in
    `sbx ls`.
  - `interactive.go`: `:session` uses the sandbox `session-<work-stream>` and
    mounts the worktree and the orch dir. `:dir` runs on the host.
- **`internal/session/session.go`**: `SandboxName`, `SandboxID`
  ("sbx's own identifier", currently unused), `SigningBroken`, `PushBroken`.
- **`internal/task/task.go`**: `StaticMCPs`, `Policies`, `SaveSandbox`
  fields, plus the naming constants derived from `sbx`.
- **`internal/tui/acpwatch.go`**: `sandboxProbe` defaults to `sbx.Alive`. It
  probes before dialing a running session's socket and reaps a starting
  session after a grace period. Inconclusive probes never reap.
  **`internal/tui/sessions.go`**: displays `SandboxName`.
- **`internal/agent`**: no sbx calls. `Launcher`/`Session`/`Spec` run argv
  (`ProcessLauncher` for acp-wrapper, `terminal.TabLauncher` for tabs). This
  is the layer the provider's exec argv feeds into, and it is unchanged.
- **Prompt templates**: `planning.tmpl` (sandbox name budget;
  `static_mcps` → the per-sandbox MCP gateway's servers; `policies` → `sbx policy`;
  the "balanced" default policy), `review.tmpl` (name cap), `wolf.tmpl`
  (sandboxed wolf has "no `sbx`"; the `sbx mcp` hint), `workingman.tmpl`
  ("anything on the host (sbx, wsp, …)").
  `prompts.Data.Sandboxed` is a sandbox-neutral boolean.
- **`start.sh`**: restarts sandboxd bound to the private ssh-agent
  (`sbx daemon stop/start -d`) and runs `check-signing.sh --yes`. It also
  documents `sbx template load` for `ACP_IMAGE`.
- **`check-signing.sh`**: end-to-end probe of a throwaway sandbox
  (`create`, `exec ssh-add -l`, `policy allow network github.com`,
  `exec ssh -T`, `rm --force`) and the sandboxd restart fix.
- **`test/start-agent.sh`**: exercises only `start.sh --check-agent`, which
  skips every sbx call.

### 2.3 Inconsistencies the inventory surfaces

- **Binary resolution is split three ways.** acp-wrapper honors `--sbx`.
  `Runner.LaunchInteractive` honors `Runner.SbxPath` for exec but not for
  ensure. `sbx.Alive` and `DefaultSandboxCreator` always use `"sbx"` from
  PATH.
- **The `sbx ls` read path is implemented three times**: `sbx.Alive`,
  `runner.readSandboxWorkspaces`, and `acpwrapper.listSandboxes`. The
  ensure/drift logic is implemented twice: `runner.DefaultSandboxCreator` and
  `acpwrapper.ensureSandbox`, which differ only in the kit and template.
- **Policies and MCPs are applied only on create.** A reused sandbox keeps
  whatever rules it was created with. The correctness argument ("the name
  encodes the task, so same name ⇒ same rules") is a convention, not a check.
- **Four test seams for one dependency**: `commandFunc`, `gitsign.RunFunc`,
  `tui.sandboxProbe`, and `daemon.reconciledSandboxProbe`.

---

## 3. Proposed interface

New package `internal/sandbox`, holding backend-neutral types, the
`Provider` interface, errors, the registry, and generic helpers. Backends
live in `internal/sandbox/sbx` (today's `internal/sbx` plus the create, exec
and secret code), `internal/sandbox/docker`, and `internal/sandbox/local`.
The fake lives in `internal/sandbox/sandboxtest`.

### 3.1 Core types

```go
package sandbox

// Mount is one host path exposed to the sandbox. Contract: it appears at the
// SAME absolute path inside the sandbox (prompts, .orch handoff files, the
// .gomodcache probe and session.json all use host paths verbatim). A provider
// that cannot honor this must not claim CapSameHostPaths, and Ensure fails.
type Mount struct {
	HostPath string
	ReadOnly bool
}

// Spec is the desired state of a sandbox. Ensure converges to it.
type Spec struct {
	Name   string  // already passed through the provider's NameRules
	Mounts []Mount // Mounts[0] is the primary (default cwd for Exec)

	// Image is the base environment: an sbx agent template ("claude") plus an
	// optional prebuilt image (today's -t/--acp-image), or a Docker image.
	Image ImageSpec
	// Kits are layered extensions (acp-kit). Empty for plain `:session` sandboxes.
	Kits []string

	MCPs     []string      // static MCP servers to attach (sbx --static-mcp)
	Policies []policy.Rule // applied in order, after create, before any Exec

	// Labels are opaque key/values the provider persists with the sandbox and
	// returns from List/Get (kind, project path, session id). Lets non-sbx
	// backends reconstruct ownership without a side database.
	Labels map[string]string
}

type ImageSpec struct {
	Agent      string // sbx agent template, e.g. "claude"
	Ref        string // optional prebuilt image; "" = provider default
	PullPolicy string // "", "missing", "always", "never"
}

// Info is what List/Get report. Mounts is what drift detection compares.
type Info struct {
	Name    string
	ID      string // provider-native id, if any (session.Session.SandboxID)
	State   State  // Running, Stopped, Unknown
	Mounts  []Mount
	Labels  map[string]string
}

type State int

const (
	StateUnknown State = iota
	StateRunning
	StateStopped
)

// ExecSpec describes a command to run inside an existing sandbox.
type ExecSpec struct {
	Argv []string          // command inside the sandbox
	Cwd  string            // "" = Mounts[0].HostPath
	Env  map[string]string // ordered deterministically by the provider
	TTY  bool              // interactive tab (sbx -it); MUST be false for ACP stdio
}
```

### 3.2 The Provider interface

```go
// Provider is one sandbox backend. All methods must be safe for concurrent
// use; workingman runs many acp-wrapper processes against one backend.
type Provider interface {
	// ID is the stable provider identifier recorded in session.json
	// ("sbx", "docker", "local").
	ID() string

	Capabilities() Capabilities
	Names() NameRules

	// Ensure makes a sandbox matching spec exist, idempotently:
	//   absent                        -> create, apply policies
	//   present, same mount set       -> reuse (no-op)
	//   present, different mount set  -> remove, recreate
	// It returns which branch was taken, plus any soft capability downgrades.
	// It does NOT start a stopped sandbox; Exec does that (sbx semantics).
	Ensure(ctx context.Context, spec Spec) (EnsureResult, error)

	// Get returns the named sandbox. ErrNotFound means definitely absent; any
	// other error is inconclusive and callers must not treat it as "gone".
	Get(ctx context.Context, name string) (Info, error)
	List(ctx context.Context) ([]Info, error)

	// Remove force-removes the sandbox. Removing an absent sandbox is not an
	// error (returns nil), matching how every call site already treats it.
	Remove(ctx context.Context, name string) error

	// ExecCommand returns the HOST argv that runs spec inside the named
	// sandbox (e.g. ["sbx","exec","-it","-w",cwd,"-e","K=V",name,"--",argv...]).
	// Returning argv rather than running it is deliberate: the same
	// result feeds a terminal tab (agent.Launcher), the ACP stdio child
	// (exec.Cmd with pipes) and short probes (CombinedOutput). Every
	// backend in view is process-shaped. The command must auto-start
	// a stopped sandbox.
	ExecCommand(name string, spec ExecSpec) ([]string, error)
}

type EnsureResult struct {
	Action    EnsureAction  // Reused, Created, Recreated
	Downgrade []Downgrade   // soft requirements that could not be honored
}
```

Generic helpers in `package sandbox`, so callers do not repeat the
`os/exec` wiring:

```go
// Alive is today's sbx.Alive tri-state, provider-agnostic.
func Alive(ctx context.Context, p Provider, name string) (bool, error) {
	_, err := p.Get(ctx, name)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrNotFound):
		return false, nil
	default:
		return false, err
	}
}

// Run executes spec in the sandbox and returns combined output. It is used by
// the gitsign preflights.
func Run(ctx context.Context, p Provider, name string, spec ExecSpec) ([]byte, error)

// Command returns an unstarted *exec.Cmd for spec (ACP stdio child).
func Command(ctx context.Context, p Provider, name string, spec ExecSpec) (*exec.Cmd, error)
```

### 3.3 Optional capability interfaces

Features that only some backends have are kept out of the core interface.
Callers type-assert, and `Capabilities` advertises the result.

```go
// Credentials reports whether the backend will inject a credential for
// requests from the sandbox to host (sbx: a custom secret scoped global or to
// the sandbox, per `sbx secret ls --json`). Used to decide ANTHROPIC_BASE_URL.
type Credentials interface {
	HasCredentialFor(ctx context.Context, sandbox, host string) (bool, error)
}

// SSHAgent describes how the host's SSH agent is reachable inside sandboxes.
type SSHAgent interface {
	// SocketPath is SSH_AUTH_SOCK inside the sandbox (sbx: /run/ssh-agent.sock).
	// "" means the provider sets it itself and callers must leave it alone.
	SSHAgentSocket() string
}

// HostDoctor covers backend-daemon maintenance that today lives in
// start.sh / check-signing.sh (sbx daemon stop/start -d bound to SSH_AUTH_SOCK).
type HostDoctor interface {
	// Restart rebinds the backend daemon to env (e.g. SSH_AUTH_SOCK).
	RestartDaemon(ctx context.Context, env map[string]string) error
}
```

### 3.4 Naming constraints

```go
// NameRules is the provider's sandbox-name grammar. Today's internal/sbx/name.go
// becomes the sbx provider's implementation. The shortening algorithm
// (FitNameTo) stays a shared helper because its determinism is a workingman
// invariant (task identity across restarts), not a backend rule.
type NameRules interface {
	MaxLen() int                 // sbx/docker: 63
	Sanitize(name string) string // sbx: "_"->"-", then FitName(MaxLen)
	Check(name string) error
}

// Budget is NameBudget generalized: room left for "<prefix>-<suffix>".
func Budget(r NameRules, prefix string) int
```

`task.MaxNameLen`, `task.MaxNameLenFor`, `taskgraph`, and the `nameBudget`
template function read `NameRules` from the configured provider instead of
the `sbx` constants. One subtlety: task names are written into task YAML by
agents. If the provider changes later with a *smaller* `MaxLen`, existing
names are still repaired by `Sanitize` at launch, which is the same
"backstop" role `SanitizeName` plays today.

### 3.5 Capabilities and negotiation

```go
type Capabilities struct {
	Isolation        Isolation // None, Container, MicroVM
	SameHostPaths    bool      // Mount contract (required; see Mount)
	ReadOnlyMounts   bool
	NetworkPolicy    PolicySupport // what network rules can be enforced
	FSPolicy         PolicySupport
	DefaultDenyNet   bool          // egress is denied unless allowed (sbx: yes)
	StaticMCPs       bool
	Kits             bool
	PrebuiltImages   bool
	AutoStartOnExec  bool
	SSHAgentForward  bool
	CredentialProxy  bool
}

type PolicySupport struct {
	AllowAll, DenyAll bool // "**"
	AllowHost         bool // allow a specific host/glob
	DenyHost          bool
}
```

Negotiation happens in `Ensure` and **fails closed for anything
security-relevant**:

| Requested | Backend lacks it | Behavior |
|---|---|---|
| `deny` rule (network or fs) | the matching `Deny*` support | **`ErrUnsupported`**. We never pretend to enforce a deny. |
| `allow` rule | `DefaultDenyNet == false` (egress is already open) | **no-op, recorded as a downgrade**. The allow is redundant. |
| `allow` rule | default-deny but no `AllowHost` | `ErrUnsupported`: the task would be unable to reach the host it declared. |
| `ReadOnly` mount | `ReadOnlyMounts` | `ErrUnsupported`. The workingman agent's read-only guarantee depends on it. |
| `MCPs` | `StaticMCPs` | `ErrUnsupported` by default. See open question Q4. |
| `Kits` | `Kits` | `ErrUnsupported` unless the image already provides `claude-acp-client` (Q5). |
| `Image.Ref` | `PrebuiltImages` | downgrade: the default image is used and the event is logged. |
| `SameHostPaths` | — | always fatal. Nothing works without it. |

Callers that want to decide up front (for example, the daemon refusing to
start the workingman agent on an `Isolation: None` provider) read
`Capabilities()` directly. The runner audits every `Downgrade` as
`sandbox_downgrade` so it shows up in the audit log, in the same way
`interactive_signing_disabled` does today.

### 3.6 Error model

```go
var (
	ErrNotFound    = errors.New("sandbox: not found")      // definitive absence
	ErrUnavailable = errors.New("sandbox: backend unavailable") // binary missing, daemon down: inconclusive
	ErrUnsupported = errors.New("sandbox: unsupported")
	ErrInvalidName = errors.New("sandbox: invalid name")
)

// Error carries the context every call site currently hand-assembles
// ("sbx create %s %v: %w: %s").
type Error struct {
	Provider string
	Op       string // "ensure", "create", "remove", "policy", "list", "exec"
	Sandbox  string
	Feature  string // for ErrUnsupported: "policy:deny:network", "mcp:github"
	Output   string // trimmed combined output from the backend, if any
	Err      error
}
```

Rules:

- **Liveness is tri-state everywhere.** Only `ErrNotFound` means gone.
  `ErrUnavailable` and anything else are inconclusive, which preserves the
  existing TUI and reconcile contract. Providers must map "binary not on
  PATH" and "daemon not running" to `ErrUnavailable`, never to an empty list.
- `Remove` on an absent sandbox returns nil.
- Best-effort call sites (`removeSandboxOnExit`) keep logging rather than
  returning. That is caller policy, not the provider's.
- User-facing remediation text (`sbx policy log`, `sbx mcp auth`,
  `check-signing.sh`) moves behind an optional
  `Hints interface{ Hint(err error) string }`, so `gitsign` and `review.go`
  stop naming sbx.

### 3.7 How call sites map to the interface

| Current call site | Becomes |
|---|---|
| `sbx.Alive` (tui `watchACPSessions`, daemon `newReconciledSession`) | `sandbox.Alive(ctx, p, name)`, where `p` is resolved from `session.json`'s new `sandbox_provider` |
| `sbx.DecodeList`, `runner.readSandboxWorkspaces`, `acpwrapper.listSandboxes` | `p.Get` / `p.List` (decoding stays private to the sbx provider) |
| `runner.DefaultSandboxCreator`, `acpwrapper.ensureSandbox`/`createSandbox`, `sameWorkspaceSet` | `p.Ensure(spec)`. The kit and template become `Spec.Kits`/`Spec.Image`. Drift comparison moves into the provider, and the default compares `Mount.HostPath` sets. |
| `policy.Rule.CLIArgs` | private to the sbx provider. `policy.Rule` stays the neutral model, and `Encode`/`Decode` stay the acp-wrapper wire format. |
| `acpwrapper.removeSandboxOnExit`, drift `rm --force` | `p.Remove` |
| `acpwrapper.Config.execArgs` (ACP) | `sandbox.Command(ctx, p, name, ExecSpec{Argv: ["claude-acp-client"], Env: …, TTY: false})` |
| `runner.Start` legacy `sbx exec -it -w` | `p.ExecCommand(name, ExecSpec{Argv: claudeCmd, Cwd: workingDir, TTY: true})` → `agent.Spec.Command` |
| `runner.LaunchInteractive` | `p.Ensure` then `p.ExecCommand(…TTY: true, Env: gitsign env)` |
| `gitsign.IdentityEnvArgs` / `SigningEnvArgs` | return `map[string]string` (`IdentityEnv`, `SigningEnv`). The `-e` spelling disappears from gitsign. |
| `gitsign.Preflight` / `PushPreflight(run, sbxPath, sandbox)` | `Preflight(ctx, p, sandbox, key)` using `sandbox.Run`. The `SSHAgent` capability decides whether to probe at all. |
| `acpwrapper.gatewaySecretConfigured` | `p.(sandbox.Credentials).HasCredentialFor(ctx, name, gatewayHost)`. Absent interface ⇒ false. |
| `sbx.SanitizeName`, `NameBudget`, `MaxNameLen`, `CheckName` | `p.Names().Sanitize/MaxLen/Check`, `sandbox.Budget` |
| `Runner.SbxPath`, `acp-wrapper --sbx` | provider config (§4). `--sbx` is kept as a deprecated alias for `--sandbox-opt sbx.path=…`. |
| `start.sh` / `check-signing.sh` sbx daemon restart | `HostDoctor` via a future `orch sandbox doctor [--fix]` subcommand. The scripts call it. Phase 6, optional. |
| Prompt text naming sbx | templates receive `.SandboxProvider` and capability booleans (for example, `{{if .Caps.StaticMCPs}}` gates the `static_mcps:` section of `planning.tmpl`) |
| `session.Session.SandboxID` | filled from `Info.ID` |

---

## 4. Configuration and selection

- **Registry**: `sandbox.Register(id string, f Factory)` with
  `type Factory func(opts map[string]string) (Provider, error)`. Backends
  register in `init()`, and `cmd/orch` and `cmd/acp-wrapper` blank-import
  the backends they ship.
- **Selection**, in precedence order:
  1. the `--sandbox-provider <id>` flag (orch and acp-wrapper);
  2. `sandbox.provider` in `~/.config/workingman/settings.yaml`, the file
     the terminal-tab work introduced;
  3. the default, `sbx`.

  ```yaml
  sandbox:
    provider: sbx          # sbx | docker | local
    options:
      sbx.path: /usr/local/bin/sbx
      docker.network: workingman-egress
  ```
- **Propagation**: orch passes `--sandbox-provider` and the options to every
  acp-wrapper, as `--sbx` is passed today. The wrapper writes
  `sandbox_provider` into `session.json`. The TUI and a restarting daemon
  then probe a session with **the provider that created it**, not the one
  currently configured. Without this, switching providers would make every
  adopted session look "gone" and get reaped.
- **Startup validation**: orch constructs the provider once at startup and
  fails fast if `ErrUnavailable` is returned (for example, the binary is
  missing). It logs the capability set to the audit log.

---

## 5. Testing strategy

1. **Fake provider** (`internal/sandbox/sandboxtest.Fake`): an in-memory map
   of `Info`, a recorded call log (`Ensure`, `Remove`, `ExecCommand` with
   specs), injectable per-op errors (including `ErrUnavailable` for the
   inconclusive path), configurable `Capabilities`, and scripted exec
   responses (`OnExec(match, output, err)`) for the gitsign preflights.
   `ExecCommand` returns a host argv that points at a tiny helper
   (`os.Args[0]` with a `-test.run=TestHelperProcess` env marker), so ACP
   stdio tests can still pipe real bytes.

   The fake replaces `acpwrapper.commandFunc`, `gitsign.RunFunc`,
   `tui.sandboxProbe`, `daemon.reconciledSandboxProbe`, and
   `runner.preflightRun`/`Sandbox`.
2. **Conformance suite** (`sandboxtest.Conformance(t, factory)`): a shared
   behavioral spec every provider must pass. It covers:
   - Ensure idempotence;
   - drift → recreate;
   - mount order insensitivity;
   - `Remove` of an absent sandbox returning nil;
   - `Get` returning `ErrNotFound` versus an error;
   - the mount appearing at the same host path (`ExecCommand` + `pwd`/`test -d`);
   - a read-only mount rejecting writes (when the capability is claimed);
   - Exec auto-starting a stopped sandbox;
   - TTY=false preserving raw stdio bytes;
   - deny policies returning `ErrUnsupported` when not claimed.

   It runs against `Fake` always. It runs against `local` always (with
   temp dirs). It runs against `sbx`/`docker` only when
   `WORKINGMAN_SANDBOX_IT=sbx|docker` is set, because CI and agent
   sandboxes have neither.
3. **Argv golden tests for the sbx provider.** The existing assertions in
   `acpwrapper_test.go`/`runner_test.go` on exact `sbx create/policy/exec`
   argv move into `internal/sandbox/sbx` with the provider's own
   `commandFunc` seam. They guard byte-for-byte backward compatibility
   during migration. `decode_test.go` and `name_test.go` move along with
   them.
4. **Caller tests** assert intent (which `Spec`/`ExecSpec`) rather than argv,
   which removes most sbx string-matching from daemon, runner and wrapper
   tests.

---

## 6. Phased migration

Each phase is independently mergeable and should produce no behavior change
for sbx users until phase 5.

| Phase | Change | Exit criterion |
|---|---|---|
| **0** | Review this doc. Resolve the open questions. | Approved. |
| **1** | Add `internal/sandbox` (types, interface, errors, registry, helpers, `Fake`, `Conformance`). Add `internal/sandbox/sbx` implementing `Provider` by **moving** `internal/sbx`, `acpwrapper.{ensure,create,list}Sandbox`, `gatewaySecretConfigured`, and `policy.CLIArgs`. Leave `internal/sbx` as thin forwarding shims. | Golden argv tests pass unchanged. Conformance passes on Fake. |
| **2** | Read paths: TUI `acpwatch` and daemon `reconcile` take a `sandbox.Provider` (or a `func(providerID) Provider` resolver) instead of `sbx.Alive`. Add `session.Session.SandboxProvider` (empty ⇒ `sbx`, for old records). | TUI/reconcile tests run on Fake. |
| **3** | acp-wrapper: `Run` goes through `p.Ensure` / `sandbox.Command` / `p.Remove` / `Credentials`. `gitsign` returns env maps and its preflights take a `Provider`. Add the `--sandbox-provider` flag (default `sbx`), and keep `--sbx`. | Wrapper tests on Fake, plus sbx golden tests. |
| **4** | Runner: replace `SandboxCreator`/`DefaultSandboxCreator` and the `sbx exec -it` wrapping in `Start` and `LaunchInteractive` with `Runner.Sandbox sandbox.Provider`. Unify the `SbxPath` handling, which fixes §2.3. Move the naming consumers (`task`, `taskgraph`, `prompts`, `daemon.sanitizeSandboxName`) to `Provider.Names()`. | `rg '"sbx"' internal/ cmd/` hits only `internal/sandbox/sbx`. |
| **5** | Selection via settings.yaml and flag. Audit capability downgrades. Templates get `.SandboxProvider`/`.Caps`. Remediation text moves to `Hints`. | Switching to `local` works end-to-end for one dev project. |
| **6** | Alternative backends: `local` first, then `docker`. Optional `orch sandbox doctor`, with `start.sh`/`check-signing.sh` delegating to it. | Conformance passes per backend. |

`internal/sbx` is deleted once nothing imports it (end of phase 4).

---

## 7. Alternative backends (sketches)

### 7.1 `local`: no isolation (dev and debugging only)

The `--wolf-host`-style escape hatch, generalized: agents run directly on the
host.

- **Capabilities**: `Isolation: None`, `SameHostPaths: true` (trivially),
  `ReadOnlyMounts: false`, `NetworkPolicy`: nothing enforceable,
  `DefaultDenyNet: false`, `StaticMCPs: false`, `Kits: false`,
  `AutoStartOnExec: true`, `SSHAgentForward: true` (host agent),
  `CredentialProxy: false`.
- **Ensure**: verifies that each mount's host path exists, then writes a
  record to `~/.workingman/local-sandboxes/<name>.json` (mounts and labels)
  so that `Get`/`List`/drift detection behave like every other backend.
  Allow rules are no-op downgrades. Deny rules and read-only mounts return
  `ErrUnsupported`, which means the **workingman agent refuses to run on
  `local`**. That is the intended result, because its read-only guarantee
  would otherwise be false.
- **ExecCommand**: `["env", "-C", cwd, "K=V"…, argv…]`. Use a small re-exec
  helper on macOS, since BSD `env` has no `-C`. `claude-acp-client` must be
  on the host PATH (this is Q5).
- **Remove**: deletes the record. It never touches the mounts.
- **Credentials**: `HasCredentialFor` returns false, so the gateway is off
  unless the user configures it in their own environment.
- Orch prints a prominent startup warning and requires
  `sandbox.options.local.i_understand_no_isolation: true`.

### 7.2 `docker`: plain containers

- **Ensure**:
  `docker create --name <n> --label workingman.sandbox=1 --label workingman.mounts=<json> -v <p>:<p>[:ro]… -w <primary> <image> sleep infinity`,
  followed by `docker start`.
  - Drift detection reads the mounts back with `docker inspect`
    (`.Mounts[].Source/.RW`), or from the label.
  - `Image.Ref` is required. The image must already contain claude and
    `claude-acp-client` (acp-kit's prebuilt `claude-code-acp` image fits).
    `Kits` therefore returns `ErrUnsupported`, unless the "kit already baked
    into the image" rule from Q5 is adopted.
- **List/Get**: `docker ps -a --filter label=workingman.sandbox=1 --format '{{json .}}'`.
  An absent container maps to `ErrNotFound`. A daemon that cannot be reached
  maps to `ErrUnavailable`.
- **ExecCommand**: `["docker","exec",("-it"),"-w",cwd,"-e","K=V"…,name,argv…]`.
  Auto-start is emulated by prefixing `docker start <n> >/dev/null &&` in an
  `sh -c`, or by the helpers calling `Start` first. See Q7.
- **Remove**: `docker rm -f <n>`. "No such container" maps to nil.
- **Network policy**:
  - deny-all is `--network none` at create;
  - allow-all is the default bridge;
  - per-host allow lists need an egress proxy (for example, a
    squid/tinyproxy sidecar on an internal network with `HTTPS_PROXY`), and
    are **unsupported in v1**.
  - So `PolicySupport{AllowAll, DenyAll}` only. A task with
    `deny ** + allow github.com` fails with `ErrUnsupported` rather than
    silently getting open egress.
- **Filesystem policy**: unsupported. Read-only mounts are supported.
- **SSH agent**:
  - on Linux, bind-mount `$SSH_AUTH_SOCK` to `/run/ssh-agent.sock` and set
    the env;
  - on Docker Desktop for macOS, mount `/run/host-services/ssh-auth.sock`.
  - `SSHAgentSocket()` returns the in-container path, so `gitsign` sets
    `SSH_AUTH_SOCK` explicitly. This is a difference from sbx, where we must
    *not* set it.
- **Credentials**: there is no proxy, so `HasCredentialFor` returns false.
  Secrets would have to be passed as env, which is weaker than sbx and is
  deliberately not done automatically (see Q6).
- **MCPs**: unsupported in v1.

---

## 8. Open questions for review

1. **Argv versus process abstraction for exec.** `ExecCommand` returns host
   argv because all current consumers (terminal tabs, ACP stdio child,
   probes) run processes. Is that acceptable, or do we want room for an
   API-only backend (for example, a remote sandbox service) with an
   `Exec(ctx) (Process, error)` that has stdio streams? The latter would
   force `terminal.TabLauncher` to grow a non-argv path.
2. **Re-applying policies on reuse.** Today, policies and MCPs are applied
   only on create, and reuse trusts the name. Should `Ensure` reconcile
   policies on every call (sbx has no `policy ls --sandbox` in our usage,
   so this needs checking), or should we record a spec hash in `Labels` and
   recreate on mismatch?
3. **Where drift detection lives.** It is in the provider in this proposal.
   The alternative is generic code in `sandbox` over `Get` + `Info.Mounts`.
   Generic code is less duplication, but sbx's `ls` may not report `:ro`
   reliably (the current code deliberately ignores it).
4. **MCP portability.** Is "static MCP" a sandbox concern at all for
   non-sbx backends, or should workingman instead write the agent's MCP
   config (`.mcp.json`) and treat the MCP gateway as a separate pluggable
   piece? The review agent depends on the `github` MCP, so on a backend
   without MCPs the review loop can't run.
5. **Kits.** Model acp-kit as a provider feature (`Spec.Kits`), or as a
   workingman-level requirement that "the image must provide
   `claude-acp-client`" with sbx's `--kit` as one way to satisfy it?
6. **Secrets and the LLM gateway.** `Credentials` only answers "is there a
   credential for host X". Do we need a write side (`SetSecret`) so that
   orch can provision the gateway token for docker/local, or does that stay
   the user's job?
7. **Auto-start semantics.** sbx's `exec` starts a stopped sandbox, and
   `:session` relies on that. Should that be a hard requirement of
   `ExecCommand`, or a separate `Start(ctx, name)` that the helpers call?
   Note that a tab launcher cannot call Go code before the argv runs.
8. **The name budget under provider switches.** Planning prompts tell agents
   the name budget from the current provider. Should the budget instead be
   fixed at the smallest `MaxLen` among the registered providers, so that
   task YAML written under one provider never needs shortening under
   another?
9. **The host-maintenance boundary.** Should `start.sh` and
   `check-signing.sh` stay sbx-specific shell (and simply be skipped for
   other providers), or move into `orch sandbox doctor` behind
   `HostDoctor`? The second option is more work, but it makes the
   ssh-agent/sandboxd binding testable.
10. **Mixed providers.** Is one provider per orch process enough, or do we
    want a per-project or per-task `sandbox_provider:` override (for
    example, review on sbx for MCPs and tasks on docker)? The session.json
    field already allows mixed sessions to be read back. The question is
    whether to allow them to be launched.
11. **The legacy no-ACP path.** `runner.Start`'s `sbx exec -it` tab path
    exists only for `--acp-kit`-less runs and tests. Should it be ported
    (phase 4) or deleted first to shrink the migration surface?
12. **Fail-closed defaults.** Is `ErrUnsupported` on deny rules, read-only
    mounts and missing MCPs the right default, or should there be a
    project-level `sandbox_best_effort: true` escape hatch?
