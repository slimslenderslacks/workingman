package slack

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/slimslenderslacks/work/internal/mcpgateway"
)

// The Slack MCP's read tools answer in a "detailed" text format, not JSON
// (docs/research/slack-mcp.md §3). It is the remote server's presentation
// layer, not a contract, so the parser is lenient about whitespace and
// reports a non-empty response it could find no "Message TS:" in as
// Malformed, for the caller to log loudly and fall back on.
//
// read_channel:
//
//	Channel: DM (D0CBT6QV8NT)
//
//	=== Message from Jim Clark <jim.clark@okta.com> (U0CC1K1MFNX) at 2026-11-19 18:23:18 PST ===
//	Message TS: 1795141398.745179
//	…text…
//	*Sent using* <@U0B1REGGAMC|Slack MCP>
//	Thread: 1 replies (latest: 2026-11-19 18:23:26 PST)
//	Reactions: white_check_mark (1)
//
// read_thread:
//
//	=== THREAD PARENT MESSAGE ===
//	From: Jim Clark <jim.clark@okta.com> (U0CC1K1MFNX)
//	Time: 2026-11-19 18:23:18 PST
//	Message TS: 1795141398.745179
//	…text…
//
//	=== THREAD REPLIES (1 total) ===
//
//	--- Reply 1 of 1 ---
//	From: …
//	Time: …
//	Message TS: 1795141406.120249
//	…text…
//
// or "No thread messsages" (sic) in place of the replies.

// Message is one message parsed from a read result.
type Message struct {
	TS       string `json:"ts"`
	UserID   string `json:"user_id,omitempty"`
	UserName string `json:"user_name,omitempty"`
	Text     string `json:"text"`
	// Replies is the "Thread: N replies" count (read_channel only).
	Replies int `json:"replies,omitempty"`
	// Reactions is the raw "Reactions:" summary, e.g. "white_check_mark (1)".
	Reactions string `json:"reactions,omitempty"`
	// ViaMCP is set when the text ended with the Slack MCP's "*Sent using*"
	// trailer (taken off Text). Any Slack MCP client adds it, not only this
	// channel, so it is informational: the sent set decides what is ours.
	ViaMCP bool `json:"via_mcp,omitempty"`
}

// ChannelPage is one parsed read_channel result.
type ChannelPage struct {
	// ChannelID is the resolved id from the "Channel: … (D…)" header; empty
	// when absent.
	ChannelID string    `json:"channel_id,omitempty"`
	Messages  []Message `json:"messages"`
	// Cursor is the next page's cursor; empty on the last page.
	Cursor string `json:"cursor,omitempty"`
	// Malformed is set when the response has content but no message could be
	// parsed from it.
	Malformed bool `json:"malformed,omitempty"`
}

// ThreadPage is one parsed read_thread result.
type ThreadPage struct {
	Parent    *Message  `json:"parent,omitempty"`
	Replies   []Message `json:"replies"`
	Cursor    string    `json:"cursor,omitempty"`
	Malformed bool      `json:"malformed,omitempty"`
}

var (
	channelHeaderRE = regexp.MustCompile(`^Channel:\s.*\(([CDG][A-Z0-9]+)\)\s*$`)
	messageHeaderRE = regexp.MustCompile(`^=== Message from (.+?) at (.+?) ===\s*$`)
	threadParentRE  = regexp.MustCompile(`^=== THREAD PARENT MESSAGE ===\s*$`)
	threadRepliesRE = regexp.MustCompile(`^=== THREAD REPLIES\b.*===\s*$`)
	replyHeaderRE   = regexp.MustCompile(`^--- Reply \d+ of \d+ ---\s*$`)
	noMessagesRE    = regexp.MustCompile(`(?i)^no (thread )?mess+ages\b`)
	tsLineRE        = regexp.MustCompile(`^Message TS:\s*(\d+\.\d+)\s*$`)
	fromLineRE      = regexp.MustCompile(`^From:\s*(.+?)\s*$`)
	timeLineRE      = regexp.MustCompile(`^Time:\s`)
	threadLineRE    = regexp.MustCompile(`^Thread:\s*(\d+)\s+repl`)
	reactionsLineRE = regexp.MustCompile(`^Reactions:\s*(.*?)\s*$`)
	userIDRE        = regexp.MustCompile(`\s*\(([UWB][A-Z0-9]+)\)\s*$`)
	emailRE         = regexp.MustCompile(`\s*<[^<>]*>\s*$`)
	cursorRE        = regexp.MustCompile(`(?i)cursor\W*"([^"]+)"`)
	// sentUsingRE is the trailer the Slack MCP appends to every post.
	sentUsingRE = regexp.MustCompile(`^\*Sent using\* <@[A-Z0-9]+\|[^>]*>\s*$`)
)

// readResult splits a read tool's result into its "messages" text and its
// "pagination_info". The tools answer with a JSON object in a text part;
// anything else is taken as the messages text itself.
func readResult(res mcpgateway.ToolResult) (messages, pagination string) {
	var obj struct {
		Messages   *string `json:"messages"`
		Pagination string  `json:"pagination_info"`
	}
	if len(res.StructuredContent) > 0 && json.Unmarshal(res.StructuredContent, &obj) == nil && obj.Messages != nil {
		return *obj.Messages, obj.Pagination
	}
	text := res.Text()
	if json.Unmarshal([]byte(strings.TrimSpace(text)), &obj) == nil && obj.Messages != nil {
		return *obj.Messages, obj.Pagination
	}
	return text, ""
}

// parseCursor finds the next-page cursor in a pagination_info text
// ("Use cursor: \"…\""); empty when there are no more pages.
func parseCursor(pagination string) string {
	if m := cursorRE.FindStringSubmatch(pagination); m != nil {
		return m[1]
	}
	return ""
}

// ParseChannel parses a read_channel result's messages text.
func ParseChannel(text, pagination string) ChannelPage {
	p := ChannelPage{Cursor: parseCursor(pagination), Messages: []Message{}}
	var cur *Message
	var body []string
	content := false
	flush := func() {
		if cur != nil && cur.TS != "" {
			finishBody(cur, body, true)
			p.Messages = append(p.Messages, *cur)
		}
		cur, body = nil, nil
	}
	for _, line := range splitLines(text) {
		switch {
		case cur == nil && channelHeaderRE.MatchString(line):
			p.ChannelID = channelHeaderRE.FindStringSubmatch(line)[1]
		case messageHeaderRE.MatchString(line):
			flush()
			content = true
			m := messageHeaderRE.FindStringSubmatch(line)
			cur = &Message{}
			cur.UserID, cur.UserName = splitFrom(m[1])
		case cur != nil && cur.TS == "" && tsLineRE.MatchString(line):
			cur.TS = tsLineRE.FindStringSubmatch(line)[1]
		case cur != nil:
			body = append(body, line)
		case strings.TrimSpace(line) != "" && !noMessagesRE.MatchString(line):
			content = true
		}
	}
	flush()
	p.Malformed = content && len(p.Messages) == 0
	return p
}

// ParseThread parses a read_thread result's messages text.
func ParseThread(text, pagination string) ThreadPage {
	p := ThreadPage{Cursor: parseCursor(pagination), Replies: []Message{}}
	var cur *Message
	var body []string
	inReplies, content := false, false
	flush := func() {
		if cur != nil && cur.TS != "" {
			finishBody(cur, body, false)
			if inReplies {
				p.Replies = append(p.Replies, *cur)
			} else if p.Parent == nil {
				m := *cur
				p.Parent = &m
			}
		}
		cur, body = nil, nil
	}
	for _, line := range splitLines(text) {
		switch {
		case threadParentRE.MatchString(line):
			flush()
			content, inReplies = true, false
			cur = &Message{}
		case threadRepliesRE.MatchString(line):
			flush()
			inReplies = true
		case replyHeaderRE.MatchString(line):
			flush()
			content, inReplies = true, true
			cur = &Message{}
		case noMessagesRE.MatchString(line):
			flush()
		case cur != nil && cur.TS == "" && fromLineRE.MatchString(line):
			cur.UserID, cur.UserName = splitFrom(fromLineRE.FindStringSubmatch(line)[1])
		case cur != nil && cur.TS == "" && timeLineRE.MatchString(line):
		case cur != nil && cur.TS == "" && tsLineRE.MatchString(line):
			cur.TS = tsLineRE.FindStringSubmatch(line)[1]
		case cur != nil:
			body = append(body, line)
		case strings.TrimSpace(line) != "":
			content = true
		}
	}
	flush()
	p.Malformed = content && p.Parent == nil && len(p.Replies) == 0
	return p
}

func splitLines(text string) []string {
	return strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
}

// splitFrom splits "Jim Clark <jim@x> (U0CC1K1MFNX)" into the user id and
// the display name.
func splitFrom(s string) (id, name string) {
	s = strings.TrimSpace(s)
	if m := userIDRE.FindStringSubmatchIndex(s); m != nil {
		id = s[m[2]:m[3]]
		s = s[:m[0]]
	}
	name = strings.TrimSpace(emailRE.ReplaceAllString(s, ""))
	return id, name
}

// finishBody sets m's text from its body lines: trailing blank lines, the
// "Thread:" and "Reactions:" summary lines (read_channel puts both at the
// end; read_thread only Reactions) and the Slack MCP trailer are taken off.
func finishBody(m *Message, body []string, channel bool) {
	trim := func() {
		for len(body) > 0 && strings.TrimSpace(body[len(body)-1]) == "" {
			body = body[:len(body)-1]
		}
	}
	trim()
	for len(body) > 0 {
		last := body[len(body)-1]
		if r := reactionsLineRE.FindStringSubmatch(last); r != nil && m.Reactions == "" {
			m.Reactions = r[1]
		} else if t := threadLineRE.FindStringSubmatch(last); t != nil && channel && m.Replies == 0 {
			m.Replies, _ = strconv.Atoi(t[1])
		} else {
			break
		}
		body = body[:len(body)-1]
		trim()
	}
	if len(body) > 0 && sentUsingRE.MatchString(body[len(body)-1]) {
		m.ViaMCP = true
		body = body[:len(body)-1]
		trim()
	}
	for len(body) > 0 && strings.TrimSpace(body[0]) == "" {
		body = body[1:]
	}
	m.Text = unescape(strings.Join(body, "\n"))
}

// unescape undoes Slack's three text escapes.
func unescape(s string) string {
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(s)
}
