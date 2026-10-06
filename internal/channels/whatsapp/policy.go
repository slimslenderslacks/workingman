package whatsapp

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/slimslenderslacks/work/internal/channels"
)

// Inbound is a received WhatsApp message plus the transport facts the policy
// needs. The embedded channels.InboundMessage supplies ChatID, SenderID and
// Text; transports fill in the rest.
type Inbound struct {
	channels.InboundMessage

	// SenderAltID is the same sender's other identity when the transport
	// knows it (Baileys' remoteJidAlt / participantAlt): a first-contact
	// sender can arrive as an opaque LID before any phone mapping exists.
	SenderAltID string
	// IsGroup is true for group chats. A chat id ending in @g.us counts as a
	// group regardless of this flag.
	IsGroup bool
	// FromMe is true when the message was sent by the account itself (the
	// owner's phone, a linked device, or our own reply echoing back).
	FromMe bool

	// Group mention data, as the transport reports it.
	BotIDs            []string // the account's own ids (phone and LID)
	MentionedIDs      []string
	QuotedParticipant string
}

// Reason says why a message was admitted or refused. It is a short stable
// token, safe to log and to match on; it never contains message content.
type Reason string

// Allow reasons.
const (
	ReasonAllowlisted   Reason = "allowlisted"
	ReasonAllowAll      Reason = "allow_all"
	ReasonSelfChat      Reason = "self_chat"
	ReasonOwnerMessage  Reason = "owner_message"
	ReasonGroupAllowed  Reason = "group_allowed"
	ReasonGroupFreeChat Reason = "group_free_response"
)

// Deny reasons.
const (
	DenyNoChat                Reason = "no_chat"
	DenyNoSender              Reason = "no_sender"
	DenyBroadcast             Reason = "broadcast"
	DenyUnsupportedChat       Reason = "unsupported_chat"
	DenyEcho                  Reason = "echo"
	DenyAllowlistEmpty        Reason = "allowlist_empty"
	DenyNotAllowlisted        Reason = "sender_not_allowlisted"
	DenyGroupDisabled         Reason = "group_disabled"
	DenyGroupNotAllowed       Reason = "group_not_allowed"
	DenyGroupSenderNotAllowed Reason = "group_sender_not_allowlisted"
	DenyMentionRequired       Reason = "mention_required"
	DenyFromMeGroup           Reason = "from_me_group"
	DenyFromMeDisabled        Reason = "from_me_disabled"
	DenyOwnerChatNotAllowed   Reason = "owner_chat_not_allowlisted"
	DenySelfChatNonSelf       Reason = "self_chat_mode_rejects_non_self"
	DenySelfChatOtherChat     Reason = "self_chat_mismatch"
)

// Decision is the verdict on one inbound message.
type Decision struct {
	Allowed bool
	Reason  Reason
	// FromOwner marks a message the account holder typed themselves (self-chat,
	// or an owner message forwarded in bot mode).
	FromOwner bool
	// Text is the message text to hand to the agent: the original, with the
	// bot's own @mention removed in groups.
	Text string
	// Reply, when non-empty, is the single rate-limited message to send back
	// to an unauthorized sender. Empty means stay silent, which is the norm.
	Reply string
}

// String renders "allow: allowlisted" / "deny: sender_not_allowlisted".
func (d Decision) String() string {
	if d.Allowed {
		return "allow: " + string(d.Reason)
	}
	return "deny: " + string(d.Reason)
}

// LogValue lets a Decision be passed straight to slog without exposing text.
func (d Decision) LogValue() slog.Value {
	return slog.GroupValue(slog.Bool("allowed", d.Allowed), slog.String("reason", string(d.Reason)))
}

// noisy denials are expected traffic, not security events, so they log at debug.
func (r Reason) noisy() bool {
	switch r {
	case DenyEcho, DenyBroadcast, DenyMentionRequired:
		return true
	}
	return false
}

const (
	maxSent       = 512  // remembered outbound message ids (echo suppression)
	maxReplyState = 4096 // senders tracked for deny-reply rate limiting
)

// AccessPolicy decides which inbound WhatsApp messages may reach the daemon.
// Construct it with NewAccessPolicy; it is safe for concurrent use.
type AccessPolicy struct {
	c        *compiled
	resolver Resolver
	log      *slog.Logger
	now      func() time.Time

	mu        sync.Mutex
	sent      map[string]struct{}
	sentOrder []string
	replied   map[string]time.Time
}

// Option customizes an AccessPolicy.
type Option func(*AccessPolicy)

// WithResolver supplies phone<->LID alias knowledge (see DirResolver).
func WithResolver(r Resolver) Option { return func(p *AccessPolicy) { p.resolver = r } }

// WithLogger sets the logger used for denial audit lines and the allow_all
// warning. Default: slog.Default().
func WithLogger(l *slog.Logger) Option { return func(p *AccessPolicy) { p.log = l } }

// WithClock overrides the time source (tests).
func WithClock(now func() time.Time) Option { return func(p *AccessPolicy) { p.now = now } }

// NewAccessPolicy validates cfg and builds the policy. An invalid config is an
// error, never a silently weaker policy.
func NewAccessPolicy(cfg AccessConfig, opts ...Option) (*AccessPolicy, error) {
	c, err := cfg.compile()
	if err != nil {
		return nil, err
	}
	p := &AccessPolicy{
		c:       c,
		log:     slog.Default(),
		now:     time.Now,
		sent:    map[string]struct{}{},
		replied: map[string]time.Time{},
	}
	for _, o := range opts {
		o(p)
	}
	if p.log == nil {
		p.log = slog.Default()
	}
	switch {
	case c.allowAll:
		p.log.Warn("whatsapp: allow_all is ENABLED: anyone who can message this number can drive the workingman agent and read orch state. Set access.allow_from instead.")
	case c.mode == ModeBot && len(c.allow) == 0:
		p.log.Warn("whatsapp: access.allow_from is empty; every inbound message will be denied")
	}
	return p, nil
}

// RecordSent remembers the id of a message we sent so its echo (a fromMe event
// carrying the same id) is dropped instead of being read as the owner typing.
// Call it with the id Channel.Send returns.
func (p *AccessPolicy) RecordSent(messageID string) {
	if messageID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.sent[messageID]; ok {
		return
	}
	p.sent[messageID] = struct{}{}
	p.sentOrder = append(p.sentOrder, messageID)
	if len(p.sentOrder) > maxSent {
		delete(p.sent, p.sentOrder[0])
		p.sentOrder = p.sentOrder[1:]
	}
}

// Decide rules on one inbound message. Call it exactly once per message: a
// returned Reply consumes the sender's rate-limit slot. Every denial is
// logged (reason and redacted ids only, never the text) and the Decision
// carries the same Reason for the caller's own audit trail.
func (p *AccessPolicy) Decide(in Inbound) Decision {
	d := p.decide(in)
	if d.Allowed {
		d.Reply = ""
	}
	p.audit(in, d)
	return d
}

func (p *AccessPolicy) deny(r Reason) Decision { return Decision{Reason: r} }

func (p *AccessPolicy) decide(in Inbound) Decision {
	chat := strings.TrimSpace(in.ChatID)
	if chat == "" {
		return p.deny(DenyNoChat)
	}
	// Broadcast pseudo-chats are dropped in every mode, fromMe included.
	if IsBroadcast(chat) {
		return p.deny(DenyBroadcast)
	}
	kind := KindOf(chat)
	isGroup := in.IsGroup || kind == KindGroup
	if !isGroup && kind != KindBare && kind != KindPhone && kind != KindLID {
		return p.deny(DenyUnsupportedChat)
	}

	if in.FromMe {
		return p.decideFromMe(in, chat, isGroup)
	}
	if p.c.mode == ModeSelfChat {
		return p.deny(DenySelfChatNonSelf)
	}
	if isGroup {
		return p.decideGroup(in, chat)
	}
	return p.decideDM(in)
}

// decideFromMe covers messages sent by the account itself.
func (p *AccessPolicy) decideFromMe(in Inbound, chat string, isGroup bool) Decision {
	if isGroup {
		return p.deny(DenyFromMeGroup)
	}
	if p.isEcho(in) {
		return p.deny(DenyEcho)
	}
	if p.c.mode == ModeSelfChat {
		// Only the owner's own chat: their number or their LID. Anyone the
		// owner happens to message from their phone is not for us.
		chatAliases := Expand(p.resolver, chat)
		for _, s := range p.c.self {
			if intersects(chatAliases, Expand(p.resolver, s)) {
				return Decision{Allowed: true, Reason: ReasonSelfChat, FromOwner: true, Text: in.Text}
			}
		}
		return p.deny(DenySelfChatOtherChat)
	}
	// Bot mode: the owner typing from their phone in a customer's chat. Off
	// unless opted in, and then only for chats on the allowlist, checked
	// against the *chat* (the sender is the owner, who is not on it).
	if !p.c.forwardOwner {
		return p.deny(DenyFromMeDisabled)
	}
	if !p.c.allowAll && !p.matches(p.c.allow, chat) {
		return p.deny(DenyOwnerChatNotAllowed)
	}
	return Decision{Allowed: true, Reason: ReasonOwnerMessage, FromOwner: true, Text: in.Text}
}

// isEcho reports whether a fromMe message is our own reply coming back.
func (p *AccessPolicy) isEcho(in Inbound) bool {
	if p.c.replyPrefix != "" && strings.HasPrefix(in.Text, p.c.replyPrefix) {
		return true
	}
	if in.MessageID == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.sent[in.MessageID]
	return ok
}

// senderIDs returns the usable sender identities for in.
func senderIDs(in Inbound) []string {
	var out []string
	for _, s := range []string{in.SenderID, in.SenderAltID} {
		if NormalizeID(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

// matches reports whether id (or any phone/LID alias of it) is in entries,
// resolving both sides so an allowlist of phone numbers admits a LID sender
// and vice versa.
func (p *AccessPolicy) matches(entries []string, id string) bool {
	cand := Expand(p.resolver, id)
	if len(cand) == 0 {
		return false
	}
	for _, e := range entries {
		if intersects(cand, Expand(p.resolver, e)) {
			return true
		}
	}
	return false
}

// senderAllowed applies the user allowlist to any of the sender's identities.
func (p *AccessPolicy) senderAllowed(ids []string) bool {
	for _, id := range ids {
		if p.matches(p.c.allow, id) {
			return true
		}
	}
	return false
}

func (p *AccessPolicy) decideDM(in Inbound) Decision {
	ids := senderIDs(in)
	if len(ids) == 0 {
		return p.deny(DenyNoSender)
	}
	if p.c.allowAll {
		return Decision{Allowed: true, Reason: ReasonAllowAll, Text: in.Text}
	}
	if len(p.c.allow) == 0 {
		return p.deny(DenyAllowlistEmpty)
	}
	if p.senderAllowed(ids) {
		return Decision{Allowed: true, Reason: ReasonAllowlisted, Text: in.Text}
	}
	d := p.deny(DenyNotAllowlisted)
	d.Reply = p.replyFor(ids[0])
	return d
}

func (p *AccessPolicy) decideGroup(in Inbound, chat string) Decision {
	if !p.c.groups {
		return p.deny(DenyGroupDisabled)
	}
	n := NormalizeID(chat)
	if _, ok := p.c.groupAllow[n]; !ok {
		return p.deny(DenyGroupNotAllowed)
	}
	// A listed group is not a free-for-all: this channel drives an agent, so
	// the individual sender still has to be on the allowlist.
	ids := senderIDs(in)
	if len(ids) == 0 {
		return p.deny(DenyNoSender)
	}
	if !p.c.allowAll && !p.senderAllowed(ids) {
		return p.deny(DenyGroupSenderNotAllowed)
	}

	text := in.Text
	if _, free := p.c.freeChats[n]; free || !p.c.requireMention {
		return Decision{Allowed: true, Reason: ReasonGroupAllowed, Text: p.cleanMention(text, in)}
	}
	if strings.HasPrefix(strings.TrimSpace(text), "/") ||
		p.repliesToBot(in) || p.mentionsBot(in) || p.matchesPattern(text) {
		return Decision{Allowed: true, Reason: ReasonGroupAllowed, Text: p.cleanMention(text, in)}
	}
	return p.deny(DenyMentionRequired)
}

// botAliases is the set of bare ids that mean "us" for this message.
func (p *AccessPolicy) botAliases(in Inbound) map[string]struct{} {
	out := map[string]struct{}{}
	for _, b := range in.BotIDs {
		for a := range Expand(p.resolver, b) {
			out[a] = struct{}{}
		}
	}
	return out
}

func (p *AccessPolicy) repliesToBot(in Inbound) bool {
	q := NormalizeID(in.QuotedParticipant)
	if q == "" {
		return false
	}
	_, ok := p.botAliases(in)[q]
	return ok
}

func (p *AccessPolicy) mentionsBot(in Inbound) bool {
	bots := p.botAliases(in)
	if len(bots) == 0 {
		return false
	}
	for _, m := range in.MentionedIDs {
		if _, ok := bots[NormalizeID(m)]; ok {
			return true
		}
	}
	body := strings.ToLower(in.Text)
	for b := range bots {
		if strings.Contains(body, "@"+b) || strings.Contains(body, b) {
			return true
		}
	}
	return false
}

func (p *AccessPolicy) matchesPattern(text string) bool {
	for _, re := range p.c.patterns {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// cleanMention removes a leading "@<bot id>[,:-]" from text; falls back to the
// original when nothing else would remain.
func (p *AccessPolicy) cleanMention(text string, in Inbound) string {
	if text == "" {
		return text
	}
	cleaned := text
	for b := range p.botAliases(in) {
		re := regexp.MustCompile(`@` + regexp.QuoteMeta(b) + `\b[,:\-]*\s*`)
		cleaned = re.ReplaceAllString(cleaned, "")
	}
	if cleaned = strings.TrimSpace(cleaned); cleaned == "" {
		return text
	}
	return cleaned
}

// replyFor returns the configured deny reply if one is set and this sender
// has not been answered within the interval; it records the reply.
func (p *AccessPolicy) replyFor(senderID string) string {
	if p.c.denyReply == "" {
		return ""
	}
	key := Canonical(p.resolver, senderID)
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if last, ok := p.replied[key]; ok && now.Sub(last) < p.c.denyInterval {
		return ""
	}
	if len(p.replied) >= maxReplyState {
		for k, t := range p.replied {
			if now.Sub(t) >= p.c.denyInterval {
				delete(p.replied, k)
			}
		}
		if len(p.replied) >= maxReplyState {
			// Flooded with distinct strangers: stop replying rather than grow.
			return ""
		}
	}
	p.replied[key] = now
	return p.c.denyReply
}

// audit writes one structured line per decision: reason, kind and redacted
// ids. Message text is never logged. Denials are Info (they are the security
// record); routine noise and admissions are Debug.
func (p *AccessPolicy) audit(in Inbound, d Decision) {
	ctx := context.Background()
	lvl := slog.LevelDebug
	msg := "whatsapp: message admitted"
	if !d.Allowed {
		msg = "whatsapp: message denied"
		if !d.Reason.noisy() {
			lvl = slog.LevelInfo
		}
	}
	p.log.LogAttrs(ctx, lvl, msg, p.attrs(in, d)...)
}

func (p *AccessPolicy) attrs(in Inbound, d Decision) []slog.Attr {
	return []slog.Attr{
		slog.String("channel", in.Channel),
		slog.String("reason", string(d.Reason)),
		slog.String("chat", RedactID(in.ChatID)),
		slog.String("chat_kind", KindOf(in.ChatID).String()),
		slog.String("sender", RedactID(in.SenderID)),
		slog.Bool("from_me", in.FromMe),
		slog.Bool("reply", d.Reply != ""),
	}
}

// RedactID masks all but the last four digits of an id for logs, keeping the
// domain: "15551234567@s.whatsapp.net" becomes "***4567@s.whatsapp.net".
func RedactID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	user := NormalizeID(id)
	_, domain, _ := strings.Cut(id, "@")
	if len(user) > 4 {
		user = user[len(user)-4:]
	}
	out := "***" + user
	if domain != "" {
		out += "@" + domain
	}
	return out
}
