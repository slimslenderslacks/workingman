package mcpgateway

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"
)

// ToolCaller is what a consumer of a daemon-owned MCP session needs: one tool
// call. *DaemonSession implements it; consumers depend on the interface so
// tests can fake it.
type ToolCaller interface {
	CallTool(ctx context.Context, name string, args map[string]any) (ToolResult, error)
}

// SessionConn is the slice of *Session a DaemonSession uses — the test seam
// for faking tool calls.
type SessionConn interface {
	CallTool(ctx context.Context, name string, args map[string]any) (ToolResult, error)
	Close(ctx context.Context) error
}

// DaemonSession is one long-lived MCP session over a GatewaySource (usually a
// DaemonGateway), shared by every tool call a host process makes.
//
// Session lifecycle:
//   - Opened lazily on the first call and reused for every call after it.
//   - A call failing with ErrSessionLost drops the session; the next call
//     recreates it (first attempt immediately, then with exponential backoff
//     while attempts keep failing). If the gateway itself is gone — opening a
//     session on its URL is answered ErrSessionLost — the gateway is
//     invalidated so a fresh one is minted, and the open is retried on the
//     new URL.
//   - Close (DELETE) at shutdown, before the gateway is deleted.
//
// Configure it with a struct literal and use it through a pointer; zero
// optional fields take the defaults below. Safe for concurrent use once
// configured.
type DaemonSession struct {
	// Gateway hands out the gateway URL. Required.
	Gateway GatewaySource
	// Name prefixes audit events (<Name>_mcp_*) and log lines
	// ("<Name> MCP: …"). Required.
	Name string
	// ClientName is the MCP clientInfo name of sessions opened by the default
	// Open.
	ClientName string
	// Open opens a session on a gateway URL. Defaults to OpenSession with
	// ClientName.
	Open func(ctx context.Context, url string) (SessionConn, error)
	// Audit records one audit event (e.g. (*audit.Logger).Log). Nil drops them.
	Audit func(event string, kv ...string)
	// Logf writes one log line. Defaults to log.Printf.
	Logf func(format string, args ...any)
	// Clock defaults to time.Now.
	Clock func() time.Time
	// BackoffBase and BackoffMax bound the retry delay after a failed open
	// (defaults 5s and 5m). CallTimeout bounds one tool call, OpenTimeout
	// getting a session (gateway URL + initialize), both 30s by default;
	// CloseTimeout bounds the session DELETE in Close (default 5s).
	BackoffBase  time.Duration
	BackoffMax   time.Duration
	CallTimeout  time.Duration
	OpenTimeout  time.Duration
	CloseTimeout time.Duration

	// openMu serializes session creation so concurrent callers never open two.
	openMu sync.Mutex

	mu       sync.Mutex
	sess     SessionConn
	url      string // gateway URL sess was opened on
	lost     bool   // a session existed and was lost; the next open is a recreation
	failures int    // consecutive failed opens
	lastErr  error
	nextTry  time.Time
	closed   bool
}

const (
	daemonSessionCallTimeout  = 30 * time.Second
	daemonSessionOpenTimeout  = 30 * time.Second
	daemonSessionCloseTimeout = 5 * time.Second
	daemonSessionBackoffBase  = 5 * time.Second
	daemonSessionBackoffMax   = 5 * time.Minute
)

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

func (m *DaemonSession) audit(event string, kv ...string) {
	if m.Audit != nil {
		m.Audit(m.Name+"_mcp_"+event, kv...)
	}
}

func (m *DaemonSession) logf(format string, args ...any) {
	logf := m.Logf
	if logf == nil {
		logf = log.Printf
	}
	logf(m.Name+" MCP: "+format, args...)
}

func (m *DaemonSession) now() time.Time {
	if m.Clock != nil {
		return m.Clock()
	}
	return time.Now()
}

func (m *DaemonSession) open(ctx context.Context, url string) (SessionConn, error) {
	if m.Open != nil {
		return m.Open(ctx, url)
	}
	return OpenSession(ctx, url, SessionOptions{ClientName: m.ClientName})
}

// session returns the live session, opening (or recreating) one when there is
// none and the backoff window allows.
func (m *DaemonSession) session(ctx context.Context) (SessionConn, error) {
	m.mu.Lock()
	if s := m.sess; s != nil {
		m.mu.Unlock()
		return s, nil
	}
	m.mu.Unlock()

	m.openMu.Lock()
	defer m.openMu.Unlock()
	m.mu.Lock()
	if s := m.sess; s != nil { // another caller opened it while we waited
		m.mu.Unlock()
		return s, nil
	}
	if m.closed {
		m.mu.Unlock()
		return nil, fmt.Errorf("%s MCP session: daemon is shutting down", m.Name)
	}
	if wait := m.nextTry.Sub(m.now()); wait > 0 {
		err := fmt.Errorf("%s MCP session unavailable (last error: %v); next retry in %s", m.Name, m.lastErr, wait.Round(time.Second))
		m.mu.Unlock()
		return nil, err
	}
	recreate, attempt := m.lost, m.failures+1
	m.mu.Unlock()

	if recreate {
		m.audit("session_recreate", "attempt", strconv.Itoa(attempt))
		m.logf("recreating lost session (attempt %d)", attempt)
	}
	ctx, cancel := context.WithTimeout(ctx, orDefault(m.OpenTimeout, daemonSessionOpenTimeout))
	defer cancel()
	s, url, err := m.openOnGateway(ctx)
	if err != nil {
		m.fail(ctx, url, err)
		return nil, err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		closeCtx, cancelClose := context.WithTimeout(context.WithoutCancel(ctx), orDefault(m.CloseTimeout, daemonSessionCloseTimeout))
		defer cancelClose()
		_ = s.Close(closeCtx)
		return nil, fmt.Errorf("%s MCP session: daemon is shutting down", m.Name)
	}
	m.sess, m.url = s, url
	m.lost, m.failures, m.lastErr, m.nextTry = false, 0, nil, time.Time{}
	m.mu.Unlock()

	id := connID(s)
	if recreate {
		m.audit("session_recreated", "url", url, "session", id, "attempts", strconv.Itoa(attempt))
		m.logf("session %s recreated on %s after %d attempt(s)", id, url, attempt)
	} else {
		m.audit("session_opened", "url", url, "session", id)
		m.logf("opened session %s on %s", id, url)
	}
	return s, nil
}

// openOnGateway gets the gateway URL and opens a session there. A gateway that
// answers the open with ErrSessionLost is gone (deleted, or standalone-codemode
// restarted): it is invalidated and the open retried once on a fresh gateway.
func (m *DaemonSession) openOnGateway(ctx context.Context) (SessionConn, string, error) {
	var url string
	for try := 0; try < 2; try++ {
		var err error
		url, err = m.Gateway.URL(ctx)
		if err != nil {
			return nil, url, fmt.Errorf("gateway: %w", err)
		}
		s, err := m.open(ctx, url)
		if err == nil {
			return s, url, nil
		}
		if !errors.Is(err, ErrSessionLost) || try == 1 {
			return nil, url, fmt.Errorf("initialize: %w", err)
		}
		m.audit("gateway_gone", "url", url, "err", err.Error())
		m.logf("gateway %s is gone (%v); invalidating it", url, err)
		m.Gateway.Invalidate(ctx, url)
	}
	return nil, url, errors.New("unreachable")
}

// fail records a failed session open and schedules the next attempt.
func (m *DaemonSession) fail(ctx context.Context, url string, err error) {
	if ctx.Err() != nil && errors.Is(context.Cause(ctx), context.Canceled) {
		return // shutdown cut it short
	}
	base, maxDelay := orDefault(m.BackoffBase, daemonSessionBackoffBase), orDefault(m.BackoffMax, daemonSessionBackoffMax)
	m.mu.Lock()
	m.failures++
	n := m.failures
	delay := base << min(n-1, 30)
	if delay <= 0 || delay > maxDelay {
		delay = maxDelay
	}
	m.lastErr = err
	m.nextTry = m.now().Add(delay)
	m.mu.Unlock()
	m.audit("session_error",
		"url", url,
		"attempt", strconv.Itoa(n),
		"retry_in", delay.String(),
		"err", err.Error())
	m.logf("cannot open session on %q (attempt %d, retrying in %s): %v", url, n, delay, err)
}

// dropSession forgets s after it was lost so the next call recreates it.
func (m *DaemonSession) dropSession(s SessionConn, err error) {
	m.mu.Lock()
	if m.sess != s {
		m.mu.Unlock()
		return
	}
	url := m.url
	m.sess, m.url, m.lost = nil, "", true
	m.mu.Unlock()
	m.audit("session_lost", "url", url, "session", connID(s), "err", err.Error())
	m.logf("session %s on %s lost: %v", connID(s), url, err)
}

// CallTool implements ToolCaller: CallToolReporting with no report.
func (m *DaemonSession) CallTool(ctx context.Context, name string, args map[string]any) (ToolResult, error) {
	return m.CallToolReporting(ctx, name, args, nil)
}

// CallToolReporting calls tool with args on the session. A lost session is
// dropped and — when a new one can be had right away — the call retried once
// on it. report, when non-nil, is called with every failed tool call, and with
// the final error when no session could be had (a failed open itself is
// already audited as <Name>_mcp_session_error), so the caller can log it with
// its own context.
func (m *DaemonSession) CallToolReporting(ctx context.Context, tool string, args map[string]any, report func(error)) (ToolResult, error) {
	if report == nil {
		report = func(error) {}
	}
	var res ToolResult
	var err error
	for try := 0; try < 2; try++ {
		var s SessionConn
		s, err = m.session(ctx)
		if err != nil {
			err = fmt.Errorf("mcp session: %w", err)
			break
		}
		cctx, cancel := context.WithTimeout(ctx, orDefault(m.CallTimeout, daemonSessionCallTimeout))
		res, err = s.CallTool(cctx, tool, args)
		cancel()
		if err == nil {
			return res, nil
		}
		report(err)
		if !errors.Is(err, ErrSessionLost) {
			return res, err
		}
		m.dropSession(s, err)
	}
	if err != nil && !errors.Is(err, ErrSessionLost) {
		report(err)
	}
	return res, err
}

// Close ends the session (best-effort DELETE) and stops further opens.
func (m *DaemonSession) Close() {
	m.openMu.Lock()
	defer m.openMu.Unlock()
	m.mu.Lock()
	m.closed = true
	s, url := m.sess, m.url
	m.sess, m.url = nil, ""
	m.mu.Unlock()
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), orDefault(m.CloseTimeout, daemonSessionCloseTimeout))
	defer cancel()
	if err := s.Close(ctx); err != nil {
		m.audit("session_close_error", "url", url, "session", connID(s), "err", err.Error())
		m.logf("could not close session %s on %s: %v", connID(s), url, err)
		return
	}
	m.audit("session_closed", "url", url, "session", connID(s))
	m.logf("closed session %s on %s", connID(s), url)
}

func connID(s SessionConn) string {
	if v, ok := s.(interface{ ID() string }); ok {
		return v.ID()
	}
	return ""
}

// ToolLister is implemented by sessions that can list their tools (*Session
// does). A consumer that resolves tool names from tools/list depends on it.
type ToolLister interface {
	ListTools(ctx context.Context) ([]Tool, error)
}

// ListTools implements ToolLister on the session, opening it if needed. A lost
// session is dropped, as for CallTool, and the list retried once on a new one.
// The session's connection must implement ToolLister (the default Open's
// *Session does).
func (m *DaemonSession) ListTools(ctx context.Context) ([]Tool, error) {
	var err error
	for try := 0; try < 2; try++ {
		var s SessionConn
		s, err = m.session(ctx)
		if err != nil {
			return nil, fmt.Errorf("mcp session: %w", err)
		}
		l, ok := s.(ToolLister)
		if !ok {
			return nil, fmt.Errorf("%s MCP session: tools/list not supported", m.Name)
		}
		cctx, cancel := context.WithTimeout(ctx, orDefault(m.CallTimeout, daemonSessionCallTimeout))
		var tools []Tool
		tools, err = l.ListTools(cctx)
		cancel()
		if err == nil {
			return tools, nil
		}
		if !errors.Is(err, ErrSessionLost) {
			return nil, err
		}
		m.dropSession(s, err)
	}
	return nil, err
}
