// Package notify is the daemon's user-notification interface: how an agent
// that needs human attention reaches a person.
//
// Role: daemon
//
// Today the caller is the daemon, when a project becomes blocked and the wolf
// agent needs a human. Sender is intentionally a
// one-method interface, so production can use the Osascript implementation,
// tests can use a Recorder, and messaging channels plug in without touching
// the daemon. Topics ("wolf", "workingman", "daemon") are optional: a
// TopicSender receives them, and [SendTopic] falls back to a plain Send for
// senders that do not understand them.
//
// In production cmd/orch builds Multi{Osascript, Async{channels.Notifier}}: the
// macOS alert and the messaging channels both get every notification, and a
// slow network channel cannot stall the daemon's dispatch loop.
//
// # Key types
//
//   - [Sender] — the one-method notification interface the daemon holds.
//   - [TopicSender] — a Sender that also accepts a routing topic; use
//     [SendTopic] to call either kind.
//   - [Osascript] — macOS display-notification via osascript.
//   - [Multi] — fans out to several senders concurrently, isolating failures
//     and panics.
//   - [Async] — makes Send return immediately and reports delivery errors to a
//     callback.
//   - [Noop] — the daemon's default when no notifier is configured.
//   - [Recorder] / [Call] — a test sender that captures calls.
//
// # Collaborators
//
// Has no internal dependencies. Used by internal/daemon (WithNotifier, its
// notifier field), by internal/channels (Notifier implements Sender and
// TopicSender over a channel Registry) and by cmd/orch, which assembles the
// sender chain. See docs/wolf-notify.md.
package notify
