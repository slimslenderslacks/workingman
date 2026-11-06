// Package acpclient is the client side of an ACP session: it dials a session's
// agent.sock, speaks the Agent Client Protocol over it, and decodes the streamed
// agent output into Events.
//
// Role: shared (TUI watcher and daemon, via acpchat)
//
// The socket is the one the acp-wrapper bridge exposes at
// ~/.workingman/sessions/<id>/agent.sock. Through it a Client sends prompts to
// the sandboxed Claude agent and receives the streamed assistant output for
// incremental display.
//
// The transport is newline-delimited JSON-RPC 2.0 (see protocol.go and the
// acp-wrapper bridge). A single Client owns one connection and one read loop. The
// read loop classifies every inbound frame: responses unblock the in-flight
// request that issued them; session/update notifications are decoded into
// Events. Callers drive the session with the blocking Connect/Prompt methods and,
// concurrently, range over Events() to render streamed chunks and react to the
// connection's lifecycle (connected → streaming → completed, and
// disconnected/errored on the way out).
//
// Lifecycle signalling is the contract downstream code builds on: the session
// tabs render from Events, and cleanup keys off StateDisconnected. In particular,
// a socket whose backing sandbox is gone accepts the connection and then closes
// it immediately (the bridge tears down clients once the ACP client's stdout
// hits EOF); the read loop surfaces that as a StateDisconnected event and fails
// every pending call with ErrConnectionClosed, so the caller can clean up the
// dead session directory.
//
// Concurrency: Connect, Prompt, and Close are each safe to call from one
// goroutine while another ranges over Events(); the read loop is the sole emitter
// on the events channel and is the sole closer of it. Several TUIs can share one
// session through the bridge's fan-out, but request/response correlation assumes
// a single *driving* client per session — additional clients should watch
// (consume Events) rather than issue their own requests, since the shared agent
// stdout would deliver every client's responses to every client.
//
// A second driver (the daemon "tuning in" to a session a TUI also watches) is
// supported through DialWith + Adopt: Options.DistinctIDs moves the client's
// request ids into a private range so the TUI's responses never complete the
// daemon's calls (or vice versa), Adopt binds to the session id the first
// client created instead of running session/new, and Options.OnRequest /
// ObserveResponses expose the agent→client requests and the other clients'
// response frames that the shared fan-out also delivers here.
//
// # Key types
//
//   - [Client] — one connection to an agent.sock; [Dial] / [DialWith] open it,
//     [Client.Connect] runs initialize + session/new, [Client.Adopt] joins an
//     existing session instead, [Client.Prompt] runs one turn.
//   - [Event], [EventKind], [State] — what [Client.Events] streams: assistant,
//     thought, tool-call and plan updates, plus the connection lifecycle.
//     [State.IsTerminal] tells a watcher when to stop and clean up.
//   - [Options] and [RequestHandler] — the knobs for a second participant in a
//     shared session (distinct request ids, serving agent→client requests,
//     observing other clients' responses).
//   - [ConfigOption] / [ConfigValue] — the session config (notably the model,
//     [ConfigIDModel]) reported by the agent and changed via
//     [Client.SetConfigOption] / [Client.SetModel].
//   - [ParseStreamFrame] — decodes one line of a session's stream.log into the
//     same Event the live read loop would emit, for scrollback replay.
//   - [ErrConnectionClosed] — every pending call fails with it when the socket
//     drops; the "this session is dead" signal.
//
// # Collaborators
//
// Standard library only; no internal imports. The server end of the socket is
// [github.com/slimslenderslacks/work/internal/acpwrapper] (its bridge fans one
// agent's stdio out to every connected client). Used by internal/tui (the ACP
// watcher dials each session, sends the opening prompt, renders Events and
// replays stream.log with ParseStreamFrame) and by
// [github.com/slimslenderslacks/work/internal/acpchat] (the daemon-side
// conversation, which uses DialWith + Adopt to share a session with the TUI).
package acpclient
