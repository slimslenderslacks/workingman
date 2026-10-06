package whatsapp

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/channels"
)

const (
	ownerPhone = "15551234567"
	ownerLID   = "99887766554433"
	otherPhone = "15557654321"
	groupID    = "120363041234@g.us"
	botPhone   = "15550001111"
	botLID     = "5544332211"
)

func dm(sender string) Inbound {
	return Inbound{InboundMessage: channels.InboundMessage{
		ChatID: sender, SenderID: sender, MessageID: "m1", Text: "hello",
	}}
}

func grp(sender string) Inbound {
	return Inbound{
		IsGroup: true,
		InboundMessage: channels.InboundMessage{
			ChatID: groupID, SenderID: sender, MessageID: "m1", Text: "hello",
		},
	}
}

func mustPolicy(t *testing.T, cfg AccessConfig, opts ...Option) *AccessPolicy {
	t.Helper()
	p, err := NewAccessPolicy(cfg, append([]Option{WithLogger(discard())}, opts...)...)
	if err != nil {
		t.Fatalf("NewAccessPolicy: %v", err)
	}
	return p
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)) }

func linked() *StaticResolver {
	r := &StaticResolver{}
	r.Link(ownerPhone, ownerLID)
	r.Link(botPhone, botLID)
	return r
}

// TestDefaultDeniesEverything: the zero config is the fail-closed default.
func TestDefaultDeniesEverything(t *testing.T) {
	p := mustPolicy(t, AccessConfig{})
	for name, in := range map[string]Inbound{
		"dm":     dm(ownerPhone),
		"jid":    dm(ownerPhone + "@s.whatsapp.net"),
		"lid":    dm(ownerLID + "@lid"),
		"group":  grp(ownerPhone),
		"fromMe": func() Inbound { i := dm(ownerPhone); i.FromMe = true; return i }(),
	} {
		if d := p.Decide(in); d.Allowed {
			t.Errorf("%s: allowed with zero config (%s)", name, d)
		}
	}
	if d := p.Decide(dm(ownerPhone)); d.Reason != DenyAllowlistEmpty {
		t.Errorf("reason = %s, want %s", d.Reason, DenyAllowlistEmpty)
	}
}

func TestDMMatrix(t *testing.T) {
	cfg := AccessConfig{AllowFrom: []string{"+1 (555) 123-4567"}}
	tests := []struct {
		name   string
		cfg    AccessConfig
		res    Resolver
		in     Inbound
		allow  bool
		reason Reason
	}{
		{"bare phone allowed", cfg, nil, dm(ownerPhone), true, ReasonAllowlisted},
		{"phone jid allowed", cfg, nil, dm(ownerPhone + "@s.whatsapp.net"), true, ReasonAllowlisted},
		{"device suffix allowed", cfg, nil, dm(ownerPhone + ":12@s.whatsapp.net"), true, ReasonAllowlisted},
		{"plus prefix allowed", cfg, nil, dm("+" + ownerPhone), true, ReasonAllowlisted},
		{"other denied", cfg, nil, dm(otherPhone), false, DenyNotAllowlisted},
		{"prefix of allowed number denied", cfg, nil, dm("1555123456"), false, DenyNotAllowlisted},
		{"superset of allowed number denied", cfg, nil, dm(ownerPhone + "9"), false, DenyNotAllowlisted},
		{"unmapped lid denied", cfg, nil, dm(ownerLID + "@lid"), false, DenyNotAllowlisted},
		{"mapped lid allowed", cfg, linked(), dm(ownerLID + "@lid"), true, ReasonAllowlisted},
		{"mapped lid with device allowed", cfg, linked(), dm(ownerLID + ":7@lid"), true, ReasonAllowlisted},
		{"other lid still denied with mapping", cfg, linked(), dm("123123@lid"), false, DenyNotAllowlisted},
		{
			"lid entry admits phone sender",
			AccessConfig{AllowFrom: []string{ownerLID + "@lid"}}, linked(), dm(ownerPhone), true, ReasonAllowlisted,
		},
		{
			"first-contact lid with phone alt allowed",
			cfg, nil, func() Inbound { i := dm(ownerLID + "@lid"); i.SenderAltID = ownerPhone + "@s.whatsapp.net"; return i }(),
			true, ReasonAllowlisted,
		},
		{
			"alt id cannot smuggle a non-allowlisted sender",
			cfg, nil, func() Inbound { i := dm(otherPhone); i.SenderAltID = "999@lid"; return i }(),
			false, DenyNotAllowlisted,
		},
		{"empty sender denied", cfg, nil, func() Inbound { i := dm(ownerPhone); i.SenderID = ""; return i }(), false, DenyNoSender},
		{"junk sender denied", cfg, nil, func() Inbound { i := dm(ownerPhone); i.SenderID = "@lid"; return i }(), false, DenyNoSender},
		{"empty chat denied", cfg, nil, func() Inbound { i := dm(ownerPhone); i.ChatID = ""; return i }(), false, DenyNoChat},
		{"status broadcast denied", cfg, nil, func() Inbound { i := dm(ownerPhone); i.ChatID = "status@broadcast"; return i }(), false, DenyBroadcast},
		{"newsletter denied", cfg, nil, func() Inbound { i := dm(ownerPhone); i.ChatID = "1203@newsletter"; return i }(), false, DenyBroadcast},
		{"unknown chat domain denied", cfg, nil, func() Inbound { i := dm(ownerPhone); i.ChatID = "5@hosted"; return i }(), false, DenyUnsupportedChat},
		{"empty allowlist denied", AccessConfig{}, nil, dm(ownerPhone), false, DenyAllowlistEmpty},
		{"allow all admits stranger", AccessConfig{AllowAll: true}, nil, dm(otherPhone), true, ReasonAllowAll},
		{"allow all still needs a sender", AccessConfig{AllowAll: true}, nil, func() Inbound { i := dm(ownerPhone); i.SenderID = ""; return i }(), false, DenyNoSender},
		{"allow all still drops broadcast", AccessConfig{AllowAll: true}, nil, func() Inbound { i := dm(ownerPhone); i.ChatID = "status@broadcast"; return i }(), false, DenyBroadcast},
		{
			"two entries second matches",
			AccessConfig{AllowFrom: []string{otherPhone, ownerPhone}}, nil, dm(ownerPhone), true, ReasonAllowlisted,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := mustPolicy(t, tt.cfg, WithResolver(tt.res)).Decide(tt.in)
			if d.Allowed != tt.allow || d.Reason != tt.reason {
				t.Errorf("got %s (allowed=%v), want allowed=%v reason=%s", d, d.Allowed, tt.allow, tt.reason)
			}
		})
	}
}

func TestSelfChat(t *testing.T) {
	cfg := AccessConfig{Mode: ModeSelfChat, SelfIDs: []string{ownerPhone, ownerLID + "@lid"}}
	fromMe := func(chat string) Inbound {
		i := dm(chat)
		i.FromMe = true
		return i
	}
	tests := []struct {
		name   string
		cfg    AccessConfig
		res    Resolver
		in     Inbound
		allow  bool
		reason Reason
		owner  bool
	}{
		{"own phone chat", cfg, nil, fromMe(ownerPhone + "@s.whatsapp.net"), true, ReasonSelfChat, true},
		{"own lid chat", cfg, nil, fromMe(ownerLID + "@lid"), true, ReasonSelfChat, true},
		{"own chat with device suffix", cfg, nil, fromMe(ownerPhone + ":3@s.whatsapp.net"), true, ReasonSelfChat, true},
		{
			"only lid configured, phone chat via mapping",
			AccessConfig{Mode: ModeSelfChat, SelfIDs: []string{ownerLID}}, linked(),
			fromMe(ownerPhone + "@s.whatsapp.net"), true, ReasonSelfChat, true,
		},
		{"owner messaging someone else", cfg, nil, fromMe(otherPhone + "@s.whatsapp.net"), false, DenySelfChatOtherChat, false},
		{"stranger dm", cfg, nil, dm(otherPhone), false, DenySelfChatNonSelf, false},
		{"stranger dm even if id matches self", cfg, nil, dm(ownerPhone), false, DenySelfChatNonSelf, false},
		{"owner in group", cfg, nil, func() Inbound { i := grp(ownerPhone); i.FromMe = true; return i }(), false, DenyFromMeGroup, false},
		{"group from others", cfg, nil, grp(otherPhone), false, DenySelfChatNonSelf, false},
		{"status update", cfg, nil, fromMe("status@broadcast"), false, DenyBroadcast, false},
		{
			"our reply prefix is an echo", AccessConfig{Mode: ModeSelfChat, SelfIDs: []string{ownerPhone}, ReplyPrefix: "[wm] "},
			nil, func() Inbound { i := fromMe(ownerPhone); i.Text = "[wm] 3 projects open"; return i }(), false, DenyEcho, false,
		},
		{
			"owner text that merely contains the prefix", AccessConfig{Mode: ModeSelfChat, SelfIDs: []string{ownerPhone}, ReplyPrefix: "[wm] "},
			nil, func() Inbound { i := fromMe(ownerPhone); i.Text = "what is [wm] ?"; return i }(), true, ReasonSelfChat, true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := mustPolicy(t, tt.cfg, WithResolver(tt.res)).Decide(tt.in)
			if d.Allowed != tt.allow || d.Reason != tt.reason || d.FromOwner != tt.owner {
				t.Errorf("got %s owner=%v, want allowed=%v reason=%s owner=%v", d, d.FromOwner, tt.allow, tt.reason, tt.owner)
			}
		})
	}
}

func TestSelfChatIgnoresAllowlistEntries(t *testing.T) {
	// A stranger on allow_from still cannot talk in self-chat mode: the mode
	// is "owner only", whatever else is configured.
	p := mustPolicy(t, AccessConfig{Mode: ModeSelfChat, SelfIDs: []string{ownerPhone}, AllowFrom: []string{otherPhone}})
	if d := p.Decide(dm(otherPhone)); d.Allowed {
		t.Fatalf("stranger admitted in self-chat mode: %s", d)
	}
}

func TestEchoSuppression(t *testing.T) {
	p := mustPolicy(t, AccessConfig{Mode: ModeSelfChat, SelfIDs: []string{ownerPhone}})
	in := dm(ownerPhone)
	in.FromMe = true
	in.MessageID = "OUT-1"
	if d := p.Decide(in); !d.Allowed {
		t.Fatalf("unrecorded id should pass as owner message: %s", d)
	}
	p.RecordSent("OUT-1")
	p.RecordSent("OUT-1") // idempotent
	p.RecordSent("")      // ignored
	if d := p.Decide(in); d.Allowed || d.Reason != DenyEcho {
		t.Fatalf("recorded id must be an echo, got %s", d)
	}
	in.MessageID = "typed-by-owner"
	if d := p.Decide(in); !d.Allowed {
		t.Fatalf("other id should pass: %s", d)
	}
}

func TestEchoMemoryIsBounded(t *testing.T) {
	p := mustPolicy(t, AccessConfig{Mode: ModeSelfChat, SelfIDs: []string{ownerPhone}})
	for i := 0; i < maxSent+10; i++ {
		p.RecordSent("id-" + itoa(i))
	}
	if len(p.sent) != maxSent || len(p.sentOrder) != maxSent {
		t.Fatalf("sent=%d order=%d, want %d", len(p.sent), len(p.sentOrder), maxSent)
	}
	in := dm(ownerPhone)
	in.FromMe = true
	in.MessageID = "id-0" // evicted
	if d := p.Decide(in); !d.Allowed {
		t.Errorf("evicted id should no longer be an echo: %s", d)
	}
	in.MessageID = "id-" + itoa(maxSent+9)
	if d := p.Decide(in); d.Allowed {
		t.Errorf("newest id should still be an echo: %s", d)
	}
}

func TestOwnerMessageGate(t *testing.T) {
	fromMe := func(chat string) Inbound {
		i := dm(chat)
		i.FromMe = true
		i.SenderID = "999@lid" // the owner, who is never on the allowlist
		return i
	}
	tests := []struct {
		name   string
		cfg    AccessConfig
		in     Inbound
		allow  bool
		reason Reason
	}{
		{"disabled by default", AccessConfig{AllowFrom: []string{otherPhone}}, fromMe(otherPhone), false, DenyFromMeDisabled},
		{
			"forwarded when chat allowlisted",
			AccessConfig{AllowFrom: []string{otherPhone}, ForwardOwnerMessages: true}, fromMe(otherPhone + "@s.whatsapp.net"), true, ReasonOwnerMessage,
		},
		{
			"dropped when chat not allowlisted (no leak of owner replies to strangers)",
			AccessConfig{AllowFrom: []string{ownerPhone}, ForwardOwnerMessages: true}, fromMe(otherPhone), false, DenyOwnerChatNotAllowed,
		},
		{
			"empty allowlist drops",
			AccessConfig{ForwardOwnerMessages: true}, fromMe(otherPhone), false, DenyOwnerChatNotAllowed,
		},
		{
			"allow all forwards", AccessConfig{AllowAll: true, ForwardOwnerMessages: true}, fromMe(otherPhone), true, ReasonOwnerMessage,
		},
		{
			"group dropped", AccessConfig{AllowFrom: []string{otherPhone}, ForwardOwnerMessages: true},
			func() Inbound { i := grp("999@lid"); i.FromMe = true; return i }(), false, DenyFromMeGroup,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := mustPolicy(t, tt.cfg).Decide(tt.in)
			if d.Allowed != tt.allow || d.Reason != tt.reason {
				t.Errorf("got %s, want allowed=%v reason=%s", d, tt.allow, tt.reason)
			}
			if d.Allowed && !d.FromOwner {
				t.Error("allowed owner message must be marked FromOwner")
			}
		})
	}
}

func TestOwnerMessageEcho(t *testing.T) {
	p := mustPolicy(t, AccessConfig{AllowFrom: []string{otherPhone}, ForwardOwnerMessages: true})
	p.RecordSent("OUT-9")
	in := dm(otherPhone)
	in.FromMe = true
	in.MessageID = "OUT-9"
	if d := p.Decide(in); d.Reason != DenyEcho {
		t.Fatalf("got %s, want echo", d)
	}
}

func TestGroups(t *testing.T) {
	base := AccessConfig{AllowFrom: []string{ownerPhone}}
	enabled := AccessConfig{AllowFrom: []string{ownerPhone}, Groups: true, GroupAllowFrom: []string{groupID}}
	mentionCfg := AccessConfig{
		AllowFrom: []string{ownerPhone}, Groups: true, GroupAllowFrom: []string{groupID},
		RequireMention: true, MentionPatterns: []string{`^hey wm\b`},
	}
	withBot := func(i Inbound) Inbound {
		i.BotIDs = []string{botPhone + "@s.whatsapp.net", botLID + ":2@lid"}
		return i
	}
	text := func(i Inbound, s string) Inbound { i.Text = s; return i }
	otherGroup := func(i Inbound) Inbound { i.ChatID = "120363999999@g.us"; return i }
	noFlag := func(i Inbound) Inbound { i.IsGroup = false; return i } // @g.us alone must still mean group

	tests := []struct {
		name   string
		cfg    AccessConfig
		res    Resolver
		in     Inbound
		allow  bool
		reason Reason
		text   string
	}{
		{"groups ignored by default", base, nil, grp(ownerPhone), false, DenyGroupDisabled, ""},
		{"@g.us without IsGroup flag is still a group", base, nil, noFlag(grp(ownerPhone)), false, DenyGroupDisabled, ""},
		{"enabled: listed group + allowlisted sender", enabled, nil, grp(ownerPhone), true, ReasonGroupAllowed, "hello"},
		{"enabled: unlisted group", enabled, nil, otherGroup(grp(ownerPhone)), false, DenyGroupNotAllowed, ""},
		{"enabled: stranger in listed group", enabled, nil, grp(otherPhone), false, DenyGroupSenderNotAllowed, ""},
		{"enabled: lid participant mapped to owner", enabled, linked(), grp(ownerLID + "@lid"), true, ReasonGroupAllowed, "hello"},
		{"enabled: lid participant unmapped", enabled, nil, grp(ownerLID + "@lid"), false, DenyGroupSenderNotAllowed, ""},
		{"enabled: participant alt id", enabled, nil, func() Inbound { i := grp(ownerLID + "@lid"); i.SenderAltID = ownerPhone + "@s.whatsapp.net"; return i }(), true, ReasonGroupAllowed, "hello"},
		{"enabled: no sender", enabled, nil, func() Inbound { i := grp(ownerPhone); i.SenderID = ""; return i }(), false, DenyNoSender, ""},
		{"enabled: group id with device-ish suffix", enabled, nil, func() Inbound { i := grp(ownerPhone); i.ChatID = "120363041234:5@g.us"; return i }(), true, ReasonGroupAllowed, "hello"},
		{
			"enabled + allow all: any sender in listed group",
			AccessConfig{AllowAll: true, Groups: true, GroupAllowFrom: []string{groupID}}, nil, grp(otherPhone), true, ReasonGroupAllowed, "hello",
		},
		{
			"allow all does not open unlisted groups",
			AccessConfig{AllowAll: true, Groups: true, GroupAllowFrom: []string{groupID}}, nil, otherGroup(grp(otherPhone)), false, DenyGroupNotAllowed, "",
		},
		{"mention required: plain text", mentionCfg, nil, withBot(grp(ownerPhone)), false, DenyMentionRequired, ""},
		{"mention required: slash command", mentionCfg, nil, withBot(text(grp(ownerPhone), "/status")), true, ReasonGroupAllowed, "/status"},
		{"mention required: pattern", mentionCfg, nil, withBot(text(grp(ownerPhone), "Hey WM what's running")), true, ReasonGroupAllowed, "Hey WM what's running"},
		{
			"mention required: explicit mention ids",
			mentionCfg, nil, withBot(func() Inbound {
				i := grp(ownerPhone)
				i.MentionedIDs = []string{botPhone + "@s.whatsapp.net"}
				return i
			}()),
			true, ReasonGroupAllowed, "hello",
		},
		{
			"mention required: mention by LID alias of the bot",
			mentionCfg, linked(), func() Inbound {
				i := grp(ownerPhone)
				i.BotIDs = []string{botPhone + "@s.whatsapp.net"}
				i.MentionedIDs = []string{botLID + "@lid"}
				return i
			}(), true, ReasonGroupAllowed, "hello",
		},
		{
			"mention required: mentioning someone else",
			mentionCfg, nil, withBot(func() Inbound { i := grp(ownerPhone); i.MentionedIDs = []string{"777@lid"}; return i }()),
			false, DenyMentionRequired, "",
		},
		{"mention required: @number in body, stripped", mentionCfg, nil, withBot(text(grp(ownerPhone), "@"+botPhone+" list projects")), true, ReasonGroupAllowed, "list projects"},
		{"mention required: @number only keeps text", mentionCfg, nil, withBot(text(grp(ownerPhone), "@"+botPhone)), true, ReasonGroupAllowed, "@" + botPhone},
		{
			"mention required: reply to bot",
			mentionCfg, nil, withBot(func() Inbound { i := grp(ownerPhone); i.QuotedParticipant = botLID + ":4@lid"; return i }()),
			true, ReasonGroupAllowed, "hello",
		},
		{
			"mention required: reply to someone else",
			mentionCfg, nil, withBot(func() Inbound { i := grp(ownerPhone); i.QuotedParticipant = otherPhone + "@s.whatsapp.net"; return i }()),
			false, DenyMentionRequired, "",
		},
		{"mention required: no bot ids known", mentionCfg, nil, func() Inbound { i := grp(ownerPhone); i.MentionedIDs = []string{botPhone}; return i }(), false, DenyMentionRequired, ""},
		{
			"mention does not bypass the sender allowlist",
			mentionCfg, nil, withBot(text(grp(otherPhone), "/status")), false, DenyGroupSenderNotAllowed, "",
		},
		{
			"free response chat skips mention",
			AccessConfig{AllowFrom: []string{ownerPhone}, Groups: true, GroupAllowFrom: []string{groupID}, RequireMention: true, FreeResponseChats: []string{groupID}},
			nil, withBot(grp(ownerPhone)), true, ReasonGroupAllowed, "hello",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := mustPolicy(t, tt.cfg, WithResolver(tt.res)).Decide(tt.in)
			if d.Allowed != tt.allow || d.Reason != tt.reason {
				t.Fatalf("got %s, want allowed=%v reason=%s", d, tt.allow, tt.reason)
			}
			if tt.allow && d.Text != tt.text {
				t.Errorf("Text = %q, want %q", d.Text, tt.text)
			}
			if !tt.allow && (d.Text != "" || d.Reply != "") {
				t.Errorf("denied decision carries Text=%q Reply=%q", d.Text, d.Reply)
			}
		})
	}
}

func TestDenyReplyRateLimit(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	p := mustPolicy(t, AccessConfig{
		AllowFrom: []string{ownerPhone}, DenyReply: "not for you", DenyReplyInterval: time.Minute,
	}, WithClock(clock), WithResolver(linked()))

	reply := func(sender string) string { return p.Decide(dm(sender)).Reply }

	if got := reply(otherPhone); got != "not for you" {
		t.Fatalf("first denial reply = %q", got)
	}
	if got := reply(otherPhone); got != "" {
		t.Fatalf("second denial inside interval replied %q", got)
	}
	if got := reply("123123@lid"); got != "not for you" {
		t.Fatalf("different sender should get its own reply, got %q", got)
	}
	now = now.Add(59 * time.Second)
	if got := reply(otherPhone); got != "" {
		t.Fatalf("still inside interval, got %q", got)
	}
	now = now.Add(2 * time.Second)
	if got := reply(otherPhone); got != "not for you" {
		t.Fatalf("after interval should reply again, got %q", got)
	}
	// Allowed senders never get the deny reply.
	if d := p.Decide(dm(ownerPhone)); !d.Allowed || d.Reply != "" {
		t.Fatalf("allowed decision: %+v", d)
	}
}

func TestDenyReplyOnlyForUnauthorizedDMs(t *testing.T) {
	cfg := AccessConfig{
		AllowFrom: []string{ownerPhone}, DenyReply: "no", Groups: true, GroupAllowFrom: []string{groupID},
	}
	p := mustPolicy(t, cfg)
	cases := map[string]Inbound{
		"group stranger":  grp(otherPhone),
		"other group":     func() Inbound { i := grp(otherPhone); i.ChatID = "1@g.us"; return i }(),
		"broadcast":       func() Inbound { i := dm(otherPhone); i.ChatID = "status@broadcast"; return i }(),
		"no sender":       func() Inbound { i := dm(otherPhone); i.SenderID = ""; return i }(),
		"fromMe disabled": func() Inbound { i := dm(otherPhone); i.FromMe = true; return i }(),
	}
	for name, in := range cases {
		if d := p.Decide(in); d.Allowed || d.Reply != "" {
			t.Errorf("%s: %+v", name, d)
		}
	}
	// And an empty allowlist stays silent too.
	if d := mustPolicy(t, AccessConfig{DenyReply: "no"}).Decide(dm(otherPhone)); d.Reply != "" {
		t.Errorf("empty allowlist replied %q", d.Reply)
	}
}

func TestDenyReplyDefaultsToSilence(t *testing.T) {
	if d := mustPolicy(t, AccessConfig{AllowFrom: []string{ownerPhone}}).Decide(dm(otherPhone)); d.Reply != "" {
		t.Fatalf("no deny_reply configured but replied %q", d.Reply)
	}
}

func TestDenyReplyStateIsBounded(t *testing.T) {
	now := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	p := mustPolicy(t, AccessConfig{AllowFrom: []string{ownerPhone}, DenyReply: "no", DenyReplyInterval: time.Hour},
		WithClock(func() time.Time { return now }))
	replies := 0
	for i := 0; i < maxReplyState+500; i++ {
		if p.Decide(dm("2"+itoa(1000000+i))).Reply != "" {
			replies++
		}
	}
	if replies != maxReplyState || len(p.replied) > maxReplyState {
		t.Fatalf("replies=%d tracked=%d, want both capped at %d", replies, len(p.replied), maxReplyState)
	}
	// Once the interval elapses the table is pruned and replies resume.
	now = now.Add(2 * time.Hour)
	if p.Decide(dm("2999999999")).Reply == "" {
		t.Fatal("expected replies to resume after expiry")
	}
}

func TestAuditLogsReasonNeverBody(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	p, err := NewAccessPolicy(AccessConfig{AllowFrom: []string{ownerPhone}}, WithLogger(log))
	if err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	in := dm(otherPhone + "@s.whatsapp.net")
	in.Text = "SUPER-SECRET-BODY"
	in.Channel = "whatsapp"
	p.Decide(in)
	out := buf.String()
	for _, want := range []string{"level=INFO", "reason=sender_not_allowlisted", "sender=***4321@s.whatsapp.net", "channel=whatsapp"} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "SUPER-SECRET-BODY") || strings.Contains(out, otherPhone) {
		t.Errorf("log leaks body or full number:\n%s", out)
	}

	// Routine noise is debug-level, not an Info audit line.
	buf.Reset()
	b := dm(otherPhone)
	b.ChatID = "status@broadcast"
	p.Decide(b)
	if !strings.Contains(buf.String(), "level=DEBUG") || strings.Contains(buf.String(), "level=INFO") {
		t.Errorf("broadcast denial should be debug:\n%s", buf.String())
	}
}

func TestStartupWarnings(t *testing.T) {
	warn := func(cfg AccessConfig) string {
		var buf bytes.Buffer
		if _, err := NewAccessPolicy(cfg, WithLogger(slog.New(slog.NewTextHandler(&buf, nil)))); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	if out := warn(AccessConfig{AllowAll: true}); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "allow_all is ENABLED") {
		t.Errorf("allow_all must warn loudly, got %q", out)
	}
	if out := warn(AccessConfig{}); !strings.Contains(out, "allow_from is empty") {
		t.Errorf("empty allowlist should warn, got %q", out)
	}
	if out := warn(AccessConfig{AllowFrom: []string{ownerPhone}}); out != "" {
		t.Errorf("healthy config should be quiet, got %q", out)
	}
}

func TestDecisionStringAndLogValue(t *testing.T) {
	if s := (Decision{Allowed: true, Reason: ReasonSelfChat}).String(); s != "allow: self_chat" {
		t.Error(s)
	}
	if s := (Decision{Reason: DenyEcho}).String(); s != "deny: echo" {
		t.Error(s)
	}
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("x", "decision", Decision{Reason: DenyEcho, Text: "body"})
	if strings.Contains(buf.String(), "body") || !strings.Contains(buf.String(), "decision.reason=echo") {
		t.Errorf("LogValue output: %s", buf.String())
	}
}

func TestConcurrentUse(t *testing.T) {
	p := mustPolicy(t, AccessConfig{AllowFrom: []string{ownerPhone}, DenyReply: "no"})
	done := make(chan struct{})
	for g := 0; g < 8; g++ {
		go func(g int) {
			defer func() { done <- struct{}{} }()
			for i := 0; i < 200; i++ {
				p.RecordSent("id" + itoa(g*1000+i))
				p.Decide(dm(otherPhone))
				p.Decide(dm(ownerPhone))
			}
		}(g)
	}
	for g := 0; g < 8; g++ {
		<-done
	}
}
