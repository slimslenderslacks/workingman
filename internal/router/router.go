// Package router connects inbound channel messages (WhatsApp, …) to agent
// conversations and sends the replies back. It is the "ear" of the daemon's
// messaging channels: a Router is installed as the channels.InboundHandler and
// decides, per message, whether the text is a router command, a reply to a wolf
// notification, a message for the wolf the chat is bound to, or — by default —
// a question for the workingman agent.
//
// The routing rules (documented for users in docs/channels.md):
//
//  1. A reply-to a known notification goes to THAT wolf session as a prompt;
//     the wolf's answer comes back labelled "[wolf <work-stream>]".
//  2. Slash commands are answered by the router itself, with no LLM:
//     /help, /status, /wolf [work-stream], /agent, /who.
//  3. Otherwise the chat is bound to the workingman agent (the default) or to
//     the wolf chosen with /wolf, until /agent or the wolf session ends.
//  4. While a chat is bound to a wolf it also sees what the wolf says to other
//     clients (the TUI user), and the wolf's permission requests are put to the
//     chat, whose next message answers them; unanswered ones time out rejected.
//  5. Each chat is served one message at a time through a bounded queue, with
//     acks for queueing and slow turns, a turn timeout, an immediate reply when
//     the workingman agent is down, and secrets redacted from every reply.
//  6. Everything is audit-logged under a hashed chat reference, never the text.
//
// Senders are NOT authorised here: a channel applies its access policy before
// it calls the handler, so only approved senders ever reach the Router.
//
// The Router depends on interfaces only (Source, Outbound, Agent), so it has no
// import of the daemon package; cmd/orch adapts the daemon to Source.
package router

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/slimslenderslacks/work/internal/acpchat"
	"github.com/slimslenderslacks/work/internal/channels"
)

// Defaults of Config.
const (
	DefaultTurnTimeout       = 5 * time.Minute
	DefaultThinkingAfter     = 10 * time.Second
	DefaultTypingEvery       = 20 * time.Second
	DefaultQueueSize         = 5
	DefaultPermissionTimeout = 2 * time.Minute
	DefaultAttachTimeout     = 15 * time.Second
	DefaultSendTimeout       = 20 * time.Second
	DefaultMaxReplyRunes     = 12000
)

// AgentStartingMessage is the immediate reply while the workingman agent is not
// running (still launching, or being restarted by the daemon).
const AgentStartingMessage = "The workingman agent is starting up, try again in a minute."

// Session is a live agent session the router can talk to.
type Session struct {
	// Key is the daemon's session-map key (channels.ConversationTarget.SessionKey
	// for a wolf).
	Key string
	// ID is the launcher's name for the live session
	// (channels.ConversationTarget.SessionID); it changes on relaunch.
	ID string
	// Ref is what Attach takes: the session directory. Empty means the session
	// cannot be attached to (a wolf running on the host under tmux).
	Ref string
	// WorkStream labels a wolf's project; unused for the workingman agent.
	WorkStream string
}

// Source is what the router needs to know about the daemon.
type Source interface {
	// WorkingmanAgent returns the live workingman agent session, or false while
	// it is not running.
	WorkingmanAgent() (Session, bool)
	// WolfSessions lists the live wolf sessions.
	WolfSessions() []Session
	// Status is the one-line daemon summary behind /status.
	Status() string
}

// Outbound is the channel registry as the router uses it. *channels.Registry
// implements it.
type Outbound interface {
	Send(ctx context.Context, channel string, msg channels.OutboundMessage) (string, error)
	Get(name string) (channels.Channel, bool)
}

// Agent is one attached ACP conversation. *acpchat.Conversation implements it.
type Agent interface {
	Ask(ctx context.Context, text string, opts ...acpchat.AskOption) (string, error)
	Events() <-chan acpchat.Event
	Close() error
}

// Attacher joins the live session at ref. AttachACP is the production one.
type Attacher func(ctx context.Context, ref string, opts acpchat.Options) (Agent, error)

// AttachACP attaches with acpchat.Attach.
func AttachACP(ctx context.Context, ref string, opts acpchat.Options) (Agent, error) {
	c, err := acpchat.Attach(ctx, ref, opts)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// Auditor records audit events; *audit.Logger implements it.
type Auditor interface {
	Log(event string, kv ...string)
}

// Config configures a Router. Source and Out are required; zero values of the
// rest select the defaults above.
type Config struct {
	Source Source
	Out    Outbound
	// Index resolves a reply-to ID to the wolf session a notification came from,
	// and records the replies the router relays so those can be replied to as
	// well. May be nil (no reply-to routing).
	Index channels.ConversationIndex
	Audit Auditor
	// Attach defaults to AttachACP.
	Attach Attacher
	// AttachOptions seed every Attach (Root, DiscoverTimeout, …); the router sets
	// OnPermission and PermissionTimeout itself.
	AttachOptions acpchat.Options

	// TurnTimeout cancels a turn that runs longer.
	TurnTimeout time.Duration
	// ThinkingAfter is when a turn that is still running gets a "…thinking" ack.
	ThinkingAfter time.Duration
	// TypingEvery is the typing-indicator refresh interval on channels that have
	// one.
	TypingEvery time.Duration
	// QueueSize bounds the messages queued per chat behind the running turn.
	QueueSize int
	// PermissionTimeout is how long a permission request waits for the human
	// before it is rejected.
	PermissionTimeout time.Duration
	AttachTimeout     time.Duration
	SendTimeout       time.Duration
	// MaxReplyRunes truncates a reply (after redaction) beyond this length.
	MaxReplyRunes int
}

func (c *Config) applyDefaults() {
	if c.Attach == nil {
		c.Attach = AttachACP
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&c.TurnTimeout, DefaultTurnTimeout)
	def(&c.ThinkingAfter, DefaultThinkingAfter)
	def(&c.TypingEvery, DefaultTypingEvery)
	def(&c.PermissionTimeout, DefaultPermissionTimeout)
	def(&c.AttachTimeout, DefaultAttachTimeout)
	def(&c.SendTimeout, DefaultSendTimeout)
	if c.QueueSize <= 0 {
		c.QueueSize = DefaultQueueSize
	}
	if c.MaxReplyRunes <= 0 {
		c.MaxReplyRunes = DefaultMaxReplyRunes
	}
}

// Router routes inbound messages. Safe for concurrent use.
type Router struct {
	cfg    Config
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	chats  map[string]*chat
	convs  map[string]*conv         // by session key
	active map[string]map[*chat]int // session key → chats with a turn in flight
	closed bool
}

// chat is the per-chat state; every field is guarded by Router.mu.
type chat struct {
	channel, id string
	ref         string // hashed id for the audit log

	wolf    *Session // nil: bound to the workingman agent
	queue   []item
	busy    bool
	pending *permission
}

type item struct {
	msg  channels.InboundMessage
	text string
}

// target is a session a message is being sent to.
type target struct {
	wolf bool
	Session
}

// label prefixes replies from a wolf: "[wolf <work-stream>]".
func (t target) label() string {
	if t.wolf {
		return "wolf " + t.WorkStream
	}
	return ""
}

func (t target) conversationTarget() channels.ConversationTarget {
	return channels.ConversationTarget{
		Kind:       channels.ConversationKindWolf,
		WorkStream: t.WorkStream,
		SessionKey: t.Key,
		SessionID:  t.ID,
	}
}

// New returns a Router whose background work (queued turns, observers) runs
// under ctx; cancelling ctx or calling Close stops it.
func New(ctx context.Context, cfg Config) (*Router, error) {
	if cfg.Source == nil || cfg.Out == nil {
		return nil, errors.New("router: Source and Out are required")
	}
	cfg.applyDefaults()
	ctx, cancel := context.WithCancel(ctx)
	return &Router{
		cfg:    cfg,
		ctx:    ctx,
		cancel: cancel,
		chats:  map[string]*chat{},
		convs:  map[string]*conv{},
		active: map[string]map[*chat]int{},
	}, nil
}

// Handler is the channels.InboundHandler to pass to Registry.Start.
func (r *Router) Handler() channels.InboundHandler { return r.Handle }

// Close detaches every conversation and waits for the router's goroutines.
// Pending permission requests are rejected. Idempotent.
func (r *Router) Close() {
	r.cancel()
	r.mu.Lock()
	r.closed = true
	convs := make([]*conv, 0, len(r.convs))
	for _, cv := range r.convs {
		convs = append(convs, cv)
	}
	r.convs = map[string]*conv{}
	r.mu.Unlock()
	for _, cv := range convs {
		cv.close()
	}
	r.wg.Wait()
}

// Handle routes one inbound message. It returns quickly: slow work (attaching,
// asking an agent) runs on the router's own goroutines. The handler's ctx is
// deliberately unused — a channel may hand over a request-scoped one.
func (r *Router) Handle(_ context.Context, msg channels.InboundMessage) {
	text := strings.TrimSpace(msg.Text)
	if text == "" || msg.ChatID == "" || r.ctx.Err() != nil {
		return
	}
	c := r.chatFor(msg.Channel, msg.ChatID)
	r.log("inbound", c, "runes", fmt.Sprint(len([]rune(text))), "reply_to", boolStr(msg.ReplyToID != ""))

	if name, args, ok := parseCommand(text); ok {
		r.handleCommand(c, name, args)
		return
	}
	if r.answerPermission(c, text) {
		return
	}
	r.enqueue(c, msg, text)
}

func (r *Router) chatFor(channel, id string) *chat {
	key := channel + "\x00" + id
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.chats[key]
	if !ok {
		c = &chat{channel: channel, id: id, ref: chatRef(channel, id)}
		r.chats[key] = c
	}
	return c
}

// chatRef is how a chat appears in the audit log: the channel plus the first
// eight hex digits of the chat id's SHA-256. Enough to correlate lines, not
// enough to recover a phone number.
func chatRef(channel, id string) string {
	sum := sha256.Sum256([]byte(id))
	return channel + ":" + hex.EncodeToString(sum[:])[:8]
}

// log writes an audit line for chat c. It never receives message text.
func (r *Router) log(event string, c *chat, kv ...string) {
	if r.cfg.Audit == nil {
		return
	}
	r.cfg.Audit.Log("router_"+event, append([]string{"chat", c.ref}, kv...)...)
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// goFn runs fn on a tracked goroutine, unless the router is closed.
func (r *Router) goFn(fn func()) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.wg.Add(1)
	r.mu.Unlock()
	go func() {
		defer r.wg.Done()
		fn()
	}()
}

// --- queueing ---

// enqueue adds a turn to the chat's queue, starting its worker if idle.
func (r *Router) enqueue(c *chat, msg channels.InboundMessage, text string) {
	r.mu.Lock()
	if len(c.queue) >= r.cfg.QueueSize {
		r.mu.Unlock()
		r.log("queue_full", c)
		r.notify(c, fmt.Sprintf("I'm still working on %d earlier messages and the queue is full, so I dropped that one. Please resend it in a bit.", len(c.queue)+1))
		return
	}
	busy := c.busy
	c.queue = append(c.queue, item{msg: msg, text: text})
	position := len(c.queue)
	c.busy = true
	r.mu.Unlock()

	if !busy {
		r.goFn(func() { r.work(c) })
		return
	}
	r.log("queued", c, "position", fmt.Sprint(position))
	r.notify(c, fmt.Sprintf("The agent is busy with your previous message; yours is queued (#%d).", position))
}

// work drains the chat's queue one turn at a time.
func (r *Router) work(c *chat) {
	for {
		r.mu.Lock()
		if len(c.queue) == 0 || r.closed {
			c.busy = false
			c.queue = nil
			r.mu.Unlock()
			return
		}
		it := c.queue[0]
		c.queue = c.queue[1:]
		r.mu.Unlock()
		r.process(c, it)
	}
}

// process picks the destination of one queued message.
func (r *Router) process(c *chat, it item) {
	// Rule 1: a reply to a notification goes to the wolf that sent it.
	if it.msg.ReplyToID != "" && r.cfg.Index != nil {
		if ct, ok := r.cfg.Index.Lookup(c.channel, it.msg.ReplyToID); ok && ct.Kind == channels.ConversationKindWolf {
			tgt, problem := r.resolveReplyTarget(ct)
			if problem != "" {
				r.log("reply_unroutable", c, "work_stream", ct.WorkStream)
				r.notify(c, problem)
				return
			}
			r.log("route", c, "to", "wolf_reply", "work_stream", tgt.WorkStream)
			r.turn(c, tgt, it.text)
			return
		}
	}
	// Rule 3: the chat's binding. A binding that has just ended already told the
	// user; the message is not forwarded to a different agent than it was meant
	// for.
	w, bound, ended := r.boundWolf(c)
	if ended {
		return
	}
	if bound {
		r.log("route", c, "to", "wolf", "work_stream", w.WorkStream)
		r.turn(c, target{wolf: true, Session: w}, it.text)
		return
	}
	r.log("route", c, "to", "agent")
	r.turn(c, target{}, it.text)
}

// resolveReplyTarget finds the live wolf session a notification came from, or
// explains why there is none.
func (r *Router) resolveReplyTarget(ct channels.ConversationTarget) (target, string) {
	ws := ct.WorkStream
	if ws == "" {
		ws = "that project"
	}
	for _, w := range r.cfg.Source.WolfSessions() {
		if w.Key != ct.SessionKey {
			continue
		}
		if ct.SessionID != "" && w.ID != ct.SessionID {
			return target{}, fmt.Sprintf("That message was from an earlier wolf session for %s; a newer one is running. Use /wolf %s to talk to it.", ws, ws)
		}
		if w.Ref == "" {
			return target{}, fmt.Sprintf("The wolf for %s runs on the host, so I can't reach it from here.", ws)
		}
		return target{wolf: true, Session: w}, ""
	}
	return target{}, fmt.Sprintf("The wolf session for %s has ended, so I can't pass your reply on. Ask me directly, or use /wolf when one is running.", ws)
}

// boundWolf returns the live wolf the chat is bound to. A binding whose wolf has
// ended is dropped and the user told (ended=true) — rule 3's fall-back.
func (r *Router) boundWolf(c *chat) (w Session, bound, ended bool) {
	r.mu.Lock()
	b := c.wolf
	r.mu.Unlock()
	if b == nil {
		return Session{}, false, false
	}
	for _, w := range r.cfg.Source.WolfSessions() {
		if w.Key == b.Key && w.ID == b.ID && w.Ref != "" {
			return w, true, false
		}
	}
	r.unbindIf(c, b)
	r.log("binding_ended", c, "work_stream", b.WorkStream)
	r.notify(c, fmt.Sprintf("The wolf for %s has ended. You're back on the workingman agent; send your message again to ask it, or /wolf when another wolf is running.", b.WorkStream))
	return Session{}, false, true
}

// unbindIf returns the chat to the workingman agent if it is still bound to b.
func (r *Router) unbindIf(c *chat, b *Session) {
	r.mu.Lock()
	if c.wolf == b {
		c.wolf = nil
	}
	r.mu.Unlock()
}
