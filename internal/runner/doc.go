// Package runner turns the daemon's request to launch an agent (a [Plan]) into
// a running session: it prepares the workspace, writes the .orch handoff, and
// starts the agent over ACP or in a terminal tab.
//
// Role: daemon
//
// For each [Runner.Start] the runner:
//
//  1. Resolves the working directory. Agents that run in the project's
//     control dir (project, planning, wolf, review, workingman) get
//     Plan.WorkingDir as-is. Task, commit and archive agents get a wsp
//     workspace from workspace.Manager. Planning and an ACP wolf also get the
//     project's wsp worktree as a second mount, so they can read source.
//  2. Renders the kind's instructions with internal/prompts.
//  3. Writes .orch/context.yaml, .orch/instructions.md and any skills with
//     setup.Apply.
//  4. Launches the agent on one of the two paths below.
//
// The two launch paths:
//
//   - ACP (production, [Runner.UsesACP]): every autonomous kind, plus the
//     wolf unless WolfOnHost is set. The runner writes an initial session.json
//     and starts an acp-wrapper host process through AcpLauncher
//     (agent.ProcessLauncher). The wrapper creates the sbx sandbox, runs the
//     ACP claude client and serves agent.sock, and the TUI watches the session
//     over that socket.
//   - Terminal tab (legacy, and the --wolf-host escape hatch): the runner
//     builds a claude argv with the [CommandBuilder], optionally ensures a
//     sandbox with the [SandboxCreator] and wraps the argv in `sbx exec`, then
//     opens it with Launcher (terminal.TabLauncher).
//
// MCP gateway: when Runner.MCPGatewayURL is set, every sandbox gets its own
// sub-gateway of the host's standalone-codemode gateway (internal/mcpgateway)
// over [Runner.MCPServersFor] — the plan's StaticMCPs (a task's static_mcps)
// when it has any, else Runner.MCPGatewayServers. The sandbox is allowed to
// reach the gateway's port and gets MCP_URL pointing at it; nothing is sent
// as `sbx create --static-mcp` any more. On the ACP path the runner only
// forwards --mcp-gateway-url / --mcp-server to acp-wrapper, which owns the
// gateway like it owns the sandbox; the terminal-tab and `:session` paths
// mint it here and pass MCP_URL to `sbx exec -e`. Either way the gateway is
// POSTed before `sbx create` and the network policy is applied before the
// first `sbx exec`; see docs/mcp-gateway.md.
//
// [Runner.LaunchInteractive] opens the TUI's ad-hoc `:dir` (host shell) and
// `:session` (claude in a per-work-stream sandbox) tabs. It sets up git
// identity and SSH commit signing, and these sessions have no daemon lifecycle.
//
// Names are deterministic, so a relaunch after a restart reuses the same
// sandbox and session directory. [SandboxNameFor] and [ACPSandboxNameFor]
// derive sbx sandbox names from the kind, work stream and task, and the ACP
// session id is derived from the session name.
//
// # Key types
//
//   - [Runner] — the configured launcher. It holds the workspace manager, both
//     launchers, the acp-wrapper settings (Kit, Image, SessionsRoot, ...), and
//     the wolf's timeouts. cmd/orch builds one and passes it to the daemon.
//   - [Plan] — one launch request. Fields that don't apply to a Kind are
//     ignored; the field comments say which kinds read each one.
//   - [Observe] — the orch state the workingman agent may read: roots,
//     snapshot, audit log, sessions.
//   - [InteractiveSpec] — the request for [Runner.LaunchInteractive].
//   - [CommandBuilder] and [DefaultCommandBuilder] — the claude argv for the
//     terminal-tab path; tests replace it with `sleep`.
//   - [SandboxCreator], [SandboxSpec] and [DefaultSandboxCreator] — the
//     idempotent `sbx create` and `sbx policy` step for the terminal-tab and
//     `:session` paths. It applies the MCP gateway's allow rules and releases
//     a removed sandbox's gateway.
//
// # Collaborators
//
// Uses internal/agent (Kind, Spec, Launcher), internal/prompts and
// internal/setup (the .orch handoff), internal/workspace (wsp workspaces),
// internal/terminal (TabLauncher's already-open handling), internal/mcpgateway
// (per-sandbox MCP gateways), internal/session
// (session.json records for ACP sessions), internal/sbx and internal/policy
// (sandbox names and network rules), internal/gitsign (identity and signing
// for interactive tabs), internal/task (model defaults) and internal/audit.
// Used by internal/daemon, which calls Start, UsesACP, StartFailure,
// SessionLogPath, ResolveSessionsRoot and ACPSandboxNameFor, and routes the
// TUI's `:dir`/`:session` requests to LaunchInteractive. cmd/orch builds the
// Runner and hands it to the daemon. See agents.md and docs/terminal-tabs.md.
package runner
