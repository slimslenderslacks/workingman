package whatsapp

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/channels"
)

func TestAccessConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     AccessConfig
		wantErr string // substring; "" means valid
	}{
		{"zero config is valid and denies", AccessConfig{}, ""},
		{"bot with allowlist", AccessConfig{Mode: ModeBot, AllowFrom: []string{"+1 555 123 4567", "9988@lid", "15551234567:3@s.whatsapp.net"}}, ""},
		{"unknown mode", AccessConfig{Mode: "yolo"}, `mode: "yolo"`},
		{"wildcard in allow_from", AccessConfig{AllowFrom: []string{"*"}}, "allow_from[0]: wildcard is not allowed"},
		{"typo entry", AccessConfig{AllowFrom: []string{"alice"}}, `allow_from[0]: "alice" is not a phone number or LID`},
		{"empty entry", AccessConfig{AllowFrom: []string{" "}}, "allow_from[0]"},
		{"jid junk entry", AccessConfig{AllowFrom: []string{"status@broadcast"}}, "allow_from[0]"},
		{"self-chat needs self ids", AccessConfig{Mode: ModeSelfChat}, "self_ids: required"},
		{"self-chat valid", AccessConfig{Mode: ModeSelfChat, SelfIDs: []string{"15551234567"}}, ""},
		{"self-chat + allow_all", AccessConfig{Mode: ModeSelfChat, SelfIDs: []string{"1"}, AllowAll: true}, "allow_all: contradicts"},
		{"self-chat + forward owner", AccessConfig{Mode: ModeSelfChat, SelfIDs: []string{"1"}, ForwardOwnerMessages: true}, "forward_owner_messages"},
		{"self ids in bot mode", AccessConfig{SelfIDs: []string{"1"}}, "self_ids: only applies"},
		{"groups without list", AccessConfig{Groups: true}, "group_allow_from is empty"},
		{"groups wildcard", AccessConfig{Groups: true, GroupAllowFrom: []string{"*"}}, "group_allow_from[0]: wildcard"},
		{"groups bad id", AccessConfig{Groups: true, GroupAllowFrom: []string{"my-group"}}, "group_allow_from[0]"},
		{"groups valid", AccessConfig{Groups: true, GroupAllowFrom: []string{"120363041234@g.us", "1555-1700000@g.us", "120363"}}, ""},
		{"group settings without enabling", AccessConfig{GroupAllowFrom: []string{"120363@g.us"}}, "groups is not enabled"},
		{"bad free chat", AccessConfig{Groups: true, GroupAllowFrom: []string{"1@g.us"}, FreeResponseChats: []string{"x"}}, "free_response_chats[0]"},
		{"bad pattern", AccessConfig{Groups: true, GroupAllowFrom: []string{"1@g.us"}, MentionPatterns: []string{"("}}, "mention_patterns[0]"},
		{"negative interval", AccessConfig{DenyReplyInterval: -time.Second}, "deny_reply_interval"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewAccessPolicy(tt.cfg, WithLogger(discard()))
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("expected error containing %q", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestAccessConfigReportsAllProblems(t *testing.T) {
	_, err := NewAccessPolicy(AccessConfig{Mode: "x", AllowFrom: []string{"*", "bob"}}, WithLogger(discard()))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"access.mode", "access.allow_from[0]", "access.allow_from[1]"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

func TestDefaultDenyReplyInterval(t *testing.T) {
	c, err := AccessConfig{DenyReply: "x"}.compile()
	if err != nil {
		t.Fatal(err)
	}
	if c.denyInterval != DefaultDenyReplyInterval {
		t.Errorf("interval = %v", c.denyInterval)
	}
}

func TestEntryNormalization(t *testing.T) {
	c, err := AccessConfig{AllowFrom: []string{"+1 (555) 123-4567", "15551234567@s.whatsapp.net", "99887766:12@lid", " +44 20 7946 0958 "}}.compile()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"15551234567", "15551234567", "99887766", "442079460958"}
	if !reflect.DeepEqual(c.allow, want) {
		t.Errorf("allow = %v, want %v", c.allow, want)
	}
}

func TestParseAccessOptions(t *testing.T) {
	// Parse through the real channels config path so the YAML shapes users will
	// write (including bare integers for phone numbers) are what is tested.
	cfg, err := channels.Parse([]byte(`
channels:
  whatsapp:
    type: whatsapp
    options:
      phone_number_id: "123"
      access:
        mode: bot
        allow_from: [15551234567, "+1 555 765 4321"]
        groups: true
        group_allow_from: ["120363041234@g.us"]
        require_mention: true
        mention_patterns: ["^wm\\b"]
        deny_reply: "private bot"
        deny_reply_interval: 30m
        reply_prefix: "[wm] "
`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseAccessOptions(cfg.Channels["whatsapp"].Options)
	if err != nil {
		t.Fatal(err)
	}
	want := AccessConfig{
		Mode: ModeBot, AllowFrom: []string{"15551234567", "+1 555 765 4321"},
		Groups: true, GroupAllowFrom: []string{"120363041234@g.us"}, RequireMention: true,
		MentionPatterns: []string{`^wm\b`}, DenyReply: "private bot", DenyReplyInterval: 30 * time.Minute, ReplyPrefix: "[wm] ",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
	if _, err := NewAccessPolicy(got, WithLogger(discard())); err != nil {
		t.Errorf("parsed config should build: %v", err)
	}
}

func TestParseAccessOptionsFailClosed(t *testing.T) {
	// Missing block: zero config, which denies everyone.
	for _, opts := range []map[string]any{nil, {}, {"phone_number_id": "1"}, {"access": nil}} {
		cfg, err := ParseAccessOptions(opts)
		if err != nil || !reflect.DeepEqual(cfg, AccessConfig{}) {
			t.Errorf("opts %v: cfg=%+v err=%v", opts, cfg, err)
		}
	}
	// A typo'd key is an error, not a silently empty allowlist.
	_, err := ParseAccessOptions(map[string]any{"access": map[string]any{"allowfrom": []string{"1"}}})
	if err == nil || !strings.Contains(err.Error(), "allowfrom") {
		t.Errorf("typo should be rejected, got %v", err)
	}
	// Wrong types are errors too.
	if _, err := ParseAccessOptions(map[string]any{"access": map[string]any{"allow_all": "maybe"}}); err == nil {
		t.Error("string for bool should be rejected")
	}
	if _, err := ParseAccessOptions(map[string]any{"access": "open"}); err == nil {
		t.Error("scalar access block should be rejected")
	}
}
