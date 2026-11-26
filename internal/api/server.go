package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Version is the API version, the path prefix's last segment.
const Version = "v1"

// Prefix is the path every endpoint lives under.
const Prefix = "/api/" + Version

// Error codes used in the error body.
const (
	CodeNotFound         = "not_found"
	CodeMethodNotAllowed = "method_not_allowed"
	CodeUnauthorized     = "unauthorized"
	CodeMisdirected      = "misdirected"
	CodeBadRequest       = "bad_request"
	CodeInternal         = "internal"
)

// Source is the daemon state the API serves. Every value it returns is
// marshalled to JSON as is, so it must already be redacted.
type Source interface {
	// DaemonInfo is the snapshot's daemon block.
	DaemonInfo() any
	// Snapshot is the snapshot.json document, built now, with up to
	// auditEvents trailing audit events (<= 0: the daemon's default).
	Snapshot(auditEvents int) any
	// Cursor is the position the state just returned is consistent with:
	// the latest audit seq until the event stream has its own.
	Cursor() uint64
	// Resources serves every other read endpoint.
	Resources
}

// Server is the API's HTTP listener. Create it with [New], then [Server.Start]
// and [Server.Close].
type Server struct {
	cfg    Config
	src    Source
	tokens *tokenFile
	// Log records lifecycle events (api_listen, api_listen_error, ...) in the
	// audit log. Its signature is audit.Logger.Log. Nil discards them.
	Log func(event string, kv ...string)
	// now is the clock for generated_at; tests replace it.
	now func() time.Time

	mu       sync.Mutex
	ln       net.Listener
	srv      *http.Server
	addr     string // the bound socket path or host:port
	port     string // TCP port for the Host check; "" on a unix socket
	infoFile string // api.json written for a port-0 listener
	done     chan struct{}
}

// New returns a server for cfg over src. It does not listen yet.
func New(cfg Config, src Source) *Server {
	return &Server{cfg: cfg, src: src, tokens: &tokenFile{path: cfg.TokenPath}, now: time.Now}
}

func (s *Server) log(event string, kv ...string) {
	if s.Log != nil {
		s.Log(event, kv...)
	}
}

// Addr is the bound socket path or host:port ("" before Start).
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Start makes sure the token file exists, binds the listener and serves in
// the background. An error means the API is not running; the caller (the
// daemon) logs it as api_listen_error and carries on. Start also logs that
// event itself, so the daemon need not.
func (s *Server) Start() error {
	if err := s.start(); err != nil {
		s.log("api_listen_error", "mode", string(s.cfg.Mode), "listen", s.cfg.Listen, "err", err.Error())
		return err
	}
	s.log("api_listen", "mode", string(s.cfg.Mode), "addr", s.Addr(), "token", s.cfg.TokenPath)
	return nil
}

func (s *Server) start() error {
	if !s.cfg.Enabled() {
		return fmt.Errorf("api: not enabled (mode %q)", s.cfg.Mode)
	}
	if s.cfg.TokenPath == "" {
		return errors.New("api: no token path configured")
	}
	if _, _, err := EnsureToken(s.cfg.TokenPath, false); err != nil {
		return err
	}
	var (
		ln   net.Listener
		port string
		err  error
	)
	switch s.cfg.Mode {
	case ModeUnix:
		ln, err = listenUnix(s.cfg.Listen)
	case ModeLoopback:
		if err = checkLoopback(s.cfg.Listen); err != nil {
			return err
		}
		ln, err = net.Listen("tcp", s.cfg.Listen)
		if err == nil {
			_, port, _ = net.SplitHostPort(ln.Addr().String())
		}
	}
	if err != nil {
		return fmt.Errorf("api: listen %s: %w", s.cfg.Listen, err)
	}
	addr := ln.Addr().String()
	if s.cfg.Mode == ModeUnix {
		addr = s.cfg.Listen
	}
	infoFile := ""
	if s.cfg.Mode == ModeLoopback && s.cfg.InfoPath != "" && requestedPortZero(s.cfg.Listen) {
		if err := writeInfo(s.cfg.InfoPath, addr); err != nil {
			ln.Close()
			return err
		}
		infoFile = s.cfg.InfoPath
	}
	srv := &http.Server{
		Handler:           s.Handler(port),
		ReadHeaderTimeout: 10 * time.Second,
	}
	done := make(chan struct{})
	s.mu.Lock()
	s.ln, s.srv, s.addr, s.port, s.infoFile, s.done = ln, srv, addr, port, infoFile, done
	s.mu.Unlock()
	go func() {
		defer close(done)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log("api_serve_error", "addr", addr, "err", err.Error())
		}
	}()
	return nil
}

// Close stops the listener, waits up to the context's deadline for requests
// in flight, and removes the socket file and api.json it created.
func (s *Server) Close(ctx context.Context) error {
	s.mu.Lock()
	srv, done, addr, infoFile := s.srv, s.done, s.addr, s.infoFile
	s.srv = nil
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	err := srv.Shutdown(ctx)
	if err != nil {
		srv.Close()
	}
	<-done
	if s.cfg.Mode == ModeUnix {
		os.Remove(addr)
	}
	if infoFile != "" {
		os.Remove(infoFile)
	}
	s.log("api_stop", "addr", addr)
	return err
}

// listenUnix binds a unix socket at path with mode 0600. The socket is
// bound in a private temporary directory beside path, chmodded and then
// renamed into place, so it is never reachable with looser permissions. A
// stale socket left by a crashed daemon is replaced; a live one (another
// daemon answers on it) or a non-socket file at path is an error.
func listenUnix(path string) (net.Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&fs.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("%s is in use by another process (is another orch daemon running?)", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
	}
	tmpDir, err := os.MkdirTemp(dir, ".api-sock-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)
	// The temp name is short on purpose: sun_path is limited to ~104 bytes.
	tmp := filepath.Join(tmpDir, "s")
	ln, err := net.Listen("unix", tmp)
	if err != nil {
		return nil, err
	}
	ul := ln.(*net.UnixListener)
	// Close would unlink the temp name, which is gone after the rename;
	// Server.Close removes the real path instead.
	ul.SetUnlinkOnClose(false)
	if err := os.Chmod(tmp, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

func requestedPortZero(addr string) bool {
	_, port, err := net.SplitHostPort(addr)
	return err == nil && port == "0"
}

// writeInfo records a random-port listener's address for clients.
func writeInfo(path, addr string) error {
	body, _ := json.Marshal(map[string]string{"addr": addr, "url": "http://" + addr})
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("api: %w", err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("api: write %s: %w", path, err)
	}
	return nil
}

// Handler returns the API's HTTP handler. port is the TCP port the listener
// is bound to, which an allowed Host must name; "" for a unix socket, where
// any port (or none) is accepted.
func (s *Server) Handler(port string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(Prefix+"/status", s.get(s.handleStatus))
	mux.HandleFunc(Prefix+"/snapshot", s.get(s.handleSnapshot))
	s.routes(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, CodeNotFound, "no such endpoint: "+r.URL.Path)
	})
	return s.guard(port, mux)
}

// guard applies the checks every request goes through, in order: the Host
// allowlist (421), then the bearer token (401). It also sets the headers
// every response carries.
func (s *Server) guard(port string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Cache-Control", "no-cache")
		if !allowedHost(r.Host, port) {
			writeError(w, http.StatusMisdirectedRequest, CodeMisdirected, "host "+strconv.Quote(r.Host)+" is not allowed; use localhost, 127.0.0.1 or [::1]")
			return
		}
		if !s.tokens.check(r.Header.Get("Authorization")) {
			h.Set("WWW-Authenticate", `Bearer realm="workingman"`)
			writeError(w, http.StatusUnauthorized, CodeUnauthorized, "missing or wrong bearer token; see `orch api token`")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// allowedHost is the DNS-rebinding guard: the Host header must name the
// loopback interface. On TCP it must also carry the listener's port. Any
// other name, host.docker.internal included, is refused.
func allowedHost(host, port string) bool {
	name, p, err := net.SplitHostPort(host)
	if err != nil {
		// No port in the header.
		name, p = host, ""
		if strings.HasPrefix(name, "[") && strings.HasSuffix(name, "]") {
			name = name[1 : len(name)-1]
		}
	}
	switch strings.ToLower(name) {
	case "localhost", "127.0.0.1", "::1":
	default:
		return false
	}
	return port == "" || p == port
}

// get restricts a handler to GET and HEAD.
func (s *Server) get(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, r.Method+" is not allowed on "+r.URL.Path)
			return
		}
		h(w, r)
	}
}

// Status is the GET /api/v1/status body.
type Status struct {
	// Version is the orch build that serves the API.
	Version     string    `json:"version"`
	GeneratedAt time.Time `json:"generated_at"`
	Daemon      any       `json:"daemon"`
	API         APIInfo   `json:"api"`
	Cursor      uint64    `json:"cursor"`
}

// APIInfo describes the listener in [Status].
type APIInfo struct {
	Version string `json:"version"`
	// Binding is "unix" or "loopback".
	Binding string `json:"binding"`
	// Listen is the socket path or host:port.
	Listen string `json:"listen"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, Status{
		Version:     buildVersion(),
		GeneratedAt: s.now().UTC(),
		Daemon:      s.src.DaemonInfo(),
		API:         APIInfo{Version: Version, Binding: string(s.cfg.Mode), Listen: s.Addr()},
		Cursor:      s.src.Cursor(),
	})
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	n := 0
	if v := r.URL.Query().Get("audit_events"); v != "" {
		var err error
		n, err = strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, CodeBadRequest, "audit_events must be a non-negative integer")
			return
		}
		n = min(n, MaxAuditLimit)
	}
	writeJSON(w, r, http.StatusOK, s.src.Snapshot(n))
}

// List is the envelope every list endpoint returns. Cursor is the
// event-stream position the list is consistent with, so a client can fetch a
// list and then follow events from cursor without a gap.
type List[T any] struct {
	Items       []T       `json:"items"`
	GeneratedAt time.Time `json:"generated_at"`
	Cursor      uint64    `json:"cursor"`
}

// NewList builds a list envelope; a nil items becomes [] so clients never
// see null.
func NewList[T any](items []T, generatedAt time.Time, cursor uint64) List[T] {
	if items == nil {
		items = []T{}
	}
	return List[T]{Items: items, GeneratedAt: generatedAt.UTC(), Cursor: cursor}
}

// ErrorBody is the shape of every error response.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail is the inside of [ErrorBody].
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	body, _ := json.Marshal(ErrorBody{Error: ErrorDetail{Code: code, Message: msg}})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(append(body, '\n'))
}

// writeJSON writes v as JSON with an ETag. A request whose If-None-Match
// names that ETag gets 304 and no body.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, "encode response: "+err.Error())
		return
	}
	body = append(body, '\n')
	etag := ETag(body)
	h := w.Header()
	h.Set("ETag", etag)
	if status == http.StatusOK && etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		w.Write(body)
	}
}

// ETag is the strong entity tag of a JSON body. A top-level generated_at is
// left out of the hash: it changes on every request without the content
// changing, and would make every revalidation miss.
func ETag(body []byte) string {
	hashed := body
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) == nil {
		if _, ok := top["generated_at"]; ok {
			delete(top, "generated_at")
			// Marshal sorts the keys, so the hash is deterministic.
			if b, err := json.Marshal(top); err == nil {
				hashed = b
			}
		}
	}
	sum := sha256.Sum256(hashed)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

// etagMatches implements If-None-Match's weak comparison: "*" or any listed
// tag equal to etag, ignoring a W/ prefix.
func etagMatches(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	if header == "*" {
		return true
	}
	for _, t := range strings.Split(header, ",") {
		t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
		if t == etag {
			return true
		}
	}
	return false
}

// buildVersion is the orch module version from the build info ("(devel)" for
// a plain `go build`), or "unknown".
func buildVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "unknown"
}
