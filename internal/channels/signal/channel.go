package signal

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/slimslenderslacks/work/internal/channels"
	"github.com/slimslenderslacks/work/internal/channels/whatsapp"
)

const (
	// MaxMessageLength is the longest text (in characters) sent as one Signal message.
	MaxMessageLength = 8000

	sseRetryInitial = 2 * time.Second
	sseRetryMax     = 60 * time.Second
	// sendGap is the minimum spacing between sends on one channel.
	sendGap = 200 * time.Millisecond

	maxSeen     = 1024 // remembered inbound message keys, for dedupe
	maxSentTS   = 512  // remembered outbound timestamps, for echo suppression
	maxSSELine  = 16 << 20
	checkTimout = 5 * time.Second
)

// Channel is the Signal transport. Build it with New or Factory.
type Channel struct {
	name   string
	cfg    Config
	policy *AccessPolicy
	log    *slog.Logger
	rpc    *rpcClient
	stream *http.Client // SSE: no overall timeout
	pace   *pacer

	// Seams for tests.
	retryInitial, retryMax time.Duration
	maxLen                 int
	sleep                  func(ctx context.Context, d time.Duration) error

	mu       sync.Mutex
	started  bool
	closed   bool
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	handler  channels.InboundHandler
	runCtx   context.Context
	ownUUID  string
	seen     map[string]struct{}
	seenQ    []string
	sent     map[string]struct{} // outbound timestamps awaiting their sync echo
	sentQ    []string
	inflight int       // sends to our own chat currently running
	deferred []pending // note-to-self messages held while inflight > 0
}

// pending is a note-to-self message held back until in-flight sends to the
// same chat have recorded their timestamps, so an echo of our own reply that
// races the send's response is not mistaken for the owner typing.
type pending struct {
	ts      string
	deliver func()
}

var (
	_ channels.Channel = (*Channel)(nil)
	_ channels.Typer   = (*Channel)(nil)
	_ channels.Chunker = (*Channel)(nil)
)

// New builds the channel from a validated Config.
func New(name string, cfg Config, logger *slog.Logger) *Channel {
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("channel", name)
	c := &Channel{
		name:         name,
		cfg:          cfg,
		policy:       NewAccessPolicy(cfg, logger),
		log:          logger,
		rpc:          &rpcClient{base: cfg.HTTPURL, hc: &http.Client{}},
		stream:       &http.Client{},
		pace:         newPacer(sendGap),
		retryInitial: sseRetryInitial,
		retryMax:     sseRetryMax,
		maxLen:       MaxMessageLength,
		sleep:        sleepCtx,
		seen:         map[string]struct{}{},
		sent:         map[string]struct{}{},
	}
	return c
}

// Factory returns the channels.Factory for `type: signal`.
func Factory(logger *slog.Logger) channels.Factory {
	return func(name string, spec channels.ChannelConfig, creds channels.Credentials) (channels.Channel, error) {
		cfg, err := ParseOptions(spec.Options, creds)
		if err != nil {
			return nil, err
		}
		return New(name, cfg, logger), nil
	}
}

// Name implements channels.Channel.
func (c *Channel) Name() string { return c.name }

// Policy exposes the access policy (for status reporting and tests).
func (c *Channel) Policy() *AccessPolicy { return c.policy }

// Start connects to the signal-cli daemon's event stream and returns at once;
// the stream is consumed, and reconnected with backoff, until ctx ends or Close
// is called. A daemon that is not up yet is not an error: it is logged and the
// reconnect loop keeps trying.
func (c *Channel) Start(ctx context.Context, h channels.InboundHandler) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("signal: channel closed")
	}
	if c.started {
		return errors.New("signal: already started")
	}
	c.started = true
	c.handler = h
	runCtx, cancel := context.WithCancel(ctx)
	c.runCtx, c.cancel = runCtx, cancel

	if err := c.check(runCtx); err != nil {
		c.log.Warn("signal: signal-cli daemon not reachable yet; will keep retrying", "url", c.cfg.HTTPURL, "err", err.Error())
	}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.listen(runCtx)
	}()
	return nil
}

// check probes signal-cli's health endpoint.
func (c *Channel) check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, checkTimout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.HTTPURL+"/api/v1/check", nil)
	if err != nil {
		return err
	}
	resp, err := c.rpc.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// Close stops the event stream and waits for it. Idempotent.
func (c *Channel) Close() error {
	c.mu.Lock()
	c.closed = true
	cancel := c.cancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	c.wg.Wait()
	return nil
}

// listen runs the SSE consumer with jittered exponential backoff.
func (c *Channel) listen(ctx context.Context) {
	backoff := c.retryInitial
	for ctx.Err() == nil {
		connected, err := c.consume(ctx)
		if ctx.Err() != nil {
			return
		}
		if connected {
			backoff = c.retryInitial
		}
		if err != nil {
			c.log.Warn("signal: event stream ended", "err", err.Error(), "retry_in", backoff.String())
		}
		wait := backoff + time.Duration(float64(backoff)*0.2*rand.Float64())
		if c.sleep(ctx, wait) != nil {
			return
		}
		backoff = min(backoff*2, c.retryMax)
	}
}

// consume reads one SSE connection to its end. connected reports whether the
// daemon accepted the connection (so the caller resets its backoff).
func (c *Channel) consume(ctx context.Context) (connected bool, err error) {
	u := c.cfg.HTTPURL + "/api/v1/events?account=" + url.QueryEscape(c.cfg.Account)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.stream.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("events: HTTP %d", resp.StatusCode)
	}
	c.log.Info("signal: event stream connected")

	r := bufio.NewReaderSize(resp.Body, 64<<10)
	for {
		line, rerr := readLine(r)
		line = strings.TrimRight(line, "\r\n")
		// signal-cli emits one event per "data:" line; comments (":") are keepalives.
		if payload, ok := strings.CutPrefix(line, "data:"); ok {
			if payload = strings.TrimSpace(payload); payload != "" {
				c.handleData(ctx, []byte(payload))
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return true, errors.New("stream closed by daemon")
			}
			return true, rerr
		}
	}
}

func readLine(r *bufio.Reader) (string, error) {
	var sb strings.Builder
	for {
		part, isPrefix, err := r.ReadLine()
		sb.Write(part)
		if sb.Len() > maxSSELine {
			return "", errors.New("signal: SSE line too long")
		}
		if err != nil || !isPrefix {
			return sb.String(), err
		}
	}
}

// handleData processes one SSE payload.
func (c *Channel) handleData(ctx context.Context, data []byte) {
	env, err := unwrapEnvelope(data)
	if err != nil {
		c.log.Debug("signal: ignoring undecodable event", "err", err.Error())
		return
	}
	if env == nil {
		return
	}
	c.mu.Lock()
	own := c.ownUUID
	c.mu.Unlock()
	p, learned := parseEnvelope(env, c.cfg.Account, own)
	if learned != "" {
		c.mu.Lock()
		c.ownUUID = learned
		c.mu.Unlock()
	}
	if p == nil {
		return
	}
	p.in.Channel = c.name

	deliver := func() { c.admit(ctx, p) }
	if p.noteToSelf {
		c.mu.Lock()
		if c.inflight > 0 {
			c.deferred = append(c.deferred, pending{ts: p.timestamp, deliver: deliver})
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
		if c.consumeSent(p.timestamp) {
			return // our own reply coming back
		}
	}
	deliver()
}

// admit dedupes, applies the access policy and hands the message to the handler.
func (c *Channel) admit(ctx context.Context, p *parsed) {
	if c.alreadySeen(p.in.ChatID + "|" + p.in.SenderID + "|" + p.timestamp) {
		return
	}
	if d := c.policy.Decide(p.in); !d.Allowed {
		return
	}
	c.mu.Lock()
	h := c.handler
	c.mu.Unlock()
	if h != nil {
		h(ctx, p.in.InboundMessage)
	}
}

func (c *Channel) alreadySeen(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.seen[key]; ok {
		return true
	}
	c.seen[key] = struct{}{}
	c.seenQ = append(c.seenQ, key)
	if len(c.seenQ) > maxSeen {
		delete(c.seen, c.seenQ[0])
		c.seenQ = c.seenQ[1:]
	}
	return false
}

func (c *Channel) recordSent(ts string) {
	if ts == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.sent[ts]; ok {
		return
	}
	c.sent[ts] = struct{}{}
	c.sentQ = append(c.sentQ, ts)
	if len(c.sentQ) > maxSentTS {
		delete(c.sent, c.sentQ[0])
		c.sentQ = c.sentQ[1:]
	}
}

// consumeSent reports (and forgets) whether ts is one of our own outbound timestamps.
func (c *Channel) consumeSent(ts string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.sent[ts]; !ok {
		return false
	}
	delete(c.sent, ts)
	return true
}

// ChunkMessage implements channels.Chunker: pieces of at most MaxMessageLength
// characters, split on paragraph, line, then word boundaries.
func (c *Channel) ChunkMessage(text string) []string {
	return whatsapp.ChunkMessage(text, c.maxLen)
}

// Typing implements channels.Typer.
func (c *Channel) Typing(ctx context.Context, chatID string) error {
	params := c.target(map[string]any{"account": c.cfg.Account}, chatID)
	_, err := c.rpc.call(ctx, "sendTyping", params)
	return err
}

// target adds the routing key for chatID: a group id or a recipient.
func (c *Channel) target(params map[string]any, chatID string) map[string]any {
	if g, ok := strings.CutPrefix(chatID, GroupPrefix); ok {
		params["groupId"] = g
	} else {
		params["recipient"] = []string{chatID}
	}
	return params
}

// Send implements channels.Channel. The Topic becomes a "[topic] " prefix,
// Markdown becomes Signal text styles (unless plain_text), long text is sent as
// several messages, and the Signal timestamp of the first one is returned as
// the message id.
func (c *Channel) Send(ctx context.Context, msg channels.OutboundMessage) (string, error) {
	if strings.TrimSpace(msg.ChatID) == "" {
		return "", errors.New("signal: send: empty chat id")
	}
	text := channels.FormatTopic(msg.Topic, msg.Text)
	if strings.TrimSpace(text) == "" {
		return "", nil
	}
	if c.isSelf(msg.ChatID) {
		c.mu.Lock()
		c.inflight++
		c.mu.Unlock()
		defer c.finishSelfSend()
	}

	var first string
	for i, chunk := range c.ChunkMessage(text) {
		params := c.target(map[string]any{"account": c.cfg.Account}, msg.ChatID)
		body, styles := chunk, []string(nil)
		if !c.cfg.PlainText {
			body, styles = MarkdownToSignal(chunk)
		}
		if strings.TrimSpace(body) == "" {
			continue
		}
		params["message"] = body
		switch len(styles) {
		case 0:
		case 1:
			params["textStyle"] = styles[0]
		default:
			params["textStyles"] = styles
		}
		if i == 0 {
			c.quote(params, msg)
		}
		ts, err := c.sendOne(ctx, params)
		if err != nil {
			return first, err
		}
		c.recordSent(ts)
		if first == "" {
			first = ts
		}
	}
	return first, nil
}

// isSelf reports whether chatID is the account's own "Note to Self" chat.
func (c *Channel) isSelf(chatID string) bool {
	n, ok := NormalizeNumber(chatID)
	return ok && n == c.cfg.Account
}

// finishSelfSend ends an in-flight send to our own chat and releases the
// note-to-self messages held meanwhile, dropping any that were our own echoes.
func (c *Channel) finishSelfSend() {
	c.mu.Lock()
	c.inflight--
	var release []pending
	if c.inflight == 0 {
		release, c.deferred = c.deferred, nil
	}
	c.mu.Unlock()
	for _, p := range release {
		if !c.consumeSent(p.ts) {
			p.deliver()
		}
	}
}

// quote adds quote parameters when replying to a message whose author is known
// (a direct chat: the author is the chat itself).
func (c *Channel) quote(params map[string]any, msg channels.OutboundMessage) {
	ts, err := strconv.ParseInt(strings.TrimSpace(msg.ReplyToID), 10, 64)
	if err != nil || ts <= 0 || strings.HasPrefix(msg.ChatID, GroupPrefix) {
		return
	}
	params["quoteTimestamp"] = ts
	params["quoteAuthor"] = msg.ChatID
}

// sendOne performs one `send` with pacing and a single rate-limit retry.
func (c *Channel) sendOne(ctx context.Context, params map[string]any) (string, error) {
	var lastErr error
	for attempt := 0; attempt < maxSendAttempts; attempt++ {
		if err := c.pace.wait(ctx); err != nil {
			return "", err
		}
		raw, err := c.rpc.call(ctx, "send", params)
		if err == nil {
			ts, perr := parseSendResult(raw)
			if perr == nil {
				return ts, nil
			}
			err = perr
		}
		var rl *rateLimitError
		if !errors.As(err, &rl) {
			return "", err
		}
		lastErr = err
		wait := rl.retryAfter
		if wait <= 0 {
			wait = defaultRetryAfter
		}
		if wait > maxRetryAfter {
			return "", fmt.Errorf("%w (retry after %s is too long)", err, wait)
		}
		c.pace.backoff(wait)
		c.log.Warn("signal: rate limited", "retry_after", wait.String())
	}
	return "", lastErr
}
