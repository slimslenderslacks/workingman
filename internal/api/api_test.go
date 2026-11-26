package api

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeSource is a Source with fixed content.
type fakeSource struct {
	info   any
	snap   func(n int) any
	cursor uint64
}

func (f *fakeSource) DaemonInfo() any { return f.info }
func (f *fakeSource) Snapshot(n int) any {
	if f.snap != nil {
		return f.snap(n)
	}
	return map[string]any{"version": 1, "n": n}
}
func (f *fakeSource) Cursor() uint64 { return f.cursor }

// shortTempDir is a temp dir under /tmp: a unix socket path must fit in
// sun_path (~104 bytes), which macOS's $TMPDIR can exceed.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "wmapi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestParseConfig(t *testing.T) {
	dir := "/home/u/.workingman"
	for _, tc := range []struct {
		mode, listen string
		want         Config
		wantErr      string
	}{
		{mode: "", want: Config{Mode: ModeOff}},
		{mode: "off", want: Config{Mode: ModeOff}},
		{mode: "off", listen: "127.0.0.1:1", wantErr: "needs --api=unix"},
		{mode: "unix", want: Config{Mode: ModeUnix, Listen: dir + "/api.sock", TokenPath: dir + "/api/token"}},
		{mode: "unix", listen: "/x/y.sock", want: Config{Mode: ModeUnix, Listen: "/x/y.sock", TokenPath: dir + "/api/token"}},
		{mode: "loopback", want: Config{Mode: ModeLoopback, Listen: "127.0.0.1:7078", TokenPath: dir + "/api/token", InfoPath: dir + "/api.json"}},
		{mode: "loopback", listen: "[::1]:9000", want: Config{Mode: ModeLoopback, Listen: "[::1]:9000", TokenPath: dir + "/api/token", InfoPath: dir + "/api.json"}},
		{mode: "loopback", listen: "localhost:0", want: Config{Mode: ModeLoopback, Listen: "localhost:0", TokenPath: dir + "/api/token", InfoPath: dir + "/api.json"}},
		{mode: "loopback", listen: "0.0.0.0:7078", wantErr: "only listens on loopback"},
		{mode: "loopback", listen: ":7078", wantErr: "only listens on loopback"},
		{mode: "loopback", listen: "192.168.1.5:7078", wantErr: "only listens on loopback"},
		{mode: "loopback", listen: "127.0.0.1", wantErr: "missing port"},
		{mode: "tcp", wantErr: "unknown --api value"},
	} {
		got, err := ParseConfig(tc.mode, tc.listen, dir)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("ParseConfig(%q, %q) err = %v, want %q", tc.mode, tc.listen, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseConfig(%q, %q): %v", tc.mode, tc.listen, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseConfig(%q, %q) = %+v, want %+v", tc.mode, tc.listen, got, tc.want)
		}
	}
}

func TestEnsureTokenCreatesReusesAndRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wm", "api", "token")
	tok, created, err := EnsureToken(path, false)
	if err != nil || !created {
		t.Fatalf("first EnsureToken: tok=%q created=%v err=%v", tok, created, err)
	}
	if len(tok) != 43 || strings.ContainsAny(tok, "+/=") {
		t.Errorf("token %q is not 32 bytes of unpadded base64url", tok)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("token mode = %v, want 0600", fi.Mode().Perm())
	}
	di, _ := os.Stat(filepath.Dir(path))
	if di.Mode().Perm() != 0o700 {
		t.Errorf("token dir mode = %v, want 0700", di.Mode().Perm())
	}
	again, created, err := EnsureToken(path, false)
	if err != nil || created || again != tok {
		t.Errorf("second EnsureToken = %q, %v, %v; want the same token, not created", again, created, err)
	}
	rotated, created, err := EnsureToken(path, true)
	if err != nil || !created || rotated == tok {
		t.Errorf("rotate = %q, %v, %v; want a new token", rotated, created, err)
	}
	if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := EnsureToken(path, false); err == nil {
		t.Error("EnsureToken accepted an empty token file")
	}
}

func newTestServer(t *testing.T, src Source) (*Server, string) {
	t.Helper()
	tokPath := filepath.Join(t.TempDir(), "api", "token")
	tok, _, err := EnsureToken(tokPath, false)
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{Mode: ModeLoopback, Listen: "127.0.0.1:0", TokenPath: tokPath}, src)
	return s, tok
}

func do(t *testing.T, h http.Handler, method, target, host, token string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.Host = host
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body %q: %v", rec.Body.String(), err)
	}
	return body.Error.Code
}

func TestHostAllowlist(t *testing.T) {
	s, tok := newTestServer(t, &fakeSource{})
	h := s.Handler("7078")
	for _, host := range []string{"127.0.0.1:7078", "localhost:7078", "LOCALHOST:7078", "[::1]:7078"} {
		if rec := do(t, h, "GET", Prefix+"/status", host, tok, nil); rec.Code != http.StatusOK {
			t.Errorf("Host %q: %d %s, want 200", host, rec.Code, rec.Body)
		}
	}
	for _, host := range []string{
		"host.docker.internal:7078", "evil.example:7078", "127.0.0.1:7079", "127.0.0.1",
		"127.0.0.2:7078", "0.0.0.0:7078", "", "localhost.evil.example:7078",
	} {
		rec := do(t, h, "GET", Prefix+"/status", host, tok, nil)
		if rec.Code != http.StatusMisdirectedRequest || errorCode(t, rec) != CodeMisdirected {
			t.Errorf("Host %q: %d %s, want 421 misdirected", host, rec.Code, rec.Body)
		}
	}
	// On a unix socket there is no port to match, but the name still is.
	uh := s.Handler("")
	for _, host := range []string{"localhost", "localhost:80", "127.0.0.1", "[::1]"} {
		if rec := do(t, uh, "GET", Prefix+"/status", host, tok, nil); rec.Code != http.StatusOK {
			t.Errorf("unix Host %q: %d, want 200", host, rec.Code)
		}
	}
	if rec := do(t, uh, "GET", Prefix+"/status", "host.docker.internal", tok, nil); rec.Code != http.StatusMisdirectedRequest {
		t.Errorf("unix Host host.docker.internal: %d, want 421", rec.Code)
	}
}

func TestBearerTokenRequired(t *testing.T) {
	s, tok := newTestServer(t, &fakeSource{})
	h := s.Handler("")
	for name, auth := range map[string]string{
		"none":   "",
		"wrong":  "Bearer nope",
		"scheme": "Basic " + tok,
		"bare":   tok,
	} {
		rec := do(t, h, "GET", Prefix+"/status", "localhost", "", map[string]string{"Authorization": auth})
		if rec.Code != http.StatusUnauthorized || errorCode(t, rec) != CodeUnauthorized {
			t.Errorf("%s: %d %s, want 401", name, rec.Code, rec.Body)
		}
		if rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s: no WWW-Authenticate header", name)
		}
	}
	// Unknown paths are behind the token too, so they reveal nothing.
	if rec := do(t, h, "GET", "/api/v1/nope", "localhost", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("unknown path without token: %d, want 401", rec.Code)
	}
	if rec := do(t, h, "GET", "/api/v1/nope", "localhost", tok, nil); rec.Code != http.StatusNotFound || errorCode(t, rec) != CodeNotFound {
		t.Errorf("unknown path: %d, want 404 not_found", rec.Code)
	}
}

func TestRotatedTokenTakesEffectWithoutRestart(t *testing.T) {
	s, old := newTestServer(t, &fakeSource{})
	h := s.Handler("")
	if rec := do(t, h, "GET", Prefix+"/status", "localhost", old, nil); rec.Code != http.StatusOK {
		t.Fatalf("old token: %d", rec.Code)
	}
	// Make sure the mtime moves even on a coarse-grained filesystem.
	time.Sleep(10 * time.Millisecond)
	fresh, _, err := EnsureToken(s.cfg.TokenPath, true)
	if err != nil {
		t.Fatal(err)
	}
	if rec := do(t, h, "GET", Prefix+"/status", "localhost", old, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("old token after rotate: %d, want 401", rec.Code)
	}
	if rec := do(t, h, "GET", Prefix+"/status", "localhost", fresh, nil); rec.Code != http.StatusOK {
		t.Errorf("new token: %d, want 200", rec.Code)
	}
	// A missing token file fails closed.
	os.Remove(s.cfg.TokenPath)
	if rec := do(t, h, "GET", Prefix+"/status", "localhost", fresh, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("token file gone: %d, want 401", rec.Code)
	}
}

func TestOnlyGETAndHEAD(t *testing.T) {
	s, tok := newTestServer(t, &fakeSource{})
	h := s.Handler("")
	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		rec := do(t, h, m, Prefix+"/snapshot", "localhost", tok, nil)
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("%s: %d allow=%q, want 405 GET, HEAD", m, rec.Code, rec.Header().Get("Allow"))
		}
	}
	rec := do(t, h, "HEAD", Prefix+"/snapshot", "localhost", tok, nil)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get("ETag") == "" {
		t.Errorf("HEAD: %d body=%d etag=%q", rec.Code, rec.Body.Len(), rec.Header().Get("ETag"))
	}
}

func TestStatusBody(t *testing.T) {
	src := &fakeSource{info: map[string]any{"state": "running", "pid": 42}, cursor: 17}
	s, tok := newTestServer(t, src)
	s.addr = "127.0.0.1:7078"
	rec := do(t, s.Handler(""), "GET", Prefix+"/status", "localhost", tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("missing security headers: %v", rec.Header())
	}
	var got struct {
		Version     string         `json:"version"`
		GeneratedAt time.Time      `json:"generated_at"`
		Daemon      map[string]any `json:"daemon"`
		API         APIInfo        `json:"api"`
		Cursor      uint64         `json:"cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Version == "" || got.GeneratedAt.IsZero() || got.Daemon["state"] != "running" || got.Cursor != 17 {
		t.Errorf("status = %+v", got)
	}
	if got.API != (APIInfo{Version: "v1", Binding: "loopback", Listen: "127.0.0.1:7078"}) {
		t.Errorf("api = %+v", got.API)
	}
}

func TestSnapshotAuditEventsParam(t *testing.T) {
	var asked []int
	src := &fakeSource{snap: func(n int) any { asked = append(asked, n); return map[string]any{"version": 1} }}
	s, tok := newTestServer(t, src)
	h := s.Handler("")
	for _, q := range []string{"", "?audit_events=5", "?audit_events=100000"} {
		if rec := do(t, h, "GET", Prefix+"/snapshot"+q, "localhost", tok, nil); rec.Code != http.StatusOK {
			t.Errorf("%q: %d", q, rec.Code)
		}
	}
	if want := []int{0, 5, maxAuditEvents}; len(asked) != 3 || asked[0] != want[0] || asked[1] != want[1] || asked[2] != want[2] {
		t.Errorf("Snapshot asked for %v, want %v", asked, want)
	}
	for _, q := range []string{"?audit_events=-1", "?audit_events=x"} {
		rec := do(t, h, "GET", Prefix+"/snapshot"+q, "localhost", tok, nil)
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != CodeBadRequest {
			t.Errorf("%q: %d, want 400", q, rec.Code)
		}
	}
}

func TestETagAndIfNoneMatch(t *testing.T) {
	n := 0
	src := &fakeSource{snap: func(int) any {
		// generated_at moves on every build; the etag must not.
		return map[string]any{"generated_at": time.Now().Add(time.Duration(n) * time.Second), "version": 1, "state": n / 2}
	}}
	s, tok := newTestServer(t, src)
	h := s.Handler("")
	get := func(hdr map[string]string) *httptest.ResponseRecorder {
		rec := do(t, h, "GET", Prefix+"/snapshot", "localhost", tok, hdr)
		n++
		return rec
	}
	first := get(nil)
	etag := first.Header().Get("ETag")
	if first.Code != http.StatusOK || !strings.HasPrefix(etag, `"`) {
		t.Fatalf("first: %d etag=%q", first.Code, etag)
	}
	rec := get(map[string]string{"If-None-Match": etag})
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 || rec.Header().Get("ETag") != etag {
		t.Errorf("unchanged: %d body=%q etag=%q, want 304 with the same etag", rec.Code, rec.Body, rec.Header().Get("ETag"))
	}
	// n is now 2, so the content changed and the old etag no longer matches.
	rec = get(map[string]string{"If-None-Match": `"other", ` + etag})
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") == etag {
		t.Errorf("changed content with stale If-None-Match: %d etag=%q, want 200 and a new etag", rec.Code, rec.Header().Get("ETag"))
	}
	// A weak validator matches too (If-None-Match uses weak comparison).
	if rec2 := get(map[string]string{"If-None-Match": "W/" + rec.Header().Get("ETag")}); rec2.Code != http.StatusNotModified {
		t.Errorf("weak If-None-Match: %d, want 304", rec2.Code)
	}
	if rec := get(map[string]string{"If-None-Match": "*"}); rec.Code != http.StatusNotModified {
		t.Errorf("If-None-Match *: %d, want 304", rec.Code)
	}
}

func TestListEnvelope(t *testing.T) {
	at := time.Date(2026, 11, 25, 9, 31, 2, 0, time.FixedZone("x", 3600))
	body, err := json.Marshal(NewList[string](nil, at, 18234))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(body), `{"items":[],"generated_at":"2026-11-25T08:31:02Z","cursor":18234}`; got != want {
		t.Errorf("list = %s, want %s", got, want)
	}
}

func get(t *testing.T, c *http.Client, url, token string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func unixClient(path string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}}
}

func TestUnixSocketLifecycle(t *testing.T) {
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "wm", "api.sock")
	var events []string
	cfg := Config{Mode: ModeUnix, Listen: sock, TokenPath: filepath.Join(dir, "wm", "api", "token")}
	s := New(cfg, &fakeSource{info: map[string]any{"state": "running"}})
	s.Log = func(ev string, kv ...string) { events = append(events, ev) }
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %v, want a 0600 socket", fi.Mode())
	}
	if di, _ := os.Stat(filepath.Dir(sock)); di.Mode().Perm() != 0o700 {
		t.Errorf("socket dir mode = %v, want 0700", di.Mode().Perm())
	}
	tok, err := ReadToken(cfg.TokenPath)
	if err != nil {
		t.Fatalf("Start did not create the token: %v", err)
	}
	c := unixClient(sock)
	if code, body := get(t, c, "http://localhost"+Prefix+"/status", tok); code != http.StatusOK {
		t.Errorf("status over the socket: %d %s", code, body)
	}
	if code, _ := get(t, c, "http://localhost"+Prefix+"/status", ""); code != http.StatusUnauthorized {
		t.Errorf("no token over the socket: %d, want 401", code)
	}

	// A second server on the same socket refuses to steal it.
	other := New(cfg, &fakeSource{})
	if err := other.Start(); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Errorf("second Start = %v, want an in-use error", err)
	}

	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(sock); !os.IsNotExist(err) {
		t.Errorf("socket still there after Close: %v", err)
	}
	if strings.Join(events, ",") != "api_listen,api_stop" {
		t.Errorf("events = %v", events)
	}
	// Close twice is harmless.
	if err := s.Close(context.Background()); err != nil {
		t.Error(err)
	}
}

func TestUnixSocketReplacesStaleSocket(t *testing.T) {
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "api.sock")
	// A socket file with nobody listening, as a crashed daemon leaves.
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	s := New(Config{Mode: ModeUnix, Listen: sock, TokenPath: filepath.Join(dir, "token")}, &fakeSource{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start over a stale socket: %v", err)
	}
	s.Close(context.Background())
}

func TestListenErrorIsLoggedAndReturned(t *testing.T) {
	dir := shortTempDir(t)
	notSock := filepath.Join(dir, "api.sock")
	if err := os.WriteFile(notSock, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logged []string
	s := New(Config{Mode: ModeUnix, Listen: notSock, TokenPath: filepath.Join(dir, "token")}, &fakeSource{})
	s.Log = func(ev string, kv ...string) { logged = append(logged, ev+" "+strings.Join(kv, " ")) }
	if err := s.Start(); err == nil {
		t.Fatal("Start over a regular file succeeded")
	}
	if len(logged) != 1 || !strings.HasPrefix(logged[0], "api_listen_error ") || !strings.Contains(logged[0], "not a socket") {
		t.Errorf("logged = %v", logged)
	}
	if b, _ := os.ReadFile(notSock); string(b) != "x" {
		t.Error("Start clobbered a file that is not a socket")
	}
	if s.Addr() != "" {
		t.Errorf("Addr = %q after a failed Start", s.Addr())
	}
	if err := s.Close(context.Background()); err != nil {
		t.Error(err)
	}
}

func TestLoopbackRandomPortWritesInfo(t *testing.T) {
	dir := t.TempDir()
	info := filepath.Join(dir, "api.json")
	s := New(Config{Mode: ModeLoopback, Listen: "127.0.0.1:0", TokenPath: filepath.Join(dir, "api", "token"), InfoPath: info}, &fakeSource{})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	var rec struct{ Addr, URL string }
	b, err := os.ReadFile(info)
	if err != nil || json.Unmarshal(b, &rec) != nil || rec.Addr != s.Addr() || rec.URL != "http://"+s.Addr() {
		t.Fatalf("api.json = %s (%v), addr %s", b, err, s.Addr())
	}
	tok, _ := ReadToken(filepath.Join(dir, "api", "token"))
	if code, body := get(t, http.DefaultClient, "http://"+s.Addr()+Prefix+"/status", tok); code != http.StatusOK {
		t.Errorf("status: %d %s", code, body)
	}
	// The port is part of the allowlisted Host: a rebinding name is refused
	// even when it reaches the right socket.
	req, _ := http.NewRequest("GET", "http://"+s.Addr()+Prefix+"/status", nil)
	req.Host = "evil.example"
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("rebinding Host: %d, want 421", resp.StatusCode)
	}
	s.Close(context.Background())
	if _, err := os.Stat(info); !os.IsNotExist(err) {
		t.Errorf("api.json left behind: %v", err)
	}
}

func TestStartRefusesNonLoopback(t *testing.T) {
	dir := t.TempDir()
	s := New(Config{Mode: ModeLoopback, Listen: "0.0.0.0:0", TokenPath: filepath.Join(dir, "token")}, &fakeSource{})
	if err := s.Start(); err == nil {
		s.Close(context.Background())
		t.Fatal("Start bound a non-loopback address")
	}
}
