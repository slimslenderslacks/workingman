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
// Channel is the Cloud API backend (Meta Graph API). CloudClient is its
// outbound half: SendText converts Markdown to WhatsApp markup (FormatMessage),
// splits long text under 4096 characters (ChunkMessage), retries 429/5xx with
// bounded backoff, and reports Graph errors as *GraphError. A send outside the
// 24-hour customer-service window satisfies errors.Is(err,
// ErrOutsideServiceWindow) so callers can fall back. The access token is
// redacted from every error and log line. The inbound webhook is separate.
// It ports hermes-agent's gateway/platforms/whatsapp_cloud.py (send path) and
// the formatting half of whatsapp_common.py.
//
// The one rule that shapes everything here is fail closed: this channel
// drives an agent that can read orch state, so a zero-value config, an empty
// allowlist, a missing self id, or an unknown chat type all deny.
package whatsapp
