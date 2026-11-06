// Package bridge runs and talks to the vendored Baileys WhatsApp bridge: the
// "personal number, QR pairing, no public webhook" WhatsApp backend.
//
// Role: daemon
//
// It owns everything about the Node side and nothing about WhatsApp policy:
//
//   - assets/ holds the vendored bridge (see assets/NOTICE.md), embedded in the
//     daemon binary and extracted to an install directory on demand (Install).
//   - FindNode checks that node >= 18 is available and explains what to do if not.
//   - Supervisor keeps a bridge process alive with exponential backoff.
//   - Client speaks the bridge's loopback HTTP API.
//
// # Key types
//
//   - [Supervisor] / [SupervisorStatus] / [Backoff] — runs one bridge at a time
//     and restarts it when it exits, until logged out.
//   - [Process] / [Starter] / [Launch] — one running bridge, how to start one,
//     and the real node invocation; tests substitute fake Starters.
//   - [Client] — the loopback HTTP API: health, long-polled [Event] batches, send,
//     typing, mark-read; failures are [APIError].
//   - [Install] / [Extract] / [Ready] — put the embedded assets on disk and
//     run npm install.
//   - [FindNode] / [FindNPM] — locate the Node toolchain.
//
// # Collaborators
//
// Has no internal dependencies. The channel that ties these together and
// applies the AccessPolicy is whatsapp.BridgeChannel in
// internal/channels/whatsapp, its only user. See docs/whatsapp-bridge.md.
package bridge
