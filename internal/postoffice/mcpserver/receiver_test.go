package mcpserver

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/slimslenderslacks/work/internal/mcpgateway"
	"github.com/slimslenderslacks/work/internal/postoffice"
)

// stepUp registers the Step-Up agent's gateway (fixed sandbox "step-up",
// identified without a session) and returns its id.
func (e *env) stepUp() string {
	e.t.Helper()
	if err := e.gateways.Save(mcpgateway.Record{Sandbox: "step-up", ID: "gw-step-up", Servers: []string{"post-office"}, CreatedAt: time.Now()}); err != nil {
		e.t.Fatal(err)
	}
	return "gw-step-up"
}

func toolNames(t *testing.T, c *client) (names []string, byName map[string]*mcp.Tool) {
	t.Helper()
	res, err := c.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	byName = map[string]*mcp.Tool{}
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
		byName[tl.Name] = tl
	}
	slices.Sort(names)
	return names, byName
}

func hasProperty(t *testing.T, tl *mcp.Tool, prop string) bool {
	t.Helper()
	data, _ := json.Marshal(tl.InputSchema)
	var m struct {
		Properties map[string]any `json:"properties"`
	}
	_ = json.Unmarshal(data, &m)
	_, ok := m.Properties[prop]
	return ok
}

func TestReceiverToolsListedOnlyForRecipients(t *testing.T) {
	e := newEnv(t)
	senderTools := []string{"cancel_request", "check_response", "list_requests", "send_notification", "send_request", "wait_for_response"}
	allTools := []string{"cancel_request", "check_response", "list_inbox", "list_requests", "next_message", "reply", "send_notification", "send_request", "wait_for_response"}

	names, byName := toolNames(t, e.connect(e.agent("wm-p-a", "a", "post-office")))
	if !slices.Equal(names, senderTools) {
		t.Fatalf("task tools = %v, want %v", names, senderTools)
	}
	if hasProperty(t, byName["send_request"], "in_reply_to") || strings.Contains(byName["send_request"].Description, "in_reply_to") {
		t.Fatal("a task sees in_reply_to on send_request")
	}

	names, byName = toolNames(t, e.connect(e.stepUp()))
	if !slices.Equal(names, allTools) {
		t.Fatalf("step-up tools = %v, want %v", names, allTools)
	}
	if !hasProperty(t, byName["send_request"], "in_reply_to") {
		t.Fatal("step-up's send_request has no in_reply_to")
	}
	if !strings.Contains(byName["next_message"].Description, "(max 3)") || !strings.Contains(byName["next_message"].Description, "Each message is returned once") {
		t.Fatalf("next_message description: %s", byName["next_message"].Description)
	}

	// A gateway whose record is not saved yet (the gateway attaching
	// post-office during POST /gateways) cannot be told apart, and keeps
	// the list it gets: it sees everything, and calls are checked.
	names, _ = toolNames(t, e.connect("gw-pending"))
	if !slices.Equal(names, allTools) {
		t.Fatalf("unidentified tools = %v, want %v", names, allTools)
	}
}

func TestReceiverToolsRefusedForOthers(t *testing.T) {
	e := newEnv(t)
	task := e.connect(e.agent("wm-p-a", "a", "post-office"))
	text, _ := task.call("send_request", map[string]any{"body": "q", "wait_seconds": 0, "recipient": "step-up"})
	id := field(text, "request_id")
	for tool, args := range map[string]map[string]any{
		"next_message": {"timeout_seconds": 0},
		"reply":        {"request_id": id, "response": "granted"},
		"list_inbox":   nil,
	} {
		text, _ := task.call(tool, args)
		wantStatus(t, text, "rejected")
		if !strings.Contains(field(text, "reason"), "only for registered recipients (step-up)") {
			t.Fatalf("%s: %s", tool, text)
		}
	}
	text, _ = task.call("send_request", map[string]any{"body": "q", "wait_seconds": 0, "in_reply_to": id})
	wantStatus(t, text, "rejected")
	if field(text, "reason") != "in_reply_to is only for registered recipients" {
		t.Fatalf("in_reply_to from a task: %s", text)
	}
	if m, _ := e.store.Get(id); m.ReceivedAt != nil {
		t.Fatal("a task's next_message received step-up's message")
	}
}

func TestNextMessageDeliversOnce(t *testing.T) {
	e := newEnv(t)
	task := e.connect(e.agent("wm-proj-fix-ci", "fix-ci", "post-office"))
	su := e.connect(e.stepUp())

	text, _ := su.call("next_message", map[string]any{"timeout_seconds": 0})
	wantStatus(t, text, "empty")

	// Operator-addressed and other senders' traffic is not step-up's.
	task.call("send_request", map[string]any{"body": "for the operator", "wait_seconds": 0})
	text, _ = task.call("send_request", map[string]any{
		"subject": "Need network access to proxy.golang.org", "body": "go mod download fails\nplease allow it",
		"recipient": "step-up", "wait_seconds": 0, "options": []string{"ok"},
	})
	first := field(text, "request_id")
	text, _ = task.call("send_request", map[string]any{"body": "second ask", "recipient": "Step-Up", "wait_seconds": 0})
	second := field(text, "request_id")

	text, sc := su.call("next_message", map[string]any{"timeout_seconds": 0})
	wantStatus(t, text, "message")
	if field(text, "kind") != "request" || field(text, "request_id") != first ||
		field(text, "from") != "task fix-ci (proj)" || field(text, "from_sandbox") != "wm-proj-fix-ci" ||
		field(text, "subject") != "Need network access to proxy.golang.org" || field(text, "options") != "ok" {
		t.Fatalf("first message:\n%s", text)
	}
	if !strings.HasSuffix(text, "\nbody:\ngo mod download fails\nplease allow it") || sc["body"] != "go mod download fails\nplease allow it" {
		t.Fatalf("body must be last:\n%s", text)
	}
	m, _ := e.store.Get(first)
	if m.ReceivedAt == nil {
		t.Fatal("received_at not set")
	}
	// The sender sees that it was received.
	text, _ = task.call("check_response", map[string]any{"request_id": first})
	wantStatus(t, text, "pending")
	if field(text, "received_at") == "" {
		t.Fatalf("check_response after receipt:\n%s", text)
	}

	text, _ = su.call("next_message", map[string]any{"timeout_seconds": 0})
	if field(text, "request_id") != second {
		t.Fatalf("second message:\n%s", text)
	}
	text, _ = su.call("next_message", map[string]any{"timeout_seconds": 0})
	wantStatus(t, text, "empty")

	// Both stay in list_inbox until answered.
	text, sc = su.call("list_inbox", nil)
	wantStatus(t, text, "ok")
	if !strings.Contains(text, "\n"+first+" request received wm-proj-fix-ci ") || !strings.Contains(text, "\n"+second+" request received ") {
		t.Fatalf("list_inbox:\n%s", text)
	}
	if rows, _ := sc["messages"].([]any); len(rows) != 2 {
		t.Fatalf("structured messages = %v", sc["messages"])
	}
}

func TestNextMessageConcurrentCallsDeliverOnce(t *testing.T) {
	e := newEnv(t)
	task := e.connect(e.agent("wm-p-a", "a", "post-office"))
	gw := e.stepUp()
	const messages, callers = 12, 6
	sent := map[string]bool{}
	for range messages {
		text, _ := task.call("send_notification", map[string]any{"subject": "s", "body": "b", "recipient": "step-up"})
		sent[field(text, "message_id")] = true
	}
	var mu sync.Mutex
	got := map[string]int{}
	var wg sync.WaitGroup
	for range callers {
		c := e.connect(gw)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				text, _, err := c.tryCall(context.Background(), "next_message", map[string]any{"timeout_seconds": 0}, nil)
				if err != nil {
					t.Error(err)
					return
				}
				if field(text, "status") != "message" {
					return
				}
				mu.Lock()
				got[field(text, "message_id")]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(got) != messages {
		t.Fatalf("delivered %d distinct messages, want %d", len(got), messages)
	}
	for id, n := range got {
		if n != 1 || !sent[id] {
			t.Fatalf("%s delivered %d times (sent: %v)", id, n, sent[id])
		}
	}
}

func TestNextMessageWaitsAndWakes(t *testing.T) {
	e := newEnv(t)
	task := e.connect(e.agent("wm-p-a", "a", "post-office"))
	su := e.connect(e.stepUp())

	start := time.Now()
	text, _ := su.call("next_message", map[string]any{"timeout_seconds": 1})
	wantStatus(t, text, "empty")
	if d := time.Since(start); d < 900*time.Millisecond || d > 3*time.Second {
		t.Fatalf("waited %s, want about 1s", d)
	}

	time.AfterFunc(200*time.Millisecond, func() {
		task.call("send_request", map[string]any{"body": "hello", "recipient": "step-up", "wait_seconds": 0})
	})
	start = time.Now()
	text, _, err := su.tryCall(context.Background(), "next_message", map[string]any{"timeout_seconds": 3}, "tok")
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, text, "message")
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("woke after %s", d)
	}
	su.mu.Lock()
	beats := slices.Clone(su.progress)
	su.mu.Unlock()
	if len(beats) == 0 || beats[0] != "waiting for messages to step-up" {
		t.Fatalf("heartbeats = %q", beats)
	}
}

// A message written by another process (or before a restart) is found by
// polling, without an in-process wake.
func TestNextMessageFindsMessagesFromTheStore(t *testing.T) {
	e := newEnv(t)
	su := e.connect(e.stepUp())
	m, err := postoffice.NewMessage(postoffice.Draft{Kind: postoffice.KindRequest, From: postoffice.Sender{Sandbox: "wm-x"}, Recipient: "step-up", Body: "direct"}, e.now())
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(150*time.Millisecond, func() { _ = e.store.Put(m) })
	text, _ := su.call("next_message", map[string]any{"timeout_seconds": 2})
	if field(text, "request_id") != m.ID {
		t.Fatalf("next_message:\n%s", text)
	}
}

func TestNotificationToRecipientClosedOnReceipt(t *testing.T) {
	e := newEnv(t)
	task := e.connect(e.agent("wm-p-a", "a", "post-office"))
	su := e.connect(e.stepUp())
	text, _ := task.call("send_notification", map[string]any{"subject": "fyi", "body": "done", "recipient": "step-up"})
	id := field(text, "message_id")
	text, _ = su.call("next_message", map[string]any{"timeout_seconds": 0})
	if field(text, "kind") != "notification" || field(text, "message_id") != id {
		t.Fatalf("next_message:\n%s", text)
	}
	d, ok, _ := e.store.Done(id)
	if !ok || d.Resolution.Outcome != postoffice.OutcomeDismissed || d.ReceivedAt == nil || d.Resolution.Via() != "step-up" {
		t.Fatalf("done = %+v %v", d, ok)
	}
	text, _ = su.call("list_inbox", nil)
	if strings.Contains(text, id) {
		t.Fatalf("list_inbox still shows the notification:\n%s", text)
	}
	text, _ = su.call("reply", map[string]any{"request_id": id, "response": "x"})
	wantStatus(t, text, "rejected")
}

func TestReplyAnswersTheSender(t *testing.T) {
	e := newEnv(t)
	task := e.connect(e.agent("wm-p-a", "a", "post-office"))
	other := e.connect(e.agent("wm-p-b", "b", "post-office"))
	su := e.connect(e.stepUp())
	text, _ := task.call("send_request", map[string]any{"body": "need proxy.golang.org", "recipient": "step-up", "wait_seconds": 0, "options": []string{"granted", "refused"}})
	id := field(text, "request_id")
	su.call("next_message", map[string]any{"timeout_seconds": 0})

	time.AfterFunc(200*time.Millisecond, func() {
		text, _ := su.call("reply", map[string]any{"request_id": id, "response": "Granted"})
		if field(text, "status") != "answered" || field(text, "option") != "granted" {
			t.Errorf("reply:\n%s", text)
		}
	})
	text, _ = task.call("wait_for_response", map[string]any{"request_id": id, "timeout_seconds": 3})
	wantStatus(t, text, "answered")
	if field(text, "responder") != "agent" || field(text, "via") != "step-up" || field(text, "option") != "granted" || !strings.HasSuffix(text, "response:\nGranted") {
		t.Fatalf("sender sees:\n%s", text)
	}
	// A second reply finds it closed; it leaves list_inbox.
	text, _ = su.call("reply", map[string]any{"request_id": id, "response": "again"})
	wantStatus(t, text, "closed")
	if field(text, "outcome") != "answered" || field(text, "via") != "step-up" {
		t.Fatalf("second reply:\n%s", text)
	}
	if text, _ := su.call("list_inbox", nil); strings.Contains(text, id) {
		t.Fatalf("list_inbox after reply:\n%s", text)
	}

	// Only messages addressed to step-up may be answered.
	text, _ = other.call("send_request", map[string]any{"body": "for the operator", "wait_seconds": 0})
	text, _ = su.call("reply", map[string]any{"request_id": field(text, "request_id"), "response": "x"})
	wantStatus(t, text, "rejected")
	if field(text, "reason") != "not addressed to you" {
		t.Fatalf("reply to an operator request:\n%s", text)
	}
	text, _ = su.call("reply", map[string]any{"request_id": "msg_bogus", "response": "x"})
	wantStatus(t, text, "rejected")
}

func TestReplyAfterCancelOrExpiry(t *testing.T) {
	e := newEnv(t)
	task := e.connect(e.agent("wm-p-a", "a", "post-office"))
	su := e.connect(e.stepUp())

	text, _ := task.call("send_request", map[string]any{"body": "q", "recipient": "step-up", "wait_seconds": 0})
	cancelled := field(text, "request_id")
	su.call("next_message", map[string]any{"timeout_seconds": 0})
	task.call("cancel_request", map[string]any{"request_id": cancelled, "reason": "found another way"})
	text, _ = su.call("reply", map[string]any{"request_id": cancelled, "response": "granted"})
	wantStatus(t, text, "closed")
	if field(text, "outcome") != "cancelled" || field(text, "reason") != "found another way" {
		t.Fatalf("reply after cancel:\n%s", text)
	}

	text, _ = task.call("send_request", map[string]any{"body": "q", "recipient": "step-up", "wait_seconds": 0, "expires_in_minutes": 1})
	expiring := field(text, "request_id")
	e.skew.Store(int64(2 * time.Minute))
	// Expired before step-up took it: next_message skips it.
	text, _ = su.call("next_message", map[string]any{"timeout_seconds": 0})
	wantStatus(t, text, "empty")
	text, _ = su.call("reply", map[string]any{"request_id": expiring, "response": "late"})
	wantStatus(t, text, "closed")
	if field(text, "outcome") != "expired" {
		t.Fatalf("reply after expiry:\n%s", text)
	}
	text, _ = task.call("check_response", map[string]any{"request_id": expiring})
	wantStatus(t, text, "expired")
}

// TestReplyCancelExpiryRace races step-up's reply, the sender's
// cancel_request and expiry on the same requests: exactly one outcome wins,
// and the sender, step-up and the done record all agree on it.
func TestReplyCancelExpiryRace(t *testing.T) {
	e := newEnv(t)
	task := e.connect(e.agent("wm-p-a", "a", "post-office"))
	su := e.connect(e.stepUp())
	const rounds = 15
	for i := range rounds {
		text, _ := task.call("send_request", map[string]any{"body": "q", "recipient": "step-up", "wait_seconds": 0})
		id := field(text, "request_id")
		su.call("next_message", map[string]any{"timeout_seconds": 0})

		var replyText, cancelText string
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(3)
		go func() {
			defer wg.Done()
			<-start
			replyText, _, _ = su.tryCall(context.Background(), "reply", map[string]any{"request_id": id, "response": "granted"}, nil)
		}()
		go func() {
			defer wg.Done()
			<-start
			cancelText, _, _ = task.tryCall(context.Background(), "cancel_request", map[string]any{"request_id": id}, nil)
		}()
		go func() {
			defer wg.Done()
			<-start
			if i%3 == 0 {
				// Expiry, as the ticker claims it.
				_, _ = e.store.Claim(postoffice.Resolution{ID: id, Outcome: postoffice.OutcomeExpired})
			}
		}()
		close(start)
		wg.Wait()

		var d postoffice.DoneRecord
		deadline := time.Now().Add(3 * time.Second)
		for {
			var ok bool
			if d, ok, _ = e.store.Done(id); ok || time.Now().After(deadline) {
				if !ok {
					t.Fatalf("round %d: %s never finalised", i, id)
				}
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		won := string(d.Resolution.Outcome)
		// The sender's view.
		if got := field(cancelText, "status"); got != won {
			t.Fatalf("round %d: cancel_request says %q, done says %q\n%s", i, got, won, cancelText)
		}
		// Step-Up's view: answered when its reply won, else closed with the
		// winner.
		switch status := field(replyText, "status"); {
		case won == "answered" && status == "answered":
		case won != "answered" && status == "closed" && field(replyText, "outcome") == won:
		default:
			t.Fatalf("round %d: reply says %q/%q, done says %q\n%s", i, status, field(replyText, "outcome"), won, replyText)
		}
		text, _ = task.call("check_response", map[string]any{"request_id": id})
		wantStatus(t, text, won)
	}
}

// TestReceiverRestartRecovery: received_at survives a post-office restart,
// so next_message does not hand a message out again; list_inbox recovers
// the work in progress, and a reply after the restart reaches the sender.
func TestReceiverRestartRecovery(t *testing.T) {
	root := t.TempDir()
	open := func() (*Server, func()) {
		store, err := postoffice.Open(filepath.Join(root, "po"))
		if err != nil {
			t.Fatal(err)
		}
		srv, err := New(Config{
			Store:    store,
			Resolver: &Resolver{Gateways: mcpgateway.Store{Dir: filepath.Join(root, "gw")}},
			MaxWait:  time.Second, OutboxPoll: 50 * time.Millisecond, Logf: t.Logf,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		bg := make(chan struct{})
		go func() { defer close(bg); srv.Run(ctx) }()
		return srv, func() { cancel(); <-bg }
	}
	if err := (mcpgateway.Store{Dir: filepath.Join(root, "gw")}).Save(mcpgateway.Record{Sandbox: "step-up", ID: "gw-su", Servers: []string{"post-office"}}); err != nil {
		t.Fatal(err)
	}
	stepUp := Caller{GatewayID: "gw-su", Sender: postoffice.Sender{Agent: "step-up", Sandbox: "step-up"}}
	sender := postoffice.Sender{Agent: "task", Sandbox: "wm-p-a", Task: "a"}

	srv, stop := open()
	var ids []string
	for _, body := range []string{"one", "two"} {
		m, err := postoffice.NewMessage(postoffice.Draft{Kind: postoffice.KindRequest, From: sender, Recipient: "step-up", Body: body}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if err := srv.cfg.Store.Put(m); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.ID)
	}
	ctx := context.Background()
	if r := srv.nextMessage(ctx, nil, stepUp, nextMessageIn{TimeoutSeconds: new(0)}); field(r.text(), "request_id") != ids[0] {
		t.Fatalf("before restart:\n%s", r.text())
	}
	stop()

	srv, stop = open()
	defer stop()
	r := srv.nextMessage(ctx, nil, stepUp, nextMessageIn{TimeoutSeconds: new(0)})
	if field(r.text(), "request_id") != ids[1] {
		t.Fatalf("after restart, next_message returned:\n%s", r.text())
	}
	r = srv.nextMessage(ctx, nil, stepUp, nextMessageIn{TimeoutSeconds: new(0)})
	wantStatus(t, r.text(), "empty")
	inbox := srv.listInbox(ctx, nil, stepUp, struct{}{}).text()
	for _, id := range ids {
		if !strings.Contains(inbox, id+" request received ") {
			t.Fatalf("list_inbox after restart:\n%s", inbox)
		}
	}
	r = srv.reply(ctx, nil, stepUp, replyIn{RequestID: ids[0], Response: "done"})
	wantStatus(t, r.text(), "answered")
	d, ok, _ := srv.cfg.Store.Done(ids[0])
	if !ok || d.Resolution.Responder == nil || d.Resolution.Responder.Kind != postoffice.ResponderAgent || d.Resolution.Response != "done" {
		t.Fatalf("done = %+v %v", d, ok)
	}
}

func TestInReplyTo(t *testing.T) {
	e := newEnv(t)
	task := e.connect(e.agent("wm-p-a", "a", "post-office"))
	su := e.connect(e.stepUp())

	// Holding nothing: no link.
	text, _ := su.call("send_request", map[string]any{"body": "unrelated", "wait_seconds": 0})
	wantStatus(t, text, "pending")
	if m, _ := e.store.Get(field(text, "request_id")); m.InReplyTo != "" || field(text, "in_reply_to") != "" {
		t.Fatalf("unlinked request: %+v\n%s", m, text)
	}

	text, _ = task.call("send_request", map[string]any{"body": "need proxy.golang.org", "recipient": "step-up", "wait_seconds": 0})
	msgA := field(text, "request_id")
	// Sent but not received yet: still not held.
	text, _ = su.call("send_request", map[string]any{"body": "x", "wait_seconds": 0})
	if field(text, "in_reply_to") != "" {
		t.Fatalf("linked to an unreceived request:\n%s", text)
	}
	su.call("next_message", map[string]any{"timeout_seconds": 0})

	// Holding exactly one: linked automatically.
	text, _ = su.call("send_request", map[string]any{
		"subject": "Approve network allow proxy.golang.org?", "body": "CHANGE: sandbox_network_allow proxy.golang.org → wm-p-a",
		"options": []string{"approve", "deny"}, "wait_seconds": 0,
	})
	wantStatus(t, text, "pending")
	msgB := field(text, "request_id")
	if field(text, "in_reply_to") != msgA || !strings.Contains(field(text, "note"), "the request you hold") {
		t.Fatalf("automatic link:\n%s", text)
	}
	if m, _ := e.store.Get(msgB); m.InReplyTo != msgA || m.Recipient != "operator" || m.From.Agent != "step-up" {
		t.Fatalf("approval record = %+v", m)
	}

	// Holding two: ambiguous unless named.
	text, _ = task.call("send_request", map[string]any{"body": "another", "recipient": "step-up", "wait_seconds": 0})
	msgC := field(text, "request_id")
	su.call("next_message", map[string]any{"timeout_seconds": 0})
	text, _ = su.call("send_request", map[string]any{"body": "which?", "wait_seconds": 0})
	wantStatus(t, text, "rejected")
	if !strings.Contains(field(text, "reason"), "you hold 2 received requests") {
		t.Fatalf("ambiguous:\n%s", text)
	}
	text, _ = su.call("send_request", map[string]any{"body": "for C", "wait_seconds": 0, "in_reply_to": msgC})
	wantStatus(t, text, "pending")
	if m, _ := e.store.Get(field(text, "request_id")); m.InReplyTo != msgC {
		t.Fatalf("explicit link = %q", m.InReplyTo)
	}

	// Explicit links must name an open request addressed to step-up.
	text, _ = su.call("send_request", map[string]any{"body": "x", "wait_seconds": 0, "in_reply_to": msgB})
	wantStatus(t, text, "rejected")
	if field(text, "reason") != "in_reply_to: not addressed to you" {
		t.Fatalf("link to own request:\n%s", text)
	}
	su.call("reply", map[string]any{"request_id": msgC, "response": "done"})
	text, _ = su.call("send_request", map[string]any{"body": "x", "wait_seconds": 0, "in_reply_to": msgC})
	wantStatus(t, text, "rejected")
	if !strings.Contains(field(text, "reason"), "already closed (answered)") {
		t.Fatalf("link to a closed request:\n%s", text)
	}
	// With C answered, A is the only one held again.
	text, _ = su.call("send_request", map[string]any{"body": "x", "wait_seconds": 0})
	if field(text, "in_reply_to") != msgA {
		t.Fatalf("after answering C:\n%s", text)
	}

	// Step-Up cannot address itself.
	text, _ = su.call("send_request", map[string]any{"body": "x", "wait_seconds": 0, "recipient": "step-up"})
	wantStatus(t, text, "rejected")
}

// Step-Up can read the resource of a message addressed to it, but not
// another agent's operator request.
func TestRecipientReadsAddressedResource(t *testing.T) {
	e := newEnv(t)
	task := e.connect(e.agent("wm-p-a", "a", "post-office"))
	su := e.connect(e.stepUp())
	text, _ := task.call("send_request", map[string]any{"body": "q", "recipient": "step-up", "wait_seconds": 0})
	if _, err := su.cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: ResourceScheme + field(text, "request_id")}); err != nil {
		t.Fatalf("step-up reads its message: %v", err)
	}
	text, _ = task.call("send_request", map[string]any{"body": "q", "wait_seconds": 0})
	if _, err := su.cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: ResourceScheme + field(text, "request_id")}); err == nil {
		t.Fatal("step-up read an operator request")
	}
}
