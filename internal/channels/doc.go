// Package channels is the messaging-channel foundation for the workingman
// daemon: the Channel interface transports implement, the Registry that runs
// them, and the channels.yaml config they are built from.
//
// Role: daemon
//
// Nothing in this package talks to a network. Concrete transports (WhatsApp,
// Signal) live in sub-packages and plug in through a [Factory] registered per
// channel type. The package provides:
//
//   - the [Channel] interface and its message types, plus the optional
//     [Chunker] and [Typer] capabilities;
//   - a YAML config loader for ~/.workingman/channels.yaml (or
//     $WORKINGMAN_CHANNELS_CONFIG): channel instances, topic [Route]s, notify
//     and router settings. Credentials are never inlined; they are references
//     to environment variables or a 0600 secrets file, resolved into [Secret]
//     values that redact themselves in fmt, JSON and slog;
//   - a [Registry] that builds, starts and closes the channels, delivers
//     inbound messages to one [InboundHandler], and fans topic messages out to
//     every matching route concurrently;
//   - a [Notifier] that adapts the Registry to notify.Sender, so the daemon's
//     existing notification plumbing reaches chats;
//   - a [ConversationIndex] that remembers which wolf session a sent
//     notification belongs to, so a reply to it can be routed back. The file
//     implementation persists it as JSON.
//
// # Key types
//
//   - [Channel] — one bidirectional transport; implemented by signal.Channel,
//     whatsapp.Channel, whatsapp.BridgeChannel and channeltest.Fake.
//   - [InboundMessage] / [OutboundMessage] / [InboundHandler] — what a channel
//     receives and sends, and the callback it hands inbound messages to
//     (router.Router in production).
//   - [Registry] — owns the configured channels; built with [Build] or
//     [BuildFromFile]; Send, SendTopic, Start and Close.
//   - [Factory] — builds a Channel of one transport type from its
//     [ChannelConfig] and resolved [Credentials].
//   - [Config] / [ChannelConfig] / [Route] / [CredentialRef] — the parsed
//     channels.yaml; [Load], [LoadOptional] and [Parse] read and validate it.
//   - [Secret] — a credential value that never prints itself; call Reveal at
//     the point of use.
//   - [Notifier] — notify.Sender and notify.TopicSender over a Registry.
//   - [ConversationIndex] / [FileConversationIndex] / [ConversationTarget] —
//     maps a channel message ID to the wolf session it came from.
//
// # Collaborators
//
// Uses [github.com/slimslenderslacks/work/internal/notify] for the Sender
// interfaces Notifier implements. Transports in internal/channels/signal and
// internal/channels/whatsapp implement Channel and provide Factories;
// internal/router is the InboundHandler; internal/daemon sends topic
// notifications through the Registry and records them in the
// ConversationIndex; cmd/orch wires it all together and the setup
// sub-packages edit the config. See docs/channels.md.
package channels
