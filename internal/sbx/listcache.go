package sbx

import (
	"context"
	"sync"
	"time"
)

// DefaultListTTL is how long a ListCache reuses one `sbx ls --json` answer.
const DefaultListTTL = 5 * time.Second

// ListCache answers [Client.List] from a recent call for TTL, so a reader
// that asks often (the daemon's API, once per request) runs `sbx ls` at most
// once per TTL. Concurrent callers that find the entry stale share one call:
// the cache's lock is held while sbx runs. A failed call is cached for the
// TTL as well, so an sbx outage does not turn every request into a retry
// loop. The zero value is usable: it lists through a zero Client with
// DefaultListTTL.
type ListCache struct {
	Client Client
	TTL    time.Duration // 0: DefaultListTTL
	// Now is the clock (nil: time.Now); a test seam.
	Now func() time.Time

	mu   sync.Mutex
	at   time.Time
	ok   bool // a call has been made since the last Invalidate
	list []Sandbox
	err  error
}

// List returns the sandboxes `sbx ls --json` reported at most TTL ago,
// running it now when the cached answer is older or missing. The returned
// slice is a copy. A non-nil error means the query failed (see [Alive]): the
// list is then empty and says nothing about which sandboxes exist.
func (c *ListCache) List(ctx context.Context) ([]Sandbox, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if !c.ok || now.Sub(c.at) >= c.ttl() || now.Before(c.at) {
		c.list, c.err = c.Client.List(ctx)
		c.at, c.ok = c.now(), true
	}
	if c.err != nil {
		return nil, c.err
	}
	out := make([]Sandbox, len(c.list))
	for i, s := range c.list {
		s.Workspaces = append([]string(nil), s.Workspaces...)
		out[i] = s
	}
	return out, nil
}

// Invalidate drops the cached answer, so the next List runs sbx.
func (c *ListCache) Invalidate() {
	c.mu.Lock()
	c.ok = false
	c.mu.Unlock()
}

func (c *ListCache) ttl() time.Duration {
	if c.TTL > 0 {
		return c.TTL
	}
	return DefaultListTTL
}

func (c *ListCache) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}
