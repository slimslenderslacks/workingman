package signal

import (
	"encoding/json"
	"testing"
)

func TestParseEnvelope(t *testing.T) {
	const ownUUID = "11111111-2222-3333-4444-555555555555"
	tests := []struct {
		name string
		env  string
		own  string

		wantNil    bool
		wantChat   string
		wantSender string
		wantText   string
		wantMsgID  string
		wantReply  string
		wantNote   bool
		wantGroup  string
		wantLearn  string
	}{
		{
			name:     "direct message with number",
			env:      `{"sourceNumber":"+15557654321","sourceUuid":"AAAAAAAA-0000-0000-0000-000000000001","sourceName":"Pat","timestamp":1700000000001,"dataMessage":{"timestamp":1700000000001,"message":"hello"}}`,
			wantChat: "+15557654321", wantSender: "+15557654321", wantText: "hello", wantMsgID: "1700000000001",
		},
		{
			name:     "uuid-only sender",
			env:      `{"sourceUuid":"aaaaaaaa-0000-0000-0000-000000000001","timestamp":5,"dataMessage":{"message":"hi"}}`,
			wantChat: "aaaaaaaa-0000-0000-0000-000000000001", wantSender: "aaaaaaaa-0000-0000-0000-000000000001", wantText: "hi", wantMsgID: "5",
		},
		{
			name:     "legacy source field holding a number",
			env:      `{"source":"+15557654321","timestamp":6,"dataMessage":{"message":"hi"}}`,
			wantChat: "+15557654321", wantSender: "+15557654321", wantText: "hi", wantMsgID: "6",
		},
		{
			name:     "legacy source field holding a uuid",
			env:      `{"source":"AAAAAAAA-0000-0000-0000-000000000001","timestamp":6,"dataMessage":{"message":"hi"}}`,
			wantChat: "aaaaaaaa-0000-0000-0000-000000000001", wantSender: "aaaaaaaa-0000-0000-0000-000000000001", wantText: "hi", wantMsgID: "6",
		},
		{
			name:     "quote becomes reply id",
			env:      `{"sourceNumber":"+15557654321","timestamp":7,"dataMessage":{"message":"yes","quote":{"id":1699999999000,"author":"+15550001111","text":"ok?"}}}`,
			wantChat: "+15557654321", wantSender: "+15557654321", wantText: "yes", wantMsgID: "7", wantReply: "1699999999000",
		},
		{
			name:     "group message",
			env:      `{"sourceNumber":"+15557654321","timestamp":8,"dataMessage":{"message":"team","groupInfo":{"groupId":"R3JvdXA="}}}`,
			wantChat: "group:R3JvdXA=", wantSender: "+15557654321", wantText: "team", wantMsgID: "8", wantGroup: "R3JvdXA=",
		},
		{
			name:     "edited message uses the edit's data message",
			env:      `{"sourceNumber":"+15557654321","timestamp":9,"editMessage":{"targetSentTimestamp":8,"dataMessage":{"message":"fixed"}}}`,
			wantChat: "+15557654321", wantSender: "+15557654321", wantText: "fixed", wantMsgID: "9",
		},
		{
			name:     "mention placeholder rendered",
			env:      `{"sourceNumber":"+15557654321","timestamp":10,"dataMessage":{"message":"hey ￼ go","mentions":[{"name":"Bot","number":"+15550001111","start":4,"length":1}]}}`,
			wantChat: "+15557654321", wantSender: "+15557654321", wantText: "hey @Bot go", wantMsgID: "10",
		},
		{
			name:     "note to self (sync to own number)",
			env:      `{"sourceNumber":"+15550001111","sourceUuid":"` + ownUUID + `","timestamp":11,"syncMessage":{"sentMessage":{"destinationNumber":"+15550001111","timestamp":11,"message":"remind me"}}}`,
			wantChat: testAccount, wantSender: testAccount, wantText: "remind me", wantMsgID: "11", wantNote: true, wantLearn: ownUUID,
		},
		{
			name: "note to self addressed by own uuid",
			env:  `{"sourceNumber":"+15550001111","timestamp":12,"syncMessage":{"sentMessage":{"destinationUuid":"` + ownUUID + `","message":"by uuid"}}}`,
			own:  ownUUID, wantChat: testAccount, wantSender: testAccount, wantText: "by uuid", wantMsgID: "12", wantNote: true,
		},
		{
			name:    "sync message to someone else",
			env:     `{"sourceNumber":"+15550001111","timestamp":13,"syncMessage":{"sentMessage":{"destinationNumber":"+15557654321","message":"to pat"}}}`,
			wantNil: true,
		},
		{
			name:    "sync group message is not input",
			env:     `{"sourceNumber":"+15550001111","timestamp":13,"syncMessage":{"sentMessage":{"destinationNumber":"+15550001111","message":"x","groupInfo":{"groupId":"g"}}}}`,
			wantNil: true,
		},
		{
			name:    "sync read receipts",
			env:     `{"sourceNumber":"+15550001111","timestamp":14,"syncMessage":{"readMessages":[{"sender":"+15557654321","timestamp":1}]}}`,
			wantNil: true,
		},
		{
			name:    "own direct message is ignored",
			env:     `{"sourceNumber":"+15550001111","timestamp":15,"dataMessage":{"message":"loop"}}`,
			wantNil: true,
		},
		{name: "receipt", env: `{"sourceNumber":"+15557654321","timestamp":16,"receiptMessage":{"type":"DELIVERY","timestamps":[1]}}`, wantNil: true},
		{name: "typing", env: `{"sourceNumber":"+15557654321","timestamp":17,"typingMessage":{"action":"STARTED"}}`, wantNil: true},
		{name: "story", env: `{"sourceNumber":"+15557654321","timestamp":18,"storyMessage":{"allowsReplies":true}}`, wantNil: true},
		{name: "empty text (reaction/attachment only)", env: `{"sourceNumber":"+15557654321","timestamp":19,"dataMessage":{"message":"  ","reaction":{"emoji":"x"}}}`, wantNil: true},
		{name: "no sender", env: `{"timestamp":20,"dataMessage":{"message":"anon"}}`, wantNil: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var env envelope
			if err := json.Unmarshal([]byte(tt.env), &env); err != nil {
				t.Fatalf("bad fixture: %v", err)
			}
			p, learned := parseEnvelope(&env, testAccount, tt.own)
			if learned != tt.wantLearn {
				t.Errorf("learned uuid = %q, want %q", learned, tt.wantLearn)
			}
			if tt.wantNil {
				if p != nil {
					t.Fatalf("parsed %+v, want ignored", p.in)
				}
				return
			}
			if p == nil {
				t.Fatal("ignored, want a message")
			}
			got := p.in
			if got.ChatID != tt.wantChat || got.SenderID != tt.wantSender || got.Text != tt.wantText ||
				got.MessageID != tt.wantMsgID || got.ReplyToID != tt.wantReply ||
				got.NoteToSelf != tt.wantNote || got.GroupID != tt.wantGroup {
				t.Errorf("got chat=%q sender=%q text=%q id=%q reply=%q note=%v group=%q\nwant chat=%q sender=%q text=%q id=%q reply=%q note=%v group=%q",
					got.ChatID, got.SenderID, got.Text, got.MessageID, got.ReplyToID, got.NoteToSelf, got.GroupID,
					tt.wantChat, tt.wantSender, tt.wantText, tt.wantMsgID, tt.wantReply, tt.wantNote, tt.wantGroup)
			}
			if p.timestamp != tt.wantMsgID {
				t.Errorf("dedupe timestamp = %q, want %q", p.timestamp, tt.wantMsgID)
			}
		})
	}
}

func TestUnwrapEnvelope(t *testing.T) {
	tests := []struct {
		name, data string
		want       bool
		wantErr    bool
	}{
		{"daemon form", `{"envelope":{"sourceNumber":"+1"},"account":"+15550001111"}`, true, false},
		{"json-rpc notification", `{"jsonrpc":"2.0","method":"receive","params":{"envelope":{"sourceNumber":"+1"}}}`, true, false},
		{"no envelope", `{"account":"+15550001111"}`, false, false},
		{"garbage", `not json`, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, err := unwrapEnvelope([]byte(tt.data))
			if (err != nil) != tt.wantErr || (env != nil) != tt.want {
				t.Errorf("env=%v err=%v, want env=%v err=%v", env != nil, err, tt.want, tt.wantErr)
			}
		})
	}
}
