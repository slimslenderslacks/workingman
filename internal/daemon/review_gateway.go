package daemon

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/mcpgateway"
)

// The daemon's own MCP gateway: a static sub-gateway of the host's
// standalone-codemode gateway over ONLY the github server, used by the PR
// fingerprint check (no `gh`, no local GitHub auth). It is minted on the same
// /gateways API acp-wrapper uses for agent sandboxes (mcpgateway.Client) and
// its record is kept in an mcpgateway.Store under a fixed key, so a gateway a
// crashed run left behind is DELETEd before the next one is minted.
//
// Lifecycle:
//   - Run starts reviewGateway.run, which deletes a stale recorded gateway and
//     creates the new one off the startup path. A failure is logged and
//     retried with exponential backoff; it never stops the daemon.
//   - URL hands the gateway to callers, trying a creation on demand once the
//     backoff window has passed. Invalidate drops a gateway a caller found
//     gone (e.g. standalone-codemode restarted) so a fresh one is minted.
//   - shutdown DELETEs the gateway and removes its record.

const (
	// reviewGatewayKey is the Store key of the daemon's gateway record.
	reviewGatewayKey = "orch-daemon"
	// reviewGatewayServer is the only catalog server the gateway is minted over.
	reviewGatewayServer = "github"

	reviewGatewayBackoffBase = 5 * time.Second
	reviewGatewayBackoffMax  = 5 * time.Minute
	// reviewGatewayCloseTimeout bounds the DELETE at shutdown.
	reviewGatewayCloseTimeout = 10 * time.Second
)

// ReviewGatewayConfig configures the daemon's github-only MCP gateway.
type ReviewGatewayConfig struct {
	// BaseURL is the standalone-codemode root, e.g. http://localhost:3333
	// (cmd/orch's --mcp-gateway-url). Empty disables the gateway.
	BaseURL string
	// StateDir is the mcpgateway.Store directory for the gateway record. Use a
	// directory of its own (see ReviewGatewayStateDir), not the per-sandbox
	// gateways directory: acp-wrapper's Sweep releases every record there whose
	// name is not a live sandbox.
	StateDir string
}

// ReviewGatewayStateDir is the default ReviewGatewayConfig.StateDir for a
// per-sandbox gateway store directory (mcpgateway.StateDir): a "daemon"
// subdirectory, which mcpgateway.Store.List — and so acp-wrapper's Sweep —
// skips.
func ReviewGatewayStateDir(gatewaysDir string) string {
	return filepath.Join(gatewaysDir, "daemon")
}

// WithReviewGateway gives the daemon its own github-only MCP gateway for the
// PR fingerprint check (see review_gateway.go). An empty BaseURL leaves it
// disabled.
func WithReviewGateway(cfg ReviewGatewayConfig) Option {
	return func(d *Daemon) {
		base := strings.TrimSuffix(strings.TrimSpace(cfg.BaseURL), "/")
		if base == "" {
			return
		}
		d.reviewGW = newReviewGateway(base, mcpgateway.Store{Dir: cfg.StateDir}, d.audit)
	}
}

// reviewGatewaySource is what the rest of the daemon (the fingerprint check)
// needs of the gateway; tests fake it.
type reviewGatewaySource interface {
	// URL returns the gateway's MCP endpoint, creating the gateway on demand
	// if none is live and the retry backoff has elapsed.
	URL(ctx context.Context) (string, error)
	// Invalidate drops the gateway at url (one a caller found gone or broken)
	// so the next URL mints a new one. A url that is no longer current is
	// ignored.
	Invalidate(ctx context.Context, url string)
}

// gatewayAPI is the slice of mcpgateway.Client the daemon gateway uses — the
// test seam for faking creation and deletion.
type gatewayAPI interface {
	Create(ctx context.Context, servers []string, dynamic bool) (mcpgateway.Gateway, error)
	Delete(ctx context.Context, id string) error
}

// reviewGateway owns the daemon's gateway. Safe for concurrent use.
type reviewGateway struct {
	endpoint string
	store    mcpgateway.Store
	audit    *audit.Logger
	// clientFor returns the API client for a standalone-codemode root (a stale
	// record may name a different root than endpoint).
	clientFor   func(baseURL string) gatewayAPI
	logf        func(format string, args ...any) // daemon.log
	clock       func() time.Time
	backoffBase time.Duration
	backoffMax  time.Duration

	// attemptMu serializes create/delete round trips so the background loop
	// and an on-demand URL never mint two gateways.
	attemptMu sync.Mutex

	mu       sync.Mutex
	rec      mcpgateway.Record // the live gateway; ID == "" when none
	lastErr  error
	failures int
	nextTry  time.Time
	closed   bool
	kick     chan struct{}
	done     chan struct{} // closed when run returns; nil until run starts
}

func newReviewGateway(endpoint string, store mcpgateway.Store, a *audit.Logger) *reviewGateway {
	return &reviewGateway{
		endpoint:    endpoint,
		store:       store,
		audit:       a,
		clientFor:   func(base string) gatewayAPI { return mcpgateway.Client{BaseURL: base} },
		logf:        log.Printf,
		clock:       time.Now,
		backoffBase: reviewGatewayBackoffBase,
		backoffMax:  reviewGatewayBackoffMax,
		kick:        make(chan struct{}, 1),
	}
}

// startReviewGateway launches the gateway's create/retry loop. No-op when the
// gateway is not configured.
func (d *Daemon) startReviewGateway(ctx context.Context) {
	if d.reviewGW == nil {
		return
	}
	d.reviewGW.mu.Lock()
	d.reviewGW.done = make(chan struct{})
	d.reviewGW.mu.Unlock()
	go d.reviewGW.run(ctx)
}

// reviewGateway returns the daemon's gateway, or nil when not configured.
func (d *Daemon) reviewGateway() reviewGatewaySource {
	if d.reviewGatewaySrc != nil {
		return d.reviewGatewaySrc
	}
	if d.reviewGW == nil {
		return nil
	}
	return d.reviewGW
}

// run keeps a gateway minted until ctx is done: it attempts creation, then
// sleeps until the backoff elapses (after a failure) or until Invalidate kicks
// it (after a success).
func (g *reviewGateway) run(ctx context.Context) {
	defer close(g.done)
	for {
		if ctx.Err() != nil {
			return
		}
		g.attempt(ctx)
		g.mu.Lock()
		live, wait := g.rec.ID != "", g.nextTry.Sub(g.clock())
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

// URL implements reviewGatewaySource.
func (g *reviewGateway) URL(ctx context.Context) (string, error) {
	g.mu.Lock()
	if g.rec.ID != "" {
		u := g.rec.URL
		g.mu.Unlock()
		return u, nil
	}
	if g.closed {
		g.mu.Unlock()
		return "", fmt.Errorf("review gateway: daemon is shutting down")
	}
	if wait := g.nextTry.Sub(g.clock()); wait > 0 {
		err := fmt.Errorf("review gateway %s unavailable (last error: %v); next retry in %s", g.endpoint, g.lastErr, wait.Round(time.Second))
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
		return "", fmt.Errorf("review gateway %s unavailable", g.endpoint)
	}
	return g.rec.URL, nil
}

// Invalidate implements reviewGatewaySource.
func (g *reviewGateway) Invalidate(ctx context.Context, url string) {
	g.attemptMu.Lock()
	defer g.attemptMu.Unlock()
	g.mu.Lock()
	rec := g.rec
	if rec.ID == "" || rec.URL != url {
		g.mu.Unlock()
		return
	}
	g.rec = mcpgateway.Record{}
	g.nextTry = time.Time{}
	g.mu.Unlock()
	g.audit.Log("review_gateway_invalidated", "id", rec.ID, "url", rec.URL)
	g.logf("review gateway: gateway %s (%s) invalidated; minting a new one", rec.ID, rec.URL)
	g.deleteRecorded(ctx, rec)
	select {
	case g.kick <- struct{}{}:
	default:
	}
}

// attempt mints the gateway unless one is live: it first DELETEs any gateway
// recorded by an earlier run, then POSTs a new one over github only and
// records it. A failure is logged, counted and returned; the next attempt is
// not due until the backoff elapses.
func (g *reviewGateway) attempt(ctx context.Context) error {
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
	gw, err := g.clientFor(g.endpoint).Create(ctx, []string{reviewGatewayServer}, false)
	if err != nil {
		g.fail(ctx, err)
		return err
	}
	rec := mcpgateway.Record{
		Sandbox:   reviewGatewayKey,
		BaseURL:   g.endpoint,
		ID:        gw.ID,
		URL:       gw.URL,
		Requested: []string{reviewGatewayServer},
		Servers:   []string{reviewGatewayServer},
		CreatedAt: g.clock().UTC(),
	}
	if err := g.store.Save(rec); err != nil {
		// The gateway still works; only crash cleanup is lost.
		g.audit.Log("review_gateway_record_error", "id", gw.ID, "dir", g.store.Dir, "err", err.Error())
		g.logf("review gateway: WARNING cannot record gateway %s in %s (it will leak if orch crashes): %v", gw.ID, g.store.Dir, err)
	}
	g.mu.Lock()
	if g.closed {
		// shutdown began while we were creating: don't leave it behind.
		g.mu.Unlock()
		g.deleteRecorded(context.WithoutCancel(ctx), rec)
		return fmt.Errorf("review gateway: daemon is shutting down")
	}
	g.rec = rec
	g.lastErr = nil
	g.failures = 0
	g.nextTry = time.Time{}
	g.mu.Unlock()

	fields := []string{"id", gw.ID, "url", gw.URL, "endpoint", g.endpoint, "servers", reviewGatewayServer}
	gaps := mcpgateway.Manager{Servers: []string{reviewGatewayServer}}.Gaps(gw)
	if len(gaps) > 0 {
		fields = append(fields, "gaps", strings.Join(gaps, "; "))
	}
	g.audit.Log("review_gateway_created", fields...)
	g.logf("review gateway: created gateway %s at %s on %s over %s", gw.ID, gw.URL, g.endpoint, reviewGatewayServer)
	for _, gap := range gaps {
		g.logf("review gateway: WARNING gateway %s does not provide %s", gw.ID, gap)
	}
	return nil
}

// fail records a failed creation attempt and schedules the next one. An
// attempt cut short by shutdown (ctx done) is not reported.
func (g *reviewGateway) fail(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return
	}
	g.mu.Lock()
	g.failures++
	n := g.failures
	delay := g.backoffBase << min(n-1, 30)
	if delay <= 0 || delay > g.backoffMax {
		delay = g.backoffMax
	}
	g.lastErr = err
	g.nextTry = g.clock().Add(delay)
	g.mu.Unlock()
	g.audit.Log("review_gateway_create_error",
		"endpoint", g.endpoint,
		"attempt", strconv.Itoa(n),
		"retry_in", delay.String(),
		"err", err.Error())
	g.logf("review gateway: create on %s failed (attempt %d, retrying in %s): %v", g.endpoint, n, delay, err)
}

// deleteStale DELETEs a gateway recorded under reviewGatewayKey — one this
// process did not mint, so a previous run left it behind — and removes the
// record. It fails only when that gateway is on the current endpoint and
// cannot be deleted (creating a new one would then fail the same way, and
// keeping the record means a later attempt still cleans it up); a record for
// a different endpoint that can't be reached is dropped.
func (g *reviewGateway) deleteStale(ctx context.Context) error {
	if g.store.Dir == "" {
		return nil
	}
	rec, ok, err := g.store.Load(reviewGatewayKey)
	if err != nil {
		g.audit.Log("review_gateway_record_error", "dir", g.store.Dir, "err", err.Error())
		g.logf("review gateway: WARNING unreadable gateway record, dropping it: %v", err)
		_ = g.store.Remove(reviewGatewayKey)
		return nil
	}
	if !ok {
		return nil
	}
	base := rec.BaseURL
	if base == "" {
		base = g.endpoint
	}
	if rec.ID != "" {
		if err := g.clientFor(base).Delete(ctx, rec.ID); err != nil {
			g.audit.Log("review_gateway_stale_delete_error", "id", rec.ID, "endpoint", base, "err", err.Error())
			g.logf("review gateway: could not delete stale gateway %s on %s: %v", rec.ID, base, err)
			if base == g.endpoint {
				return fmt.Errorf("delete stale gateway %s: %w", rec.ID, err)
			}
		} else {
			g.audit.Log("review_gateway_stale_deleted", "id", rec.ID, "endpoint", base, "created_at", rec.CreatedAt.Format(time.RFC3339))
			g.logf("review gateway: deleted stale gateway %s on %s left by a previous run", rec.ID, base)
		}
	}
	if err := g.store.Remove(reviewGatewayKey); err != nil {
		g.logf("review gateway: WARNING %v", err)
	}
	return nil
}

// deleteRecorded DELETEs rec's gateway and, on success, removes its record. A
// failed DELETE keeps the record so the next startup retries it as stale.
func (g *reviewGateway) deleteRecorded(ctx context.Context, rec mcpgateway.Record) bool {
	if err := g.clientFor(rec.BaseURL).Delete(ctx, rec.ID); err != nil {
		g.audit.Log("review_gateway_delete_error", "id", rec.ID, "endpoint", rec.BaseURL, "err", err.Error())
		g.logf("review gateway: could not delete gateway %s on %s: %v", rec.ID, rec.BaseURL, err)
		return false
	}
	if err := g.store.Remove(reviewGatewayKey); err != nil {
		g.logf("review gateway: WARNING %v", err)
	}
	g.audit.Log("review_gateway_deleted", "id", rec.ID, "endpoint", rec.BaseURL)
	g.logf("review gateway: deleted gateway %s on %s", rec.ID, rec.BaseURL)
	return true
}

// close stops further creation, waits for the run loop (whose ctx is already
// done) and DELETEs the live gateway. Called from Daemon.shutdown.
func (g *reviewGateway) close() {
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
	g.rec = mcpgateway.Record{}
	g.mu.Unlock()
	if rec.ID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), reviewGatewayCloseTimeout)
	defer cancel()
	g.deleteRecorded(ctx, rec)
}
