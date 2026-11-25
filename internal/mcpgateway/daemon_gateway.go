package mcpgateway

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DaemonGateway is a long-lived static sub-gateway owned by a host process
// (the orch daemon) rather than a sandbox: minted over a fixed set of catalog
// servers on the same /gateways API acp-wrapper uses, with its record kept in
// a Store under a fixed Key, so a gateway a crashed run left behind is
// DELETEd before the next one is minted.
//
// Lifecycle:
//   - Start runs the create/retry loop, which deletes a stale recorded gateway
//     and creates the new one off the caller's startup path. A failure is
//     logged and retried with exponential backoff; it never stops the caller.
//   - URL hands the gateway to callers, trying a creation on demand once the
//     backoff window has passed. Invalidate drops a gateway a caller found
//     gone (e.g. standalone-codemode restarted) so a fresh one is minted.
//   - Close DELETEs the gateway and removes its record.
//
// Configure it with a struct literal and use it through a pointer; zero
// optional fields take the defaults below. Safe for concurrent use once
// configured.
type DaemonGateway struct {
	// Key is the Store key of the gateway record (Record.Sandbox). Required.
	Key string
	// Servers are the only catalog servers the gateway is minted over.
	Servers []string
	// BaseURL is the standalone-codemode root, e.g. http://localhost:3333.
	BaseURL string
	// Store keeps the gateway record. Use a directory of its own, not the
	// per-sandbox gateways directory: acp-wrapper's Sweep releases every
	// record there whose name is not a live sandbox. Store.Dir == "" keeps no
	// record (and so no crash cleanup).
	Store Store
	// Name prefixes audit events (<Name>_gateway_*) and log lines
	// ("<Name> gateway: …"). Defaults to Key.
	Name string
	// Audit records one audit event (e.g. (*audit.Logger).Log). Nil drops them.
	Audit func(event string, kv ...string)
	// Logf writes one log line. Defaults to log.Printf.
	Logf func(format string, args ...any)
	// Clock defaults to time.Now.
	Clock func() time.Time
	// ClientFor returns the API client for a standalone-codemode root (a
	// stale record may name a different root than BaseURL). Defaults to
	// Client{BaseURL: base}.
	ClientFor func(baseURL string) GatewayAPI
	// BackoffBase and BackoffMax bound the retry delay after a failed
	// creation (defaults 5s and 5m); CloseTimeout bounds the DELETE in Close
	// (default 10s).
	BackoffBase  time.Duration
	BackoffMax   time.Duration
	CloseTimeout time.Duration

	initOnce sync.Once

	// attemptMu serializes create/delete round trips so the background loop
	// and an on-demand URL never mint two gateways.
	attemptMu sync.Mutex

	mu       sync.Mutex
	rec      Record // the live gateway; ID == "" when none
	lastErr  error
	failures int
	nextTry  time.Time
	closed   bool
	kick     chan struct{}
	done     chan struct{} // closed when run returns; nil until Start
}

const (
	daemonGatewayBackoffBase  = 5 * time.Second
	daemonGatewayBackoffMax   = 5 * time.Minute
	daemonGatewayCloseTimeout = 10 * time.Second
)

// GatewaySource is what a DaemonSession needs of a gateway; *DaemonGateway
// implements it and tests fake it.
type GatewaySource interface {
	// URL returns the gateway's MCP endpoint, creating the gateway on demand
	// if none is live and the retry backoff has elapsed.
	URL(ctx context.Context) (string, error)
	// Invalidate drops the gateway at url (one a caller found gone or broken)
	// so the next URL mints a new one. A url that is no longer current is
	// ignored.
	Invalidate(ctx context.Context, url string)
}

// GatewayAPI is the slice of Client a DaemonGateway uses — the test seam for
// faking creation and deletion.
type GatewayAPI interface {
	Create(ctx context.Context, servers []string, dynamic bool) (Gateway, error)
	Delete(ctx context.Context, id string) error
}

func (g *DaemonGateway) init() {
	g.initOnce.Do(func() {
		g.kick = make(chan struct{}, 1)
	})
}

func (g *DaemonGateway) name() string {
	if g.Name != "" {
		return g.Name
	}
	return g.Key
}

func (g *DaemonGateway) audit(event string, kv ...string) {
	if g.Audit != nil {
		g.Audit(g.name()+"_gateway_"+event, kv...)
	}
}

func (g *DaemonGateway) logf(format string, args ...any) {
	logf := g.Logf
	if logf == nil {
		logf = log.Printf
	}
	logf(g.name()+" gateway: "+format, args...)
}

func (g *DaemonGateway) now() time.Time {
	if g.Clock != nil {
		return g.Clock()
	}
	return time.Now()
}

func (g *DaemonGateway) client(base string) GatewayAPI {
	if g.ClientFor != nil {
		return g.ClientFor(base)
	}
	return Client{BaseURL: base}
}

func (g *DaemonGateway) servers() string { return strings.Join(g.Servers, ",") }

// Start launches the create/retry loop until ctx is done. Call it at most
// once, and call Close after ctx is done.
func (g *DaemonGateway) Start(ctx context.Context) {
	g.init()
	g.mu.Lock()
	g.done = make(chan struct{})
	g.mu.Unlock()
	go g.run(ctx)
}

// run keeps a gateway minted until ctx is done: it attempts creation, then
// sleeps until the backoff elapses (after a failure) or until Invalidate kicks
// it (after a success).
func (g *DaemonGateway) run(ctx context.Context) {
	defer close(g.done)
	for {
		if ctx.Err() != nil {
			return
		}
		g.attempt(ctx)
		g.mu.Lock()
		live, wait := g.rec.ID != "", g.nextTry.Sub(g.now())
		g.mu.Unlock()
		var timer *time.Timer
		var due <-chan time.Time
		if !live {
			if wait <= 0 {
				continue
			}
			timer = time.NewTimer(wait)
			due = timer.C
		}
		select {
		case <-ctx.Done():
		case <-g.kick:
		case <-due:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// URL implements GatewaySource.
func (g *DaemonGateway) URL(ctx context.Context) (string, error) {
	g.init()
	g.mu.Lock()
	if g.rec.ID != "" {
		u := g.rec.URL
		g.mu.Unlock()
		return u, nil
	}
	if g.closed {
		g.mu.Unlock()
		return "", fmt.Errorf("%s gateway: daemon is shutting down", g.name())
	}
	if wait := g.nextTry.Sub(g.now()); wait > 0 {
		err := fmt.Errorf("%s gateway %s unavailable (last error: %v); next retry in %s", g.name(), g.BaseURL, g.lastErr, wait.Round(time.Second))
		g.mu.Unlock()
		return "", err
	}
	g.mu.Unlock()
	if err := g.attempt(ctx); err != nil {
		return "", err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.rec.ID == "" {
		return "", fmt.Errorf("%s gateway %s unavailable", g.name(), g.BaseURL)
	}
	return g.rec.URL, nil
}

// Invalidate implements GatewaySource.
func (g *DaemonGateway) Invalidate(ctx context.Context, url string) {
	g.init()
	g.attemptMu.Lock()
	defer g.attemptMu.Unlock()
	g.mu.Lock()
	rec := g.rec
	if rec.ID == "" || rec.URL != url {
		g.mu.Unlock()
		return
	}
	g.rec = Record{}
	g.nextTry = time.Time{}
	g.mu.Unlock()
	g.audit("invalidated", "id", rec.ID, "url", rec.URL)
	g.logf("gateway %s (%s) invalidated; minting a new one", rec.ID, rec.URL)
	g.deleteRecorded(ctx, rec)
	select {
	case g.kick <- struct{}{}:
	default:
	}
}

// attempt mints the gateway unless one is live: it first DELETEs any gateway
// recorded by an earlier run, then POSTs a new one over Servers and records
// it. A failure is logged, counted and returned; the next attempt is not due
// until the backoff elapses.
func (g *DaemonGateway) attempt(ctx context.Context) error {
	g.attemptMu.Lock()
	defer g.attemptMu.Unlock()
	g.mu.Lock()
	if g.closed || g.rec.ID != "" {
		g.mu.Unlock()
		return nil
	}
	g.mu.Unlock()

	if err := g.deleteStale(ctx); err != nil {
		g.fail(ctx, err)
		return err
	}
	gw, err := g.client(g.BaseURL).Create(ctx, append([]string(nil), g.Servers...), false)
	if err != nil {
		g.fail(ctx, err)
		return err
	}
	rec := Record{
		Sandbox:   g.Key,
		BaseURL:   g.BaseURL,
		ID:        gw.ID,
		URL:       gw.URL,
		Requested: append([]string(nil), g.Servers...),
		Servers:   append([]string(nil), g.Servers...),
		CreatedAt: g.now().UTC(),
	}
	if err := g.Store.Save(rec); err != nil {
		// The gateway still works; only crash cleanup is lost.
		g.audit("record_error", "id", gw.ID, "dir", g.Store.Dir, "err", err.Error())
		g.logf("WARNING cannot record gateway %s in %s (it will leak if orch crashes): %v", gw.ID, g.Store.Dir, err)
	}
	g.mu.Lock()
	if g.closed {
		// Close began while we were creating: don't leave it behind.
		g.mu.Unlock()
		g.deleteRecorded(context.WithoutCancel(ctx), rec)
		return fmt.Errorf("%s gateway: daemon is shutting down", g.name())
	}
	g.rec = rec
	g.lastErr = nil
	g.failures = 0
	g.nextTry = time.Time{}
	g.mu.Unlock()

	fields := []string{"id", gw.ID, "url", gw.URL, "endpoint", g.BaseURL, "servers", g.servers()}
	gaps := Manager{Servers: g.Servers}.Gaps(gw)
	if len(gaps) > 0 {
		fields = append(fields, "gaps", strings.Join(gaps, "; "))
	}
	g.audit("created", fields...)
	g.logf("created gateway %s at %s on %s over %s", gw.ID, gw.URL, g.BaseURL, g.servers())
	for _, gap := range gaps {
		g.logf("WARNING gateway %s does not provide %s", gw.ID, gap)
	}
	return nil
}

// fail records a failed creation attempt and schedules the next one. An
// attempt cut short by shutdown (ctx done) is not reported.
func (g *DaemonGateway) fail(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return
	}
	base, maxDelay := g.BackoffBase, g.BackoffMax
	if base <= 0 {
		base = daemonGatewayBackoffBase
	}
	if maxDelay <= 0 {
		maxDelay = daemonGatewayBackoffMax
	}
	g.mu.Lock()
	g.failures++
	n := g.failures
	delay := base << min(n-1, 30)
	if delay <= 0 || delay > maxDelay {
		delay = maxDelay
	}
	g.lastErr = err
	g.nextTry = g.now().Add(delay)
	g.mu.Unlock()
	g.audit("create_error",
		"endpoint", g.BaseURL,
		"attempt", strconv.Itoa(n),
		"retry_in", delay.String(),
		"err", err.Error())
	g.logf("create on %s failed (attempt %d, retrying in %s): %v", g.BaseURL, n, delay, err)
}

// deleteStale DELETEs a gateway recorded under Key — one this process did not
// mint, so a previous run left it behind — and removes the record. It fails
// only when that gateway is on the current endpoint and cannot be deleted
// (creating a new one would then fail the same way, and keeping the record
// means a later attempt still cleans it up); a record for a different
// endpoint that can't be reached is dropped.
func (g *DaemonGateway) deleteStale(ctx context.Context) error {
	if g.Store.Dir == "" {
		return nil
	}
	rec, ok, err := g.Store.Load(g.Key)
	if err != nil {
		g.audit("record_error", "dir", g.Store.Dir, "err", err.Error())
		g.logf("WARNING unreadable gateway record, dropping it: %v", err)
		_ = g.Store.Remove(g.Key)
		return nil
	}
	if !ok {
		return nil
	}
	base := rec.BaseURL
	if base == "" {
		base = g.BaseURL
	}
	if rec.ID != "" {
		if err := g.client(base).Delete(ctx, rec.ID); err != nil {
			g.audit("stale_delete_error", "id", rec.ID, "endpoint", base, "err", err.Error())
			g.logf("could not delete stale gateway %s on %s: %v", rec.ID, base, err)
			if base == g.BaseURL {
				return fmt.Errorf("delete stale gateway %s: %w", rec.ID, err)
			}
		} else {
			g.audit("stale_deleted", "id", rec.ID, "endpoint", base, "created_at", rec.CreatedAt.Format(time.RFC3339))
			g.logf("deleted stale gateway %s on %s left by a previous run", rec.ID, base)
		}
	}
	if err := g.Store.Remove(g.Key); err != nil {
		g.logf("WARNING %v", err)
	}
	return nil
}

// deleteRecorded DELETEs rec's gateway and, on success, removes its record. A
// failed DELETE keeps the record so the next startup retries it as stale.
func (g *DaemonGateway) deleteRecorded(ctx context.Context, rec Record) bool {
	if err := g.client(rec.BaseURL).Delete(ctx, rec.ID); err != nil {
		g.audit("delete_error", "id", rec.ID, "endpoint", rec.BaseURL, "err", err.Error())
		g.logf("could not delete gateway %s on %s: %v", rec.ID, rec.BaseURL, err)
		return false
	}
	if err := g.Store.Remove(g.Key); err != nil {
		g.logf("WARNING %v", err)
	}
	g.audit("deleted", "id", rec.ID, "endpoint", rec.BaseURL)
	g.logf("deleted gateway %s on %s", rec.ID, rec.BaseURL)
	return true
}

// Close stops further creation, waits for the Start loop (whose ctx must
// already be done) and DELETEs the live gateway.
func (g *DaemonGateway) Close() {
	g.init()
	g.mu.Lock()
	g.closed = true
	done := g.done
	g.mu.Unlock()
	if done != nil {
		<-done
	}
	g.attemptMu.Lock()
	defer g.attemptMu.Unlock()
	g.mu.Lock()
	rec := g.rec
	g.rec = Record{}
	g.mu.Unlock()
	if rec.ID == "" {
		return
	}
	timeout := g.CloseTimeout
	if timeout <= 0 {
		timeout = daemonGatewayCloseTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	g.deleteRecorded(ctx, rec)
}
