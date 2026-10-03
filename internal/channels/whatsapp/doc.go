// Package whatsapp holds the WhatsApp-specific pieces of the messaging
// channels: today, the identity helpers and the AccessPolicy that decides
// which inbound messages may reach the daemon's agents.
//
// It ports hermes-agent's gateway/whatsapp_identity.py, the allowlist and
// gating half of gateway/platforms/whatsapp_common.py, and the bridge's
// allowlist.js / owner_message_gate.js. It is a pure library: no network, no
// HTTP client, no transport. It only needs the types in internal/channels,
// so the Cloud API client and any bridge adapter can share it.
//
// The one rule that shapes everything here is fail closed: this channel
// drives an agent that can read orch state, so a zero-value config, an empty
// allowlist, a missing self id, or an unknown chat type all deny.
package whatsapp
