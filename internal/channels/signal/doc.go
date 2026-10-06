// Package signal is the Signal channel: a channels.Channel that talks to a
// signal-cli daemon running in HTTP mode (`signal-cli -a +15551234567 daemon
// --http 127.0.0.1:8080`). Inbound messages arrive over the daemon's SSE
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
package signal
