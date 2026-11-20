package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/slimslenderslacks/work/internal/mcpgateway"
)

// fakeSlack is an in-memory Slack MCP behind a Caller: it keeps messages per
// conversation, answers the four tools the channel uses in the server's
// detailed text format, and can inject header-script failures.
type fakeSlack struct {
	mu sync.Mutex
	// prefix is the backend prefix tools are served under.
	prefix string
	// resolve maps an id given as channel_id to the conversation id (a user
	// id to its DM).
	resolve map[string]string
	msgs    map[string][]*fakeMsg // conversation → messages, oldest first
	clock   int64                 // ts seconds of the next message
	// headerFailures makes the next N calls fail as the header script does.
	headerFailures int
	// garble makes read_channel answer in an unknown format.
	garble bool
	// loseResponses makes the next N sends post but fail as a dropped stream
	// does; hideLost also keeps those posts out of the reads made right after
	// (so the channel cannot reconcile them) until reveal.
	loseResponses int
	hideLost      bool
	calls         []fakeCall
	listArgs      []map[string]any
	// inflight detects concurrent calls.
	inflight, maxInflight int
}

type fakeMsg struct {
	ts, thread, user, name, text string
	viaMCP, hidden               bool
	reactions                    []string
}

type fakeCall struct {
	Tool string
	Args map[string]any
}

func newFakeSlack() *fakeSlack {
	return &fakeSlack{
		prefix:  "slack_xaa-",
		resolve: map[string]string{"U0CC1K1MFNX": "D0CBT6QV8NT"},
		msgs:    map[string][]*fakeMsg{},
		clock:   1795200000,
	}
}

// now is the fake's clock: just after the last message posted.
func (f *fakeSlack) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return time.Unix(f.clock, 500_000_000)
}

func (f *fakeSlack) conv(id string) string {
	if c, ok := f.resolve[id]; ok {
		return c
	}
	return id
}

func (f *fakeSlack) nextTS() string {
	f.clock++
	return fmt.Sprintf("%d.%06d", f.clock, 100)
}

// human posts as the operator without the MCP trailer (the phone), returning
// its ts. thread is the parent ts, or "" for top level.
func (f *fakeSlack) human(conv, thread, text string) string {
	return f.humanAs(conv, thread, "U0CC1K1MFNX", "Jim Clark", text)
}

func (f *fakeSlack) humanAs(conv, thread, user, name, text string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	conv = f.conv(conv)
	m := &fakeMsg{ts: f.nextTS(), thread: thread, user: user, name: name, text: text}
	f.msgs[conv] = append(f.msgs[conv], m)
	return m.ts
}

// reveal makes hidden posts visible to reads.
func (f *fakeSlack) reveal() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ms := range f.msgs {
		for _, m := range ms {
			m.hidden = false
		}
	}
}

func (f *fakeSlack) callsTo(suffix string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeCall
	for _, c := range f.calls {
		if strings.HasSuffix(c.Tool, suffix) {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeSlack) find(conv, ts string) *fakeMsg {
	for _, m := range f.msgs[conv] {
		if m.ts == ts {
			return m
		}
	}
	return nil
}

func (f *fakeSlack) ListTools(context.Context) ([]mcpgateway.Tool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []mcpgateway.Tool
	for _, s := range []string{toolSend, toolReaction, toolChannel, toolThread, toolProfile, toolChannels, "slack_search_users"} {
		out = append(out, mcpgateway.Tool{Name: f.prefix + s})
	}
	return out, nil
}

func (f *fakeSlack) CallTool(_ context.Context, name string, args map[string]any) (mcpgateway.ToolResult, error) {
	f.mu.Lock()
	f.inflight++
	f.maxInflight = max(f.maxInflight, f.inflight)
	f.mu.Unlock()
	time.Sleep(time.Millisecond) // widen any race window
	f.mu.Lock()
	defer f.mu.Unlock()
	defer func() { f.inflight-- }()
	f.calls = append(f.calls, fakeCall{Tool: name, Args: args})
	if f.headerFailures > 0 {
		f.headerFailures--
		return mcpgateway.ToolResult{}, &mcpgateway.RPCError{Code: -32005, Message: `calling "tools/call": rejected by transport: mcpruntime: backend "slack": header script failed: /opt/okta-cpe/Scripts/fetch-mcp-headers-xaa.sh exited with status 1`}
	}
	if !strings.HasPrefix(name, f.prefix) {
		return mcpgateway.ToolResult{}, &mcpgateway.RPCError{Code: -32602, Message: "unknown tool " + name}
	}
	str := func(k string) string { s, _ := args[k].(string); return s }
	conv := f.conv(str("channel_id"))
	switch strings.TrimPrefix(name, f.prefix) {
	case toolSend:
		// Slack renders emoji back as shortcodes.
		text := strings.NewReplacer("❓", ":question:", "✅", ":white_check_mark:").Replace(str("message"))
		m := &fakeMsg{ts: f.nextTS(), thread: str("thread_ts"), user: "U0CC1K1MFNX", name: "Jim Clark", text: text, viaMCP: true}
		f.msgs[conv] = append(f.msgs[conv], m)
		if f.loseResponses > 0 {
			f.loseResponses--
			m.hidden = f.hideLost
			return mcpgateway.ToolResult{}, fmt.Errorf("mcp: tools/call: event stream ended without a response to id 11: %w", io.ErrUnexpectedEOF)
		}
		out, _ := json.Marshal(map[string]any{
			"message_link":    "https://x.slack.com/archives/" + conv + "/p" + strings.ReplaceAll(m.ts, ".", ""),
			"message_context": map[string]string{"message_ts": m.ts, "channel_id": conv},
		})
		return mcpgateway.ToolResult{Content: []mcpgateway.Content{{Type: "text", Text: string(out)}}, StructuredContent: out}, nil
	case toolReaction:
		m := f.find(conv, str("message_ts"))
		if m == nil {
			return mcpgateway.ToolResult{IsError: true}, &mcpgateway.ToolError{Tool: name, Text: "message_not_found"}
		}
		m.reactions = append(m.reactions, str("emoji"))
		return textResult(`{"result":"Successfully added :` + str("emoji") + `: reaction to the message."}`), nil
	case toolChannel:
		return textResult(f.renderChannel(conv, str("oldest"))), nil
	case toolThread:
		return textResult(f.renderThread(conv, str("message_ts"), str("oldest"))), nil
	case toolProfile:
		return textResult(`{"result":"User ID: U0CC1K1MFNX\nUsername: jim.clark\nDisplay Name: jim.clark\nReal Name: Jim Clark\nBot: No\n"}`), nil
	case toolChannels:
		f.listArgs = append(f.listArgs, args)
		return textResult(`{"result":"## My Channels (showing 2, more results available)\n\n### #wm-jim\n- **ID:** G0WMJIM01\n- **Type:** Private Channel\n- **Archived:** No\n\n### DM with Jon May\n- **ID:** D0CJVBFKL23\n- **Type:** Direct Message\n- **User ID:** U01F5258FDF\n- **Archived:** No\n\n---\nPagination: More results available. Use cursor: \"aW1f\"\n"}`), nil
	}
	return mcpgateway.ToolResult{}, &mcpgateway.RPCError{Code: -32602, Message: "unknown tool " + name}
}

func textResult(s string) mcpgateway.ToolResult {
	return mcpgateway.ToolResult{Content: []mcpgateway.Content{{Type: "text", Text: s}}}
}

func wrap(messages, pagination string) string {
	out, _ := json.Marshal(map[string]string{"messages": messages, "pagination_info": pagination})
	return string(out)
}

func (m *fakeMsg) body() string {
	var b strings.Builder
	b.WriteString(m.text + "\n")
	if m.viaMCP {
		b.WriteString("*Sent using* <@U0B1REGGAMC|Slack MCP>\n")
	}
	return b.String()
}

func reactionsLine(rs []string) string {
	if len(rs) == 0 {
		return ""
	}
	counts := map[string]int{}
	for _, r := range rs {
		counts[r]++
	}
	var parts []string
	for r, n := range counts {
		parts = append(parts, fmt.Sprintf("%s (%d)", r, n))
	}
	sort.Strings(parts)
	return "Reactions: " + strings.Join(parts, ", ") + "\n"
}

func (f *fakeSlack) renderChannel(conv, oldest string) string {
	if f.garble {
		return wrap("Channel: DM ("+conv+")\n\n[1] new format\n", "There are no more messages available.\n")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Channel: DM (%s)\n\n", conv)
	all := f.msgs[conv]
	for i := len(all) - 1; i >= 0; i-- { // newest first
		m := all[i]
		if m.thread != "" || m.hidden || (oldest != "" && !tsLess(oldest, m.ts)) {
			continue
		}
		fmt.Fprintf(&b, "=== Message from %s <x@okta.com> (%s) at 2026-11-20 10:00:00 PST === \nMessage TS: %s\n", m.name, m.user, m.ts)
		b.WriteString(m.body())
		n := 0
		for _, r := range all {
			if r.thread == m.ts {
				n++
			}
		}
		if n > 0 {
			fmt.Fprintf(&b, "Thread: %d replies (latest: 2026-11-20 10:01:00 PST)\n", n)
		}
		b.WriteString(reactionsLine(m.reactions))
		b.WriteString("\n")
	}
	return wrap(b.String(), "There are no more messages available.\n")
}

func (f *fakeSlack) renderThread(conv, parent, oldest string) string {
	p := f.find(conv, parent)
	if p == nil {
		return wrap("No thread messsages\n", "")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "=== THREAD PARENT MESSAGE ===\nFrom: %s <x@okta.com> (%s)\nTime: 2026-11-20 10:00:00 PST\nMessage TS: %s\n", p.name, p.user, p.ts)
	b.WriteString(p.body())
	b.WriteString(reactionsLine(p.reactions))
	var replies []*fakeMsg
	for _, m := range f.msgs[conv] {
		if m.thread == parent && !m.hidden && (oldest == "" || tsLess(oldest, m.ts)) {
			replies = append(replies, m)
		}
	}
	if len(replies) == 0 {
		b.WriteString("\nNo thread messsages\n")
		return wrap(b.String(), "There are no more messages in this thread.\n")
	}
	fmt.Fprintf(&b, "\n=== THREAD REPLIES (%d total) ===\n", len(replies))
	for i, m := range replies {
		fmt.Fprintf(&b, "\n--- Reply %d of %d ---\nFrom: %s <x@okta.com> (%s)\nTime: 2026-11-20 10:01:00 PST\nMessage TS: %s\n", i+1, len(replies), m.name, m.user, m.ts)
		b.WriteString(m.body())
		b.WriteString(reactionsLine(m.reactions))
	}
	return wrap(b.String(), "There are no more messages in this thread.\n")
}
