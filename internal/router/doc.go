// Package router connects inbound channel messages (WhatsApp, Signal) to agent
// conversations and sends the replies back.
//
// Role: daemon
//
// It is the "ear" of the daemon's messaging channels: a [Router] is installed
// as the channels.InboundHandler and decides, per message, whether the text is
// a router command, a reply to a wolf notification, a message for the wolf the
// chat is bound to, or, by default, a question for the workingman agent.
//
// The routing rules (documented for users in docs/channels.md):
//
//  1. A reply-to a known notification goes to THAT wolf session as a prompt;
//     the wolf's answer comes back labelled "[wolf <work-stream>]".
//  2. Slash commands are answered by the router itself, with no LLM:
//     /help, /status, /wolf [work-stream], /agent, /who.
//  3. Otherwise the chat is bound to the workingman agent (the default) or to
//     the wolf chosen with /wolf, until /agent or the wolf session ends.
//  4. While a chat is bound to a wolf it also sees what the wolf says to other
//     clients (the TUI user), and the wolf's permission requests are put to the
//     chat, whose next message answers them; unanswered ones time out rejected.
//  5. Each chat is served one message at a time through a bounded queue, with
//     acks for queueing and slow turns, a turn timeout, an immediate reply when
//     the workingman agent is down, and secrets redacted from every reply.
//  6. Everything is audit-logged under a hashed chat reference, never the text.
//
// Senders are NOT authorised here: a channel applies its access policy before
// it calls the handler, so only approved senders ever reach the Router.
//
// The Router depends on interfaces only ([Source], [Outbound], [Agent]), so it
// has no import of the daemon package; cmd/orch adapts the daemon to Source.
//
// # Key types
//
//   - [Router] — routes inbound messages; [New] starts it and [Router.Handler]
//     is the function to pass to channels.Registry.Start. Safe for concurrent
//     use.
//   - [Config] — Source and Out (required) plus the conversation index,
//     auditor, attacher and the timeout and queue knobs from channels.yaml's
//     router section.
//   - [Source] — what the router needs from the daemon: the live workingman
//     agent, the live wolf sessions, and a /status line. Implemented in
//     cmd/orch.
//   - [Session] — one live agent session, addressed by its session directory.
//   - [Outbound] — sending replies; *channels.Registry implements it.
//   - [Agent] / [Attacher] — an attached ACP conversation and how to get one;
//     [AttachACP] (*acpchat.Conversation) is the production attacher, tests
//     substitute fakes.
//   - [Auditor] — audit-event sink; *audit.Logger implements it.
//
// # Collaborators
//
// Uses [github.com/slimslenderslacks/work/internal/channels] for the message
// types, the ConversationIndex and the Chunker / Typer capabilities, and
// [github.com/slimslenderslacks/work/internal/acpchat] to join agent sessions.
// [github.com/slimslenderslacks/work/internal/audit] is imported only for
// redaction helpers; the Auditor interface stands in for its Logger. Used by
// cmd/orch, which builds the Router when channels are configured. See
// docs/channels.md.
package router
