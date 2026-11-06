// Package signal is the Signal channel: a channels.Channel that talks to a
// signal-cli daemon running in HTTP mode.
//
// Role: daemon
//
// The signal-cli daemon is started separately (`signal-cli -a +15551234567
// daemon --http 127.0.0.1:8080`). Inbound messages arrive over the daemon's SSE
// stream (/api/v1/events); outbound messages are JSON-RPC 2.0 calls to
// /api/v1/rpc. It mirrors hermes-agent's gateway/platforms/signal.py.
//
// Configuration (channels.yaml):
//
//	channels:
//	  signal:
//	    type: signal
//	    credentials:                       # optional: keep the number out of the file
//	      account: {env: SIGNAL_ACCOUNT}
//	    options:
//	      http_url: http://127.0.0.1:8080  # default
//	      account: "+15551234567"          # or credentials.account
//	      access:
//	        allow_from: ["+15557654321"]   # empty => nobody (fail closed)
//	        # allow_all: true              # DANGEROUS opt-in; logs a loud warning
//	        # note_to_self: true           # admit the account's own "Note to Self"
//	        # groups: true
//	        # group_allow_from: ["<base64 group id>"]
//	      # plain_text: true               # never send Signal text styles
//
// Access is default-deny, like the WhatsApp channel: the zero policy admits no
// one, a denial carries a stable Reason and never logs message text, and group
// messages are ignored unless groups are enabled and the group is listed.
//
// Chats are addressed as an E.164 number or UUID for a direct chat and
// "group:<groupId>" for a group. A message's ID is its Signal timestamp, which
// is also what Send returns, so a quoted reply (quote.id) maps straight back to
// the notification it answers.
//
// # Key types
//
//   - [Channel] — the Signal transport; built by [Factory] (registered under
//     [Type] in cmd/orch) or [New]. Start consumes the SSE stream, Send makes
//     JSON-RPC calls.
//   - [Options] / [Config] — the raw `options:` block and its validated form;
//     [ParseOptions] turns one into the other.
//   - [AccessConfig] / [AccessPolicy] / [Decision] / [Reason] — the
//     default-deny admission policy every inbound [Inbound] message passes.
//   - [Probe] / [Info] and [SendOnce] — a reachability check and a one-shot
//     send, used by `orch signal status` and `orch signal test`.
//   - [MarkdownToSignal] — converts Markdown to Signal text plus style ranges.
//
// # Collaborators
//
// Implements [github.com/slimslenderslacks/work/internal/channels.Channel]
// and borrows the message chunker from
// [github.com/slimslenderslacks/work/internal/channels/whatsapp]. Used by
// cmd/orch (the factory and the `orch signal` commands) and by
// internal/channels/signal/setup. See docs/signal.md.
package signal
