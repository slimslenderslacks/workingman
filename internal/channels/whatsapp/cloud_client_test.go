package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/slimslenderslacks/work/internal/channels"
)

const testToken = "EAAGsupersecrettokenvalue1234567890"

// fakeGraph is a stand-in Graph API: it records every request and answers from
// a script (the last entry repeats).
type fakeGraph struct {
	srv *httptest.Server

	mu      sync.Mutex
	reqs    []graphReq
	script  []graphReply
	nextWID int
}

type graphReq struct {
	Method, Path, Auth, ContentType string
	Body                            map[string]any
}

type graphReply struct {
	Status  int
	Body    string // empty: a normal {"messages":[{"id":"wamid.N"}]}
	Headers map[string]string
}

func newFakeGraph(t *testing.T, script ...graphReply) *fakeGraph {
	t.Helper()
	g := &fakeGraph{script: script}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		g.mu.Lock()
		g.reqs = append(g.reqs, graphReq{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), body})
		i := min(len(g.reqs)-1, len(g.script)-1)
		var reply graphReply
		if i >= 0 {
			reply = g.script[i]
		}
		g.nextWID++
		wid := g.nextWID
		g.mu.Unlock()

		if reply.Status == 0 {
			reply.Status = http.StatusOK
		}
		if reply.Body == "" && reply.Status == http.StatusOK {
			reply.Body = fmt.Sprintf(`{"messaging_product":"whatsapp","contacts":[{"input":"1","wa_id":"1"}],"messages":[{"id":"wamid.%d"}]}`, wid)
		}
		for k, v := range reply.Headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(reply.Status)
		_, _ = io.WriteString(w, reply.Body)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGraph) requests() []graphReq {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]graphReq(nil), g.reqs...)
}

// fastRetry retries without real waiting; sleeps records what was asked for.
func fastRetry(c *CloudClient) *[]time.Duration {
	var mu sync.Mutex
	var slept []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) bool {
		mu.Lock()
		defer mu.Unlock()
		slept = append(slept, d)
		return true
	}
	return &slept
}

func newTestClient(t *testing.T, g *fakeGraph, opts ...CloudOption) *CloudClient {
	t.Helper()
	cfg := CloudConfig{PhoneNumberID: "1098765", AccessToken: channels.NewSecret(testToken), GraphBaseURL: g.srv.URL}
	opts = append([]CloudOption{WithRetryPolicy(RetryPolicy{MaxAttempts: 4, BaseDelay: time.Second, MaxDelay: 8 * time.Second})}, opts...)
	c, err := NewCloudClient(cfg, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func graphErrBody(code int, msg string) string {
	return fmt.Sprintf(`{"error":{"message":%q,"type":"OAuthException","code":%d,"fbtrace_id":"AbC123"}}`, msg, code)
}

func TestSendText_PayloadShape(t *testing.T) {
	g := newFakeGraph(t)
	c := newTestClient(t, g)

	ids, err := c.SendText(context.Background(), "+1 (555) 123-4567", "hello **world**", "wamid.QUOTED")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "wamid.1" {
		t.Fatalf("ids = %v", ids)
	}
	reqs := g.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	r := reqs[0]
	if r.Method != http.MethodPost || r.Path != "/v20.0/1098765/messages" {
		t.Errorf("request = %s %s", r.Method, r.Path)
	}
	if r.Auth != "Bearer "+testToken {
		t.Errorf("Authorization = %q", r.Auth)
	}
	if r.ContentType != "application/json" {
		t.Errorf("Content-Type = %q", r.ContentType)
	}
	want := map[string]any{
		"messaging_product": "whatsapp",
		"recipient_type":    "individual",
		"to":                "15551234567",
		"type":              "text",
		"text":              map[string]any{"body": "hello *world*", "preview_url": true},
		"context":           map[string]any{"message_id": "wamid.QUOTED"},
	}
	if got, w := mustJSON(t, r.Body), mustJSON(t, want); got != w {
		t.Errorf("payload\n got %s\nwant %s", got, w)
	}
}

func TestSendText_NoReplyContextWhenNotReplying(t *testing.T) {
	g := newFakeGraph(t)
	if _, err := newTestClient(t, g).SendText(context.Background(), "15551234567", "hi", ""); err != nil {
		t.Fatal(err)
	}
	if _, has := g.requests()[0].Body["context"]; has {
		t.Error("a context block was sent with no reply target")
	}
}

func TestSendText_CustomVersionAndBaseURL(t *testing.T) {
	g := newFakeGraph(t)
	cfg := CloudConfig{PhoneNumberID: "42", AccessToken: channels.NewSecret(testToken), APIVersion: "v21.0", GraphBaseURL: g.srv.URL + "/"}
	c, err := NewCloudClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SendText(context.Background(), "15551234567", "hi", ""); err != nil {
		t.Fatal(err)
	}
	if p := g.requests()[0].Path; p != "/v21.0/42/messages" {
		t.Errorf("path = %q", p)
	}
}

func TestSendText_ChunksLongTextAndQuotesFirstChunkOnly(t *testing.T) {
	g := newFakeGraph(t)
	c := newTestClient(t, g)
	text := strings.Repeat("A paragraph of reasonable length, repeated.\n\n", 300) // ~13k chars

	ids, err := c.SendText(context.Background(), "15551234567", text, "wamid.Q")
	if err != nil {
		t.Fatal(err)
	}
	reqs := g.requests()
	if len(reqs) < 3 || len(ids) != len(reqs) {
		t.Fatalf("%d requests, %d ids", len(reqs), len(ids))
	}
	for i, r := range reqs {
		body := r.Body["text"].(map[string]any)["body"].(string)
		if n := utf8.RuneCountInString(body); n > MaxMessageLength {
			t.Errorf("chunk %d is %d chars", i+1, n)
		}
		if !strings.HasSuffix(body, fmt.Sprintf(" (%d/%d)", i+1, len(reqs))) {
			t.Errorf("chunk %d missing its page marker", i+1)
		}
		_, quoted := r.Body["context"]
		if quoted != (i == 0) {
			t.Errorf("chunk %d quoted = %v; only the first chunk should quote", i+1, quoted)
		}
	}
	for i, id := range ids {
		if want := fmt.Sprintf("wamid.%d", i+1); id != want {
			t.Errorf("ids[%d] = %q, want %q", i, id, want)
		}
	}
}

func TestSendText_PartialFailureReturnsDeliveredIDs(t *testing.T) {
	g := newFakeGraph(t,
		graphReply{},
		graphReply{Status: 400, Body: graphErrBody(131026, "Message undeliverable")},
	)
	c := newTestClient(t, g)
	ids, err := c.SendText(context.Background(), "15551234567", strings.Repeat("word ", 2000), "")
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(ids) != 1 || ids[0] != "wamid.1" {
		t.Errorf("ids = %v, want the one delivered chunk", ids)
	}
}

func TestSendText_Validation(t *testing.T) {
	g := newFakeGraph(t)
	c := newTestClient(t, g)
	ctx := context.Background()

	if _, err := c.SendText(ctx, "15551234567", " \n\t ", ""); !errors.Is(err, ErrEmptyMessage) {
		t.Errorf("blank text: %v", err)
	}
	for _, to := range []string{"120363041234@g.us", "status@broadcast", "123@newsletter"} {
		if _, err := c.SendText(ctx, to, "hi", ""); !errors.Is(err, ErrUnsupportedRecipient) {
			t.Errorf("to %q: %v", to, err)
		}
	}
	if _, err := c.SendText(ctx, "not a number", "hi", ""); err == nil {
		t.Error("expected an error for a non-numeric recipient")
	}
	if n := len(g.requests()); n != 0 {
		t.Errorf("%d requests reached the API for invalid input", n)
	}
}

func TestSendText_RecipientForms(t *testing.T) {
	g := newFakeGraph(t)
	c := newTestClient(t, g)
	for _, to := range []string{"15551234567", "+15551234567", "1 555 123 4567", "15551234567@s.whatsapp.net", "15551234567:7@s.whatsapp.net"} {
		if _, err := c.SendText(context.Background(), to, "hi", ""); err != nil {
			t.Fatalf("%q: %v", to, err)
		}
	}
	for i, r := range g.requests() {
		if r.Body["to"] != "15551234567" {
			t.Errorf("request %d to = %v", i, r.Body["to"])
		}
	}
}

func TestSendText_ErrorTyping(t *testing.T) {
	g := newFakeGraph(t, graphReply{Status: 400, Body: graphErrBody(131047, "Re-engagement message")})
	c := newTestClient(t, g)

	_, err := c.SendText(context.Background(), "15551234567", "hi", "")
	if !errors.Is(err, ErrOutsideServiceWindow) || !IsOutsideServiceWindow(err) {
		t.Fatalf("err = %v; want ErrOutsideServiceWindow", err)
	}
	var ge *GraphError
	if !errors.As(err, &ge) {
		t.Fatalf("err is not a *GraphError: %T", err)
	}
	if ge.Status != 400 || ge.Code != 131047 || ge.Type != "OAuthException" || ge.FBTraceID != "AbC123" || ge.Message != "Re-engagement message" {
		t.Errorf("GraphError = %+v", ge)
	}
	if got := err.Error(); got != "whatsapp: graph error 131047 (HTTP 400): Re-engagement message" {
		t.Errorf("Error() = %q", got)
	}
	if n := len(g.requests()); n != 1 {
		t.Errorf("a closed window was retried: %d requests", n)
	}
}

func TestSendText_OtherGraphErrorsAreNotWindowErrors(t *testing.T) {
	g := newFakeGraph(t, graphReply{Status: 400, Body: graphErrBody(131030, "Recipient phone number not in allowed list")})
	_, err := newTestClient(t, g).SendText(context.Background(), "15551234567", "hi", "")
	if err == nil || IsOutsideServiceWindow(err) {
		t.Fatalf("err = %v", err)
	}
	var ge *GraphError
	if !errors.As(err, &ge) || ge.Code != 131030 || ge.Retryable() {
		t.Errorf("GraphError = %+v", ge)
	}
}

func TestGraphError_NonJSONBody(t *testing.T) {
	g := newFakeGraph(t, graphReply{Status: 502, Body: "<html>Bad Gateway</html>"})
	c := newTestClient(t, g, WithRetryPolicy(NoRetry))
	_, err := c.SendText(context.Background(), "15551234567", "hi", "")
	var ge *GraphError
	if !errors.As(err, &ge) || ge.Status != 502 || ge.Code != 0 || ge.Message != "<html>Bad Gateway</html>" {
		t.Fatalf("err = %v (%+v)", err, ge)
	}
	if want := "whatsapp: HTTP 502: <html>Bad Gateway</html>"; err.Error() != want {
		t.Errorf("Error() = %q", err.Error())
	}
}

func TestGraphError_AuthFailure(t *testing.T) {
	g := newFakeGraph(t, graphReply{Status: 401, Body: graphErrBody(190, "Error validating access token: Session has expired")})
	_, err := newTestClient(t, g).SendText(context.Background(), "15551234567", "hi", "")
	var ge *GraphError
	if !errors.As(err, &ge) || !ge.AuthFailure() || ge.Retryable() {
		t.Fatalf("err = %v (%+v)", err, ge)
	}
	if n := len(g.requests()); n != 1 {
		t.Errorf("an auth failure was retried: %d requests", n)
	}
}

func TestRetry_429ThenSuccess(t *testing.T) {
	g := newFakeGraph(t,
		graphReply{Status: 429, Body: graphErrBody(130429, "Rate limit hit")},
		graphReply{Status: 429, Body: graphErrBody(130429, "Rate limit hit")},
		graphReply{},
	)
	c := newTestClient(t, g)
	slept := fastRetry(c)

	ids, err := c.SendText(context.Background(), "15551234567", "hi", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || len(g.requests()) != 3 {
		t.Fatalf("ids=%v requests=%d", ids, len(g.requests()))
	}
	if want := []time.Duration{time.Second, 2 * time.Second}; !equalDurations(*slept, want) {
		t.Errorf("backoff = %v, want %v", *slept, want)
	}
}

func TestRetry_5xxThenSuccess(t *testing.T) {
	for _, status := range []int{500, 502, 503, 504} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			g := newFakeGraph(t, graphReply{Status: status, Body: "upstream trouble"}, graphReply{})
			c := newTestClient(t, g)
			fastRetry(c)
			if _, err := c.SendText(context.Background(), "15551234567", "hi", ""); err != nil {
				t.Fatal(err)
			}
			if n := len(g.requests()); n != 2 {
				t.Errorf("%d requests", n)
			}
		})
	}
}

func TestRetry_RateLimitCodeOnA400(t *testing.T) {
	g := newFakeGraph(t, graphReply{Status: 400, Body: graphErrBody(80007, "Rate limit issues")}, graphReply{})
	c := newTestClient(t, g)
	fastRetry(c)
	if _, err := c.SendText(context.Background(), "15551234567", "hi", ""); err != nil {
		t.Fatal(err)
	}
	if n := len(g.requests()); n != 2 {
		t.Errorf("%d requests", n)
	}
}

func TestRetry_IsBounded(t *testing.T) {
	g := newFakeGraph(t, graphReply{Status: 503, Body: "down"})
	c := newTestClient(t, g)
	slept := fastRetry(c)

	_, err := c.SendText(context.Background(), "15551234567", "hi", "")
	var ge *GraphError
	if !errors.As(err, &ge) || ge.Status != 503 {
		t.Fatalf("err = %v", err)
	}
	if n := len(g.requests()); n != 4 {
		t.Errorf("%d attempts, want MaxAttempts=4", n)
	}
	if want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}; !equalDurations(*slept, want) {
		t.Errorf("backoff = %v, want %v", *slept, want)
	}
}

func TestRetry_BackoffCapsAtMaxDelay(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 10, BaseDelay: time.Second, MaxDelay: 5 * time.Second}
	var got []time.Duration
	for retry := 1; retry <= 6; retry++ {
		got = append(got, p.delay(retry, 0))
	}
	want := []time.Duration{1, 2, 4, 5, 5, 5}
	for i := range want {
		if got[i] != want[i]*time.Second {
			t.Fatalf("delays = %v", got)
		}
	}
}

func TestRetry_JitterOnlyShortens(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 3, BaseDelay: time.Second, MaxDelay: 8 * time.Second, Jitter: 0.5}
	for range 200 {
		if d := p.delay(2, 0); d > 2*time.Second || d < time.Second {
			t.Fatalf("delay %v outside [1s, 2s]", d)
		}
	}
}

func TestRetry_HonorsRetryAfter(t *testing.T) {
	g := newFakeGraph(t,
		graphReply{Status: 429, Body: graphErrBody(130429, "slow down"), Headers: map[string]string{"Retry-After": "3"}},
		graphReply{},
	)
	c := newTestClient(t, g)
	slept := fastRetry(c)
	if _, err := c.SendText(context.Background(), "15551234567", "hi", ""); err != nil {
		t.Fatal(err)
	}
	if want := []time.Duration{3 * time.Second}; !equalDurations(*slept, want) {
		t.Errorf("waited %v, want the server's %v", *slept, want)
	}
}

func TestRetry_RetryAfterBeyondCapFailsFast(t *testing.T) {
	g := newFakeGraph(t, graphReply{Status: 429, Body: graphErrBody(130429, "slow down"), Headers: map[string]string{"Retry-After": "600"}})
	c := newTestClient(t, g)
	slept := fastRetry(c)
	_, err := c.SendText(context.Background(), "15551234567", "hi", "")
	var ge *GraphError
	if !errors.As(err, &ge) || ge.RetryAfter != 10*time.Minute {
		t.Fatalf("err = %v (%+v)", err, ge)
	}
	if len(g.requests()) != 1 || len(*slept) != 0 {
		t.Errorf("requests=%d sleeps=%v; a 10-minute Retry-After must not be waited out", len(g.requests()), *slept)
	}
}

func TestRetry_NotForClientErrors(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404} {
		g := newFakeGraph(t, graphReply{Status: status, Body: graphErrBody(100, "Invalid parameter")})
		c := newTestClient(t, g)
		fastRetry(c)
		if _, err := c.SendText(context.Background(), "15551234567", "hi", ""); err == nil {
			t.Fatalf("status %d: expected an error", status)
		}
		if n := len(g.requests()); n != 1 {
			t.Errorf("status %d was retried: %d requests", status, n)
		}
	}
}

func TestRetry_StopsWhenContextEnds(t *testing.T) {
	g := newFakeGraph(t, graphReply{Status: 503, Body: "down"})
	c := newTestClient(t, g)
	ctx, cancel := context.WithCancel(context.Background())
	c.sleep = func(ctx context.Context, _ time.Duration) bool { cancel(); return false }

	_, err := c.SendText(ctx, "15551234567", "hi", "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n := len(g.requests()); n != 1 {
		t.Errorf("%d requests after cancel", n)
	}
}

func TestRetry_TransportErrorsAfterConnectAreNotRetried(t *testing.T) {
	// A server that hangs up mid-request: the message may have been accepted,
	// so repeating it could deliver twice.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer srv.Close()
	c, err := NewCloudClient(CloudConfig{PhoneNumberID: "1", AccessToken: channels.NewSecret(testToken), GraphBaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	fastRetry(c)
	if _, err := c.SendText(context.Background(), "15551234567", "hi", ""); err == nil {
		t.Fatal("expected an error")
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("%d requests; a possibly-delivered message must not be resent", n)
	}
}

func TestRetry_ConnectionRefusedIsRetried(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close() // nothing listens any more
	c, err := NewCloudClient(CloudConfig{PhoneNumberID: "1", AccessToken: channels.NewSecret(testToken), GraphBaseURL: base},
		WithRetryPolicy(RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}))
	if err != nil {
		t.Fatal(err)
	}
	slept := fastRetry(c)
	if _, err := c.SendText(context.Background(), "15551234567", "hi", ""); err == nil {
		t.Fatal("expected an error")
	}
	if len(*slept) != 2 {
		t.Errorf("retried %d times, want 2", len(*slept))
	}
}

func TestMarkReadAndTyping(t *testing.T) {
	g := newFakeGraph(t, graphReply{Body: `{"success":true}`})
	c := newTestClient(t, g)
	ctx := context.Background()

	if err := c.MarkRead(ctx, "wamid.IN1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Typing(ctx, "wamid.IN2"); err != nil {
		t.Fatal(err)
	}
	reqs := g.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests", len(reqs))
	}
	if got, want := mustJSON(t, reqs[0].Body), mustJSON(t, map[string]any{"messaging_product": "whatsapp", "status": "read", "message_id": "wamid.IN1"}); got != want {
		t.Errorf("read receipt\n got %s\nwant %s", got, want)
	}
	if got, want := mustJSON(t, reqs[1].Body), mustJSON(t, map[string]any{"messaging_product": "whatsapp", "status": "read", "message_id": "wamid.IN2", "typing_indicator": map[string]any{"type": "text"}}); got != want {
		t.Errorf("typing\n got %s\nwant %s", got, want)
	}
	if reqs[0].Path != "/v20.0/1098765/messages" {
		t.Errorf("path = %q", reqs[0].Path)
	}
	if err := c.MarkRead(ctx, " "); err == nil {
		t.Error("expected an error for an empty message id")
	}
}

func TestTyping_IsBestEffortWithoutRetries(t *testing.T) {
	g := newFakeGraph(t, graphReply{Status: 503, Body: "down"})
	c := newTestClient(t, g)
	slept := fastRetry(c)
	if err := c.Typing(context.Background(), "wamid.X"); err == nil {
		t.Error("expected the failure to be reported")
	}
	if len(g.requests()) != 1 || len(*slept) != 0 {
		t.Errorf("requests=%d sleeps=%v; typing indicators must not retry", len(g.requests()), *slept)
	}
}

func TestTyping_OldWamidIsReported(t *testing.T) {
	g := newFakeGraph(t, graphReply{Status: 400, Body: graphErrBody(131009, "Parameter value is not valid")})
	err := newTestClient(t, g).Typing(context.Background(), "wamid.OLD")
	var ge *GraphError
	if !errors.As(err, &ge) || ge.Code != GraphCodeInvalidParameter {
		t.Fatalf("err = %v", err)
	}
}

func TestConfigValidate(t *testing.T) {
	tok := channels.NewSecret("tok")
	cases := []struct {
		name string
		cfg  CloudConfig
		want string // substring of the error; "" means valid
	}{
		{"minimal", CloudConfig{PhoneNumberID: "123", AccessToken: tok}, ""},
		{"all fields", CloudConfig{PhoneNumberID: "123", AccessToken: tok, APIVersion: "v21.0", GraphBaseURL: "http://localhost:1", WABAID: "9", AppSecret: tok, VerifyToken: tok}, ""},
		{"no phone id", CloudConfig{AccessToken: tok}, "phone_number_id is required"},
		{"phone number instead of id", CloudConfig{PhoneNumberID: "+1 555", AccessToken: tok}, "numeric id"},
		{"no token", CloudConfig{PhoneNumberID: "123"}, "access_token is required"},
		{"bad version", CloudConfig{PhoneNumberID: "123", AccessToken: tok, APIVersion: "20"}, "v20.0"},
		{"bad base", CloudConfig{PhoneNumberID: "123", AccessToken: tok, GraphBaseURL: "ftp://x"}, "graph_base_url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}

	if err := (CloudConfig{}).Validate(); err == nil || !strings.Contains(err.Error(), "phone_number_id") || !strings.Contains(err.Error(), "access_token") {
		t.Errorf("a bare config should report every problem: %v", err)
	}
}

func TestConfigDefaults(t *testing.T) {
	c, err := NewCloudClient(CloudConfig{PhoneNumberID: " 123 ", AccessToken: channels.NewSecret("tok")})
	if err != nil {
		t.Fatal(err)
	}
	cfg := c.Config()
	if cfg.APIVersion != "v20.0" || cfg.GraphBaseURL != "https://graph.facebook.com" || cfg.PhoneNumberID != "123" {
		t.Errorf("config = %+v", cfg)
	}
	if got := c.graphURL("messages"); got != "https://graph.facebook.com/v20.0/123/messages" {
		t.Errorf("graphURL = %q", got)
	}
}

// ---- token redaction ----

func TestRedaction_ErrorsNeverContainTheToken(t *testing.T) {
	leak := "Invalid token " + testToken + " for Bearer " + testToken
	cases := map[string]graphReply{
		"graph error body":        {Status: 401, Body: graphErrBody(190, leak)},
		"non-JSON body":           {Status: 500, Body: "proxy says: Authorization: Bearer " + testToken},
		"error type":              {Status: 400, Body: `{"error":{"message":"m","type":"` + testToken + `","code":1}}`},
		"query-string style leak": {Status: 400, Body: "bad request ?access_token=" + testToken + "&x=1"},
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			g := newFakeGraph(t, reply)
			c := newTestClient(t, g, WithRetryPolicy(NoRetry))
			_, err := c.SendText(context.Background(), "15551234567", "hi", "")
			if err == nil {
				t.Fatal("expected an error")
			}
			assertNoSecret(t, "error", err.Error())
			var ge *GraphError
			if errors.As(err, &ge) {
				assertNoSecret(t, "GraphError.Message", ge.Message)
				assertNoSecret(t, "GraphError.Type", ge.Type)
				assertNoSecret(t, "GraphError %+v", fmt.Sprintf("%+v", ge))
			}
		})
	}
}

func TestRedaction_TransportErrorsNeverContainTheToken(t *testing.T) {
	// A token embedded in the base URL (a misconfiguration) shows up in
	// net/http's error text; it must still be scrubbed.
	c, err := NewCloudClient(CloudConfig{PhoneNumberID: "1", AccessToken: channels.NewSecret(testToken), GraphBaseURL: "http://127.0.0.1:1/" + testToken}, WithRetryPolicy(NoRetry))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.SendText(context.Background(), "15551234567", "hi", "")
	if err == nil {
		t.Fatal("expected an error")
	}
	assertNoSecret(t, "transport error", err.Error())
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Errorf("expected a redaction marker in %q", err)
	}
	// The cause chain survives for errors.Is / errors.As.
	var ue interface{ Unwrap() error }
	if !errors.As(err, &ue) {
		t.Error("error chain was cut")
	}
}

func TestRedaction_LogsNeverContainTheToken(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	g := newFakeGraph(t,
		graphReply{Status: 503, Body: "down, Bearer " + testToken},
		graphReply{Status: 400, Body: graphErrBody(131047, "re-engage "+testToken)},
	)
	c := newTestClient(t, g, WithCloudLogger(logger))
	fastRetry(c)
	_, _ = c.SendText(context.Background(), "15551234567", "a private message body", "")
	_ = c.Typing(context.Background(), "wamid.X")

	logs := buf.String()
	if logs == "" {
		t.Fatal("expected some log output")
	}
	assertNoSecret(t, "log output", logs)
	if strings.Contains(logs, "a private message body") {
		t.Error("message text was logged")
	}
	if strings.Contains(logs, "15551234567") {
		t.Error("full recipient number was logged")
	}
}

func TestRedaction_Secret(t *testing.T) {
	cfg := CloudConfig{PhoneNumberID: "1", AccessToken: channels.NewSecret(testToken), AppSecret: channels.NewSecret("appsecret-xyz"), VerifyToken: channels.NewSecret("verify-me-123")}
	for _, s := range []string{fmt.Sprintf("%v", cfg), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg)} {
		assertNoSecret(t, "formatted config", s)
		if strings.Contains(s, "appsecret-xyz") || strings.Contains(s, "verify-me-123") {
			t.Errorf("config printed a secret: %s", s)
		}
	}
	c, err := NewCloudClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Redact("a=" + testToken + " b=appsecret-xyz c=verify-me-123"); strings.Contains(got, "xyz") || strings.Contains(got, testToken) || strings.Contains(got, "verify-me") {
		t.Errorf("Redact = %q", got)
	}
}

func TestRedact_LeavesOrdinaryTextAlone(t *testing.T) {
	c, _ := NewCloudClient(CloudConfig{PhoneNumberID: "1", AccessToken: channels.NewSecret(testToken)})
	for _, s := range []string{"graph error 131047 (HTTP 400): Re-engagement message", "bearers of bad news", "EA short", ""} {
		if got := c.Redact(s); got != s {
			t.Errorf("Redact(%q) = %q", s, got)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cases := map[string]time.Duration{
		"":        0,
		"7":       7 * time.Second,
		"-5":      0,
		"garbage": 0,
		now.Add(90 * time.Second).UTC().Format(http.TimeFormat): 90 * time.Second,
		now.Add(-time.Hour).UTC().Format(http.TimeFormat):       0,
	}
	for in, want := range cases {
		if got := parseRetryAfter(in, now); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

func assertNoSecret(t *testing.T, what, s string) {
	t.Helper()
	if strings.Contains(s, testToken) || strings.Contains(s, testToken[:12]) {
		t.Errorf("%s leaks the access token: %s", what, s)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v) // map keys marshal sorted
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func equalDurations(a, b []time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
