// Package whatsapp is the WhatsApp messaging channel: the Cloud API and
// Baileys bridge backends, plus the identity helpers and AccessPolicy they
// share.
//
// Role: daemon
//
// The AccessPolicy decides which inbound messages may reach the daemon's
// agents. It ports hermes-agent's gateway/whatsapp_identity.py, the allowlist and
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
// redacted from every error and log line.
//
// Channel.Start is the inbound half (cloud_webhook.go): an http.Server, bound
// to 127.0.0.1:8090 by default and meant to sit behind a tunnel
// (cloudflared, ngrok, ...), that answers Meta's hub.challenge handshake,
// verifies X-Hub-Signature-256 over the raw body, caps the body at 3 MiB,
// acknowledges with 200 and processes asynchronously, de-duplicates by wamid,
// and runs each text-bearing message through the AccessPolicy before the
// InboundHandler. It refuses to start without an app secret unless
// options.insecure_skip_signature is set. It ports hermes-agent's
// gateway/platforms/whatsapp_cloud.py (send and webhook paths) and the
// formatting half of whatsapp_common.py.
//
// The setup sub-package backs `orch whatsapp setup|status|test`: field
// validators, the channels.yaml/secrets-file writer and the Meta console
// instructions. CloudClient.PhoneNumberInfo is the Graph check they share.
//
// The one rule that shapes everything here is fail closed: this channel
// drives an agent that can read orch state, so a zero-value config, an empty
// allowlist, a missing self id, or an unknown chat type all deny.
//
// # Key types
//
//   - [Factory] / [FactoryDeps] — the channels.Factory cmd/orch registers for
//     type "whatsapp"; it picks the [Backend] from options.mode. [CloudFactory]
//     builds the Cloud backend alone.
//   - [Channel] — the Cloud API backend (webhook in, [CloudClient] out);
//     built with [NewChannel].
//   - [BridgeChannel] — the Baileys backend over a QR-paired personal number;
//     [Pair] / [PairOptions] run the pairing for `orch whatsapp pair`.
//   - [CloudClient] / [CloudConfig] / [GraphError] — the Graph API sender and
//     its typed errors; [IsOutsideServiceWindow] detects the 24-hour window.
//   - [AccessConfig] / [AccessPolicy] / [Decision] / [Reason] — the
//     fail-closed admission policy every [Inbound] message passes; [Mode]
//     chooses bot or self-chat.
//   - [Resolver] / [DirResolver] / [StaticResolver] — phone-number <-> LID
//     aliasing used by [Canonical] and [Expand] to match ids.
//
// # Collaborators
//
// Implements [github.com/slimslenderslacks/work/internal/channels.Channel];
// BridgeChannel drives the Node process through
// [github.com/slimslenderslacks/work/internal/channels/whatsapp/bridge]. Used
// by cmd/orch, by internal/channels/whatsapp/setup (validation and the Graph
// check) and by internal/channels/signal (ChunkMessage). See
// docs/channels.md and docs/whatsapp-bridge.md.
package whatsapp
