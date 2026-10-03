package whatsapp

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/slimslenderslacks/work/internal/channels"
)

// Credential names, under `credentials:` in the channel config.
const (
	CredAccessToken = "access_token" // required
	CredAppSecret   = "app_secret"   // webhook signature verification
	CredVerifyToken = "verify_token" // webhook hub.challenge handshake
)

// Cloud backend option keys, under `options:` in the channel config. The
// `mode` and `access` keys are shared with the bridge backend and read
// elsewhere; unrecognised keys are ignored here because the webhook half adds
// its own.
const (
	OptPhoneNumberID = "phone_number_id"
	OptWABAID        = "waba_id"
	OptAPIVersion    = "api_version"
	OptGraphBaseURL  = "graph_base_url"
)

// lastInboundCap bounds the chat -> latest inbound wamid cache.
const lastInboundCap = 1000

// Channel is the WhatsApp Cloud API backend: a channels.Channel that sends
// through a CloudClient. The webhook (inbound) half is not wired up yet;
// Start only records the handler.
//
//	channels:
//	  whatsapp:
//	    type: whatsapp
//	    credentials:
//	      access_token: {env: WHATSAPP_ACCESS_TOKEN}
//	    options:
//	      phone_number_id: "109876543210987"
//	      # api_version: v20.0
//
// Remember the 24-hour customer-service window (ErrOutsideServiceWindow):
// notifications to a chat that has not written in a day fail with a typed
// error rather than being silently dropped.
type Channel struct {
	name   string
	client *CloudClient
	log    *slog.Logger

	mu      sync.Mutex
	handler channels.InboundHandler
	closed  bool
	// Latest inbound wamid per chat: Meta's typing and read-receipt calls need
	// a message id to attach to. Fed by the webhook through NoteInbound.
	inbound map[string]*list.Element // chat id -> element holding inboundEntry
	order   *list.List               // most recently noted first
}

type inboundEntry struct{ chat, wamid string }

var _ channels.Channel = (*Channel)(nil)

// NewChannel builds the Cloud channel named name around client.
func NewChannel(name string, client *CloudClient, logger *slog.Logger) *Channel {
	if logger == nil {
		logger = slog.Default()
	}
	return &Channel{
		name:    name,
		client:  client,
		log:     logger.With("channel", name),
		inbound: map[string]*list.Element{},
		order:   list.New(),
	}
}

// CloudConfigFromSpec builds the CloudConfig for a channel spec: ids and
// versions from options, secrets from the resolved credentials.
func CloudConfigFromSpec(spec channels.ChannelConfig, creds channels.Credentials) (CloudConfig, error) {
	cfg := CloudConfig{
		AccessToken: creds[CredAccessToken],
		AppSecret:   creds[CredAppSecret],
		VerifyToken: creds[CredVerifyToken],
	}
	var errs []error
	for key, dst := range map[string]*string{
		OptPhoneNumberID: &cfg.PhoneNumberID,
		OptWABAID:        &cfg.WABAID,
		OptAPIVersion:    &cfg.APIVersion,
		OptGraphBaseURL:  &cfg.GraphBaseURL,
	} {
		v, err := stringOption(spec.Options, key)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		*dst = v
	}
	if err := errors.Join(errs...); err != nil {
		return CloudConfig{}, err
	}
	return cfg, nil
}

// stringOption reads options[key] as text. YAML turns an unquoted
// phone_number_id into an int, so numbers are accepted and rendered in full.
func stringOption(options map[string]any, key string) (string, error) {
	raw, ok := options[key]
	if !ok || raw == nil {
		return "", nil
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v), nil
	case int, int64, uint64:
		return fmt.Sprint(v), nil
	}
	return "", fmt.Errorf("whatsapp: options.%s must be a string", key)
}

// CloudFactory returns the channels.Factory for the Cloud backend; pass it as
// FactoryDeps.Cloud so `type: whatsapp` with the default `mode: cloud` builds
// a Channel. Bad config fails here, at construction, not on the first send.
func CloudFactory(logger *slog.Logger, opts ...CloudOption) channels.Factory {
	return func(name string, spec channels.ChannelConfig, creds channels.Credentials) (channels.Channel, error) {
		cfg, err := CloudConfigFromSpec(spec, creds)
		if err != nil {
			return nil, err
		}
		opts := append([]CloudOption{WithCloudLogger(logger)}, opts...)
		client, err := NewCloudClient(cfg, opts...)
		if err != nil {
			return nil, err
		}
		return NewChannel(name, client, logger), nil
	}
}

// Name implements channels.Channel.
func (c *Channel) Name() string { return c.name }

// Client exposes the underlying CloudClient (for `whatsapp test` style
// tooling and the webhook half).
func (c *Channel) Client() *CloudClient { return c.client }

// Start implements channels.Channel. The inbound webhook is not implemented
// yet, so it records the handler and returns nil; the channel can send.
func (c *Channel) Start(_ context.Context, h channels.InboundHandler) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return channels.ErrNotRunning
	}
	c.handler = h
	return nil
}

// Send implements channels.Channel. It returns the wamid of the last chunk
// when the text was long enough to be split. On a partial failure the error
// is returned and no id, but chunks already delivered stay delivered.
func (c *Channel) Send(ctx context.Context, msg channels.OutboundMessage) (string, error) {
	if c.isClosed() {
		return "", channels.ErrNotRunning
	}
	ids, err := c.client.SendText(ctx, msg.ChatID, channels.FormatTopic(msg.Topic, msg.Text), msg.ReplyToID)
	if err != nil {
		return "", err
	}
	if len(ids) == 0 {
		return "", nil
	}
	return ids[len(ids)-1], nil
}

// Typing shows the typing indicator in chatID, against the latest inbound
// message noted for it. With none noted (nothing received since the daemon
// started) it does nothing. Best effort.
func (c *Channel) Typing(ctx context.Context, chatID string) error {
	if c.isClosed() {
		return channels.ErrNotRunning
	}
	wamid := c.lastInbound(chatID)
	if wamid == "" {
		return nil
	}
	return c.client.Typing(ctx, wamid)
}

// MarkRead marks wamid read. Best effort.
func (c *Channel) MarkRead(ctx context.Context, wamid string) error {
	if c.isClosed() {
		return channels.ErrNotRunning
	}
	return c.client.MarkRead(ctx, wamid)
}

// NoteInbound records wamid as the latest message received from chatID, for Typing.
func (c *Channel) NoteInbound(chatID, wamid string) {
	chatID = recipientKey(chatID)
	if chatID == "" || wamid == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.inbound[chatID]; ok {
		el.Value = inboundEntry{chatID, wamid}
		c.order.MoveToFront(el)
		return
	}
	c.inbound[chatID] = c.order.PushFront(inboundEntry{chatID, wamid})
	for c.order.Len() > lastInboundCap {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.inbound, oldest.Value.(inboundEntry).chat)
	}
}

func (c *Channel) lastInbound(chatID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.inbound[recipientKey(chatID)]; ok {
		return el.Value.(inboundEntry).wamid
	}
	return ""
}

// recipientKey makes "+1 555 123 4567" and "15551234567@s.whatsapp.net" the same chat.
func recipientKey(chatID string) string {
	id, err := cloudRecipient(chatID)
	if err != nil {
		return ""
	}
	return id
}

func (c *Channel) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// Close implements channels.Channel. Idempotent.
func (c *Channel) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.handler = nil
	return nil
}
