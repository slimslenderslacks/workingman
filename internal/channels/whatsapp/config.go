package whatsapp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Mode is how the daemon's WhatsApp number relates to the person using it.
type Mode string

const (
	// ModeBot is a number of its own: other people's messages arrive as
	// incoming and are admitted by the allowlist. The default.
	ModeBot Mode = "bot"
	// ModeSelfChat is the "personal number" case: the user messages
	// themselves, and only their own messages in their own chat are admitted.
	ModeSelfChat Mode = "self-chat"
)

// DefaultDenyReplyInterval spaces deny replies to the same sender.
const DefaultDenyReplyInterval = time.Hour

// AccessConfig is the `access:` block under a WhatsApp channel's options:
//
//	channels:
//	  whatsapp:
//	    type: whatsapp
//	    options:
//	      access:
//	        mode: bot                    # or self-chat
//	        allow_from: ["+1 555 123 4567", "1555@lid"]
//	        # allow_all: true            # DANGEROUS opt-in; logs a loud warning
//	        # self_ids: ["15551234567", "9988776655@lid"]   # self-chat only
//	        # forward_owner_messages: true                  # bot mode only
//	        # groups: true
//	        # group_allow_from: ["120363041234@g.us"]
//	        # require_mention: true
//	        # mention_patterns: ["^hey wm\\b"]
//	        # free_response_chats: ["120363041234@g.us"]
//	        # deny_reply: "Sorry, I can't chat with you."
//	        # deny_reply_interval: 1h
//	        # reply_prefix: "[wm] "      # marks our own replies so they are not re-read
//
// The zero value denies everything.
type AccessConfig struct {
	Mode                 Mode          `yaml:"mode"`
	AllowFrom            []string      `yaml:"allow_from"`
	AllowAll             bool          `yaml:"allow_all"`
	SelfIDs              []string      `yaml:"self_ids"`
	ForwardOwnerMessages bool          `yaml:"forward_owner_messages"`
	Groups               bool          `yaml:"groups"`
	GroupAllowFrom       []string      `yaml:"group_allow_from"`
	RequireMention       bool          `yaml:"require_mention"`
	MentionPatterns      []string      `yaml:"mention_patterns"`
	FreeResponseChats    []string      `yaml:"free_response_chats"`
	DenyReply            string        `yaml:"deny_reply"`
	DenyReplyInterval    time.Duration `yaml:"deny_reply_interval"`
	ReplyPrefix          string        `yaml:"reply_prefix"`
}

// AccessOptionsKey is the key inside channels.ChannelConfig.Options that holds
// the AccessConfig.
const AccessOptionsKey = "access"

// ParseAccessOptions extracts and strictly decodes the `access` block from a
// channel's free-form options. A missing block yields the zero AccessConfig,
// which denies everyone. Unknown keys are errors so a typo such as
// `allowfrom` cannot silently leave the policy on its fail-closed default
// unnoticed, or worse, appear to be doing something it is not.
func ParseAccessOptions(options map[string]any) (AccessConfig, error) {
	var cfg AccessConfig
	raw, ok := options[AccessOptionsKey]
	if !ok || raw == nil {
		return cfg, nil
	}
	data, err := yaml.Marshal(raw)
	if err != nil {
		return cfg, fmt.Errorf("whatsapp: options.%s: %w", AccessOptionsKey, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return AccessConfig{}, fmt.Errorf("whatsapp: options.%s: %w", AccessOptionsKey, err)
	}
	return cfg, nil
}

// compiled is a validated AccessConfig.
type compiled struct {
	mode           Mode
	allow          []string // normalized, digits only
	allowAll       bool
	self           []string
	forwardOwner   bool
	groups         bool
	groupAllow     map[string]struct{}
	requireMention bool
	patterns       []*regexp.Regexp
	freeChats      map[string]struct{}
	denyReply      string
	denyInterval   time.Duration
	replyPrefix    string
}

// compile validates cfg. Every problem is reported at once, prefixed with its
// YAML key. Misconfiguration is an error rather than a quietly weaker policy.
func (cfg AccessConfig) compile() (*compiled, error) {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf("access."+format, args...)) }

	c := &compiled{
		mode:           cfg.Mode,
		allowAll:       cfg.AllowAll,
		forwardOwner:   cfg.ForwardOwnerMessages,
		groups:         cfg.Groups,
		requireMention: cfg.RequireMention,
		denyReply:      strings.TrimSpace(cfg.DenyReply),
		denyInterval:   cfg.DenyReplyInterval,
		replyPrefix:    cfg.ReplyPrefix,
	}
	switch c.mode {
	case "":
		c.mode = ModeBot
	case ModeBot, ModeSelfChat:
	default:
		add("mode: %q is not one of %q, %q", cfg.Mode, ModeBot, ModeSelfChat)
	}

	c.allow = userEntries("allow_from", cfg.AllowFrom, add)
	c.self = userEntries("self_ids", cfg.SelfIDs, add)

	c.groupAllow = map[string]struct{}{}
	for i, e := range cfg.GroupAllowFrom {
		n := NormalizeID(e)
		switch {
		case strings.TrimSpace(e) == "*":
			add("group_allow_from[%d]: wildcard is not allowed; list the group ids", i)
		case !groupIDRE.MatchString(n):
			add("group_allow_from[%d]: %q is not a group id", i, e)
		default:
			c.groupAllow[n] = struct{}{}
		}
	}
	c.freeChats = map[string]struct{}{}
	for i, e := range cfg.FreeResponseChats {
		if n := NormalizeID(e); groupIDRE.MatchString(n) {
			c.freeChats[n] = struct{}{}
		} else {
			add("free_response_chats[%d]: %q is not a chat id", i, e)
		}
	}
	for i, p := range cfg.MentionPatterns {
		if strings.TrimSpace(p) == "" {
			continue
		}
		re, err := regexp.Compile("(?i)" + p)
		if err != nil {
			add("mention_patterns[%d]: %v", i, err)
			continue
		}
		c.patterns = append(c.patterns, re)
	}

	if c.mode == ModeSelfChat {
		if len(c.self) == 0 {
			add("self_ids: required in self-chat mode (the account's own phone number and/or LID)")
		}
		if c.allowAll {
			add("allow_all: contradicts self-chat mode, which only ever admits the owner")
		}
		if c.forwardOwner {
			add("forward_owner_messages: only applies in bot mode")
		}
	} else if len(c.self) > 0 {
		add("self_ids: only applies in self-chat mode")
	}
	if c.groups && len(c.groupAllow) == 0 {
		add("groups: enabled but group_allow_from is empty; list the group ids to serve")
	}
	if !c.groups && (len(cfg.GroupAllowFrom) > 0 || c.requireMention) {
		add("groups: group settings given but groups is not enabled")
	}
	if c.denyInterval < 0 {
		add("deny_reply_interval: must not be negative")
	}
	if c.denyInterval == 0 {
		c.denyInterval = DefaultDenyReplyInterval
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return c, nil
}

// userEntries normalizes person ids: "+1 (555) 123-4567" and "5551234567@lid"
// both work; "*" and anything not reducible to digits are rejected so a typo
// can never become a match-everything or match-nothing surprise.
func userEntries(key string, entries []string, add func(string, ...any)) []string {
	var out []string
	for i, e := range entries {
		raw := strings.TrimSpace(e)
		if raw == "*" {
			add("%s[%d]: wildcard is not allowed; use allow_all: true to open the channel", key, i)
			continue
		}
		n := NormalizeID(raw)
		if !strings.Contains(raw, "@") && barePhoneRE.MatchString(raw) {
			n = digitsOnly(raw) // "+1 (555) 123-4567"
		}
		if !digitsRE.MatchString(n) {
			add("%s[%d]: %q is not a phone number or LID", key, i, e)
			continue
		}
		out = append(out, n)
	}
	return out
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
