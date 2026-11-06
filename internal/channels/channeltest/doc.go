// Package channeltest provides an in-memory fake channels.Channel for tests of
// code that sends to, or receives from, a channel.
//
// Role: daemon (test helper)
//
// The fake records every Send, lets a test inject inbound messages into the
// handler passed to Start, and can be scripted to fail or panic, so tests of
// the Registry, the router and the daemon's channel notifications need no
// network or real transport. Import it only from _test.go files.
//
// # Key types
//
//   - [Fake] — a scriptable Channel: StartErr / SendErr / CloseErr, SendFunc
//     and PanicOnSend script failures; [Fake.Inject] delivers an inbound
//     message; [Fake.Sent], [Fake.StartCount] and [Fake.CloseCount] inspect
//     what happened.
//   - [New] — builds a Fake with the given instance name.
//
// # Collaborators
//
// Implements [github.com/slimslenderslacks/work/internal/channels.Channel].
// Used by the tests of internal/channels, internal/router and internal/daemon.
package channeltest
