package sbx

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A ListCache runs sbx once per TTL, refreshes after it, and caches a failure
// for the TTL too.
func TestListCacheReusesWithinTTL(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	now := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	c := &ListCache{
		Client: Client{Attempts: 1, Run: func(context.Context, string, ...string) ([]byte, []byte, error) {
			n := calls.Add(1)
			if fail.Load() {
				return nil, []byte("boom"), errors.New("exit 1")
			}
			if n == 1 {
				return []byte(`[{"name":"a","workspaces":["/x"]}]`), nil, nil
			}
			return []byte(`[{"name":"a"},{"name":"b"}]`), nil, nil
		}},
		TTL: 5 * time.Second,
		Now: func() time.Time { return now },
	}
	ctx := context.Background()
	list, err := c.List(ctx)
	if err != nil || len(list) != 1 || list[0].Name != "a" {
		t.Fatalf("first List = %+v, %v", list, err)
	}
	list[0].Workspaces[0] = "/mutated"
	now = now.Add(4 * time.Second)
	again, err := c.List(ctx)
	if err != nil || calls.Load() != 1 || again[0].Workspaces[0] != "/x" {
		t.Fatalf("within TTL: %+v, %v after %d calls; want the cached, unmutated answer from 1 call", again, err, calls.Load())
	}
	now = now.Add(2 * time.Second)
	if list, err := c.List(ctx); err != nil || len(list) != 2 || calls.Load() != 2 {
		t.Fatalf("after TTL: %+v, %v after %d calls; want a fresh call", list, err, calls.Load())
	}

	fail.Store(true)
	c.Invalidate()
	if _, err := c.List(ctx); err == nil {
		t.Fatal("failing sbx: no error")
	}
	if _, err := c.List(ctx); err == nil || calls.Load() != 3 {
		t.Fatalf("failure not cached: err=%v calls=%d", err, calls.Load())
	}
}

// Concurrent callers on a stale cache share one sbx call.
func TestListCacheSharesOneCall(t *testing.T) {
	var calls atomic.Int32
	c := &ListCache{Client: Client{Attempts: 1, Run: func(context.Context, string, ...string) ([]byte, []byte, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return []byte(`[]`), nil, nil
	}}}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.List(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Errorf("sbx ran %d times for 8 concurrent callers, want 1", n)
	}
}
