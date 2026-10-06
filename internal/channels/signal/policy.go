package signal

import (
	"context"
	"log/slog"
	"strings"

	"github.com/slimslenderslacks/work/internal/channels"
)

// Inbound is a received Signal message plus the transport facts the policy
// needs. The embedded InboundMessage supplies ChatID, SenderID, Channel and Text.
type Inbound struct {
	channels.InboundMessage

	// SenderNumber and SenderUUID are the sender's identities as signal-cli
	// reports them; either may be empty (sealed sender / phone-number privacy).
	SenderNumber string
	SenderUUID   string
	// GroupID is set for group messages.
	GroupID string
	// NoteToSelf marks a message the account owner wrote in their own
	// "Note to Self" conversation (a sync message from a linked device).
	NoteToSelf bool
}

// Reason says why a message was admitted or refused: a short stable token that
// is safe to log and match on and never contains message content.
type Reason string

// Allow reasons.
const (
	ReasonAllowlisted  Reason = "allowlisted"
	ReasonAllowAll     Reason = "allow_all"
	ReasonNoteToSelf   Reason = "note_to_self"
	ReasonGroupAllowed Reason = "group_allowed"
)

// Deny reasons.
const (
	DenyNoChat                Reason = "no_chat"
	DenyNoSender              Reason = "no_sender"
	DenyAllowlistEmpty        Reason = "allowlist_empty"
	DenyNotAllowlisted        Reason = "sender_not_allowlisted"
	DenyNoteToSelfDisabled    Reason = "note_to_self_disabled"
	DenyGroupDisabled         Reason = "group_disabled"
	DenyGroupNotAllowed       Reason = "group_not_allowed"
	DenyGroupSenderNotAllowed Reason = "group_sender_not_allowlisted"
)

// Decision is the verdict on one inbound message.
type Decision struct {
	Allowed bool
	Reason  Reason
	// FromOwner marks a message the account holder wrote in Note to Self.
	FromOwner bool
}

// String renders "allow: allowlisted" / "deny: sender_not_allowlisted".
func (d Decision) String() string {
	if d.Allowed {
		return "allow: " + string(d.Reason)
	}
	return "deny: " + string(d.Reason)
}

// LogValue lets a Decision go straight to slog without exposing anything else.
func (d Decision) LogValue() slog.Value {
	return slog.GroupValue(slog.Bool("allowed", d.Allowed), slog.String("reason", string(d.Reason)))
}

// AccessPolicy decides which inbound Signal messages may reach the daemon. It
// is default-deny and immutable after construction, so safe for concurrent use.
type AccessPolicy struct {
	allow      map[string]struct{}
	allowAll   bool
	noteToSelf bool
	groups     bool
	groupAllow map[string]struct{}
	log        *slog.Logger
}

// NewAccessPolicy builds the policy from a validated Config. A nil logger uses
// slog.Default().
func NewAccessPolicy(cfg Config, log *slog.Logger) *AccessPolicy {
	if log == nil {
		log = slog.Default()
	}
	a := cfg.Access
	p := &AccessPolicy{
		allow:      map[string]struct{}{},
		allowAll:   a.AllowAll,
		noteToSelf: a.NoteToSelf,
		groups:     a.Groups,
		groupAllow: map[string]struct{}{},
		log:        log,
	}
	for _, e := range a.AllowFrom {
		if n, ok := normalizeID(e); ok {
			p.allow[n] = struct{}{}
		}
	}
	for _, g := range a.GroupAllowFrom {
		p.groupAllow[strings.TrimPrefix(strings.TrimSpace(g), GroupPrefix)] = struct{}{}
	}
	switch {
	case p.allowAll:
		log.Warn("signal: allow_all is ENABLED: anyone who can message this number can drive the workingman agent and read orch state. Set access.allow_from instead.")
	case len(p.allow) == 0 && !p.noteToSelf:
		log.Warn("signal: access.allow_from is empty; every inbound message will be denied")
	}
	return p
}

// Decide rules on one inbound message and logs the verdict (reason and
// redacted ids only, never the text). Denials are Info, admissions Debug.
func (p *AccessPolicy) Decide(in Inbound) Decision {
	d := p.decide(in)
	lvl, msg := slog.LevelDebug, "signal: message admitted"
	if !d.Allowed {
		lvl, msg = slog.LevelInfo, "signal: message denied"
	}
	p.log.LogAttrs(context.Background(), lvl, msg,
		slog.String("channel", in.Channel),
		slog.String("reason", string(d.Reason)),
		slog.String("chat", RedactID(in.ChatID)),
		slog.String("sender", RedactID(in.SenderID)),
		slog.Bool("group", in.GroupID != ""),
	)
	return d
}

func (p *AccessPolicy) deny(r Reason) Decision { return Decision{Reason: r} }

func (p *AccessPolicy) decide(in Inbound) Decision {
	if strings.TrimSpace(in.ChatID) == "" {
		return p.deny(DenyNoChat)
	}
	if in.NoteToSelf {
		// Only the owner's linked devices can produce one, but it stays opt-in.
		if !p.noteToSelf {
			return p.deny(DenyNoteToSelfDisabled)
		}
		return Decision{Allowed: true, Reason: ReasonNoteToSelf, FromOwner: true}
	}
	if in.SenderNumber == "" && in.SenderUUID == "" {
		return p.deny(DenyNoSender)
	}
	if in.GroupID != "" {
		return p.decideGroup(in)
	}
	return p.decideDM(in)
}

func (p *AccessPolicy) decideDM(in Inbound) Decision {
	if p.allowAll {
		return Decision{Allowed: true, Reason: ReasonAllowAll}
	}
	if len(p.allow) == 0 {
		return p.deny(DenyAllowlistEmpty)
	}
	if p.senderListed(in) {
		return Decision{Allowed: true, Reason: ReasonAllowlisted}
	}
	return p.deny(DenyNotAllowlisted)
}

func (p *AccessPolicy) decideGroup(in Inbound) Decision {
	if !p.groups {
		return p.deny(DenyGroupDisabled)
	}
	if _, ok := p.groupAllow[in.GroupID]; !ok {
		return p.deny(DenyGroupNotAllowed)
	}
	// The group being served is not enough: the sender must be someone we
	// would talk to in a direct chat too.
	if p.allowAll {
		return Decision{Allowed: true, Reason: ReasonAllowAll}
	}
	if p.senderListed(in) {
		return Decision{Allowed: true, Reason: ReasonGroupAllowed}
	}
	return p.deny(DenyGroupSenderNotAllowed)
}

// senderListed reports whether the sender's number or UUID is on the allowlist.
func (p *AccessPolicy) senderListed(in Inbound) bool {
	for _, id := range []string{in.SenderNumber, in.SenderUUID} {
		if n, ok := normalizeID(id); ok {
			if _, hit := p.allow[n]; hit {
				return true
			}
		}
	}
	return false
}

// RedactID masks an id for logs: "+15557654321" becomes "***4321", a UUID
// "***<last 4>", and a group chat keeps its prefix.
func RedactID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	prefix := ""
	if strings.HasPrefix(id, GroupPrefix) {
		prefix, id = GroupPrefix, strings.TrimPrefix(id, GroupPrefix)
	}
	r := []rune(id)
	if len(r) > 4 {
		r = r[len(r)-4:]
	}
	return prefix + "***" + string(r)
}
