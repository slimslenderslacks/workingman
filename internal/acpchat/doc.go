// Package acpchat lets the daemon join the ACP conversation of an agent that
// is already running, send it prompts and watch what it says, while a TUI stays
// attached to the same session.
//
// Role: daemon
//
// It is the reusable "tune in" primitive behind the messaging channels: the
// router attaches to the wolf, or the workingman agent, sends inbound text as a
// prompt, collects the reply, and relays what the agent says to other clients.
//
// # How a session is shared
//
// Every session directory (<SessionsRoot>/<id>/) holds session.json and the
// agent.sock the acp-wrapper bridge serves. The bridge multiplexes one sandboxed
// ACP agent over any number of unix-socket clients: every agent frame is copied
// to every client, and every client's frames are written to the agent's stdin.
// The TUI's watcher is normally the first client — it runs initialize +
// session/new + session/set_mode and sends the opening prompt — so the
// conversation already exists, as an ACP sessionId, before we arrive.
//
// # Joining the existing conversation
//
// Attach does NOT run session/new (that would start a second, empty
// conversation next to the one the human is in) and does NOT use session/load
// (ACP obliges the agent to replay the whole history as session/update
// notifications, which the bridge would fan out to the TUI too, duplicating its
// scrollback). Instead it adopts the existing sessionId — acpclient.Client.Adopt
// — and sends session/prompt straight to it. The id is recovered from the
// session's stream.log (the bridge records every agent frame there, and every
// session/update and session/new response carries the id; the latest wins), or,
// for a session nobody has talked to yet, from the first frame seen on the live
// socket. If the TUI later re-runs session/new (it does after its own restart)
// the response is fanned out to us as well, and the conversation follows it.
// This has been exercised against an in-process fake agent speaking the ACP
// frames the real ones do (see the tests); the wire behaviour of the real
// claude-acp-client under adoption — a prompt to a session this connection never
// created — is the one thing a live smoke test should confirm.
//
// # Sharing hazards handled here
//
//   - Response fan-out: the bridge broadcasts every response to every client, so
//     two clients counting request ids from 1 would swallow each other's
//     answers. Conversations dial with acpclient.Options.DistinctIDs.
//   - Agent→client requests are broadcast too. Permission requests are surfaced
//     (Options.OnPermission, EventPermission) and answered; the default policy
//     rejects. The first reply wins at the agent, so a TUI that also answers can
//     race this client — in practice orch sessions run in bypassPermissions and
//     never ask.
//   - Slow clients are dropped by the bridge. The conversation notices the socket
//     closing and reconnects (same session id) for as long as the session's
//     directory says it is alive.
//
// # Key types
//
//   - [Conversation] — a live attachment to one session; [Attach] creates it.
//     [Conversation.Ask] runs one prompt turn and returns the cleaned reply;
//     [Conversation.Events] streams everything observed, including turns other
//     clients started.
//   - [Options] — how Attach finds the session and the ACP session id, retry
//     and timeout knobs, CreateIfMissing for a headless daemon, and the
//     OnPermission policy.
//   - [Event] / [EventKind] — the observation stream: text chunks (Own tells
//     this client's turns from other clients'), turn ends, permission requests,
//     reconnects, session changes, and the final EventGone.
//   - [AskOption] ([OnProgress], [FinalSegmentOnly]) and [ProgressFunc] —
//     per-Ask streaming progress and reply trimming.
//   - [PermissionRequest] / [PermissionDecision] — a tool-permission request
//     from the agent and the answer OnPermission returns.
//   - [ErrBusy], [ErrSessionGone], [ErrNoSession], [ErrClosed], [ErrRefused] —
//     the errors Attach and Ask return.
//
// # Collaborators
//
// Built on [github.com/slimslenderslacks/work/internal/acpclient] (DialWith +
// Adopt, Options.DistinctIDs, ObserveResponses) for the wire protocol, and on
// [github.com/slimslenderslacks/work/internal/session] to resolve session
// directories and read session.json and stream.log. The far end of the socket
// is [github.com/slimslenderslacks/work/internal/acpwrapper]. Used by
// internal/router, whose Agent interface *Conversation implements; the router
// relays channel messages (WhatsApp, Signal) to the agent and the agent's
// output back. See docs/channels.md.
package acpchat
