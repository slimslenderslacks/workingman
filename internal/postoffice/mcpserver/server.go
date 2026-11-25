package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/slimslenderslacks/work/internal/postoffice"
)

// Defaults (§2.5, §4.6).
const (
	// DefaultMaxWait caps one blocking tool call, under Claude Code's 60 s
	// HTTP first-byte timer and the gateway's own timeouts.
	DefaultMaxWait = 50 * time.Second
	// DefaultHeartbeat is the progress-notification interval while a call
	// blocks.
	DefaultHeartbeat = 15 * time.Second
	// DefaultExpiryTick is how often overdue messages are expired.
	DefaultExpiryTick = 30 * time.Second
	// DefaultPruneTick is how often done/ is pruned.
	DefaultPruneTick = 6 * time.Hour
	// defaultWait is the schema default for wait_seconds / timeout_seconds.
	defaultWait = 45
	// closedWindow and closedLimit bound list_requests' closed rows.
	closedWindow = 7 * 24 * time.Hour
	closedLimit  = 50
)

// codeRejected is the JSON-RPC error code for a caller post-office refuses
// (§4.3). It is in the implementation-defined server-error range.
const codeRejected = -32001

// ResourceScheme prefixes the message resource URIs (§4.7).
const ResourceScheme = "post-office://messages/"

// Config configures a Server.
type Config struct {
	// Store is the message store; required.
	Store *postoffice.Store
	// Resolver identifies callers by their gateway id; required.
	Resolver *Resolver
	// Recipients maps each registered agent recipient name to its sandbox
	// (§2.4). Nil means DefaultRecipients.
	Recipients map[string]string
	// MaxWait caps one blocking call; zero means DefaultMaxWait. It is also
	// the maximum of the wait_seconds and timeout_seconds schemas.
	MaxWait time.Duration
	// Heartbeat is the progress interval while blocked; zero means
	// DefaultHeartbeat.
	Heartbeat time.Duration
	// ExpiryTick and PruneTick drive Run; zero means the defaults.
	ExpiryTick time.Duration
	PruneTick  time.Duration
	// OutboxPoll is the outbox watcher's poll fallback; zero means
	// postoffice.DefaultOutboxPoll.
	OutboxPoll time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Logf receives one line per rejection, send and background event; nil
	// discards.
	Logf func(format string, args ...any)
	// Version is reported in the MCP implementation info.
	Version string
}

// DefaultRecipients is the v1 recipient registry: the Step-Up agent (§2.4).
func DefaultRecipients() map[string]string { return map[string]string{"step-up": "step-up"} }

// Server is the post-office MCP server (§4): the sender tools and the
// message resource over a postoffice.Store, for callers identified by the
// sub-gateway that carried the call.
type Server struct {
	cfg     Config
	mcp     *mcp.Server
	waiters postoffice.Waiters
	// sendRequestPlain is send_request as callers that are not registered
	// recipients see it: without in_reply_to (§4.5). The registered tool is
	// the recipients' version.
	sendRequestPlain *mcp.Tool
	// finalized is called after each finalisation this server performs
	// (tests use it to observe the background loop).
	finalized func(postoffice.DoneRecord)
}

// New returns a Server. Call Run for the background work (finalisation,
// expiry, pruning) and serve Handler.
func New(cfg Config) (*Server, error) {
	if cfg.Store == nil || cfg.Resolver == nil {
		return nil, errors.New("post-office: Store and Resolver are required")
	}
	if cfg.Recipients == nil {
		cfg.Recipients = DefaultRecipients()
	}
	if cfg.MaxWait <= 0 {
		cfg.MaxWait = DefaultMaxWait
	}
	cfg.MaxWait = max(cfg.MaxWait, time.Second)
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = DefaultHeartbeat
	}
	if cfg.ExpiryTick <= 0 {
		cfg.ExpiryTick = DefaultExpiryTick
	}
	if cfg.PruneTick <= 0 {
		cfg.PruneTick = DefaultPruneTick
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	s := &Server{cfg: cfg}
	s.mcp = mcp.NewServer(&mcp.Implementation{Name: ServerName, Version: cfg.Version}, &mcp.ServerOptions{
		Instructions: "Send notifications and requests to the human operator or to another agent. " +
			"Requests are durable: if send_request returns status: pending, loop on wait_for_response with the request_id. " +
			"Registered recipients (such as step-up) also receive messages with next_message and answer them with reply.",
	})
	s.mcp.AddReceivingMiddleware(s.authorize)
	s.addTools()
	s.mcp.AddResourceTemplate(&mcp.ResourceTemplate{
		Name:        "message",
		URITemplate: ResourceScheme + "{id}",
		Description: "A message you sent, as its inbox record, or its done record once it is closed. Debugging aid.",
		MIMEType:    "application/json",
	}, s.readMessage)
	return s, nil
}

// MCP returns the underlying MCP server (for in-process transports).
func (s *Server) MCP() *mcp.Server { return s.mcp }

// Handler serves MCP over streamable HTTP in stateless mode: no session
// state lives in the server, so any request can arrive on any connection.
// Responses stream as SSE so progress heartbeats reach the client. Mount it
// at /mcp behind RequireBearer.
func (s *Server) Handler() http.Handler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.mcp },
		&mcp.StreamableHTTPOptions{Stateless: true, PropagateRequestCancellation: true})
}

func (s *Server) now() time.Time { return s.cfg.Now().UTC() }

func (s *Server) logf(format string, args ...any) {
	if s.cfg.Logf != nil {
		s.cfg.Logf(format, args...)
	}
}

// --- caller identity and the capability check (§4.2, §4.3) ---------------

type callerKey struct{}

func callerFrom(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(Caller)
	return c, ok
}

// authorize resolves the caller of every tools/call and resources/read and
// refuses those post-office is not enabled for, with a JSON-RPC error.
//
// tools/list is refused only when the caller's gateway record exists and was
// not minted with post-office (the mcp-add case). A sub-gateway attaches its
// backends, and so lists their tools, while it is being created, before
// workingman has saved its record or started a session; refusing then would
// strip post-office from every sandbox. Listing reveals nothing, and every
// call is checked in full.
func (s *Server) authorize(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		var header http.Header
		if ex := req.GetExtra(); ex != nil {
			header = ex.Header
		}
		id := header.Get(GatewayIDHeader)
		switch method {
		case "tools/call", "resources/read":
			c, err := s.cfg.Resolver.Resolve(id)
			if err != nil {
				s.logf("post-office: reject %s from gateway %q: %v", method, id, err)
				return nil, &jsonrpc.Error{Code: codeRejected, Message: err.Error()}
			}
			ctx = context.WithValue(ctx, callerKey{}, c)
		case "tools/list":
			c, err := s.cfg.Resolver.Resolve(id)
			if errors.Is(err, ErrNotEnabled) {
				s.logf("post-office: reject %s from gateway %q: %v", method, id, err)
				return nil, &jsonrpc.Error{Code: codeRejected, Message: err.Error()}
			}
			res, lerr := next(ctx, method, req)
			if lr, ok := res.(*mcp.ListToolsResult); ok && lerr == nil && err == nil {
				if _, recipient := s.recipientOf(c); !recipient {
					s.senderTools(lr)
				}
			}
			return res, lerr
		}
		return next(ctx, method, req)
	}
}

// receiverTools are listed only for registered recipients (§4.5).
var receiverTools = map[string]bool{"next_message": true, "reply": true, "list_inbox": true}

// senderTools rewrites a tools/list result for an identified caller that is
// not a registered recipient: the receiver tools are dropped and
// send_request loses in_reply_to.
//
// A caller post-office cannot identify yet keeps the full list. That is
// every caller while standalone-codemode attaches post-office during
// POST /gateways — before workingman saves the gateway record — and the
// gateway keeps the tool list it gets then. Filtering there would hide
// next_message from Step-Up for good; listing reveals nothing, and every
// call is checked (receiver), so unidentified callers see everything. See
// docs/messaging-design.md §4.5.
func (s *Server) senderTools(lr *mcp.ListToolsResult) {
	tools := make([]*mcp.Tool, 0, len(lr.Tools))
	for _, t := range lr.Tools {
		switch {
		case receiverTools[t.Name]:
		case t.Name == "send_request":
			tools = append(tools, s.sendRequestPlain)
		default:
			tools = append(tools, t)
		}
	}
	lr.Tools = tools
}

// --- tools (§4.4) ------------------------------------------------------------

func (s *Server) addTools() {
	maxWait := int(s.cfg.MaxWait / time.Second)
	def := min(defaultWait, maxWait)
	add := func(name, desc, schema string, h toolFunc) {
		var sch jsonschema.Schema
		if err := json.Unmarshal([]byte(schema), &sch); err != nil {
			panic(fmt.Sprintf("post-office: %s schema: %v", name, err))
		}
		resolved, err := sch.Resolve(nil)
		if err != nil {
			panic(fmt.Sprintf("post-office: %s schema: %v", name, err))
		}
		s.mcp.AddTool(&mcp.Tool{Name: name, Description: desc, InputSchema: json.RawMessage(schema)}, h.handler(resolved))
	}
	add("send_notification", sendNotificationDesc, sendNotificationSchema, typed(s.sendNotification))
	sendDesc := fmt.Sprintf(sendRequestDesc, maxWait, maxWait)
	s.sendRequestPlain = &mcp.Tool{Name: "send_request", Description: sendDesc,
		InputSchema: json.RawMessage(fmt.Sprintf(sendRequestSchema, maxWait, def, ""))}
	add("send_request", sendDesc+sendRequestRecipientDesc,
		fmt.Sprintf(sendRequestSchema, maxWait, def, inReplyToProperty), typed(s.sendRequest))
	add("wait_for_response", fmt.Sprintf(waitDesc, maxWait),
		fmt.Sprintf(waitSchema, maxWait, def), typed(s.waitForResponse))
	add("check_response", checkDesc, checkSchema, typed(s.checkResponse))
	add("cancel_request", cancelDesc, cancelSchema, typed(s.cancelRequest))
	add("list_requests", listDesc, listSchema, typed(s.listRequests))
	// Receiver tools (§4.5), listed only for registered recipients.
	add("next_message", fmt.Sprintf(nextMessageDesc, maxWait),
		fmt.Sprintf(nextMessageSchema, maxWait, def), typed(s.nextMessage))
	add("reply", replyDesc, replySchema, typed(s.reply))
	add("list_inbox", listInboxDesc, listInboxSchema, typed(s.listInbox))
}

// toolFunc is a tool handler on raw, schema-checked arguments.
type toolFunc func(ctx context.Context, req *mcp.CallToolRequest, c Caller, args json.RawMessage) *result

// typed decodes the arguments into In for h. Absent optional fields stay
// zero (or nil for pointers), and each handler applies the schema default.
func typed[In any](h func(context.Context, *mcp.CallToolRequest, Caller, In) *result) toolFunc {
	return func(ctx context.Context, req *mcp.CallToolRequest, c Caller, args json.RawMessage) *result {
		var in In
		if len(args) > 0 {
			if err := json.Unmarshal(args, &in); err != nil {
				return rejected("invalid arguments: " + err.Error())
			}
		}
		return h(ctx, req, c, in)
	}
}

// handler adapts f to an mcp.ToolHandler that validates the arguments
// against the tool's input schema first. Server.AddTool, unlike the typed
// mcp.AddTool, leaves that to the handler; the hand-written schemas are the
// contract (§4.4), so they are checked as written. Invalid arguments are a
// status: rejected result, which the model can read and correct.
func (f toolFunc) handler(schema *jsonschema.Resolved) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		c, ok := callerFrom(ctx)
		if !ok {
			// authorize always sets it for tools/call.
			return nil, &jsonrpc.Error{Code: codeRejected, Message: ErrNoGatewayID.Error()}
		}
		args := req.Params.Arguments
		var v any = map[string]any{}
		if len(args) > 0 && string(args) != "null" {
			if err := json.Unmarshal(args, &v); err != nil {
				return rejected("invalid arguments: " + err.Error()).toolResult(), nil
			}
		}
		if err := schema.Validate(v); err != nil {
			return rejected("invalid arguments: " + err.Error()).toolResult(), nil
		}
		return f(ctx, req, c, args).toolResult(), nil
	}
}

type sendNotificationIn struct {
	Subject   string `json:"subject"`
	Body      string `json:"body"`
	Recipient string `json:"recipient"`
}

func (s *Server) sendNotification(_ context.Context, _ *mcp.CallToolRequest, c Caller, in sendNotificationIn) *result {
	if strings.TrimSpace(in.Subject) == "" || strings.TrimSpace(in.Body) == "" {
		return rejected("subject and body are required")
	}
	m, r := s.put(c, postoffice.Draft{
		Kind: postoffice.KindNotification, Recipient: in.Recipient, Subject: in.Subject, Body: in.Body,
	})
	if r != nil {
		return r
	}
	return newResult("sent").set("message_id", m.ID)
}

type sendRequestIn struct {
	Body             string   `json:"body"`
	Subject          string   `json:"subject"`
	Recipient        string   `json:"recipient"`
	Options          []string `json:"options"`
	WaitSeconds      *int     `json:"wait_seconds"`
	ExpiresInMinutes *int     `json:"expires_in_minutes"`
	InReplyTo        string   `json:"in_reply_to"`
}

func (s *Server) sendRequest(ctx context.Context, req *mcp.CallToolRequest, c Caller, in sendRequestIn) *result {
	if strings.TrimSpace(in.Body) == "" {
		return rejected("body is required")
	}
	expires := postoffice.DefaultRequestExpiry
	if in.ExpiresInMinutes != nil {
		n := *in.ExpiresInMinutes
		if n < 1 || n > int(postoffice.MaxExpiry/time.Minute) {
			return rejected(fmt.Sprintf("expires_in_minutes must be between 1 and %d", int(postoffice.MaxExpiry/time.Minute)))
		}
		expires = time.Duration(n) * time.Minute
	}
	wait, r := s.waitArg(in.WaitSeconds, "wait_seconds", 0)
	if r != nil {
		return r
	}
	inReplyTo, note, r := s.inReplyTo(c, in.InReplyTo)
	if r != nil {
		return r
	}
	m, r := s.put(c, postoffice.Draft{
		Kind: postoffice.KindRequest, Recipient: in.Recipient, Subject: in.Subject, Body: in.Body,
		Options: in.Options, ExpiresIn: expires, InReplyTo: inReplyTo,
	})
	if r != nil {
		return r
	}
	return s.await(ctx, req, m.ID, wait).set("in_reply_to", inReplyTo).set("note", note)
}

// put validates the recipient, builds the record from d with the caller as
// sender, and writes it to the inbox. A non-nil result is the rejection.
func (s *Server) put(c Caller, d postoffice.Draft) (postoffice.Message, *result) {
	rcpt := postoffice.NormalizeRecipient(d.Recipient)
	if _, ok := s.cfg.Recipients[rcpt]; rcpt != postoffice.RecipientOperator && !ok {
		return postoffice.Message{}, rejected(fmt.Sprintf("unknown recipient %q; known: %s", strings.TrimSpace(d.Recipient), s.knownRecipients()))
	}
	if self, ok := s.recipientOf(c); ok && self == rcpt {
		return postoffice.Message{}, rejected(fmt.Sprintf("you are %q; you cannot send to yourself", rcpt))
	}
	d.From = c.Sender
	m, err := postoffice.NewMessage(d, s.now())
	if err != nil {
		return postoffice.Message{}, rejected(strings.TrimPrefix(err.Error(), "postoffice: "))
	}
	if err := s.cfg.Store.Put(m); err != nil {
		s.logf("post-office: put %s: %v", m.ID, err)
		return postoffice.Message{}, rejected("could not store the message: " + err.Error())
	}
	s.logf("post-office: %s %s from %s (%s) to %s", m.Kind, m.ID, m.From.Display(), m.From.Sandbox, m.Recipient)
	if !m.ToOperator() {
		s.waiters.Wake(inboxKey(m.Recipient))
	}
	return m, nil
}

func (s *Server) knownRecipients() string {
	names := []string{postoffice.RecipientOperator}
	var agents []string
	for n := range s.cfg.Recipients {
		agents = append(agents, n)
	}
	sort.Strings(agents)
	return strings.Join(append(names, agents...), ", ")
}

// waitArg converts a wait argument in seconds (nil means the schema default)
// to a duration capped at MaxWait, rejecting values below lo.
func (s *Server) waitArg(v *int, name string, lo int) (time.Duration, *result) {
	maxWait := int(s.cfg.MaxWait / time.Second)
	if v == nil {
		return time.Duration(min(defaultWait, maxWait)) * time.Second, nil
	}
	if *v < lo || *v > maxWait {
		return 0, rejected(fmt.Sprintf("%s must be between %d and %d", name, lo, maxWait))
	}
	return time.Duration(*v) * time.Second, nil
}

type requestIDIn struct {
	RequestID      string `json:"request_id"`
	TimeoutSeconds *int   `json:"timeout_seconds"`
	Reason         string `json:"reason"`
}

func (s *Server) waitForResponse(ctx context.Context, req *mcp.CallToolRequest, c Caller, in requestIDIn) *result {
	if _, r := s.ownRequest(c, in.RequestID); r != nil {
		return r
	}
	wait, r := s.waitArg(in.TimeoutSeconds, "timeout_seconds", 1)
	if r != nil {
		return r
	}
	return s.await(ctx, req, in.RequestID, wait)
}

func (s *Server) checkResponse(_ context.Context, _ *mcp.CallToolRequest, c Caller, in requestIDIn) *result {
	if _, r := s.ownRequest(c, in.RequestID); r != nil {
		return r
	}
	r, _ := s.state(in.RequestID)
	return r
}

func (s *Server) cancelRequest(_ context.Context, _ *mcp.CallToolRequest, c Caller, in requestIDIn) *result {
	if _, r := s.ownRequest(c, in.RequestID); r != nil {
		return r
	}
	if len(in.Reason) > postoffice.MaxReason {
		return rejected(fmt.Sprintf("reason is longer than %d bytes", postoffice.MaxReason))
	}
	_, err := s.cfg.Store.Claim(postoffice.Resolution{
		ID: in.RequestID, Outcome: postoffice.OutcomeCancelled, Reason: strings.TrimSpace(in.Reason),
	})
	switch {
	case err == nil:
		s.logf("post-office: request %s cancelled by %s", in.RequestID, c.Sender.Sandbox)
	case errors.Is(err, postoffice.ErrAlreadyClosed):
		// Another outcome won; report it below.
	default:
		s.logf("post-office: cancel %s: %v", in.RequestID, err)
		return rejected("could not cancel: " + err.Error())
	}
	s.finalize(in.RequestID)
	r, _ := s.state(in.RequestID)
	return r
}

type listIn struct {
	Status string `json:"status"`
}

func (s *Server) listRequests(_ context.Context, _ *mcp.CallToolRequest, c Caller, in listIn) *result {
	which := in.Status
	if which == "" {
		which = "open"
	}
	if which != "open" && which != "closed" && which != "all" {
		return rejected(`status must be "open", "closed" or "all"`)
	}
	type row struct {
		id, state, recipient string
		created              time.Time
		subject              string
	}
	var rows []row
	now := s.now()
	if which != "closed" {
		open, err := s.cfg.Store.ListOpen()
		if err != nil {
			return rejected("could not list requests: " + err.Error())
		}
		for _, m := range open {
			if m.Kind != postoffice.KindRequest || !m.From.Same(c.Sender) {
				continue
			}
			state := "pending"
			if res, ok, _ := s.cfg.Store.Claimed(m.ID); ok {
				state = string(res.Outcome)
			} else if m.Expired(now) {
				state = string(postoffice.OutcomeExpired)
			}
			rows = append(rows, row{m.ID, state, m.Recipient, m.CreatedAt, m.Subject})
		}
	}
	if which != "open" {
		done, err := s.cfg.Store.ListDone(now.Add(-closedWindow))
		if err != nil {
			return rejected("could not list requests: " + err.Error())
		}
		var closed []row
		for _, d := range done {
			if d.Kind != postoffice.KindRequest || !d.From.Same(c.Sender) {
				continue
			}
			closed = append(closed, row{d.ID, string(d.Resolution.Outcome), d.Recipient, d.CreatedAt, d.Subject})
		}
		if len(closed) > closedLimit {
			closed = closed[len(closed)-closedLimit:]
		}
		rows = append(rows, closed...)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].id < rows[j].id })
	r := newResult("ok")
	var structured []map[string]string
	for _, x := range rows {
		r.addLine(strings.Join([]string{x.id, x.state, x.recipient, stamp(x.created), oneLine(x.subject)}, " "))
		structured = append(structured, map[string]string{
			"request_id": x.id, "state": x.state, "recipient": x.recipient,
			"created_at": stamp(x.created), "subject": x.subject,
		})
	}
	if len(rows) == 0 {
		r.addLine("(no requests)")
	}
	r.values["requests"] = structured
	return r
}

// ownRequest checks that id is a request sent by the caller. A non-nil
// result is the rejection.
func (s *Server) ownRequest(c Caller, id string) (postoffice.Message, *result) {
	id = strings.TrimSpace(id)
	if !postoffice.ValidID(id) {
		return postoffice.Message{}, rejected(fmt.Sprintf("invalid request_id %q", id))
	}
	m, _, err := s.cfg.Store.Lookup(id)
	if errors.Is(err, postoffice.ErrNotFound) {
		return postoffice.Message{}, rejected("unknown request_id " + id + "; call list_requests to see yours")
	}
	if err != nil {
		return postoffice.Message{}, rejected("could not read the request: " + err.Error())
	}
	if !m.From.Same(c.Sender) {
		s.logf("post-office: %s asked about %s, sent by %s", c.Sender.Sandbox, id, m.From.Sandbox)
		return postoffice.Message{}, rejected("not your request")
	}
	if m.Kind != postoffice.KindRequest {
		return postoffice.Message{}, rejected(id + " is a notification, not a request")
	}
	return m, nil
}

// state renders a request's current state. closed reports whether it is
// terminal. An open request whose expiry has passed is expired here and now
// rather than waiting for the ticker.
func (s *Server) state(id string) (r *result, closed bool) {
	m, res, err := s.cfg.Store.Lookup(id)
	if err != nil {
		return rejected("could not read the request: " + err.Error()), true
	}
	now := s.now()
	if res == nil && m.Expired(now) {
		if _, err := s.cfg.Store.Claim(postoffice.Resolution{ID: id, Outcome: postoffice.OutcomeExpired}); err == nil || errors.Is(err, postoffice.ErrAlreadyClosed) {
			s.finalize(id)
			return s.state(id)
		}
	}
	if res == nil {
		r = newResult("pending").
			set("request_id", id).
			set("elapsed", since(m.CreatedAt, now)).
			set("expires_at", stamp(m.ExpiresAt))
		if rec, ok, _ := s.cfg.Store.Deliveries(id); ok {
			r.set("delivered_to", append([]string{"tui"}, rec.DeliveredTo()...))
		} else if m.ToOperator() {
			r.set("delivered_to", []string{"tui"})
		}
		if m.ReceivedAt != nil {
			r.set("received_at", stamp(*m.ReceivedAt))
		}
		return r, false
	}
	r = newResult(string(res.Outcome)).
		set("request_id", id).
		set("elapsed", since(m.CreatedAt, now)).
		set("expires_at", stamp(m.ExpiresAt))
	switch res.Outcome {
	case postoffice.OutcomeAnswered:
		if res.Responder != nil {
			r.set("responder", res.Responder.Kind).set("responder_name", res.Responder.Name).set("via", res.Responder.Via)
		}
		r.set("option", res.Option).setBlock("response", res.Response)
	case postoffice.OutcomeCancelled:
		r.set("reason", res.Reason)
	}
	return r, true
}

// await blocks for up to wait for request id to close and returns its state
// (§4.6). It wakes on finalisation, sends a progress heartbeat every
// Heartbeat when the call carries a progress token, and returns pending when
// the wait or the client's context ends; neither ever cancels the request.
func (s *Server) await(ctx context.Context, req *mcp.CallToolRequest, id string, wait time.Duration) *result {
	return s.block(ctx, req, id, wait, func(bool) (*result, bool) { return s.state(id) },
		func() string { return s.heartbeat(id) })
}

// block calls check until it reports done, for up to wait, and returns its
// last result. It re-checks whenever key is woken in s.waiters and on every
// heartbeat, and sends heartbeat() as a progress notification each Heartbeat
// when the call carries a progress token. When the wait or the client's
// context ends it returns check(true)'s result, whatever it is.
func (s *Server) block(ctx context.Context, req *mcp.CallToolRequest, key string, wait time.Duration,
	check func(last bool) (*result, bool), heartbeat func() string) *result {
	start := time.Now()
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	beat := time.NewTicker(s.cfg.Heartbeat)
	defer beat.Stop()
	var token any
	if req != nil && req.Params != nil {
		token = req.Params.GetProgressToken()
	}
	for {
		// Take the wake channel before checking, so a wake between the
		// check and the select is not missed.
		woke := s.waiters.Wait(key)
		r, done := check(wait <= 0)
		if done || wait <= 0 {
			return r
		}
		select {
		case <-woke:
		case <-deadline.C:
			r, _ := check(true)
			return r
		case <-ctx.Done():
			r, _ := check(true)
			return r
		case <-beat.C:
			if token != nil && req.Session != nil {
				_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
					ProgressToken: token,
					Progress:      time.Since(start).Seconds(),
					Message:       heartbeat(),
				})
			}
		}
	}
}

// heartbeat is the progress message for a blocked call on id, such as
// "waiting for operator (12m since request)".
func (s *Server) heartbeat(id string) string {
	m, _, err := s.cfg.Store.Lookup(id)
	if err != nil {
		return "waiting"
	}
	return fmt.Sprintf("waiting for %s (%s since request)", m.Recipient, postoffice.HumanDuration(s.now().Sub(m.CreatedAt)))
}

// --- resource (§4.7) ------------------------------------------------------------

func (s *Server) readMessage(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	uri := req.Params.URI
	c, ok := callerFrom(ctx)
	if !ok {
		return nil, &jsonrpc.Error{Code: codeRejected, Message: ErrNoGatewayID.Error()}
	}
	id, _ := strings.CutPrefix(uri, ResourceScheme)
	if !postoffice.ValidID(id) {
		return nil, mcp.ResourceNotFoundError(uri)
	}
	m, res, err := s.cfg.Store.Lookup(id)
	if errors.Is(err, postoffice.ErrNotFound) {
		return nil, mcp.ResourceNotFoundError(uri)
	}
	if err != nil {
		return nil, err
	}
	// Scoped like the tools: the sender, or the agent recipient it was sent
	// to. Anyone else sees "not found", so ids cannot be probed.
	if !m.From.Same(c.Sender) && !s.isRecipient(m, c) {
		return nil, mcp.ResourceNotFoundError(uri)
	}
	var v any = m
	if res != nil {
		v = postoffice.DoneRecord{Message: m, Resolution: *res}
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
		URI: uri, MIMEType: "application/json", Text: string(data),
	}}}, nil
}

func (s *Server) isRecipient(m postoffice.Message, c Caller) bool {
	sb, ok := s.cfg.Recipients[m.Recipient]
	return ok && sb == c.Sender.Sandbox
}

// recipientOf returns the registered recipient name whose sandbox is the
// caller's, if any (§2.4).
func (s *Server) recipientOf(c Caller) (string, bool) {
	if c.Sender.Sandbox == "" {
		return "", false
	}
	var names []string
	for name, sb := range s.cfg.Recipients {
		if sb == c.Sender.Sandbox {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return "", false
	}
	sort.Strings(names)
	return names[0], true
}

// --- background work (§2.5, §3.3, §3.4) --------------------------------------

// Run finalises claims, expires overdue messages and prunes done/ until ctx
// is done. On start it recovers leftover claims, expires and prunes, so a
// crash or downtime converges. The caller holds post-office.lock.
func (s *Server) Run(ctx context.Context) {
	s.recover()
	s.expire()
	s.prune()
	changes := s.cfg.Store.Watch(ctx, s.cfg.OutboxPoll, postoffice.DirOutbox)
	expiry := time.NewTicker(s.cfg.ExpiryTick)
	defer expiry.Stop()
	prune := time.NewTicker(s.cfg.PruneTick)
	defer prune.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-changes:
			if !ok {
				return
			}
			s.recover()
		case <-expiry.C:
			s.expire()
		case <-prune.C:
			s.prune()
		}
	}
}

// recover finalises every pending claim and wakes its waiters.
func (s *Server) recover() {
	done, err := s.cfg.Store.Recover()
	if err != nil {
		s.logf("post-office: finalise: %v", err)
	}
	for _, d := range done {
		s.finished(d)
	}
}

func (s *Server) expire() {
	ids, err := s.cfg.Store.Expire()
	if err != nil {
		s.logf("post-office: expire: %v", err)
	}
	for _, id := range ids {
		s.logf("post-office: %s expired", id)
	}
	if len(ids) > 0 {
		s.recover()
	}
}

func (s *Server) prune() {
	n, err := s.cfg.Store.Prune(postoffice.DefaultRetention)
	if err != nil {
		s.logf("post-office: prune: %v", err)
	}
	if n > 0 {
		s.logf("post-office: pruned %d done records", n)
	}
}

// finalize finalises id now (after this server's own claim) and wakes its
// waiters, without waiting for the outbox watcher.
func (s *Server) finalize(id string) {
	d, err := s.cfg.Store.Finalize(id)
	if err != nil {
		if !errors.Is(err, postoffice.ErrNotFound) {
			s.logf("post-office: finalise %s: %v", id, err)
		}
		return
	}
	s.finished(d)
}

func (s *Server) finished(d postoffice.DoneRecord) {
	if !d.Orphan {
		s.logf("post-office: %s %s (%s)", d.ID, d.Resolution.Outcome, d.Resolution.Via())
	}
	s.waiters.Wake(d.ID)
	if s.finalized != nil {
		s.finalized(d)
	}
}

// --- formatting ------------------------------------------------------------------

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func since(t, now time.Time) string {
	d := max(now.Sub(t), 0).Round(time.Second)
	return d.String()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func jsonString(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(data)
}
