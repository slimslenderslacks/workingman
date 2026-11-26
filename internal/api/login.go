package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Browser login (§A.6.2). `orch gui` asks the daemon for a one-time login
// code with the bearer token and opens /login#t=<code>. The login page posts
// the code to /api/v1/session, which sets the wm_session cookie: a session
// secret distinct from the token. Browser sessions live in memory, so a
// daemon restart logs every browser out.
const (
	// LoginCodeTTL is how long a login code can be exchanged.
	LoginCodeTTL = 60 * time.Second
	// SessionIdleTimeout ends a browser session that made no request for
	// this long.
	SessionIdleTimeout = 12 * time.Hour
	// SessionCookie is the browser session's cookie.
	SessionCookie = "wm_session"
	// HeaderCSRF carries a browser session's CSRF token on every request
	// that is not GET or HEAD (the double-submit of §A.6.3).
	HeaderCSRF = "X-WM-CSRF"

	// maxLoginCodes and maxSessions bound the in-memory stores. Past
	// maxSessions the least recently used session is dropped.
	maxLoginCodes = 32
	maxSessions   = 64
)

// Scopes (§A.6.2). The API is read-only, so read is the only one yet; write
// and the others arrive with the endpoints that need them (P.4, P.5).
const ScopeRead = "read"

// Error codes of the login flow and the browser checks.
const (
	CodeForbidden   = "forbidden"
	CodeTooMany     = "too_many_requests"
	CodeUnsupported = "unsupported_media_type"
)

// auth is how a request authenticated.
type auth struct {
	// session is the browser session; nil for the bearer token.
	session *browserSession
}

type authKey struct{}

func withAuth(ctx context.Context, a auth) context.Context {
	return context.WithValue(ctx, authKey{}, a)
}

func authFrom(ctx context.Context) auth {
	a, _ := ctx.Value(authKey{}).(auth)
	return a
}

// browserSession is one logged-in browser.
type browserSession struct {
	id       string // shown in the audit log; not a secret
	csrf     string
	scopes   []string
	created  time.Time
	lastUsed time.Time
}

// logins holds the outstanding login codes and the browser sessions, both
// keyed by the SHA-256 of their secret so a lookup does not compare secrets.
type logins struct {
	mu       sync.Mutex
	codes    map[string]time.Time // code hash → expiry
	sessions map[string]*browserSession
}

func newLogins() *logins {
	return &logins{codes: map[string]time.Time{}, sessions: map[string]*browserSession{}}
}

func hashSecret(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func randomSecret(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("api: crypto/rand: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// mint returns a new login code, or "" when maxLoginCodes are outstanding.
func (l *logins) mint(now time.Time) (string, time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for h, exp := range l.codes {
		if !now.Before(exp) {
			delete(l.codes, h)
		}
	}
	if len(l.codes) >= maxLoginCodes {
		return "", time.Time{}
	}
	code := randomSecret(16)
	exp := now.Add(LoginCodeTTL)
	l.codes[hashSecret(code)] = exp
	return code, exp
}

// exchange spends a login code and returns the session secret and the new
// session. A code works once, and only before it expires.
func (l *logins) exchange(code string, now time.Time) (string, *browserSession, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	h := hashSecret(code)
	exp, ok := l.codes[h]
	if !ok {
		return "", nil, false
	}
	delete(l.codes, h)
	if !now.Before(exp) {
		return "", nil, false
	}
	l.pruneSessions(now)
	if len(l.sessions) >= maxSessions {
		l.evictOldest()
	}
	secret := randomSecret(32)
	bs := &browserSession{
		id: "bs_" + hashSecret(randomSecret(8))[:12], csrf: randomSecret(32),
		scopes: []string{ScopeRead}, created: now, lastUsed: now,
	}
	l.sessions[hashSecret(secret)] = bs
	return secret, bs, true
}

// lookup returns the live session for a cookie value and marks it used.
func (l *logins) lookup(secret string, now time.Time) *browserSession {
	if secret == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	h := hashSecret(secret)
	bs, ok := l.sessions[h]
	if !ok {
		return nil
	}
	if now.Sub(bs.lastUsed) >= SessionIdleTimeout {
		delete(l.sessions, h)
		return nil
	}
	bs.lastUsed = now
	return bs
}

// end removes a session.
func (l *logins) end(bs *browserSession) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for h, s := range l.sessions {
		if s == bs {
			delete(l.sessions, h)
		}
	}
}

func (l *logins) pruneSessions(now time.Time) {
	for h, bs := range l.sessions {
		if now.Sub(bs.lastUsed) >= SessionIdleTimeout {
			delete(l.sessions, h)
		}
	}
}

func (l *logins) evictOldest() {
	keys := make([]string, 0, len(l.sessions))
	for h := range l.sessions {
		keys = append(keys, h)
	}
	sort.Slice(keys, func(i, j int) bool { return l.sessions[keys[i]].lastUsed.Before(l.sessions[keys[j]].lastUsed) })
	delete(l.sessions, keys[0])
}

// authenticate checks a request's credentials: the bearer token when an
// Authorization header is present (and then only it), else the session
// cookie.
func (s *Server) authenticate(r *http.Request) (auth, bool) {
	if h := r.Header.Get("Authorization"); h != "" {
		return auth{}, s.tokens.check(h)
	}
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return auth{}, false
	}
	bs := s.logins.lookup(c.Value, s.now())
	if bs == nil {
		return auth{}, false
	}
	return auth{session: bs}, true
}

// checkCSRF is the double-submit check for a browser session's request that
// is not GET or HEAD: X-WM-CSRF must carry the session's CSRF token.
func checkCSRF(r *http.Request, bs *browserSession) bool {
	got := r.Header.Get(HeaderCSRF)
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(bs.csrf)) == 1
}

// sameOrigin is the Origin check of §A.6.3 for /api/ requests: an Origin
// header, when present, must be this server's own origin (no CORS, so no
// other page may call the API), and a browser's Sec-Fetch-Site must say the
// request came from this origin or from the user (a typed URL). Scripts send
// neither header.
func sameOrigin(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" && !strings.EqualFold(o, "http://"+r.Host) {
		return false
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
		return true
	}
	return false
}

// jsonBody reports whether a request body is declared application/json, so a
// cross-site form can never be a "simple request" the server accepts.
func jsonBody(r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mt == "application/json"
}

// LoginCode is the POST /api/v1/login-codes body.
type LoginCode struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
	// LoginURL opens the GUI's login page with the code in the fragment,
	// on the host the request named.
	LoginURL string `json:"login_url"`
}

// handleLoginCodes serves POST /login-codes: a one-time login code for
// `orch gui`. Only the bearer token can mint one, so a browser session
// cannot hand out logins.
func (s *Server) handleLoginCodes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, r.Method+" is not allowed on "+r.URL.Path)
		return
	}
	if authFrom(r.Context()).session != nil {
		writeError(w, http.StatusForbidden, CodeForbidden, "a login code needs the bearer token, not a browser session")
		return
	}
	code, exp := s.logins.mint(s.now())
	if code == "" {
		writeError(w, http.StatusTooManyRequests, CodeTooMany, "too many unused login codes; wait a minute")
		return
	}
	s.log("api_login_code", "expires", exp.UTC().Format(time.RFC3339))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, http.StatusOK, LoginCode{Code: code, ExpiresAt: exp.UTC(), LoginURL: "http://" + r.Host + "/login#t=" + code})
}

// SessionInfo is the body of GET and POST /api/v1/session.
type SessionInfo struct {
	// Auth is "cookie" (a browser session) or "bearer".
	Auth   string   `json:"auth"`
	ID     string   `json:"id,omitempty"`
	Scopes []string `json:"scopes"`
	// CSRF is the value a browser session sends as X-WM-CSRF on every
	// request that is not GET or HEAD. Only same-origin JS can read it.
	CSRF      string     `json:"csrf,omitempty"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
	// IdleTimeout is how long the session survives without a request, in
	// seconds.
	IdleTimeout int `json:"idle_timeout_s,omitempty"`
}

func sessionInfo(bs *browserSession) SessionInfo {
	if bs == nil {
		return SessionInfo{Auth: "bearer", Scopes: []string{ScopeRead}}
	}
	created := bs.created.UTC()
	return SessionInfo{
		Auth: "cookie", ID: bs.id, Scopes: bs.scopes, CSRF: bs.csrf,
		CreatedAt: &created, IdleTimeout: int(SessionIdleTimeout / time.Second),
	}
}

// handleLogin serves POST /session, the one /api/ request that needs no
// credentials: it spends a login code ({"code": ...}) and sets the session
// cookie. The guard has already checked Origin; a browser always sends one
// on a POST, so it is required here.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") == "" {
		writeError(w, http.StatusForbidden, CodeForbidden, "login needs an Origin header")
		return
	}
	if !jsonBody(r) {
		writeError(w, http.StatusUnsupportedMediaType, CodeUnsupported, "Content-Type must be application/json")
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil || strings.TrimSpace(body.Code) == "" {
		writeError(w, http.StatusBadRequest, CodeBadRequest, `the body must be {"code": "<login code>"}`)
		return
	}
	secret, bs, ok := s.logins.exchange(strings.TrimSpace(body.Code), s.now())
	if !ok {
		s.log("api_login_refused", "reason", "bad_code")
		writeError(w, http.StatusUnauthorized, CodeUnauthorized, "the login code is wrong, used or expired; run `orch gui` again")
		return
	}
	s.log("api_login", "session", bs.id, "scopes", strings.Join(bs.scopes, ","))
	// Not Secure: the origin is plain-http loopback.
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: secret, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, http.StatusOK, sessionInfo(bs))
}

// handleBrowserSession serves GET /session (who am I, and the CSRF token) and
// DELETE /session (log out).
func (s *Server) handleBrowserSession(w http.ResponseWriter, r *http.Request) {
	a := authFrom(r.Context())
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, r, http.StatusOK, sessionInfo(a.session))
	case http.MethodDelete:
		if a.session == nil {
			writeError(w, http.StatusBadRequest, CodeBadRequest, "no browser session to end; the bearer token is managed with `orch api token`")
			return
		}
		s.logins.end(a.session)
		s.log("api_logout", "session", a.session.id)
		http.SetCookie(w, &http.Cookie{
			Name: SessionCookie, Value: "", Path: "/", MaxAge: -1,
			HttpOnly: true, SameSite: http.SameSiteStrictMode,
		})
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, HEAD, POST, DELETE")
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, r.Method+" is not allowed on "+r.URL.Path)
	}
}
