package signal

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/channels"
)

func TestPolicyMatrix(t *testing.T) {
	const (
		pat      = "+15557654321"
		patUUID  = "aaaaaaaa-0000-0000-0000-000000000001"
		stranger = "+15559998888"
		group    = "R3JvdXA="
	)
	dm := func(number, uuid string) Inbound {
		id := number
		if id == "" {
			id = uuid
		}
		return Inbound{InboundMessage: channels.InboundMessage{ChatID: id, SenderID: id, Text: "secret body"}, SenderNumber: number, SenderUUID: uuid}
	}
	grp := func(number, uuid, gid string) Inbound {
		in := dm(number, uuid)
		in.ChatID, in.GroupID = GroupPrefix+gid, gid
		return in
	}
	note := Inbound{InboundMessage: channels.InboundMessage{ChatID: testAccount, SenderID: testAccount, Text: "x"}, SenderNumber: testAccount, NoteToSelf: true}

	tests := []struct {
		name   string
		access AccessConfig
		in     Inbound
		want   Decision
	}{
		{"zero policy denies everyone", AccessConfig{}, dm(pat, ""), Decision{Reason: DenyAllowlistEmpty}},
		{"empty allowlist denies uuid sender", AccessConfig{}, dm("", patUUID), Decision{Reason: DenyAllowlistEmpty}},
		{"allowlisted number", AccessConfig{AllowFrom: []string{pat}}, dm(pat, ""), Decision{Allowed: true, Reason: ReasonAllowlisted}},
		{"allowlisted by uuid", AccessConfig{AllowFrom: []string{patUUID}}, dm("", patUUID), Decision{Allowed: true, Reason: ReasonAllowlisted}},
		{"allowlisted number, sender arrives with both", AccessConfig{AllowFrom: []string{pat}}, dm(pat, patUUID), Decision{Allowed: true, Reason: ReasonAllowlisted}},
		{"allowlisted number, sender arrives uuid-only is not guessed", AccessConfig{AllowFrom: []string{pat}}, dm("", patUUID), Decision{Reason: DenyNotAllowlisted}},
		{"stranger denied", AccessConfig{AllowFrom: []string{pat}}, dm(stranger, ""), Decision{Reason: DenyNotAllowlisted}},
		{"allow_all admits stranger", AccessConfig{AllowAll: true}, dm(stranger, ""), Decision{Allowed: true, Reason: ReasonAllowAll}},
		{"no sender", AccessConfig{AllowAll: true}, Inbound{InboundMessage: channels.InboundMessage{ChatID: "c"}}, Decision{Reason: DenyNoSender}},
		{"no chat", AccessConfig{AllowAll: true}, Inbound{SenderNumber: pat}, Decision{Reason: DenyNoChat}},

		{"note to self off by default", AccessConfig{AllowFrom: []string{pat}}, note, Decision{Reason: DenyNoteToSelfDisabled}},
		{"note to self opt-in without allowlist", AccessConfig{NoteToSelf: true}, note, Decision{Allowed: true, Reason: ReasonNoteToSelf, FromOwner: true}},
		{"note to self opt-in does not open dms", AccessConfig{NoteToSelf: true}, dm(stranger, ""), Decision{Reason: DenyAllowlistEmpty}},

		{"groups disabled", AccessConfig{AllowFrom: []string{pat}}, grp(pat, "", group), Decision{Reason: DenyGroupDisabled}},
		{"group not listed", AccessConfig{AllowFrom: []string{pat}, Groups: true, GroupAllowFrom: []string{"other"}}, grp(pat, "", group), Decision{Reason: DenyGroupNotAllowed}},
		{"group listed, sender allowlisted", AccessConfig{AllowFrom: []string{pat}, Groups: true, GroupAllowFrom: []string{group}}, grp(pat, "", group), Decision{Allowed: true, Reason: ReasonGroupAllowed}},
		{"group listed with prefix", AccessConfig{AllowFrom: []string{pat}, Groups: true, GroupAllowFrom: []string{GroupPrefix + group}}, grp(pat, "", group), Decision{Allowed: true, Reason: ReasonGroupAllowed}},
		{"group listed, sender not allowlisted", AccessConfig{AllowFrom: []string{pat}, Groups: true, GroupAllowFrom: []string{group}}, grp(stranger, "", group), Decision{Reason: DenyGroupSenderNotAllowed}},
		{"group listed, empty allowlist", AccessConfig{Groups: true, GroupAllowFrom: []string{group}}, grp(pat, "", group), Decision{Reason: DenyGroupSenderNotAllowed}},
		{"allow_all still needs the group listed", AccessConfig{AllowAll: true, Groups: true, GroupAllowFrom: []string{"other"}}, grp(stranger, "", group), Decision{Reason: DenyGroupNotAllowed}},
		{"allow_all with listed group", AccessConfig{AllowAll: true, Groups: true, GroupAllowFrom: []string{group}}, grp(stranger, "", group), Decision{Allowed: true, Reason: ReasonAllowAll}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			p := NewAccessPolicy(Config{Account: testAccount, Access: tt.access}, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			logs.Reset() // constructor warnings are tested separately
			got := p.Decide(tt.in)
			if got != tt.want {
				t.Errorf("Decide = %v (%+v), want %v", got, got, tt.want)
			}
			if strings.Contains(logs.String(), "secret body") {
				t.Errorf("message text leaked into logs: %s", logs.String())
			}
			if strings.Contains(logs.String(), pat) || strings.Contains(logs.String(), stranger) {
				t.Errorf("full number leaked into logs: %s", logs.String())
			}
		})
	}
}

func TestPolicyWarnings(t *testing.T) {
	tests := []struct {
		name   string
		access AccessConfig
		want   string
	}{
		{"empty allowlist", AccessConfig{}, "allow_from is empty"},
		{"allow_all", AccessConfig{AllowAll: true}, "allow_all is ENABLED"},
		{"allowlist set is quiet", AccessConfig{AllowFrom: []string{"+15557654321"}}, ""},
		{"note to self only is quiet", AccessConfig{NoteToSelf: true}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			NewAccessPolicy(Config{Access: tt.access}, slog.New(slog.NewTextHandler(&logs, nil)))
			if tt.want == "" {
				if logs.Len() != 0 {
					t.Errorf("unexpected log: %s", logs.String())
				}
				return
			}
			if !strings.Contains(logs.String(), "WARN") || !strings.Contains(logs.String(), tt.want) {
				t.Errorf("log = %q, want a WARN containing %q", logs.String(), tt.want)
			}
		})
	}
}

func TestRedactID(t *testing.T) {
	for in, want := range map[string]string{
		"":                             "",
		"+15557654321":                 "***4321",
		"group:R3JvdXA=":               "group:***dXA=",
		"aaaaaaaa-0000-0000-0000-0001": "***0001",
		"+1":                           "***+1",
	} {
		if got := RedactID(in); got != want {
			t.Errorf("RedactID(%q) = %q, want %q", in, got, want)
		}
	}
}
