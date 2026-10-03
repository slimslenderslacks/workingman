package whatsapp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/channels"
)

const (
	whAppSecret   = "app-secret-for-tests"
	whVerifyToken = "verify-me"
	whPhoneID     = "1098765"
	whSender      = "15551234567"
)

// whHarness is a started webhook channel with a collecting handler.
type whHarness struct {
	t      *testing.T
	ch     *Channel
	graph  *fakeGraph
	cancel context.CancelFunc
	got    chan channels.InboundMessage
	base   string // http://host:port
}

type whOpts struct {
	secret, verify  string
	waba            string
	insecure        bool
	allow           []string
	denyReply       string
	skipStart       bool
	hook            *WebhookConfig
	handlerOverride channels.InboundHandler
}

func newWHHarness(t *testing.T, o whOpts) *whHarness {
	t.Helper()
	g := newFakeGraph(t)
	cfg := CloudConfig{
		PhoneNumberID: whPhoneID,
		AccessToken:   channels.NewSecret(testToken),
		AppSecret:     channels.NewSecret(o.secret),
		VerifyToken:   channels.NewSecret(o.verify),
		WABAID:        o.waba,
		GraphBaseURL:  g.srv.URL,
	}
	client, err := NewCloudClient(cfg, WithRetryPolicy(NoRetry))
	if err != nil {
		t.Fatal(err)
	}
	policy, err := NewAccessPolicy(AccessConfig{AllowFrom: o.allow, DenyReply: o.denyReply})
	if err != nil {
		t.Fatal(err)
	}
	hook := WebhookConfig{Host: "127.0.0.1", Port: 0, InsecureSkipSignature: o.insecure}
	if o.hook != nil {
		hook = *o.hook
	}
	ch := NewChannel("whatsapp", client, nil, WithWebhook(hook), WithAccessPolicy(policy))
	h := &whHarness{t: t, ch: ch, graph: g, got: make(chan channels.InboundMessage, 32)}
	t.Cleanup(func() { _ = ch.Close() })
	if o.skipStart {
		return h
	}
	var ctx context.Context
	ctx, h.cancel = context.WithCancel(context.Background())
	t.Cleanup(h.cancel)
	handler := o.handlerOverride
	if handler == nil {
		handler = func(_ context.Context, m channels.InboundMessage) { h.got <- m }
	}
	if err := ch.Start(ctx, handler); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h.base = "http://" + ch.WebhookAddr()
	return h
}

func (h *whHarness) url() string { return h.base + DefaultWebhookPath }

func sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// post sends body with the given signature header ("" omits it).
func (h *whHarness) post(body []byte, sig string) *http.Response {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.url(), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (h *whHarness) postSigned(body []byte) *http.Response {
	h.t.Helper()
	return h.post(body, sign(whAppSecret, body))
}

func (h *whHarness) expect(want string) channels.InboundMessage {
	h.t.Helper()
	select {
	case m := <-h.got:
		if want != "" && m.Text != want {
			h.t.Fatalf("got message %q, want %q", m.Text, want)
		}
		return m
	case <-time.After(3 * time.Second):
		h.t.Fatalf("no inbound message arrived (wanted %q)", want)
		return channels.InboundMessage{}
	}
}

// flush proves everything posted so far has been processed: the worker is
// ordered, so once a marker message comes through, earlier ones are done.
// Anything else that arrives first is a failure.
func (h *whHarness) flush() {
	h.t.Helper()
	body := whPayload(whMsg{ID: "wamid.FLUSH-" + fmt.Sprint(time.Now().UnixNano()), Text: "flush-marker"})
	if resp := h.postSigned(body); resp.StatusCode != 200 {
		h.t.Fatalf("flush status %d", resp.StatusCode)
	}
	h.expect("flush-marker")
}

type whMsg struct {
	ID, From, Type, Text string
	Raw                  map[string]any // merged into the message object
	ContextID            string
	Name                 string
	PhoneID, WABA        string
	Timestamp            string
}

func whPayload(ms ...whMsg) []byte {
	var msgs, contacts []any
	var phone, waba string
	for _, m := range ms {
		if m.From == "" {
			m.From = whSender
		}
		obj := map[string]any{"id": m.ID, "from": m.From, "timestamp": "1760000000"}
		if m.Timestamp != "" {
			obj["timestamp"] = m.Timestamp
		}
		switch m.Type {
		case "", "text":
			obj["type"] = "text"
			obj["text"] = map[string]any{"body": m.Text}
		default:
			obj["type"] = m.Type
		}
		if m.ContextID != "" {
			obj["context"] = map[string]any{"id": m.ContextID, "from": "15550001111"}
		}
		for k, v := range m.Raw {
			obj[k] = v
		}
		msgs = append(msgs, obj)
		contacts = append(contacts, map[string]any{"wa_id": m.From, "profile": map[string]any{"name": m.Name}})
		phone, waba = m.PhoneID, m.WABA
	}
	if phone == "" {
		phone = whPhoneID
	}
	if waba == "" {
		waba = "WABA-1"
	}
	return whEnvelope(waba, phone, map[string]any{"messages": msgs, "contacts": contacts})
}

func whEnvelope(waba, phone string, value map[string]any) []byte {
	value["messaging_product"] = "whatsapp"
	value["metadata"] = map[string]any{"display_phone_number": "15550001111", "phone_number_id": phone}
	b, _ := json.Marshal(map[string]any{
		"object": "whatsapp_business_account",
		"entry":  []any{map[string]any{"id": waba, "changes": []any{map[string]any{"field": "messages", "value": value}}}},
	})
	return b
}

func allowSender() whOpts {
	return whOpts{secret: whAppSecret, verify: whVerifyToken, allow: []string{whSender}}
}

func TestWebhook_VerifyHandshake(t *testing.T) {
	h := newWHHarness(t, allowSender())
	get := func(q url.Values) (int, string) {
		resp, err := http.Get(h.url() + "?" + q.Encode())
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	good := url.Values{"hub.mode": {"subscribe"}, "hub.verify_token": {whVerifyToken}, "hub.challenge": {"1158201444"}}

	if code, body := get(good); code != 200 || body != "1158201444" {
		t.Errorf("valid handshake: %d %q", code, body)
	}
	for name, mutate := range map[string]func(url.Values){
		"wrong token":       func(q url.Values) { q.Set("hub.verify_token", "nope") },
		"empty token":       func(q url.Values) { q.Del("hub.verify_token") },
		"non-ascii token":   func(q url.Values) { q.Set("hub.verify_token", "vérify") },
		"prefix of token":   func(q url.Values) { q.Set("hub.verify_token", whVerifyToken[:3]) },
		"wrong mode":        func(q url.Values) { q.Set("hub.mode", "unsubscribe") },
		"missing mode":      func(q url.Values) { q.Del("hub.mode") },
		"missing challenge": func(q url.Values) { q.Del("hub.challenge") },
	} {
		q := url.Values{}
		for k, v := range good {
			q[k] = v
		}
		mutate(q)
		code, body := get(q)
		if code == 200 || strings.Contains(body, "1158201444") {
			t.Errorf("%s: handshake succeeded (%d %q)", name, code, body)
		}
	}
	if code, _ := get(url.Values{"hub.mode": {"subscribe"}, "hub.verify_token": {"nope"}, "hub.challenge": {"x"}}); code != http.StatusForbidden {
		t.Errorf("wrong token status = %d, want 403", code)
	}
}

func TestWebhook_VerifyRefusedWithoutVerifyToken(t *testing.T) {
	o := allowSender()
	o.verify = ""
	h := newWHHarness(t, o)
	// An empty configured token must not match an empty supplied one.
	resp, err := http.Get(h.url() + "?hub.mode=subscribe&hub.verify_token=&hub.challenge=abc")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func TestWebhook_SignatureVerification(t *testing.T) {
	h := newWHHarness(t, allowSender())
	body := whPayload(whMsg{ID: "wamid.A", Text: "hello"})

	if resp := h.post(body, sign(whAppSecret, body)); resp.StatusCode != 200 {
		t.Fatalf("valid signature: status %d", resp.StatusCode)
	}
	h.expect("hello")

	cases := map[string]string{
		"missing":                 "",
		"wrong secret":            sign("other-secret", body),
		"signature of other body": sign(whAppSecret, []byte("{}")),
		"no prefix":               strings.TrimPrefix(sign(whAppSecret, body), "sha256="),
		"sha1 prefix":             "sha1=" + strings.TrimPrefix(sign(whAppSecret, body), "sha256="),
		"empty digest":            "sha256=",
		"not hex":                 "sha256=zzzz",
		"truncated":               sign(whAppSecret, body)[:20],
		"uppercase prefix":        strings.ToUpper(sign(whAppSecret, body)),
	}
	for name, sig := range cases {
		body := whPayload(whMsg{ID: "wamid.BAD-" + name, Text: "forged"})
		if resp := h.post(body, sig); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, resp.StatusCode)
		}
	}
	// A tampered body under a once-valid signature.
	tampered := bytes.Replace(body, []byte("hello"), []byte("HELLO"), 1)
	if resp := h.post(tampered, sign(whAppSecret, body)); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("tampered body: status %d, want 401", resp.StatusCode)
	}
	h.flush() // and none of the forgeries reached the handler (flush fails on anything else)

	var health map[string]any
	resp, err := http.Get(h.base + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_ = json.NewDecoder(resp.Body).Decode(&health)
	if n, _ := health["rejected_signature"].(float64); int(n) != len(cases)+1 {
		t.Errorf("rejected_signature = %v, want %d", health["rejected_signature"], len(cases)+1)
	}
}

func TestWebhook_RefusesToStartWithoutAppSecret(t *testing.T) {
	o := allowSender()
	o.secret = ""
	h := newWHHarness(t, whOptsSkip(o))
	err := h.ch.Start(context.Background(), func(context.Context, channels.InboundMessage) {})
	if err == nil || !strings.Contains(err.Error(), CredAppSecret) || !strings.Contains(err.Error(), OptInsecureSkipSignature) {
		t.Fatalf("Start without an app secret: %v", err)
	}
	if h.ch.WebhookAddr() != "" {
		t.Error("a listener was left bound")
	}
}

func whOptsSkip(o whOpts) whOpts { o.skipStart = true; return o }

func TestWebhook_InsecureSkipSignatureAcceptsUnsigned(t *testing.T) {
	o := allowSender()
	o.secret, o.insecure = "", true
	h := newWHHarness(t, o)
	if resp := h.post(whPayload(whMsg{ID: "wamid.U", Text: "unsigned"}), ""); resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	h.expect("unsigned")
}

func TestWebhook_InsecureFlagDoesNotWeakenConfiguredSecret(t *testing.T) {
	o := allowSender()
	o.insecure = true // secret is set too: signatures must still be checked
	h := newWHHarness(t, o)
	if resp := h.post(whPayload(whMsg{ID: "wamid.U", Text: "unsigned"}), ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unsigned POST with a secret configured: status %d, want 401", resp.StatusCode)
	}
}

func TestWebhook_DuplicateDeliveryIsDroppedOnce(t *testing.T) {
	h := newWHHarness(t, allowSender())
	body := whPayload(whMsg{ID: "wamid.DUP", Text: "once"})
	for range 3 {
		if resp := h.postSigned(body); resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	h.expect("once")
	h.flush()
	if n := h.ch.hook.duplicates.Load(); n != 2 {
		t.Errorf("duplicates = %d, want 2", n)
	}
}

func TestWamidCache_IsBoundedFIFO(t *testing.T) {
	c := newWamidCache(3)
	for _, id := range []string{"a", "b", "c"} {
		if !c.firstSight(id) {
			t.Fatalf("%s reported as seen", id)
		}
	}
	if c.firstSight("a") {
		t.Error("a should still be remembered")
	}
	c.firstSight("d") // evicts the oldest, a
	if len(c.seen) != 3 || c.order.Len() != 3 {
		t.Fatalf("cache holds %d/%d, want 3", len(c.seen), c.order.Len())
	}
	if !c.firstSight("a") {
		t.Error("a was evicted and should count as new again")
	}
	if c.firstSight("d") {
		t.Error("d should still be remembered")
	}
	// No id: cannot dedup, never stored.
	if !c.firstSight("") || !c.firstSight("") {
		t.Error("empty wamid must always pass")
	}
	if WamidDedupCacheSize != 5000 {
		t.Errorf("WamidDedupCacheSize = %d", WamidDedupCacheSize)
	}
}

func TestWebhook_AllowlistDeny(t *testing.T) {
	h := newWHHarness(t, allowSender())
	if resp := h.postSigned(whPayload(whMsg{ID: "wamid.X", From: "15559998888", Text: "let me in"})); resp.StatusCode != 200 {
		t.Fatalf("a denied sender is still acknowledged: status %d", resp.StatusCode)
	}
	h.flush() // fails if the stranger's message was delivered
	if n := len(h.graph.requests()); n != 0 {
		t.Errorf("%d Graph calls for a denied sender with no deny_reply (typing/read receipts leak)", n)
	}
	// And nothing was noted for typing.
	if h.ch.lastInbound("15559998888") != "" {
		t.Error("denied sender was noted as inbound")
	}
	if h.ch.lastInbound(whSender) == "" {
		t.Error("allowed sender should be noted for typing")
	}
}

func TestWebhook_EmptyAllowlistDeniesEveryone(t *testing.T) {
	o := allowSender()
	o.allow = nil
	h := newWHHarness(t, o)
	h.postSigned(whPayload(whMsg{ID: "wamid.X", Text: "hi"}))
	select {
	case m := <-h.got:
		t.Fatalf("delivered %q with an empty allowlist", m.Text)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestWebhook_DenyReplyIsSentOnce(t *testing.T) {
	o := allowSender()
	o.denyReply = "Sorry, not for you."
	h := newWHHarness(t, o)
	for i := range 2 {
		h.postSigned(whPayload(whMsg{ID: fmt.Sprintf("wamid.S%d", i), From: "15559998888", Text: "hi"}))
	}
	h.flush()
	deadline := time.Now().Add(3 * time.Second)
	for len(h.graph.requests()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // a (wrong) second reply would land by now
	reqs := h.graph.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d Graph calls, want exactly one rate-limited deny reply", len(reqs))
	}
	if reqs[0].Body["to"] != "15559998888" || reqs[0].Body["text"].(map[string]any)["body"] != "Sorry, not for you." {
		t.Errorf("deny reply = %v", reqs[0].Body)
	}
}

func TestWebhook_MalformedJSON(t *testing.T) {
	h := newWHHarness(t, allowSender())
	for name, body := range map[string]string{
		"garbage":    "{not json",
		"array":      "[]",
		"empty":      "",
		"wrong type": `{"object": 7}`,
	} {
		if resp := h.postSigned([]byte(body)); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, resp.StatusCode)
		}
	}
	// Valid JSON, unknown shape: acknowledged and ignored, never retried by Meta.
	for name, body := range map[string]string{
		"null":         "null",
		"empty object": "{}",
		"other object": `{"object":"page","entry":[]}`,
		"odd entry":    `{"object":"whatsapp_business_account","entry":[{"id":"1","changes":[{"field":"messages","value":{"messages":[{"id":"wamid.Z","type":"text"}]}}]}]}`,
	} {
		if resp := h.postSigned([]byte(body)); resp.StatusCode != 200 {
			t.Errorf("%s: status %d, want 200", name, resp.StatusCode)
		}
	}
	h.flush()
}

func TestWebhook_OversizedBody(t *testing.T) {
	h := newWHHarness(t, allowSender())
	pad := strings.Repeat("a", WebhookMaxBodyBytes)
	body := []byte(`{"object":"x","pad":"` + pad + `"}`)
	resp := h.post(body, sign(whAppSecret, body))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413", resp.StatusCode)
	}
	// Exactly at the cap is fine.
	small := []byte(`{"object":"x","pad":"`)
	tail := []byte(`"}`)
	atCap := append(append(append([]byte{}, small...), bytes.Repeat([]byte("a"), WebhookMaxBodyBytes-len(small)-len(tail))...), tail...)
	if len(atCap) != WebhookMaxBodyBytes {
		t.Fatalf("test body is %d bytes", len(atCap))
	}
	if resp := h.post(atCap, sign(whAppSecret, atCap)); resp.StatusCode != 200 {
		t.Errorf("body of exactly %d bytes: status %d, want 200", WebhookMaxBodyBytes, resp.StatusCode)
	}
	h.flush()
}

func TestWebhook_ReplyToNameAndTimestamp(t *testing.T) {
	h := newWHHarness(t, allowSender())
	h.postSigned(whPayload(whMsg{ID: "wamid.R1", Text: "yes, do it", ContextID: "wamid.QUOTED", Name: "Ada Lovelace", Timestamp: "1760000123"}))
	m := h.expect("yes, do it")
	if m.ReplyToID != "wamid.QUOTED" {
		t.Errorf("ReplyToID = %q", m.ReplyToID)
	}
	if m.SenderName != "Ada Lovelace" || m.SenderID != whSender || m.ChatID != whSender {
		t.Errorf("sender = %q/%q chat %q", m.SenderName, m.SenderID, m.ChatID)
	}
	if m.MessageID != "wamid.R1" || m.Channel != "whatsapp" {
		t.Errorf("ids = %q on %q", m.MessageID, m.Channel)
	}
	if !m.Timestamp.Equal(time.Unix(1760000123, 0)) {
		t.Errorf("Timestamp = %v", m.Timestamp)
	}

	h.postSigned(whPayload(whMsg{ID: "wamid.R2", Text: "no reply context"}))
	if m := h.expect(""); m.ReplyToID != "" || m.SenderName != "" {
		t.Errorf("unexpected reply/name: %+v", m)
	}
}

func TestWebhook_MessageKinds(t *testing.T) {
	h := newWHHarness(t, allowSender())
	send := []whMsg{
		{ID: "wamid.1", Type: "button", Raw: map[string]any{"button": map[string]any{"text": "Approve", "payload": "p"}}},
		{ID: "wamid.2", Type: "interactive", Raw: map[string]any{"interactive": map[string]any{"type": "button_reply", "button_reply": map[string]any{"id": "b1", "title": "Yes"}}}},
		{ID: "wamid.3", Type: "interactive", Raw: map[string]any{"interactive": map[string]any{"type": "list_reply", "list_reply": map[string]any{"id": "l1", "title": "Option two"}}}},
		{ID: "wamid.4", Type: "image", Raw: map[string]any{"image": map[string]any{"id": "m1", "caption": "my screenshot"}}},
		{ID: "wamid.5", Type: "image", Raw: map[string]any{"image": map[string]any{"id": "m2"}}},
		{ID: "wamid.6", Type: "audio", Raw: map[string]any{"audio": map[string]any{"id": "m3"}}},
		{ID: "wamid.7", Type: "document", Raw: map[string]any{"document": map[string]any{"id": "m4", "caption": "report"}}},
		{ID: "wamid.8", Type: "sticker"},
		{ID: "wamid.9", Type: "reaction", Raw: map[string]any{"reaction": map[string]any{"emoji": "👍", "message_id": "wamid.Q"}}},
		{ID: "wamid.10", Type: "system", Raw: map[string]any{"system": map[string]any{"body": "changed number"}}},
		{ID: "wamid.11", Type: "unsupported"},
		{ID: "wamid.12", Type: "order"},
		{ID: "wamid.13", Type: "text", Text: "   "},
	}
	want := []string{"Approve", "Yes", "Option two", "[image] my screenshot", "[image]", "[audio]", "[document] report", "[sticker]"}
	// One delivery carrying the whole batch, in order.
	h.postSigned(whPayload(send...))
	for _, w := range want {
		h.expect(w)
	}
	h.flush() // reactions, system events, unsupported and empty texts produced nothing
}

func TestWebhook_StatusesAndOtherFieldsIgnored(t *testing.T) {
	h := newWHHarness(t, allowSender())
	status := whEnvelope("WABA-1", whPhoneID, map[string]any{"statuses": []any{map[string]any{"id": "wamid.S", "status": "delivered", "recipient_id": whSender}}})
	h.postSigned(status)

	other, _ := json.Marshal(map[string]any{
		"object": "whatsapp_business_account",
		"entry": []any{map[string]any{"id": "WABA-1", "changes": []any{map[string]any{
			"field": "message_template_status_update",
			"value": map[string]any{"metadata": map[string]any{"phone_number_id": whPhoneID}, "messages": []any{map[string]any{"id": "wamid.T", "from": whSender, "type": "text", "text": map[string]any{"body": "wrong field"}}}},
		}}}},
	})
	h.postSigned(other)

	notWABA := bytes.Replace(whPayload(whMsg{ID: "wamid.N", Text: "wrong object"}), []byte("whatsapp_business_account"), []byte("page"), 1)
	h.postSigned(notWABA)
	h.flush()
}

func TestWebhook_PhoneAndWABAMatching(t *testing.T) {
	o := allowSender()
	o.waba = "WABA-1"
	h := newWHHarness(t, o)
	h.postSigned(whPayload(whMsg{ID: "wamid.P", Text: "other phone number", PhoneID: "5550000"}))
	h.postSigned(whPayload(whMsg{ID: "wamid.W", Text: "other waba", WABA: "WABA-2"}))
	h.postSigned(whPayload(whMsg{ID: "wamid.OK", Text: "ours"}))
	h.expect("ours")
	h.flush()

	// Without a configured WABA id any business account matches, but the phone number must.
	h2 := newWHHarness(t, allowSender())
	h2.postSigned(whPayload(whMsg{ID: "wamid.Q", Text: "any waba", WABA: "WABA-77"}))
	h2.expect("any waba")
	h2.postSigned(whPayload(whMsg{ID: "wamid.R", Text: "wrong phone", PhoneID: "5550000"}))
	h2.flush()
}

func TestWebhook_GroupShapedMessageDropped(t *testing.T) {
	h := newWHHarness(t, allowSender())
	h.postSigned(whPayload(whMsg{ID: "wamid.G", Text: "in a group", Raw: map[string]any{"chat": "120363041234@g.us"}}))
	h.flush()
}

func TestWebhook_NoteInboundFeedsTyping(t *testing.T) {
	h := newWHHarness(t, allowSender())
	h.postSigned(whPayload(whMsg{ID: "wamid.TYPE", Text: "hi"}))
	h.expect("hi")
	if err := h.ch.Typing(context.Background(), whSender); err != nil {
		t.Fatal(err)
	}
	reqs := h.graph.requests()
	if len(reqs) != 1 || reqs[0].Body["message_id"] != "wamid.TYPE" {
		t.Fatalf("typing requests = %+v", reqs)
	}
}

func TestWebhook_AcksBeforeHandlerFinishes(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	o := allowSender()
	o.handlerOverride = func(context.Context, channels.InboundMessage) {
		close(started)
		<-release
	}
	h := newWHHarness(t, o)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	done := make(chan *http.Response, 1)
	go func() { done <- h.postSigned(whPayload(whMsg{ID: "wamid.SLOW", Text: "slow"})) }()
	select {
	case resp := <-done:
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("webhook did not acknowledge while the handler was busy")
	}
	<-started
	close(release)
}

func TestWebhook_HandlerPanicDoesNotKillTheWorker(t *testing.T) {
	h := newWHHarness(t, allowSender())
	h.ch.hook.handler = func(_ context.Context, m channels.InboundMessage) {
		if m.Text == "boom" {
			panic("handler bug")
		}
		h.got <- m
	}
	h.postSigned(whPayload(whMsg{ID: "wamid.B", Text: "boom"}))
	h.postSigned(whPayload(whMsg{ID: "wamid.C", Text: "after"}))
	h.expect("after")
}

func TestWebhook_RoutingAndHealth(t *testing.T) {
	h := newWHHarness(t, allowSender())
	resp, err := http.Get(h.base + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var health map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil || resp.StatusCode != 200 {
		t.Fatalf("health: %d %v", resp.StatusCode, err)
	}
	if health["status"] != "ok" || health["phone_number_id"] != whPhoneID || health["webhook_path"] != DefaultWebhookPath ||
		health["verify_token_configured"] != true || health["app_secret_configured"] != true {
		t.Errorf("health = %v", health)
	}
	raw, _ := json.Marshal(health)
	for _, secret := range []string{whAppSecret, whVerifyToken, testToken} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("health leaks a secret: %s", raw)
		}
	}

	for path, want := range map[string]int{"/": 404, "/whatsapp/webhook/extra": 404, "/health/x": 404} {
		r, err := http.Get(h.base + path)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != want {
			t.Errorf("GET %s = %d, want %d", path, r.StatusCode, want)
		}
	}
	req, _ := http.NewRequest(http.MethodPut, h.url(), nil)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PUT = %d, want 405", r.StatusCode)
	}
}

func TestWebhook_CustomPath(t *testing.T) {
	o := allowSender()
	o.hook = &WebhookConfig{Path: "hooks/wa"} // missing slash is added
	h := newWHHarness(t, o)
	body := whPayload(whMsg{ID: "wamid.CP", Text: "custom"})
	req, _ := http.NewRequest(http.MethodPost, h.base+"/hooks/wa", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign(whAppSecret, body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	h.expect("custom")
	if r := h.post(body, sign(whAppSecret, body)); r.StatusCode != 404 {
		t.Errorf("default path still served: %d", r.StatusCode)
	}
}

func TestWebhook_GracefulShutdownOnContextCancel(t *testing.T) {
	h := newWHHarness(t, allowSender())
	h.postSigned(whPayload(whMsg{ID: "wamid.PRE", Text: "before shutdown"}))
	h.expect("before shutdown")

	h.cancel()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c := &http.Client{Timeout: 500 * time.Millisecond}
		resp, err := c.Get(h.base + "/health")
		if err != nil {
			break // listener is gone
		}
		resp.Body.Close()
		if time.Now().After(deadline) {
			t.Fatal("webhook still serving after its context was cancelled")
		}
		time.Sleep(20 * time.Millisecond)
	}
	closed := make(chan struct{})
	go func() { _ = h.ch.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung after shutdown")
	}
}

func TestWebhook_CloseDeliversAcknowledgedMessages(t *testing.T) {
	release := make(chan struct{})
	var seen []string
	o := allowSender()
	first := make(chan struct{})
	o.handlerOverride = func(_ context.Context, m channels.InboundMessage) {
		if m.Text == "first" {
			close(first)
			<-release
		}
		seen = append(seen, m.Text) // only the worker goroutine writes; read after Close
	}
	h := newWHHarness(t, o)
	h.postSigned(whPayload(whMsg{ID: "wamid.1", Text: "first"}))
	<-first
	h.postSigned(whPayload(whMsg{ID: "wamid.2", Text: "second"})) // acknowledged, queued behind the first

	closed := make(chan struct{})
	go func() { _ = h.ch.Close(); close(closed) }()
	time.Sleep(50 * time.Millisecond)
	close(release)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung")
	}
	if len(seen) != 2 || seen[1] != "second" {
		t.Fatalf("delivered %v; a message Meta was told was received must not be dropped on shutdown", seen)
	}
}

func TestWebhook_CloseIsIdempotentAndBlocksRestart(t *testing.T) {
	h := newWHHarness(t, allowSender())
	if err := h.ch.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.ch.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := http.Get(h.base + "/health"); err == nil {
		t.Error("still serving after Close")
	}
	if err := h.ch.Start(context.Background(), func(context.Context, channels.InboundMessage) {}); !errors.Is(err, channels.ErrNotRunning) {
		t.Errorf("Start after Close: %v", err)
	}
}

func TestWebhook_StartErrors(t *testing.T) {
	t.Run("port in use", func(t *testing.T) {
		h := newWHHarness(t, allowSender())
		_, port, _ := strings.Cut(h.ch.WebhookAddr(), ":")
		var p int
		fmt.Sscan(port, &p)
		o := whOptsSkip(allowSender())
		o.hook = &WebhookConfig{Port: p}
		h2 := newWHHarness(t, o)
		err := h2.ch.Start(context.Background(), func(context.Context, channels.InboundMessage) {})
		if err == nil || !strings.Contains(err.Error(), "listen") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("started twice", func(t *testing.T) {
		h := newWHHarness(t, allowSender())
		if err := h.ch.Start(context.Background(), func(context.Context, channels.InboundMessage) {}); err == nil {
			t.Error("second Start succeeded")
		}
	})
	t.Run("no policy", func(t *testing.T) {
		g := newFakeGraph(t)
		cfg := CloudConfig{PhoneNumberID: whPhoneID, AccessToken: channels.NewSecret(testToken), AppSecret: channels.NewSecret(whAppSecret), GraphBaseURL: g.srv.URL}
		client, _ := NewCloudClient(cfg)
		ch := NewChannel("w", client, nil, WithWebhook(WebhookConfig{}))
		defer ch.Close()
		if err := ch.Start(context.Background(), func(context.Context, channels.InboundMessage) {}); err == nil || !strings.Contains(err.Error(), "access policy") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("health path reserved", func(t *testing.T) {
		o := whOptsSkip(allowSender())
		o.hook = &WebhookConfig{Path: "/health"}
		h := newWHHarness(t, o)
		if err := h.ch.Start(context.Background(), func(context.Context, channels.InboundMessage) {}); err == nil {
			t.Error("Start accepted the health path as the webhook path")
		}
	})
	t.Run("send-only channel starts without a webhook", func(t *testing.T) {
		ch := newTestChannel(t, newFakeGraph(t))
		if err := ch.Start(context.Background(), func(context.Context, channels.InboundMessage) {}); err != nil {
			t.Fatal(err)
		}
		if ch.WebhookAddr() != "" {
			t.Error("a send-only channel opened a listener")
		}
	})
}

func TestWebhookConfigFromSpec(t *testing.T) {
	cfg, err := WebhookConfigFromSpec(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "127.0.0.1" || cfg.Port != 8090 || cfg.Path != "/whatsapp/webhook" || cfg.InsecureSkipSignature {
		t.Errorf("defaults = %+v", cfg)
	}
	cfg, err = WebhookConfigFromSpec(map[string]any{
		"webhook_host": "0.0.0.0", "webhook_port": 9000, "webhook_path": "wa", "insecure_skip_signature": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "0.0.0.0" || cfg.Port != 9000 || cfg.Path != "/wa" || !cfg.InsecureSkipSignature {
		t.Errorf("cfg = %+v", cfg)
	}
	if cfg, err = WebhookConfigFromSpec(map[string]any{"webhook_port": "8123"}); err != nil || cfg.Port != 8123 {
		t.Errorf("string port: %+v, %v", cfg, err)
	}
	for name, opts := range map[string]map[string]any{
		"port range":    {"webhook_port": 70000},
		"negative port": {"webhook_port": -1},
		"port type":     {"webhook_port": []any{1}},
		"port text":     {"webhook_port": "eighty"},
		"insecure type": {"insecure_skip_signature": "yes"},
		"host type":     {"webhook_host": 5.5},
		"health path":   {"webhook_path": "/health"},
		"path with ?":   {"webhook_path": "/a?b"},
	} {
		if _, err := WebhookConfigFromSpec(opts); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCloudFactory_WiresWebhookAndPolicy(t *testing.T) {
	g := newFakeGraph(t)
	spec := channels.ChannelConfig{Type: "whatsapp", Options: map[string]any{
		"phone_number_id": whPhoneID, "graph_base_url": g.srv.URL, "webhook_port": 0,
		"access": map[string]any{"allow_from": []any{whSender}},
	}}
	creds := channels.Credentials{CredAccessToken: channels.NewSecret(testToken), CredAppSecret: channels.NewSecret(whAppSecret), CredVerifyToken: channels.NewSecret(whVerifyToken)}
	ch, err := CloudFactory(nil)("whatsapp", spec, creds)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	got := make(chan channels.InboundMessage, 1)
	if err := ch.Start(context.Background(), func(_ context.Context, m channels.InboundMessage) { got <- m }); err != nil {
		t.Fatal(err)
	}
	cc := ch.(*Channel)
	body := whPayload(whMsg{ID: "wamid.F", Text: "via factory"})
	req, _ := http.NewRequest(http.MethodPost, "http://"+cc.WebhookAddr()+DefaultWebhookPath, bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign(whAppSecret, body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	select {
	case m := <-got:
		if m.Text != "via factory" {
			t.Errorf("text = %q", m.Text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("nothing delivered")
	}
}

func TestCloudFactory_RejectsBadWebhookAndAccessConfig(t *testing.T) {
	creds := channels.Credentials{CredAccessToken: channels.NewSecret(testToken)}
	for name, tc := range map[string]struct {
		opts map[string]any
		want string
	}{
		"bad port":    {map[string]any{"webhook_port": 99999}, "webhook_port"},
		"self-chat":   {map[string]any{"access": map[string]any{"mode": "self-chat", "self_ids": []any{"1"}}}, "bridge"},
		"access typo": {map[string]any{"access": map[string]any{"allowfrom": []any{"1"}}}, "allowfrom"},
		"invalid acl": {map[string]any{"access": map[string]any{"allow_from": []any{"*"}}}, "wildcard"},
	} {
		opts := map[string]any{"phone_number_id": "1"}
		for k, v := range tc.opts {
			opts[k] = v
		}
		_, err := CloudFactory(nil)("whatsapp", channels.ChannelConfig{Type: "whatsapp", Options: opts}, creds)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}
