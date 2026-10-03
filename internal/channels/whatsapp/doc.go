// Package whatsapp holds the WhatsApp-specific pieces of the messaging
// channels: today, the identity helpers and the AccessPolicy that decides
// which inbound messages may reach the daemon's agents.
//
// It ports hermes-agent's gateway/whatsapp_identity.py, the allowlist and
// gating half of gateway/platforms/whatsapp_common.py, and the bridge's
// allowlist.js / owner_message_gate.js. The identity helpers and AccessPolicy
// are pure: no network, no transport, so every backend shares them.
//
// BridgeChannel is the Baileys backend (personal number, QR pairing, no public
// webhook): it supervises the vendored Node bridge from the bridge
// sub-package and feeds every inbound message through the AccessPolicy. The
// backend is picked per channel with `options.mode: cloud | bridge` (cloud is
// the default); see Factory and docs/whatsapp-bridge.md.
//
// The one rule that shapes everything here is fail closed: this channel
// drives an agent that can read orch state, so a zero-value config, an empty
// allowlist, a missing self id, or an unknown chat type all deny.
package whatsapp
