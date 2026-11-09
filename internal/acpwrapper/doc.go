// Package acpwrapper is the core of the acp-wrapper binary: it runs a Claude
// ACP agent inside an sbx sandbox and serves it to clients over a per-session
// unix socket, agent.sock.
//
// Role: wrapper
//
// [Run] creates (or reuses) an sbx claude sandbox with the acp-kit ACP bridge
// layered on, execs acp-kit's `claude-acp-client` entrypoint inside it over
// `sbx exec` (it speaks the Agent Client Protocol on its stdio), and listens on
// <SessionsRoot>/<SessionID>/agent.sock. One acp-wrapper process backs one
// session; orch's runner spawns it for each ACP agent.
//
// The socket bridge (bridge.go) multiplexes the agent's one stdio pair across
// any number of clients, framed on newline-delimited JSON-RPC boundaries: one
// reader splits the agent's stdout into frames and copies each to every
// connected client, and each client's frames are written to stdin whole, under
// a mutex. A client that falls too far behind is dropped and can reconnect.
// Every agent frame is also appended to stream.log ([LogName]) so a client that
// connects later can replay the earlier output; the socket only carries the
// stream from "now".
//
// Next to the socket the wrapper keeps session.json up to date (starting,
// running, exited/failed) so the TUI and a restarting daemon can find the
// session and tell what drives it. The session ends when the agent exits or ctx
// is cancelled, or, depending on [Config]: when the last client leaves
// (ExitWhenEmpty, one-shot agents), or for a persistent session (the wolf)
// after the project has been unblocked for UnblockGrace or idle for
// IdleTimeout. On exit the sandbox is removed unless the task's status, or
// SaveSandbox, says to keep it for inspection. Before the agent starts, Run
// also routes it through the LLM gateway when a gateway secret is configured,
// checks that the SSH signing key is usable, and checks ssh push access for
// sessions that will push.
//
// # Key types
//
//   - [Config] — everything one session needs: ids and paths, kit and
//     template, workspaces ([SplitMount] / [JoinMount] handle the `:ro`
//     suffix), lifecycle mode, task/project paths, git identity and signing,
//     model, the per-sandbox MCP gateway (MCPGateway, see
//     internal/mcpgateway) and sbx policies. cmd/acp-wrapper fills it from
//     flags. [Config.SessionDir], [Config.SocketPath] and [Config.LogPath] give
//     the on-disk layout.
//   - [Run] — the blocking main loop described above.
//   - [SocketName], [LogName] — the file names inside a session directory.
//
// # Collaborators
//
// Uses [github.com/slimslenderslacks/work/internal/session] for the session
// directory layout and session.json, internal/sbx to parse `sbx ls`,
// internal/policy for the sbx policy rules applied to the sandbox,
// internal/gitsign for the signing and push preflights, and internal/task and
// internal/project to read the task status (sandbox retention) and project
// status (unblock grace). Used only by cmd/acp-wrapper. The clients on the
// other end of agent.sock are
// [github.com/slimslenderslacks/work/internal/acpclient] (the TUI watcher) and
// [github.com/slimslenderslacks/work/internal/acpchat] (the daemon's router).
// See docs/ssh-agent.md for the signing setup.
package acpwrapper
