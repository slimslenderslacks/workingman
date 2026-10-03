// Package bridge runs and talks to the vendored Baileys WhatsApp bridge: the
// "personal number, QR pairing, no public webhook" WhatsApp backend.
//
// It owns everything about the Node side and nothing about WhatsApp policy:
//
//   - assets/ holds the vendored bridge (see assets/NOTICE.md), embedded in the
//     daemon binary and extracted to an install directory on demand (Install).
//   - FindNode checks that node >= 18 is available and explains what to do if not.
//   - Supervisor keeps a bridge process alive with exponential backoff.
//   - Client speaks the bridge's loopback HTTP API.
//
// The channel that ties these together and applies the AccessPolicy is
// whatsapp.BridgeChannel.
package bridge
