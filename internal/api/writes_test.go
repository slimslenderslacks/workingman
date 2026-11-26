package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeActions is a Source with write actions that record their calls.
type fakeActions struct {
	fakeSource
	mu     sync.Mutex
	calls  []string
	reqs   []WriteRequest
	result WriteResult
	err    error
}

func (f *fakeActions) record(call string, w WriteRequest) (WriteResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	f.reqs = append(f.reqs, w)
	return f.result, f.err
}

func (f *fakeActions) NewProject(_ context.Context, w WriteRequest, req NewProjectRequest) (WriteResult, error) {
	return f.record(fmt.Sprintf("new %d %s %q", req.Root, req.Name, req.Description), w)
}

func (f *fakeActions) QueueIntake(_ context.Context, w WriteRequest, project string, req QueueIntakeRequest) (WriteResult, error) {
	return f.record(fmt.Sprintf("intake %s %s %q", project, req.Name, req.Body), w)
}

func (f *fakeActions) WithdrawIntake(_ context.Context, w WriteRequest, project, name string) (WriteResult, error) {
	return f.record("withdraw "+project+" "+name, w)
}

func (f *fakeActions) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// bearerWrite sends a write with the bearer token, as a script would.
func bearerWrite(t *testing.T, h http.Handler, tok, method, target, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Host = "127.0.0.1:7078"
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func newWriteServer(t *testing.T) (*fakeActions, http.Handler, string, *[]string) {
	t.Helper()
	src := &fakeActions{result: WriteResult{Body: map[string]string{"ok": "yes"}, ETag: `"in-1234"`, Path: "/r/p/intake/x.md", ETagAfter: `"in-1234"`}}
	s, tok := newTestServer(t, src)
	var logged []string
	var mu sync.Mutex
	s.Log = func(event string, kv ...string) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, event+" "+strings.Join(kv, " "))
	}
	return src, s.Handler("7078"), tok, &logged
}

func TestWriteEndpointsRouteAndAnswer202(t *testing.T) {
	src, h, tok, logged := newWriteServer(t)
	for _, tc := range []struct {
		method, path, body, call string
	}{
		{"POST", Prefix + "/projects", `{"root":0,"name":"gui","description":"draw"}`, `new 0 gui "draw"`},
		{"POST", Prefix + "/projects/p_0.demo/intake", `{"name":"add-card","body":"please"}`, `intake p_0.demo add-card "please"`},
		{"DELETE", Prefix + "/projects/p_0.demo/intake/add-card", "", "withdraw p_0.demo add-card"},
	} {
		rec := bearerWrite(t, h, tok, tc.method, tc.path, tc.body, nil)
		if rec.Code != http.StatusAccepted {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, rec.Code, rec.Body)
			continue
		}
		if rec.Header().Get("ETag") != `"in-1234"` || rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: headers %v", tc.path, rec.Header())
		}
		if got := src.calls[len(src.calls)-1]; got != tc.call {
			t.Errorf("%s: call %q, want %q", tc.path, got, tc.call)
		}
	}
	if len(*logged) != 3 {
		t.Errorf("audit lines: %v", *logged)
	}
	// The test logger joins keys and values with spaces.
	for _, l := range *logged {
		if !strings.HasPrefix(l, "api_write action ") || !strings.Contains(l, "client bearer") || !strings.Contains(l, "path /r/p/intake/x.md") {
			t.Errorf("audit line %q", l)
		}
	}
	if rec := do(t, h, "PUT", Prefix+"/projects", "127.0.0.1:7078", tok, nil); rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD, POST" {
		t.Errorf("PUT /projects: %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	if rec := do(t, h, "POST", Prefix+"/projects/p_0.demo/intake/x", "127.0.0.1:7078", tok, nil); rec.Header().Get("Allow") != "DELETE, GET, HEAD" {
		t.Errorf("POST intake/x: %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestWriteNoopIs200(t *testing.T) {
	src, h, tok, logged := newWriteServer(t)
	src.result.Noop = true
	rec := bearerWrite(t, h, tok, "DELETE", Prefix+"/projects/p_0.demo/intake/x", "", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("noop: %d, want 200", rec.Code)
	}
	if !strings.Contains((*logged)[0], "noop true") {
		t.Errorf("audit %q lacks noop", (*logged)[0])
	}
}

func TestWriteErrorShape(t *testing.T) {
	src, h, tok, logged := newWriteServer(t)
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("%w: bad name", ErrInvalid), 400, CodeBadRequest},
		{fmt.Errorf("%w: no project", ErrNotFound), 404, CodeNotFound},
		{fmt.Errorf("%w: taken", ErrConflict), 409, CodeConflict},
		{fmt.Errorf("%w: workingman-tui is already stopped", ErrInvalidState), 409, CodeInvalidState},
		{WithCurrent(fmt.Errorf("%w: changed", ErrPreconditionFailed), map[string]string{"status": "blocked"}), 412, CodePreconditionFailed},
		{fmt.Errorf("%w: down", ErrUnavailable), 503, CodeUnavailable},
		{fmt.Errorf("disk on fire"), 500, CodeInternal},
	} {
		src.err = tc.err
		rec := bearerWrite(t, h, tok, "POST", Prefix+"/projects/p_0.demo/intake", `{"body":"x"}`, nil)
		if rec.Code != tc.status {
			t.Errorf("%v: %d, want %d", tc.err, rec.Code, tc.status)
		}
		var body struct {
			Error struct {
				Code    string          `json:"code"`
				Message string          `json:"message"`
				Current json.RawMessage `json:"current"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s", rec.Body)
		}
		if body.Error.Code != tc.code || body.Error.Message != tc.err.Error() {
			t.Errorf("%v: body %s", tc.err, rec.Body)
		}
		if tc.status == 412 && string(body.Error.Current) != `{"status":"blocked"}` {
			t.Errorf("412 current = %s", body.Error.Current)
		}
	}
	for _, l := range *logged {
		if !strings.HasPrefix(l, "api_write_refused action queue_intake code ") {
			t.Errorf("audit line %q", l)
		}
	}
}

func TestWriteBodyChecks(t *testing.T) {
	src, h, tok, _ := newWriteServer(t)
	for name, tc := range map[string]struct {
		body string
		hdr  map[string]string
		want int
	}{
		"form type":     {body: `name=x`, hdr: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, want: 415},
		"text type":     {body: `{"body":"x"}`, hdr: map[string]string{"Content-Type": "text/plain"}, want: 415},
		"no type":       {body: `{"body":"x"}`, hdr: map[string]string{"Content-Type": ""}, want: 415},
		"unknown field": {body: `{"body":"x","status":"ready"}`, want: 400},
		"not json":      {body: `{"body":`, want: 400},
		"two values":    {body: `{"body":"x"}{}`, want: 400},
		"json charset":  {body: `{"body":"x"}`, hdr: map[string]string{"Content-Type": "application/json; charset=utf-8"}, want: 202},
	} {
		before := src.callCount()
		rec := bearerWrite(t, h, tok, "POST", Prefix+"/projects/p_0.demo/intake", tc.body, tc.hdr)
		if rec.Code != tc.want {
			t.Errorf("%s: %d %s, want %d", name, rec.Code, rec.Body, tc.want)
		}
		if tc.want != 202 && src.callCount() != before {
			t.Errorf("%s: the action ran", name)
		}
	}
	// A bodiless write needs no Content-Type.
	if rec := bearerWrite(t, h, tok, "DELETE", Prefix+"/projects/p_0.demo/intake/x", "", nil); rec.Code != 202 {
		t.Errorf("bodiless DELETE: %d %s", rec.Code, rec.Body)
	}
}

func TestWriteBrowserNeedsCSRFAndScope(t *testing.T) {
	src, h, tok, _ := newWriteServer(t)
	c, si := login(t, h, tok)
	path := Prefix + "/projects/p_0.demo/intake"
	for name, tc := range map[string]struct {
		hdr  map[string]string
		want int
	}{
		"no csrf":        {hdr: withCookie(c, nil), want: 403},
		"wrong csrf":     {hdr: withCookie(c, map[string]string{HeaderCSRF: "nope"}), want: 403},
		"no origin":      {hdr: withCookie(c, map[string]string{HeaderCSRF: si.CSRF, "Origin": ""}), want: 403},
		"foreign origin": {hdr: withCookie(c, map[string]string{HeaderCSRF: si.CSRF, "Origin": "http://evil.example"}), want: 403},
		"form":           {hdr: withCookie(c, map[string]string{HeaderCSRF: si.CSRF, "Content-Type": "application/x-www-form-urlencoded"}), want: 415},
		"ok":             {hdr: withCookie(c, map[string]string{HeaderCSRF: si.CSRF}), want: 202},
	} {
		before := src.callCount()
		rec := postJSON(t, h, "POST", path, `{"body":"x"}`, tc.hdr)
		if rec.Code != tc.want {
			t.Errorf("%s: %d %s, want %d", name, rec.Code, rec.Body, tc.want)
		}
		if (tc.want == 202) != (src.callCount() > before) {
			t.Errorf("%s: action ran = %v", name, src.callCount() > before)
		}
	}
	if got := src.reqs[len(src.reqs)-1].Client; got != si.ID {
		t.Errorf("client = %q, want the session id %q", got, si.ID)
	}

	// A session without the write scope is refused, and the refusal is
	// audited.
	s2, tok2 := newTestServer(t, src)
	var logged []string
	s2.Log = func(event string, kv ...string) { logged = append(logged, event+" "+strings.Join(kv, " ")) }
	h2 := s2.Handler("7078")
	c2, si2 := login(t, h2, tok2)
	for _, bs := range s2.logins.sessions {
		bs.scopes = []string{ScopeRead}
	}
	before := src.callCount()
	rec := postJSON(t, h2, "POST", path, `{"body":"x"}`, withCookie(c2, map[string]string{HeaderCSRF: si2.CSRF}))
	if rec.Code != http.StatusForbidden || errorCode(t, rec) != CodeForbidden || !strings.Contains(rec.Body.String(), "write scope") {
		t.Errorf("read-only session: %d %s", rec.Code, rec.Body)
	}
	if src.callCount() != before {
		t.Error("the action ran without the write scope")
	}
	if len(logged) == 0 || !strings.HasPrefix(logged[len(logged)-1], "api_write_refused action queue_intake code forbidden") {
		t.Errorf("audit: %v", logged)
	}
}

func TestWriteWithoutActionsIs503(t *testing.T) {
	s, tok := newTestServer(t, &fakeSource{})
	rec := bearerWrite(t, s.Handler(""), tok, "POST", Prefix+"/projects", `{"name":"x"}`, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("%d %s, want 503", rec.Code, rec.Body)
	}
}

func TestIdempotencyKey(t *testing.T) {
	src, h, tok, logged := newWriteServer(t)
	path := Prefix + "/projects/p_0.demo/intake"
	key := map[string]string{HeaderIdempotencyKey: "6f1e0a7c-1b8e-4c25-9b0e-3a7d5d0e8b11"}
	first := bearerWrite(t, h, tok, "POST", path, `{"body":"x"}`, key)
	if first.Code != 202 {
		t.Fatalf("first: %d %s", first.Code, first.Body)
	}
	again := bearerWrite(t, h, tok, "POST", path, `{"body":"x"}`, key)
	if again.Code != 202 || again.Body.String() != first.Body.String() || again.Header().Get(HeaderIdempotentReplay) != "true" || again.Header().Get("ETag") != `"in-1234"` {
		t.Errorf("replay: %d %v %s", again.Code, again.Header(), again.Body)
	}
	if src.callCount() != 1 {
		t.Errorf("the action ran %d times, want once", src.callCount())
	}
	if len(*logged) != 1 {
		t.Errorf("a replay was audited again: %v", *logged)
	}
	// The same key with another body is refused.
	if rec := bearerWrite(t, h, tok, "POST", path, `{"body":"y"}`, key); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("reused key: %d %s, want 422", rec.Code, rec.Body)
	}
	// A refusal is replayed too: it is the answer to that request.
	src.err = fmt.Errorf("%w: taken", ErrConflict)
	k2 := map[string]string{HeaderIdempotencyKey: "k2"}
	bearerWrite(t, h, tok, "POST", path, `{"body":"z"}`, k2)
	src.err = nil
	if rec := bearerWrite(t, h, tok, "POST", path, `{"body":"z"}`, k2); rec.Code != 409 {
		t.Errorf("replayed refusal: %d, want 409", rec.Code)
	}
	// A server error is not kept: the retry runs.
	src.err = fmt.Errorf("disk on fire")
	k3 := map[string]string{HeaderIdempotencyKey: "k3"}
	bearerWrite(t, h, tok, "POST", path, `{"body":"w"}`, k3)
	src.err = nil
	if rec := bearerWrite(t, h, tok, "POST", path, `{"body":"w"}`, k3); rec.Code != 202 || rec.Header().Get(HeaderIdempotentReplay) != "" {
		t.Errorf("retry after 500: %d %v", rec.Code, rec.Header())
	}
	// Keys are per client: a browser session with the bearer's key is a new
	// request.
	c, si := login(t, h, tok)
	n := src.callCount()
	rec := postJSON(t, h, "POST", path, `{"body":"x"}`, withCookie(c, map[string]string{HeaderCSRF: si.CSRF, HeaderIdempotencyKey: key[HeaderIdempotencyKey]}))
	if rec.Code != 202 || src.callCount() != n+1 {
		t.Errorf("other client, same key: %d calls %d→%d", rec.Code, n, src.callCount())
	}
	if rec := bearerWrite(t, h, tok, "POST", path, `{"body":"x"}`, map[string]string{HeaderIdempotencyKey: "has space"}); rec.Code != 400 {
		t.Errorf("bad key: %d, want 400", rec.Code)
	}
}

func TestIdempotencyStoreExpiresAndBounds(t *testing.T) {
	id := newIdempotency()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	if r, msg := id.begin("a", "fp", now); r != nil || msg != "" {
		t.Fatal("first begin")
	}
	if _, msg := id.begin("a", "fp", now); !strings.Contains(msg, "in progress") {
		t.Errorf("in flight: %q", msg)
	}
	id.finish("a", 202, http.Header{}, []byte("x"))
	if r, _ := id.begin("a", "fp", now.Add(IdempotencyTTL-1)); r == nil {
		t.Error("kept key not replayed")
	}
	if r, msg := id.begin("a", "fp", now.Add(IdempotencyTTL)); r != nil || msg != "" {
		t.Error("expired key replayed")
	}
	for i := 0; i < maxIdempotencyKeys+10; i++ {
		id.begin(fmt.Sprint("k", i), "fp", now)
	}
	if len(id.m) > maxIdempotencyKeys {
		t.Errorf("store holds %d keys, cap %d", len(id.m), maxIdempotencyKeys)
	}
}

func TestMatchETag(t *testing.T) {
	for _, tc := range []struct {
		header, etag string
		want         bool
	}{
		{"", `"a"`, true},
		{"", "", true},
		{`"a"`, `"a"`, true},
		{`"b", "a"`, `"a"`, true},
		{`"b"`, `"a"`, false},
		{`W/"a"`, `"a"`, false},
		{"*", `"a"`, true},
		{"*", "", false},
		{`"a"`, "", false},
	} {
		if got := MatchETag(tc.header, tc.etag); got != tc.want {
			t.Errorf("MatchETag(%q, %q) = %v, want %v", tc.header, tc.etag, got, tc.want)
		}
	}
}
