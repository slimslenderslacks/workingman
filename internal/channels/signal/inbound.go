package signal

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/slimslenderslacks/work/internal/channels"
)

// Wire types for signal-cli's receive envelopes. Only the fields the channel
// reads are declared; json.Number tolerates both numeric and string timestamps.

type envelope struct {
	Source       string       `json:"source"`
	SourceNumber string       `json:"sourceNumber"`
	SourceUUID   string       `json:"sourceUuid"`
	SourceName   string       `json:"sourceName"`
	Timestamp    json.Number  `json:"timestamp"`
	DataMessage  *dataMessage `json:"dataMessage"`
	EditMessage  *struct {
		DataMessage *dataMessage `json:"dataMessage"`
	} `json:"editMessage"`
	SyncMessage *struct {
		SentMessage *sentMessage `json:"sentMessage"`
	} `json:"syncMessage"`
	StoryMessage json.RawMessage `json:"storyMessage"`
}

type dataMessage struct {
	Timestamp json.Number `json:"timestamp"`
	Message   string      `json:"message"`
	Quote     *quote      `json:"quote"`
	GroupInfo *struct {
		GroupID string `json:"groupId"`
	} `json:"groupInfo"`
	Mentions []mention `json:"mentions"`
}

type sentMessage struct {
	dataMessage
	Destination       string `json:"destination"`
	DestinationNumber string `json:"destinationNumber"`
	DestinationUUID   string `json:"destinationUuid"`
}

// UnmarshalJSON decodes the embedded dataMessage and the destination fields
// together (encoding/json does not promote an unexported embedded struct).
func (s *sentMessage) UnmarshalJSON(b []byte) error {
	if err := json.Unmarshal(b, &s.dataMessage); err != nil {
		return err
	}
	var d struct {
		Destination       string `json:"destination"`
		DestinationNumber string `json:"destinationNumber"`
		DestinationUUID   string `json:"destinationUuid"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return err
	}
	s.Destination, s.DestinationNumber, s.DestinationUUID = d.Destination, d.DestinationNumber, d.DestinationUUID
	return nil
}

type quote struct {
	ID json.Number `json:"id"`
}

type mention struct {
	Name   string `json:"name"`
	Number string `json:"number"`
	UUID   string `json:"uuid"`
	Start  int    `json:"start"`
	Length int    `json:"length"`
}

// unwrapEnvelope extracts the envelope from an SSE payload, which is either
// {"envelope": {...}} (daemon HTTP) or a JSON-RPC {"method":"receive",
// "params":{"envelope":{...}}} notification.
func unwrapEnvelope(data []byte) (*envelope, error) {
	var top struct {
		Envelope *envelope `json:"envelope"`
		Params   *struct {
			Envelope *envelope `json:"envelope"`
		} `json:"params"`
	}
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, err
	}
	switch {
	case top.Envelope != nil:
		return top.Envelope, nil
	case top.Params != nil && top.Params.Envelope != nil:
		return top.Params.Envelope, nil
	}
	return nil, nil
}

// parsed is a candidate inbound message extracted from an envelope.
type parsed struct {
	in         Inbound
	noteToSelf bool
	timestamp  string // message timestamp as sent by Signal; also the dedupe/echo key
}

// parseEnvelope turns an envelope into an inbound message, or nil for
// everything the channel ignores: receipts, typing indicators, stories, our own
// outbound echoes to other people, and contentless data messages. ownUUID is
// the account's own service id when already known; learnedUUID reports one
// learned from this envelope (sync messages come from ourselves).
func parseEnvelope(env *envelope, account, ownUUID string) (p *parsed, learnedUUID string) {
	number := strings.TrimSpace(env.SourceNumber)
	uuid := strings.ToLower(strings.TrimSpace(env.SourceUUID))
	if number == "" && uuid == "" {
		number = strings.TrimSpace(env.Source) // legacy field: number or uuid
		if _, isNum := NormalizeNumber(number); !isNum && number != "" {
			uuid, number = strings.ToLower(number), ""
		}
	}
	if len(env.StoryMessage) > 0 {
		return nil, ""
	}

	var dm *dataMessage
	noteToSelf := false
	switch {
	case env.SyncMessage != nil:
		// Our own account's other devices. Only "Note to Self" is input.
		sm := env.SyncMessage.SentMessage
		if sm == nil {
			return nil, ""
		}
		if number == account && uuid != "" {
			learnedUUID = uuid
		}
		dest := sm.DestinationNumber
		if dest == "" {
			dest = sm.Destination
		}
		toSelf := dest == account ||
			(sm.DestinationUUID != "" && (strings.EqualFold(sm.DestinationUUID, ownUUID) || strings.EqualFold(sm.DestinationUUID, learnedUUID)))
		if !toSelf || (sm.GroupInfo != nil && sm.GroupInfo.GroupID != "") {
			return nil, learnedUUID
		}
		dm, noteToSelf = &sm.dataMessage, true
	case env.DataMessage != nil:
		dm = env.DataMessage
	case env.EditMessage != nil && env.EditMessage.DataMessage != nil:
		dm = env.EditMessage.DataMessage
	default:
		return nil, ""
	}
	if !noteToSelf && number != "" && number == account {
		return nil, learnedUUID // never answer ourselves
	}
	text := dm.Message
	if len(dm.Mentions) > 0 {
		text = renderMentions(text, dm.Mentions)
	}
	if strings.TrimSpace(text) == "" {
		return nil, learnedUUID
	}

	ts := env.Timestamp.String()
	if ts == "" {
		ts = dm.Timestamp.String()
	}
	sender := number
	if sender == "" {
		sender = uuid
	}
	chat, groupID := sender, ""
	if dm.GroupInfo != nil && dm.GroupInfo.GroupID != "" {
		groupID = dm.GroupInfo.GroupID
		chat = GroupPrefix + groupID
	}
	if noteToSelf {
		sender, chat = account, account
	}
	if sender == "" {
		return nil, learnedUUID // nothing to attribute it to; the policy never sees it
	}

	msg := parsed{noteToSelf: noteToSelf, timestamp: ts}
	msg.in = Inbound{
		InboundMessage: msgFields(chat, sender, env.SourceName, ts, text, dm),
		SenderNumber:   number,
		SenderUUID:     uuid,
		GroupID:        groupID,
		NoteToSelf:     noteToSelf,
	}
	if noteToSelf {
		msg.in.SenderNumber = account
	}
	return &msg, learnedUUID
}

// renderMentions replaces the U+FFFC placeholders Signal leaves in the text
// with "@name" (falling back to number or uuid), in order.
func renderMentions(text string, mentions []mention) string {
	units := utf16.Encode([]rune(text))
	var b strings.Builder
	pos := 0
	ordered := append([]mention(nil), mentions...)
	for i := 1; i < len(ordered); i++ { // insertion sort by start; mentions are few
		for j := i; j > 0 && ordered[j].Start < ordered[j-1].Start; j-- {
			ordered[j], ordered[j-1] = ordered[j-1], ordered[j]
		}
	}
	for _, m := range ordered {
		if m.Start < pos || m.Start >= len(units) || units[m.Start] != 0xFFFC {
			continue
		}
		who := m.Name
		if who == "" {
			who = m.Number
		}
		if who == "" {
			who = m.UUID
		}
		b.WriteString(string(utf16.Decode(units[pos:m.Start])))
		b.WriteString("@" + who)
		pos = m.Start + 1
	}
	b.WriteString(string(utf16.Decode(units[pos:])))
	return b.String()
}

// timestampOf converts a Signal millisecond timestamp, or now if unparsable.
func timestampOf(ts string, now func() time.Time) time.Time {
	if ms, err := strconv.ParseInt(ts, 10, 64); err == nil && ms > 0 {
		return time.UnixMilli(ms).UTC()
	}
	return now()
}

func msgFields(chat, sender, senderName, ts, text string, dm *dataMessage) channels.InboundMessage {
	m := channels.InboundMessage{
		ChatID:     chat,
		SenderID:   sender,
		SenderName: senderName,
		MessageID:  ts,
		Text:       text,
		Timestamp:  timestampOf(ts, time.Now),
	}
	if dm.Quote != nil {
		m.ReplyToID = dm.Quote.ID.String()
	}
	return m
}
