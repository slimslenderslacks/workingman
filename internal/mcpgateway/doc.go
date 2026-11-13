// Package mcpgateway gives every sandbox workingman creates its own MCP
// gateway: a sub-gateway of the host's standalone-codemode gateway, minted
// with POST /gateways and torn down with DELETE /gateways/{id}.
//
// Role: shared (runner and acp-wrapper)
//
// The flow, per sandbox (driven by acpwrapper.Run for ACP sessions and by
// the runner for the terminal-tab and `:session` paths):
//
//  1. Before a sandbox is created, [Manager.Prepare] POSTs a gateway over
//     [Manager.Servers] (a task's static_mcps, or the configured default
//     set). Catalog-unknown names are dropped rather than failing the whole
//     gateway.
//  2. `sbx create` (no `--static-mcp` flags), then the task's own policies,
//     then [PolicyRules]: allow network to the gateway's port, as
//     host.docker.internal:<port> and localhost:<port> for a loopback base.
//     Only then is the gateway recorded ([Manager.Save]); a failed create or
//     policy DELETEs it ([Manager.Discard]).
//  3. [Manager.Ensure] — before every launch, new or reused sandbox —
//     returns the recorded gateway when it is still live and was minted over
//     the same servers, else DELETEs the stale one and POSTs a new one. The
//     sandbox gets MCP_URL ([EnvVar]) = the gateway URL with a loopback host
//     rewritten to host.docker.internal ([SandboxURL]), re-derived from the
//     host-side URL on reuse. It is set at `sbx create` ([CreateEnvArgs])
//     and again per `sbx exec -e`, so a reused sandbox whose gateway
//     vanished (standalone-codemode keeps them in memory only) just gets a
//     new one; acp-kit's claude-acp-client registers it as the
//     `okta-gateway` MCP server. The wrapper also records it in the sandbox's
//     /etc/sandbox-persistent.sh ([PersistEnvArgs]) for shells a human opens.
//  4. Removing the sandbox (exit cleanup, or a recreate on mount drift)
//     calls [Manager.Release]; [Manager.Sweep] releases the gateways of
//     recorded sandboxes that have since disappeared.
//
// Gateway state is persisted as one JSON [Record] per sandbox in a [Store],
// by default <sessions-root>/../gateways ([StateDir]), so restarts can reuse
// or clean up gateways. Every gateway failure is best-effort: the agent runs
// without MCP_URL rather than not at all.
//
// # Key types
//
//   - [Client] — the /gateways HTTP API (Create, Delete, Alive).
//   - [Manager] — mint/reuse/release policy over a Client and a Store.
//   - [Store], [Record] — the persisted per-sandbox gateway state.
//   - [Session] — a minimal MCP client (streamable HTTP, JSON-RPC 2.0) over
//     one gateway URL: [OpenSession] (initialize + notifications/initialized,
//     keeping Mcp-Session-Id), [Session.CallTool], [Session.Close]. JSON and
//     SSE responses are both accepted. An error wrapping [ErrSessionLost]
//     (404, 400 on a known session id, connection refused/reset) means open a
//     new session; [ToolError] (isError: true) and [RPCError] leave the
//     session usable. The daemon uses it for its review fingerprint check.
//
// # Collaborators
//
// Uses internal/policy for the network rules. Used by internal/acpwrapper
// and internal/runner; cmd/orch's --mcp-gateway-url / --mcp-gateway-servers
// configure it, and cmd/acp-wrapper's --mcp-gateway-url / --mcp-server /
// --mcp-gateway-state carry it to the wrapper. See docs/mcp-gateway.md.
package mcpgateway
