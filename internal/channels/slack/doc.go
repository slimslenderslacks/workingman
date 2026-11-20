// Package slack is the Slack channel: a channels.Channel that talks to Slack
// through the Slack MCP behind the host's standalone-codemode gateway.
//
// Role: daemon
//
// The channel mints its own static sub-gateway over the `slack` catalog
// server (an mcpgateway.DaemonGateway recorded under "orch-slack") and keeps
// one MCP session on it. There is no Slack token here: the gateway's host
// header script is the authentication, and posts are made as the operator's
// own Slack user. See docs/messaging-design.md §6 and
// docs/research/slack-mcp.md.
//
// Configuration (channels.yaml):
//
//	channels:
//	  slack:
//	    type: slack
//	    options:
//	      gateway_url: http://localhost:3333   # default
//	      channel: U0CC1K1MFNX                 # channel id, or your own user id for the DM-to-self
//	      allow_from: [U0CC1K1MFNX]            # Slack user ids; empty => nobody (fail closed)
//	      poll_interval: 20s                   # default; min 5s
//	      watch_ttl: 168h                      # how long a request's thread is watched
//	routes:
//	  - {topic: post-office, channel: slack, chat: U0CC1K1MFNX}
//
// Behaviour:
//
//   - One serialized worker runs every Slack call (the header script fails
//     under concurrency), retrying header-script failures with backoff
//     (1s, 2s, 4s, 4s): they happen before the request leaves the gateway, so
//     even a send is safe to retry.
//   - Send posts with slack_send_message (a thread reply when ReplyToID is
//     set) and returns "<channel_id>:<ts>", with the resolved channel id (a
//     user id resolves to its D… DM). Every posted id goes into a persisted
//     sent set, which is how polling tells our posts from the human's: they
//     have the same author. Post-office requests (topic "post-office", text
//     starting "❓") are added to the persisted watch list.
//   - Slack has no push, so Start polls: slack_read_channel from the oldest
//     watched request, emitting fresh top-level messages not in the sent set
//     (no ReplyToID) and noting watched requests whose "Thread: N replies"
//     count rose; slack_read_thread on those emits each human reply with
//     ReplyToID = the request's id, which the conversation index routes to
//     post-office. If read_channel text cannot be parsed, the channel logs
//     loudly and reads each watched thread instead.
//   - Resolve (channels.Resolver) posts the closing note in the request's
//     thread and adds a reaction to it (white_check_mark, hourglass or
//     no_entry_sign); Slack messages cannot be edited. A later human reply in
//     a closed thread is answered "already closed" and goes nowhere.
//   - allow_from is checked on every inbound message, before the router
//     sees it; other senders are dropped and logged without their text.
//
// State lives in ~/.workingman/slack/: sent.json (30-day TTL) and watch.json
// (the watched requests and the top-level cursor), both written
// write-then-rename at 0600.
//
// `orch slack setup|status|test` use a [Client] over a temporary gateway of
// their own ([Dial]): the same tool resolution and header-script retry, one
// call at a time, without the worker. A top-level Slack MCP post starting
// with [TestPrefix] is `orch slack test`'s, and the poll skips it.
//
// # Key types
//
//   - [Channel] — the Slack transport; built by [Factory] (registered under
//     [Type] in cmd/orch) or [New] over any [Caller].
//   - [Options] / [Config] — the raw `options:` block and its validated form;
//     [ParseOptions] turns one into the other.
//   - [Caller] — the MCP session the channel needs (tool calls and
//     tools/list); *mcpgateway.DaemonSession in production.
//   - [Message] / [ChannelPage] / [ThreadPage] — the "detailed" text format of
//     slack_read_channel and slack_read_thread, parsed by [ParseChannel] and
//     [ParseThread].
//   - [Watch] / [Closed] — a watched request in watch.json; [State] and
//     [ReadState] are the read-only view `orch slack status` prints.
//   - [Client] — the setup CLI's one-shot client ([Dial], [NewClient]):
//     profile, conversation list, post, thread replies, reaction.
//   - [Profile] / [Conversation] / [ListPage] — slack_read_user_profile and
//     slack_list_user_channels text, parsed by [ParseProfile] and
//     [ParseConversations].
//
// # Collaborators
//
// Implements [github.com/slimslenderslacks/work/internal/channels.Channel],
// Chunker and Resolver over
// [github.com/slimslenderslacks/work/internal/mcpgateway]'s DaemonGateway and
// DaemonSession, and borrows the message chunker from
// [github.com/slimslenderslacks/work/internal/channels/whatsapp]. Registered
// by cmd/orch. See docs/channels.md.
package slack
