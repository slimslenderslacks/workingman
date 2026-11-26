package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// postJSON sends a JSON body with the headers a same-origin browser fetch
// would carry, plus hdr.
func postJSON(t *testing.T, h http.Handler, method, target, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Host = "127.0.0.1:7078"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://127.0.0.1:7078")
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

func mintCode(t *testing.T, h http.Handler, tok string) LoginCode {
	t.Helper()
	rec := do(t, h, "POST", Prefix+"/login-codes", "127.0.0.1:7078", tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login-codes: %d %s", rec.Code, rec.Body)
	}
	var lc LoginCode
	if err := json.Unmarshal(rec.Body.Bytes(), &lc); err != nil {
		t.Fatal(err)
	}
	return lc
}

func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookie {
			return c
		}
	}
	t.Fatalf("no %s cookie in %v", SessionCookie, rec.Header())
	return nil
}

// login runs the whole browser flow and returns the cookie and session.
func login(t *testing.T, h http.Handler, tok string) (*http.Cookie, SessionInfo) {
	t.Helper()
	lc := mintCode(t, h, tok)
	rec := postJSON(t, h, "POST", Prefix+"/session", `{"code":"`+lc.Code+`"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	var si SessionInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &si); err != nil {
		t.Fatal(err)
	}
	return sessionCookie(t, rec), si
}

func withCookie(c *http.Cookie, hdr map[string]string) map[string]string {
	out := map[string]string{"Cookie": c.Name + "=" + c.Value}
	for k, v := range hdr {
		out[k] = v
	}
	return out
}

func TestLoginCodeFlow(t *testing.T) {
	var logged []string
	s, tok := newTestServer(t, &fakeSource{})
	s.Log = func(event string, kv ...string) { logged = append(logged, event) }
	h := s.Handler("7078")

	// Only the bearer token mints a code.
	if rec := do(t, h, "POST", Prefix+"/login-codes", "127.0.0.1:7078", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("login-codes without token: %d, want 401", rec.Code)
	}
	if rec := do(t, h, "GET", Prefix+"/login-codes", "127.0.0.1:7078", tok, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET login-codes: %d, want 405", rec.Code)
	}
	lc := mintCode(t, h, tok)
	if !strings.HasPrefix(lc.LoginURL, "http://127.0.0.1:7078/login#t=") || !strings.HasSuffix(lc.LoginURL, lc.Code) {
		t.Errorf("login_url = %q", lc.LoginURL)
	}
	if d := time.Until(lc.ExpiresAt); d <= 0 || d > LoginCodeTTL {
		t.Errorf("expires_at %v is not within %v", lc.ExpiresAt, LoginCodeTTL)
	}

	rec := postJSON(t, h, "POST", Prefix+"/session", `{"code":"`+lc.Code+`"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	c := sessionCookie(t, rec)
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Secure {
		t.Errorf("cookie = %+v, want HttpOnly, SameSite=Strict, Path=/, not Secure", c)
	}
	if c.Value == tok || c.Value == lc.Code {
		t.Error("the session secret is the token or the login code")
	}
	var si SessionInfo
	json.Unmarshal(rec.Body.Bytes(), &si)
	// The default browser session reads, writes and answers (§A.6.2).
	if si.Auth != "cookie" || si.CSRF == "" || si.ID == "" || strings.Join(si.Scopes, ",") != "read,write,answer" {
		t.Errorf("session = %+v", si)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("login Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}

	// The code works once.
	if rec := postJSON(t, h, "POST", Prefix+"/session", `{"code":"`+lc.Code+`"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("reused code: %d, want 401", rec.Code)
	}

	// The cookie reads the API, without the token.
	if rec := do(t, h, "GET", Prefix+"/status", "127.0.0.1:7078", "", withCookie(c, nil)); rec.Code != http.StatusOK {
		t.Errorf("status with cookie: %d %s", rec.Code, rec.Body)
	}
	rec = do(t, h, "GET", Prefix+"/session", "127.0.0.1:7078", "", withCookie(c, nil))
	var again SessionInfo
	json.Unmarshal(rec.Body.Bytes(), &again)
	if rec.Code != http.StatusOK || again.CSRF != si.CSRF || again.ID != si.ID {
		t.Errorf("GET session: %d %+v, want %+v", rec.Code, again, si)
	}
	// A browser session cannot mint more logins, even with its CSRF token.
	rec = postJSON(t, h, "POST", Prefix+"/login-codes", "", withCookie(c, map[string]string{HeaderCSRF: si.CSRF}))
	if rec.Code != http.StatusForbidden {
		t.Errorf("login-codes with cookie: %d, want 403", rec.Code)
	}
	// A wrong cookie is a 401.
	bad := &http.Cookie{Name: SessionCookie, Value: "nope"}
	if rec := do(t, h, "GET", Prefix+"/status", "127.0.0.1:7078", "", withCookie(bad, nil)); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong cookie: %d, want 401", rec.Code)
	}

	// Logout needs the CSRF header, and ends the session.
	if rec := postJSON(t, h, "DELETE", Prefix+"/session", "", withCookie(c, nil)); rec.Code != http.StatusForbidden {
		t.Errorf("logout without CSRF: %d, want 403", rec.Code)
	}
	if rec := postJSON(t, h, "DELETE", Prefix+"/session", "", withCookie(c, map[string]string{HeaderCSRF: "wrong"})); rec.Code != http.StatusForbidden {
		t.Errorf("logout with a wrong CSRF: %d, want 403", rec.Code)
	}
	if rec := postJSON(t, h, "DELETE", Prefix+"/session", "", withCookie(c, map[string]string{HeaderCSRF: si.CSRF})); rec.Code != http.StatusNoContent {
		t.Errorf("logout: %d %s, want 204", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", Prefix+"/status", "127.0.0.1:7078", "", withCookie(c, nil)); rec.Code != http.StatusUnauthorized {
		t.Errorf("status after logout: %d, want 401", rec.Code)
	}

	want := []string{"api_login_code", "api_login", "api_login_refused", "api_logout"}
	if strings.Join(logged, ",") != strings.Join(want, ",") {
		t.Errorf("audit events = %v, want %v", logged, want)
	}
}

func TestLoginRequiresOriginAndJSON(t *testing.T) {
	s, tok := newTestServer(t, &fakeSource{})
	h := s.Handler("7078")
	for name, tc := range map[string]struct {
		hdr  map[string]string
		body string
		want int
	}{
		"no origin":      {hdr: map[string]string{"Origin": ""}, want: http.StatusForbidden},
		"foreign origin": {hdr: map[string]string{"Origin": "http://evil.example"}, want: http.StatusForbidden},
		"other port":     {hdr: map[string]string{"Origin": "http://127.0.0.1:3000"}, want: http.StatusForbidden},
		"cross site":     {hdr: map[string]string{"Sec-Fetch-Site": "cross-site"}, want: http.StatusForbidden},
		"form":           {hdr: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, want: http.StatusUnsupportedMediaType},
		"text":           {hdr: map[string]string{"Content-Type": "text/plain"}, want: http.StatusUnsupportedMediaType},
		"no code":        {body: `{}`, want: http.StatusBadRequest},
		"not json":       {body: `code=x`, want: http.StatusBadRequest},
		"wrong code":     {body: `{"code":"nope"}`, want: http.StatusUnauthorized},
	} {
		lc := mintCode(t, h, tok)
		body := tc.body
		if body == "" {
			body = `{"code":"` + lc.Code + `"}`
		}
		rec := postJSON(t, h, "POST", Prefix+"/session", body, tc.hdr)
		if rec.Code != tc.want {
			t.Errorf("%s: %d %s, want %d", name, rec.Code, rec.Body, tc.want)
		}
		if len(rec.Result().Cookies()) != 0 {
			t.Errorf("%s: set a cookie", name)
		}
	}
}

func TestLoginCodeExpiresAndSessionIdles(t *testing.T) {
	s, tok := newTestServer(t, &fakeSource{})
	now := time.Now()
	s.now = func() time.Time { return now }
	h := s.Handler("7078")

	lc := mintCode(t, h, tok)
	now = now.Add(LoginCodeTTL)
	if rec := postJSON(t, h, "POST", Prefix+"/session", `{"code":"`+lc.Code+`"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("expired code: %d, want 401", rec.Code)
	}

	c, _ := login(t, h, tok)
	now = now.Add(SessionIdleTimeout - time.Minute)
	if rec := do(t, h, "GET", Prefix+"/status", "127.0.0.1:7078", "", withCookie(c, nil)); rec.Code != http.StatusOK {
		t.Errorf("session before idle timeout: %d", rec.Code)
	}
	// The request above renewed it.
	now = now.Add(SessionIdleTimeout - time.Minute)
	if rec := do(t, h, "GET", Prefix+"/status", "127.0.0.1:7078", "", withCookie(c, nil)); rec.Code != http.StatusOK {
		t.Errorf("renewed session: %d", rec.Code)
	}
	now = now.Add(SessionIdleTimeout)
	if rec := do(t, h, "GET", Prefix+"/status", "127.0.0.1:7078", "", withCookie(c, nil)); rec.Code != http.StatusUnauthorized {
		t.Errorf("idle session: %d, want 401", rec.Code)
	}
}

func TestLoginCodeLimit(t *testing.T) {
	s, tok := newTestServer(t, &fakeSource{})
	h := s.Handler("7078")
	for i := 0; i < maxLoginCodes; i++ {
		mintCode(t, h, tok)
	}
	if rec := do(t, h, "POST", Prefix+"/login-codes", "127.0.0.1:7078", tok, nil); rec.Code != http.StatusTooManyRequests {
		t.Errorf("code past the limit: %d, want 429", rec.Code)
	}
}

func TestSessionLimitEvictsLeastRecentlyUsed(t *testing.T) {
	s, tok := newTestServer(t, &fakeSource{})
	now := time.Now()
	s.now = func() time.Time { return now }
	h := s.Handler("7078")
	first, _ := login(t, h, tok)
	for i := 1; i < maxSessions; i++ {
		now = now.Add(time.Second)
		login(t, h, tok)
	}
	if rec := do(t, h, "GET", Prefix+"/status", "127.0.0.1:7078", "", withCookie(first, nil)); rec.Code != http.StatusOK {
		t.Fatalf("first session at the limit: %d", rec.Code)
	}
	// first is now the most recently used; the next login evicts the second.
	now = now.Add(time.Second)
	login(t, h, tok)
	if rec := do(t, h, "GET", Prefix+"/status", "127.0.0.1:7078", "", withCookie(first, nil)); rec.Code != http.StatusOK {
		t.Errorf("recently used session evicted: %d", rec.Code)
	}
	if n := len(s.logins.sessions); n != maxSessions {
		t.Errorf("%d sessions, want %d", n, maxSessions)
	}
}

func TestBearerTakesPrecedenceOverCookie(t *testing.T) {
	s, tok := newTestServer(t, &fakeSource{})
	h := s.Handler("7078")
	c, _ := login(t, h, tok)
	// A wrong bearer token is not rescued by a good cookie.
	if rec := do(t, h, "GET", Prefix+"/status", "127.0.0.1:7078", "wrong", withCookie(c, nil)); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token with cookie: %d, want 401", rec.Code)
	}
	rec := do(t, h, "GET", Prefix+"/session", "127.0.0.1:7078", tok, nil)
	var si SessionInfo
	json.Unmarshal(rec.Body.Bytes(), &si)
	if si.Auth != "bearer" || si.CSRF != "" {
		t.Errorf("bearer session = %+v", si)
	}
}

func TestOriginChecksOnAPI(t *testing.T) {
	s, tok := newTestServer(t, &fakeSource{})
	h := s.Handler("7078")
	c, _ := login(t, h, tok)
	for name, tc := range map[string]struct {
		hdr  map[string]string
		want int
	}{
		"script, no headers":    {want: http.StatusOK},
		"same origin":           {hdr: map[string]string{"Origin": "http://127.0.0.1:7078", "Sec-Fetch-Site": "same-origin"}, want: http.StatusOK},
		"typed url":             {hdr: map[string]string{"Sec-Fetch-Site": "none"}, want: http.StatusOK},
		"foreign origin":        {hdr: map[string]string{"Origin": "http://evil.example"}, want: http.StatusForbidden},
		"localhost other port":  {hdr: map[string]string{"Origin": "http://localhost:7078"}, want: http.StatusForbidden},
		"null origin":           {hdr: map[string]string{"Origin": "null"}, want: http.StatusForbidden},
		"cross site, no origin": {hdr: map[string]string{"Sec-Fetch-Site": "cross-site"}, want: http.StatusForbidden},
		"same site other port":  {hdr: map[string]string{"Sec-Fetch-Site": "same-site"}, want: http.StatusForbidden},
	} {
		for _, cred := range []string{"token", "cookie"} {
			var rec *httptest.ResponseRecorder
			if cred == "token" {
				rec = do(t, h, "GET", Prefix+"/status", "127.0.0.1:7078", tok, tc.hdr)
			} else {
				rec = do(t, h, "GET", Prefix+"/status", "127.0.0.1:7078", "", withCookie(c, tc.hdr))
			}
			if rec.Code != tc.want {
				t.Errorf("%s (%s): %d %s, want %d", name, cred, rec.Code, rec.Body, tc.want)
			}
		}
	}
}

func TestBrowserHardeningHeaders(t *testing.T) {
	s, tok := newTestServer(t, &fakeSource{})
	h := s.Handler("7078")
	for _, tc := range []struct {
		path, token, csp string
	}{
		{"/", "", guiCSP},
		{"/app.js", "", guiCSP},
		{Prefix + "/status", tok, apiCSP},
		{Prefix + "/status", "", apiCSP},
	} {
		rec := do(t, h, "GET", tc.path, "127.0.0.1:7078", tc.token, nil)
		hd := rec.Header()
		if hd.Get("Content-Security-Policy") != tc.csp {
			t.Errorf("%s CSP = %q, want %q", tc.path, hd.Get("Content-Security-Policy"), tc.csp)
		}
		for k, v := range map[string]string{
			"X-Frame-Options": "DENY", "X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer",
			"Cross-Origin-Resource-Policy": "same-origin", "Cross-Origin-Opener-Policy": "same-origin",
		} {
			if hd.Get(k) != v {
				t.Errorf("%s %s = %q, want %q", tc.path, k, hd.Get(k), v)
			}
		}
	}
	for _, d := range []string{"default-src 'self'", "frame-ancestors 'none'", "object-src 'none'"} {
		if !strings.Contains(guiCSP, d) {
			t.Errorf("GUI CSP lacks %q", d)
		}
	}
	if strings.Contains(guiCSP, "unsafe-") {
		t.Errorf("GUI CSP allows unsafe sources: %s", guiCSP)
	}
}
