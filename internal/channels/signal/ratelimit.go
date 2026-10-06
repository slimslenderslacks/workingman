package signal

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Rate limiting in the spirit of hermes-agent's signal_rate_limit.py, reduced
// to what text sends need: recognize signal-cli's rate-limit errors, honor the
// server's retry-after hint, and pace every send on the channel after a 429 so
// concurrent senders do not all hit the limit again.

const (
	// rpcErrorRateLimit is signal-cli (v0.14.3+) JSON-RPC error code for RateLimitException.
	rpcErrorRateLimit = -5
	// defaultRetryAfter is the fallback wait when the server gave no hint.
	defaultRetryAfter = 4 * time.Second
	// maxRetryAfter caps how long a send will wait out a rate limit before giving up.
	maxRetryAfter = 60 * time.Second
	// maxSendAttempts is the initial attempt plus one retry.
	maxSendAttempts = 2
)

var retryAfterRE = regexp.MustCompile(`(?i)Retry after (\d+(?:\.\d+)?)\s*second`)

// rateLimitError is a send refused because of rate limiting.
type rateLimitError struct {
	msg        string
	retryAfter time.Duration // 0 when the server gave no hint
}

func (e *rateLimitError) Error() string { return "signal: rate limited: " + e.msg }

// isRateLimitMessage matches the legacy textual forms of a rate-limit error.
func isRateLimitMessage(msg string) bool {
	l := strings.ToLower(msg)
	return strings.Contains(msg, "[429]") || strings.Contains(l, "ratelimit") ||
		strings.Contains(l, "retrylaterexception") || strings.Contains(l, "retry after")
}

// retryAfterFromMessage parses "Retry after N seconds".
func retryAfterFromMessage(msg string) time.Duration {
	if m := retryAfterRE.FindStringSubmatch(msg); m != nil {
		if s, err := strconv.ParseFloat(m[1], 64); err == nil && s > 0 {
			return time.Duration(s * float64(time.Second))
		}
	}
	return 0
}

// pacer spaces sends: a minimum gap between them, pushed out further whenever
// the server asks us to back off. Safe for concurrent use.
type pacer struct {
	mu      sync.Mutex
	gap     time.Duration
	next    time.Time
	now     func() time.Time
	waitFor func(ctx context.Context, d time.Duration) error
}

func newPacer(gap time.Duration) *pacer {
	return &pacer{gap: gap, now: time.Now, waitFor: sleepCtx}
}

// wait blocks until this caller's turn, reserving the slot. It returns ctx's
// error if ctx ends first.
func (p *pacer) wait(ctx context.Context) error {
	p.mu.Lock()
	now := p.now()
	at := p.next
	if at.Before(now) {
		at = now
	}
	p.next = at.Add(p.gap)
	p.mu.Unlock()
	if d := at.Sub(now); d > 0 {
		return p.waitFor(ctx, d)
	}
	return ctx.Err()
}

// backoff delays every later send by d (the server's retry-after).
func (p *pacer) backoff(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if until := p.now().Add(d); until.After(p.next) {
		p.next = until
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
