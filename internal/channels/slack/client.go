package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/slimslenderslacks/work/internal/mcpgateway"
)

// TestPrefix starts every message `orch slack test` posts. The channel's
// poll skips a top-level Slack MCP post that starts with it, so a running
// daemon does not take the CLI's test message for the operator's.
const TestPrefix = "[orch slack test]"

// Tool suffixes only the setup CLI uses.
const (
	toolProfile  = "slack_read_user_profile"
	toolChannels = "slack_list_user_channels"
)

// RequiredTools are the tools the channel itself calls; `orch slack status`
// checks the gateway serves every one.
var RequiredTools = []string{toolSend, toolReaction, toolChannel, toolThread}

// Client is a one-shot Slack MCP client for `orch slack setup|status|test`:
// the channel's tool resolution and header-script retry, without its worker,
// state or polling. Not safe for concurrent use (the header script fails
// under concurrent calls anyway).
type Client struct {
	c       *Channel
	gateway mcpgateway.Gateway
	closeFn func(ctx context.Context)
}

// NewClient is a Client over caller (tests, or a session the caller owns).
func NewClient(caller Caller) *Client {
	return &Client{c: New("slack", Config{}, caller, "", quietLogger())}
}

// dialAttempts is how many gateways Dial mints before it settles for one
// that serves no slack tools; dialRetry is the wait between them.
const dialAttempts = 3

var dialRetry = 2 * time.Second

// Dial mints a temporary static sub-gateway over the slack catalog server on
// the standalone-codemode root gatewayURL and opens a session on it. The
// gateway is not the daemon's (that one is recorded under GatewayKey and
// left alone); Close deletes it. A gateway minted with the slack server not
// serving is replaced (up to dialAttempts in all); the last one is returned
// regardless, for the caller to report from Gateway(). An *mcpgateway.UnknownServersError means
// the catalog has no slack server.
func Dial(ctx context.Context, gatewayURL string) (*Client, error) {
	api := mcpgateway.Client{BaseURL: gatewayURL}
	var gw mcpgateway.Gateway
	for attempt := 1; ; attempt++ {
		var err error
		if gw, err = api.Create(ctx, []string{GatewayServer}, false); err != nil {
			return nil, err
		}
		// A fresh gateway sometimes serves the slack backend "connected, 0
		// tools" (its attach failed); only a new gateway recovers.
		st, reported := gw.Servers[GatewayServer]
		if !reported || st.Serving() || attempt == dialAttempts {
			break
		}
		_ = api.Delete(context.WithoutCancel(ctx), gw.ID)
		if err := sleepCtx(ctx, dialRetry); err != nil {
			return nil, err
		}
	}
	del := func(ctx context.Context) { _ = api.Delete(context.WithoutCancel(ctx), gw.ID) }
	gw.URL = rebase(gw.URL, gatewayURL)
	sess, err := mcpgateway.OpenSession(ctx, gw.URL, mcpgateway.SessionOptions{ClientName: "orch-slack-cli"})
	if err != nil {
		del(ctx)
		return nil, fmt.Errorf("slack: open a session on gateway %s: %w", gw.URL, err)
	}
	cl := NewClient(sess)
	cl.gateway = gw
	cl.closeFn = func(ctx context.Context) {
		_ = sess.Close(context.WithoutCancel(ctx))
		del(ctx)
	}
	return cl, nil
}

// rebase moves a gateway URL on the loopback onto the host of base when base
// is not on the loopback: standalone-codemode answers with its own
// localhost URL, which a caller that reached it by another name (a sandbox
// using host.docker.internal) cannot use.
func rebase(gwURL, base string) string {
	g, err := url.Parse(gwURL)
	if err != nil {
		return gwURL
	}
	b, err := url.Parse(base)
	if err != nil || b.Host == "" || loopback(b.Hostname()) || !loopback(g.Hostname()) {
		return gwURL
	}
	g.Scheme, g.Host = b.Scheme, b.Host
	return g.String()
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Gateway is the sub-gateway Dial minted (zero for NewClient).
func (cl *Client) Gateway() mcpgateway.Gateway { return cl.gateway }

// Close ends the session and deletes the gateway Dial minted.
func (cl *Client) Close(ctx context.Context) {
	if cl.closeFn != nil {
		cl.closeFn(ctx)
		cl.closeFn = nil
	}
}

// MissingTools lists which of RequiredTools the gateway does not serve.
func (cl *Client) MissingTools(ctx context.Context) ([]string, error) {
	var missing []string
	for _, s := range RequiredTools {
		if _, err := cl.c.tool(ctx, s); err != nil {
			var none *noToolError
			if !errors.As(err, &none) {
				return nil, err
			}
			missing = append(missing, s)
		}
	}
	return missing, nil
}

// Profile is the parts of slack_read_user_profile setup uses.
type Profile struct {
	UserID   string
	Username string
	RealName string
}

// Name is the best display name of the profile.
func (p Profile) Name() string {
	if p.RealName != "" {
		return p.RealName
	}
	return p.Username
}

// Self reads the operator's own profile: the user every post is made as,
// and the id of the DM-to-self.
func (cl *Client) Self(ctx context.Context) (Profile, error) {
	res, err := cl.c.call(ctx, toolProfile, map[string]any{"response_format": "detailed"})
	if err != nil {
		return Profile{}, err
	}
	p := ParseProfile(resultText(res))
	if !userRE.MatchString(p.UserID) {
		return Profile{}, fmt.Errorf("slack: read_user_profile returned no user id: %q", sample(resultText(res)))
	}
	return p, nil
}

// Conversation is one entry of slack_list_user_channels.
type Conversation struct {
	ID   string
	Name string // "#general", "DM with Jon May"
	Type string // "Public Channel", "Private Channel", "Direct Message", …
	// UserID is the other participant of a DM.
	UserID   string
	Archived bool
}

// ListPage is one page of slack_list_user_channels.
type ListPage struct {
	Conversations []Conversation
	Cursor        string // next page; "" on the last
}

// ListConversations lists the operator's conversations of the given types
// (comma-separated public_channel, private_channel, im, mpim), unarchived
// only, whose name starts with prefix ("" for all).
func (cl *Client) ListConversations(ctx context.Context, types, prefix string, limit int, cursor string) (ListPage, error) {
	args := map[string]any{"types": types, "exclude_archived": true, "limit": limit}
	if prefix != "" {
		args["name_prefix"] = prefix
	}
	if cursor != "" {
		args["cursor"] = cursor
	}
	res, err := cl.c.call(ctx, toolChannels, args)
	if err != nil {
		return ListPage{}, err
	}
	return ParseConversations(resultText(res)), nil
}

// Post sends text to chat (a channel or user id), in thread when set, and
// returns the resolved channel id and the post's ts.
func (cl *Client) Post(ctx context.Context, chat, thread, text string) (channelID, ts string, err error) {
	args := map[string]any{"channel_id": chat, "message": text}
	if thread != "" {
		args["thread_ts"] = thread
	}
	res, err := cl.c.call(ctx, toolSend, args)
	if err != nil {
		return "", "", err
	}
	channelID, ts, err = sendResult(res)
	if channelID == "" {
		channelID = chat
	}
	return channelID, ts, err
}

// Replies returns the replies in the thread of ts newer than oldest ("" for
// all), oldest first.
func (cl *Client) Replies(ctx context.Context, channelID, ts, oldest string) ([]Message, error) {
	args := map[string]any{"channel_id": channelID, "message_ts": ts, "limit": readThreadLimit, "response_format": "detailed"}
	if oldest != "" {
		args["oldest"] = oldest
	}
	res, err := cl.c.call(ctx, toolThread, args)
	if err != nil {
		return nil, err
	}
	text, pagination := readResult(res)
	p := ParseThread(text, pagination)
	if p.Malformed {
		return nil, fmt.Errorf("slack: read_thread returned text that could not be parsed: %q", sample(text))
	}
	return p.Replies, nil
}

// React adds emoji (no colons) to the message ts in channelID.
func (cl *Client) React(ctx context.Context, channelID, ts, emoji string) error {
	_, err := cl.c.call(ctx, toolReaction, map[string]any{"channel_id": channelID, "message_ts": ts, "emoji": emoji})
	return err
}

// ValidID reports whether s is a Slack channel (C…, G…, D…) or user (U…, W…)
// id, as options.channel and a route's chat take.
func ValidID(s string) bool { return idRE.MatchString(s) }

// ValidUserID reports whether s is a Slack user id, as allow_from takes.
func ValidUserID(s string) bool { return userRE.MatchString(s) }

// TSTime is a Slack ts as a time; the zero time when it does not parse.
func TSTime(ts string) time.Time { return tsTime(ts) }

// resultText is the text of a result the Slack MCP wraps as
// {"result": "…"} (profile, channel list), or the raw text otherwise.
func resultText(res mcpgateway.ToolResult) string {
	var obj struct {
		Result *string `json:"result"`
	}
	text := res.Text()
	if json.Unmarshal([]byte(strings.TrimSpace(text)), &obj) == nil && obj.Result != nil {
		return *obj.Result
	}
	return text
}

var profileLineRE = regexp.MustCompile(`^([A-Za-z ]+):\s*(.*?)\s*$`)

// ParseProfile parses slack_read_user_profile's detailed text:
//
//	User ID: U0CC1K1MFNX
//	Username: jim.clark
//	Real Name: Jim Clark
func ParseProfile(text string) Profile {
	var p Profile
	for _, line := range splitLines(text) {
		m := profileLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		switch m[1] {
		case "User ID":
			p.UserID = m[2]
		case "Username":
			p.Username = m[2]
		case "Real Name":
			p.RealName = m[2]
		}
	}
	return p
}

var (
	convHeaderRE = regexp.MustCompile(`^###\s+(.+?)\s*$`)
	convFieldRE  = regexp.MustCompile(`^-\s+\*\*(.+?):\*\*\s*(.*?)\s*$`)
)

// ParseConversations parses slack_list_user_channels' text:
//
//	### #general
//	- **ID:** C6WPNMSTC
//	- **Type:** Public Channel
//	- **Archived:** No
//
//	### DM with Jon May
//	- **ID:** D0CJVBFKL23
//	- **Type:** Direct Message
//	- **User ID:** U01F5258FDF
//
//	---
//	Pagination: More results available. Use cursor: "…"
func ParseConversations(text string) ListPage {
	var p ListPage
	var cur *Conversation
	flush := func() {
		if cur != nil && cur.ID != "" {
			p.Conversations = append(p.Conversations, *cur)
		}
		cur = nil
	}
	for _, line := range splitLines(text) {
		if m := convHeaderRE.FindStringSubmatch(line); m != nil {
			flush()
			cur = &Conversation{Name: unescape(m[1])}
			continue
		}
		if strings.HasPrefix(line, "Pagination:") {
			p.Cursor = parseCursor(line)
			continue
		}
		m := convFieldRE.FindStringSubmatch(line)
		if m == nil || cur == nil {
			continue
		}
		switch m[1] {
		case "ID":
			cur.ID = m[2]
		case "Type":
			cur.Type = m[2]
		case "User ID":
			cur.UserID = m[2]
		case "Archived":
			cur.Archived = strings.EqualFold(m[2], "yes")
		}
	}
	flush()
	return p
}
