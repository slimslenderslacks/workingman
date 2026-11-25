package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/slimslenderslacks/work/internal/channels"
	"github.com/slimslenderslacks/work/internal/channels/whatsapp"
	"github.com/slimslenderslacks/work/internal/mcpgateway"
)

const (
	// MaxMessageLength is the longest text (in characters) sent as one Slack
	// message; the MCP caps a text element at 5000.
	MaxMessageLength = 4000

	// GatewayKey is the daemon gateway record key of the channel's
	// sub-gateway, and GatewayServer the one catalog server it is minted over.
	GatewayKey    = "orch-slack"
	GatewayServer = "slack"

	// postOfficeTopic is the Dispatcher's topic (postoffice.Topic); its text is
	// already labelled, and a request (text starting with requestMark) is
	// watched for thread replies.
	postOfficeTopic = "post-office"
	requestMark     = "❓"

	readChannelLimit = 100
	readThreadLimit  = 200
	maxPages         = 10
	// closedGrace is how long a resolved request's thread is still read, so a
	// late answer gets an "already closed" reply instead of silence.
	closedGrace    = 24 * time.Hour
	pollBackoffMax = 5 * time.Minute
)

// Tool suffixes; the gateway serves them with a backend prefix
// ("slack_xaa-slack_send_message") that is resolved from tools/list.
const (
	toolSend     = "slack_send_message"
	toolReaction = "slack_add_reaction"
	toolChannel  = "slack_read_channel"
	toolThread   = "slack_read_thread"
)

// headerRetries are the waits before each retry of a call the host header
// script rejected: it fails before the request leaves the gateway, so even a
// send is safe to retry (docs/research/slack-mcp.md §3). Five tries in all,
// within the senders' 20s timeout.
var headerRetries = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second}

// Caller is what the channel needs of an MCP session: tool calls and the
// tool list. *mcpgateway.DaemonSession implements it.
type Caller interface {
	mcpgateway.ToolCaller
	mcpgateway.ToolLister
}

// GatewayResetter is implemented by a Caller that can replace its gateway.
// The channel resets a gateway that serves none of the Slack tools: the
// gateway sometimes mints with the slack backend "connected, 0 tools" (its
// attach failed), and only a fresh gateway recovers from that.
type GatewayResetter interface {
	ResetGateway(ctx context.Context)
}

// gatewayCaller is the production Caller: the session plus the gateway it
// runs on.
type gatewayCaller struct {
	*mcpgateway.DaemonSession
	gw *mcpgateway.DaemonGateway
}

// ResetGateway implements GatewayResetter: it invalidates the live gateway,
// which deletes it; the session then finds it gone and opens on a new one.
func (g gatewayCaller) ResetGateway(ctx context.Context) {
	if url, err := g.gw.URL(ctx); err == nil {
		g.gw.Invalidate(ctx, url)
	}
}

// Channel is the Slack transport over the Slack MCP. Build it with New or
// Factory.
type Channel struct {
	name     string
	cfg      Config
	log      *slog.Logger
	caller   Caller
	stateDir string
	// lifecycle hooks for a gateway and session the channel owns (Factory);
	// nil when the caller is supplied from outside (tests).
	startGateway func(ctx context.Context)
	closeCaller  func()

	// Seams for tests.
	firstPoll time.Duration // delay before the first poll (0: at once)
	now       func() time.Time
	sleep     func(ctx context.Context, d time.Duration) error
	retries   []time.Duration
	maxLen    int

	mu      sync.Mutex
	started bool
	closed  bool
	cancel  context.CancelFunc
	runCtx  context.Context
	wg      sync.WaitGroup
	jobs    chan *job
	inbox   *inbox
	handler channels.InboundHandler

	// Owned by the worker goroutine.
	st       *state
	tools    map[string]string // suffix → served name
	fallback bool              // read_channel could not be parsed: poll each thread
	failures int               // consecutive failed polls
}

var (
	_ channels.Channel  = (*Channel)(nil)
	_ channels.Chunker  = (*Channel)(nil)
	_ channels.Resolver = (*Channel)(nil)
)

// job is one unit of work for the serialized worker.
type job struct {
	ctx  context.Context
	run  func(ctx context.Context) error
	done chan error
}

// New builds the channel over caller. stateDir holds the sent set and watch
// list ("" keeps them in memory only).
func New(name string, cfg Config, caller Caller, stateDir string, logger *slog.Logger) *Channel {
	if logger == nil {
		logger = slog.Default()
	}
	return &Channel{
		name:     name,
		cfg:      cfg,
		log:      logger.With("channel", name),
		caller:   caller,
		stateDir: stateDir,
		now:      time.Now,
		sleep:    sleepCtx,
		retries:  headerRetries,
		maxLen:   MaxMessageLength,
	}
}

// FactoryDeps are what the factory needs beyond the channel's config.
type FactoryDeps struct {
	Logger *slog.Logger
	// StateDir holds sent.json and watch.json (~/.workingman/slack).
	StateDir string
	// GatewayStore keeps the sub-gateway record, so a gateway a crashed run
	// left behind is deleted (the daemon's gateways/daemon directory).
	GatewayStore mcpgateway.Store
	// Audit records one audit event; nil drops them.
	Audit func(event string, kv ...string)
}

// Factory returns the channels.Factory for `type: slack`. Each channel mints
// its own static sub-gateway over the slack catalog server on its
// gateway_url and keeps one MCP session on it.
func Factory(deps FactoryDeps) channels.Factory {
	return func(name string, spec channels.ChannelConfig, creds channels.Credentials) (channels.Channel, error) {
		cfg, err := ParseOptions(spec.Options, creds)
		if err != nil {
			return nil, err
		}
		logger := deps.Logger
		if logger == nil {
			logger = slog.Default()
		}
		logf := func(format string, args ...any) { logger.Info(fmt.Sprintf(format, args...), "channel", name) }
		key := GatewayKey
		if name != Type {
			key += "-" + name
		}
		gw := &mcpgateway.DaemonGateway{
			Key:     key,
			Servers: []string{GatewayServer},
			BaseURL: cfg.GatewayURL,
			Store:   deps.GatewayStore,
			Name:    "slack",
			Audit:   deps.Audit,
			Logf:    logf,
		}
		sess := &mcpgateway.DaemonSession{
			Gateway:    gw,
			Name:       "slack",
			ClientName: "orch-slack",
			Audit:      deps.Audit,
			Logf:       logf,
			// Reads are short; a failed open is retried at the next poll.
			BackoffBase: 2 * time.Second,
			BackoffMax:  time.Minute,
		}
		c := New(name, cfg, gatewayCaller{sess, gw}, deps.StateDir, logger)
		c.startGateway = gw.Start
		c.closeCaller = func() {
			sess.Close()
			gw.Close()
		}
		return c, nil
	}
}

// Name implements channels.Channel.
func (c *Channel) Name() string { return c.name }

// Config is the channel's validated configuration.
func (c *Channel) Config() Config { return c.cfg }

// Start loads the persisted state, starts the gateway (when the channel owns
// one) and the worker, and returns at once. Slack is first polled right away
// and then every poll_interval; an unreachable gateway is logged and retried.
func (c *Channel) Start(ctx context.Context, h channels.InboundHandler) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("slack: channel closed")
	}
	if c.started {
		return errors.New("slack: already started")
	}
	st, err := loadState(c.stateDir)
	if err != nil {
		c.log.Warn("slack: state unreadable; starting empty (replies to earlier requests are not picked up)", "dir", c.stateDir, "err", err.Error())
	}
	if st.watch.TopCursor == "" {
		// Never replay history from before the first start.
		st.watch.TopCursor = tsOf(c.now())
	}
	c.st = st
	if len(c.cfg.AllowFrom) == 0 {
		c.log.Warn("slack: allow_from is empty; every inbound message will be dropped")
	}
	c.started = true
	c.handler = h
	runCtx, cancel := context.WithCancel(ctx)
	c.runCtx, c.cancel = runCtx, cancel
	c.jobs = make(chan *job)
	c.inbox = newInbox()
	if c.startGateway != nil {
		c.startGateway(runCtx)
	}
	c.wg.Add(2)
	go func() {
		defer c.wg.Done()
		c.work(runCtx)
	}()
	go func() {
		defer c.wg.Done()
		c.deliver(runCtx)
	}()
	return nil
}

// Close stops the worker, waits for it, and ends the session and gateway the
// channel owns. Idempotent.
func (c *Channel) Close() error {
	c.mu.Lock()
	already := c.closed
	c.closed = true
	cancel := c.cancel
	c.mu.Unlock()
	if already {
		return nil
	}
	if cancel != nil {
		cancel()
	}
	c.wg.Wait()
	if c.closeCaller != nil {
		c.closeCaller()
	}
	return nil
}

// ChunkMessage implements channels.Chunker: pieces of at most
// MaxMessageLength characters, split on paragraph, line, then word
// boundaries.
func (c *Channel) ChunkMessage(text string) []string {
	return whatsapp.ChunkMessage(text, c.maxLen)
}

// Send implements channels.Channel: it posts to msg.ChatID (default: the
// configured channel), in the thread of msg.ReplyToID when set, and returns
// "<channel_id>:<ts>" with the resolved channel id. The Topic becomes a
// "[topic] " prefix, except post-office text, which carries its own label. A
// post-office request is watched for thread replies.
func (c *Channel) Send(ctx context.Context, msg channels.OutboundMessage) (string, error) {
	text := msg.Text
	if msg.Topic != postOfficeTopic {
		text = channels.FormatTopic(msg.Topic, text)
	}
	if strings.TrimSpace(text) == "" {
		return "", errors.New("slack: empty message")
	}
	chat := strings.TrimSpace(msg.ChatID)
	if chat == "" {
		chat = c.cfg.Channel
	}
	var thread string
	if msg.ReplyToID != "" {
		ch, ts := SplitMessageID(msg.ReplyToID)
		thread = ts
		if ch != "" {
			chat = ch
		}
	}
	watch := msg.Topic == postOfficeTopic && thread == "" && strings.HasPrefix(strings.TrimSpace(msg.Text), requestMark)
	var id string
	err := c.do(ctx, func(ctx context.Context) error {
		var err error
		id, err = c.post(ctx, chat, thread, text)
		if err != nil {
			return err
		}
		if watch {
			c.st.addWatch(id, chat, c.now())
		}
		c.saveState()
		return nil
	})
	return id, err
}

// Resolve implements channels.Resolver (§6.5 of docs/messaging-design.md):
// since Slack messages cannot be edited, the closing note is posted as a
// thread reply and a reaction is added to the request — white_check_mark when
// answered, hourglass when expired, no_entry_sign when cancelled. The
// reaction is best effort; the watch is marked closed so a later reply in
// the thread is told so instead of being forwarded.
func (c *Channel) Resolve(ctx context.Context, chatID, messageID string, r channels.Resolution) error {
	ch, ts := SplitMessageID(messageID)
	if ch == "" {
		ch = chatID
	}
	if ts == "" {
		return errors.New("slack: resolve: no message ts")
	}
	return c.do(ctx, func(ctx context.Context) error {
		w := c.st.watchFor(messageID)
		if w == nil || w.Closed == nil {
			if note := strings.TrimSpace(r.Note); note != "" {
				if _, err := c.post(ctx, ch, ts, note); err != nil {
					return err
				}
			}
			if w != nil {
				w.Closed = &Closed{Outcome: r.Outcome, Via: r.Via, At: c.now()}
			}
			c.saveState()
		}
		if emoji := reactionFor(r.Outcome); emoji != "" {
			_, err := c.call(ctx, toolReaction, map[string]any{"channel_id": ch, "message_ts": ts, "emoji": emoji})
			if err != nil && ctx.Err() == nil {
				c.log.Warn("slack: could not add reaction", "emoji", emoji, "err", err.Error())
			}
		}
		return nil
	})
}

func reactionFor(outcome string) string {
	switch outcome {
	case "answered":
		return "white_check_mark"
	case "expired":
		return "hourglass"
	case "cancelled":
		return "no_entry_sign"
	}
	return ""
}

// --- worker ------------------------------------------------------------------

// do runs fn on the worker and waits for it.
func (c *Channel) do(ctx context.Context, fn func(ctx context.Context) error) error {
	c.mu.Lock()
	jobs, runCtx, running := c.jobs, c.runCtx, c.started && !c.closed
	c.mu.Unlock()
	if !running {
		return fmt.Errorf("slack: %w", channels.ErrNotRunning)
	}
	j := &job{ctx: ctx, run: fn, done: make(chan error, 1)}
	select {
	case jobs <- j:
	case <-ctx.Done():
		return ctx.Err()
	case <-runCtx.Done():
		return fmt.Errorf("slack: %w", channels.ErrNotRunning)
	}
	select {
	case err := <-j.done:
		return err
	case <-runCtx.Done():
		return fmt.Errorf("slack: %w", channels.ErrNotRunning)
	}
}

// work is the single goroutine every Slack call runs on: it serves queued
// jobs and polls on a timer, backing off while polls fail.
func (c *Channel) work(ctx context.Context) {
	timer := time.NewTimer(c.firstPoll)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-c.jobs:
			c.runJob(ctx, j)
		case <-timer.C:
			err := c.poll(ctx)
			if ctx.Err() != nil {
				return
			}
			timer.Reset(c.nextPoll(err))
		}
	}
}

func (c *Channel) runJob(ctx context.Context, j *job) {
	if err := j.ctx.Err(); err != nil {
		j.done <- err
		return
	}
	jctx, cancel := context.WithCancel(j.ctx)
	stop := context.AfterFunc(ctx, cancel)
	j.done <- j.run(jctx)
	stop()
	cancel()
}

// runQueued serves the jobs waiting right now, so a send does not wait for a
// whole poll to finish.
func (c *Channel) runQueued(ctx context.Context) {
	for {
		select {
		case j := <-c.jobs:
			c.runJob(ctx, j)
		default:
			return
		}
	}
}

// nextPoll is the delay before the next poll: poll_interval, doubled for each
// consecutive failure up to pollBackoffMax.
func (c *Channel) nextPoll(err error) time.Duration {
	if err == nil {
		c.failures = 0
		return c.cfg.PollInterval
	}
	c.failures++
	d := c.cfg.PollInterval << min(c.failures, 10)
	if d > pollBackoffMax || d <= 0 {
		d = pollBackoffMax
	}
	c.log.Warn("slack: poll failed", "attempt", c.failures, "retry_in", d.String(), "err", err.Error())
	return d
}

// --- calls -------------------------------------------------------------------

// call calls the tool with the given suffix, retrying header-script failures.
func (c *Channel) call(ctx context.Context, suffix string, args map[string]any) (mcpgateway.ToolResult, error) {
	name, err := c.tool(ctx, suffix)
	if err != nil {
		return mcpgateway.ToolResult{}, err
	}
	for attempt := 0; ; attempt++ {
		res, err := c.caller.CallTool(ctx, name, args)
		if err == nil {
			return res, nil
		}
		if !headerScriptFailed(err) || attempt >= len(c.retries) {
			return res, fmt.Errorf("slack: %s: %w", suffix, err)
		}
		c.log.Info("slack: header script failed; retrying", "tool", suffix, "attempt", attempt+1, "retry_in", c.retries[attempt].String())
		if err := c.sleep(ctx, c.retries[attempt]); err != nil {
			return res, err
		}
	}
}

// headerScriptFailed reports the host header script rejecting a call (it
// fails under concurrency, before the request leaves the gateway).
func headerScriptFailed(err error) bool {
	return err != nil && strings.Contains(err.Error(), "header script failed")
}

// tool resolves a tool suffix to the name the gateway serves it under.
func (c *Channel) tool(ctx context.Context, suffix string) (string, error) {
	if c.tools == nil {
		var tools []mcpgateway.Tool
		var err error
		for attempt := 0; ; attempt++ {
			tools, err = c.caller.ListTools(ctx)
			if err == nil || !headerScriptFailed(err) || attempt >= len(c.retries) {
				break
			}
			if err := c.sleep(ctx, c.retries[attempt]); err != nil {
				return "", err
			}
		}
		if err != nil {
			return "", fmt.Errorf("slack: tools/list: %w", err)
		}
		m := map[string]string{}
		for _, s := range []string{toolSend, toolReaction, toolChannel, toolThread, toolProfile, toolChannels} {
			for _, t := range tools {
				if t.Name == s || strings.HasSuffix(t.Name, "-"+s) || strings.HasSuffix(t.Name, "_"+s) {
					m[s] = t.Name
					break
				}
			}
		}
		c.tools = m
	}
	name, ok := c.tools[suffix]
	if !ok {
		reset := len(c.tools) == 0
		c.tools = nil
		if r, can := c.caller.(GatewayResetter); can && reset {
			c.log.Warn("slack: the gateway serves no Slack tools; replacing it")
			r.ResetGateway(ctx)
		}
		return "", &noToolError{suffix}
	}
	return name, nil
}

// noToolError is a tool the gateway does not serve.
type noToolError struct{ suffix string }

func (e *noToolError) Error() string {
	return fmt.Sprintf("slack: the gateway serves no %s tool (is the slack server attached?)", e.suffix)
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// post sends text to chat (in thread when set), records it as ours, and
// returns its message ID. A send whose outcome is unknown (the response was
// lost, e.g. the gateway dropped the session mid-call) may still have been
// posted: it is looked for in Slack, and if not found remembered as
// unconfirmed, so its echo is never taken for the human's.
func (c *Channel) post(ctx context.Context, chat, thread, text string) (string, error) {
	args := map[string]any{"channel_id": chat, "message": text}
	if thread != "" {
		args["thread_ts"] = thread
	}
	start := c.now()
	res, err := c.call(ctx, toolSend, args)
	var ch, ts string
	if err == nil {
		ch, ts, err = sendResult(res)
	} else if outcomeUnknown(err) && ctx.Err() == nil {
		var found bool
		if ch, ts, found = c.reconcile(ctx, chat, thread, text, start); found {
			c.log.Warn("slack: send response lost, but the message was posted", "ts", ts, "err", err.Error())
			err = nil
		} else {
			c.st.addUnconfirmed(chat, thread, text, start)
			c.log.Warn("slack: send outcome unknown; its echo will be ignored if it appears", "err", err.Error())
		}
	}
	if err != nil {
		return "", err
	}
	if ch == "" {
		ch = chat
	}
	id := MessageID(ch, ts)
	c.st.markSent(id, c.now())
	if chat == c.cfg.Channel && c.st.watch.ChannelID == "" {
		c.st.watch.ChannelID = ch
	}
	return id, nil
}

// outcomeUnknown reports whether a failed send may nevertheless have been
// posted: anything but a tool error (the tool ran and refused), a JSON-RPC
// error (rejected before it ran) or a lost session (the gateway no longer
// knew it, so never forwarded the call).
func outcomeUnknown(err error) bool {
	var te *mcpgateway.ToolError
	var re *mcpgateway.RPCError
	return !errors.As(err, &te) && !errors.As(err, &re) && !errors.Is(err, mcpgateway.ErrSessionLost)
}

// reconcile looks for a just-posted message in Slack: a Slack MCP post in the
// same thread (or top level), newer than start, not already ours, whose text
// matches.
func (c *Channel) reconcile(ctx context.Context, chat, thread, text string, start time.Time) (channelID, ts string, found bool) {
	oldest := tsOf(start.Add(-5 * time.Second))
	var msgs []Message
	channelID = chat
	if thread != "" {
		res, err := c.call(ctx, toolThread, map[string]any{"channel_id": chat, "message_ts": thread, "oldest": oldest, "limit": readThreadLimit, "response_format": "detailed"})
		if err != nil {
			return "", "", false
		}
		msgs = ParseThread(readResult(res)).Replies
	} else {
		res, err := c.call(ctx, toolChannel, map[string]any{"channel_id": chat, "oldest": oldest, "limit": readChannelLimit, "response_format": "detailed"})
		if err != nil {
			return "", "", false
		}
		p := ParseChannel(readResult(res))
		msgs = p.Messages
		if p.ChannelID != "" {
			channelID = p.ChannelID
		}
	}
	key := echoKey(text)
	for _, m := range msgs {
		if m.ViaMCP && !c.st.isSent(MessageID(channelID, m.TS)) && echoKey(m.Text) == key {
			return channelID, m.TS, true
		}
	}
	return "", "", false
}

// sendResult reads message_context from a slack_send_message result.
func sendResult(res mcpgateway.ToolResult) (channelID, ts string, err error) {
	var out struct {
		Context struct {
			TS      string `json:"message_ts"`
			Channel string `json:"channel_id"`
		} `json:"message_context"`
	}
	raw := []byte(res.StructuredContent)
	if len(raw) == 0 || json.Unmarshal(raw, &out) != nil || out.Context.TS == "" {
		if err := json.Unmarshal([]byte(strings.TrimSpace(res.Text())), &out); err != nil || out.Context.TS == "" {
			return "", "", fmt.Errorf("slack: send_message returned no message_ts")
		}
	}
	return out.Context.Channel, out.Context.TS, nil
}

func (c *Channel) saveState() {
	if err := c.st.save(); err != nil {
		c.log.Warn("slack: could not save state", "dir", c.stateDir, "err", err.Error())
	}
}

// --- inbound delivery ----------------------------------------------------------

// inbox hands inbound messages from the worker to the handler on a goroutine
// of its own, in order: the handler may send (the router's "✅ Sent to …"),
// and a send waits on the worker.
type inbox struct {
	mu    sync.Mutex
	queue []channels.InboundMessage
	wake  chan struct{}
}

func newInbox() *inbox { return &inbox{wake: make(chan struct{}, 1)} }

func (b *inbox) push(m channels.InboundMessage) {
	b.mu.Lock()
	b.queue = append(b.queue, m)
	b.mu.Unlock()
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

func (b *inbox) take() []channels.InboundMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	q := b.queue
	b.queue = nil
	return q
}

func (c *Channel) deliver(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.inbox.wake:
		}
		for _, m := range c.inbox.take() {
			if ctx.Err() != nil {
				return
			}
			if c.handler != nil {
				c.handler(ctx, m)
			}
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
