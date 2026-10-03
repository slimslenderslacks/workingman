// Package channels is the messaging-channel foundation for the workingman
// daemon: a Channel interface that concrete transports (WhatsApp, …) implement,
// a Registry that owns the configured channels and fans messages out to them,
// a YAML config loader with topic routes, and a notify adapter so the daemon
// can notify channels through its existing notify.Sender plumbing.
//
// Nothing in this package talks to a network. Concrete channels live in
// sub-packages and plug in through a Factory registered per channel type.
package channels

import (
	"context"
	"strings"
	"time"
)

// Channel is one bidirectional messaging transport.
//
// Start must return promptly once the channel is ready (connected, webhook
// listener bound, …) and then deliver inbound messages to the handler from
// its own goroutines until ctx is cancelled or Close is called. Send must be
// safe for concurrent use. Close releases resources and must be idempotent.
type Channel interface {
	// Name is the unique instance name from the config (e.g. "whatsapp"),
	// not the transport type.
	Name() string
	Start(ctx context.Context, h InboundHandler) error
	// Send delivers msg and returns the transport's message ID for it.
	Send(ctx context.Context, msg OutboundMessage) (messageID string, err error)
	Close() error
}

// InboundHandler receives messages arriving on a channel. Implementations
// must not block for long; the Registry recovers handler panics so a bad
// handler cannot take down a channel's receive loop.
type InboundHandler func(ctx context.Context, msg InboundMessage)

// InboundMessage is a message received from a chat.
type InboundMessage struct {
	Channel    string // channel instance name; the Registry fills it in if empty
	ChatID     string
	SenderID   string
	SenderName string
	MessageID  string
	ReplyToID  string
	Text       string
	Timestamp  time.Time
}

// OutboundMessage is a message to send to a chat.
type OutboundMessage struct {
	ChatID    string
	Text      string
	ReplyToID string
	// Topic is a free-form tag ("wolf", "workingman", "daemon"). Channels
	// with thread support may use it to pick a thread; channels without
	// (WhatsApp) render it as a "[wolf]" style prefix via FormatTopic.
	Topic string
}

// FormatTopic renders text with a "[topic] " prefix, for channels that have
// no threads. An empty topic returns text unchanged.
func FormatTopic(topic, text string) string {
	topic = strings.TrimSpace(topic)
	if topic == "" {
		return text
	}
	return "[" + topic + "] " + text
}

// Chunker is implemented by channels that want long text split by the sender
// rather than inside Send: the inbound-message router sends each returned piece
// as its own message. Channels that already split inside Send (WhatsApp Cloud)
// do not implement it and receive the whole reply.
type Chunker interface {
	ChunkMessage(text string) []string
}

// Typer is implemented by channels that can show a "typing…" indicator in a
// chat. It is best effort; the router ignores errors.
type Typer interface {
	Typing(ctx context.Context, chatID string) error
}
