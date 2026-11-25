package pathlock

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestLockSerializesSamePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.yaml")
	const writers, rounds = 8, 200
	counter := 0 // guarded only by Lock(path); -race flags any overlap
	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				unlock := Lock(path)
				v := counter
				time.Sleep(time.Microsecond)
				counter = v + 1
				unlock()
			}
		}()
	}
	wg.Wait()
	if counter != writers*rounds {
		t.Fatalf("counter = %d, want %d (lost updates)", counter, writers*rounds)
	}
	if n := entries(); n != 0 {
		t.Errorf("entries after release = %d, want 0", n)
	}
}

func TestLockEquivalentPathsShareLock(t *testing.T) {
	dir := t.TempDir()
	unlock := Lock(filepath.Join(dir, "p.yaml"))
	acquired := make(chan struct{})
	go func() {
		u := Lock(filepath.Join(dir, "sub", "..", "p.yaml"))
		close(acquired)
		u()
	}()
	select {
	case <-acquired:
		t.Fatal("equivalent path acquired the lock while it was held")
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never acquired the lock after release")
	}
}

func TestLockDifferentPathsIndependent(t *testing.T) {
	dir := t.TempDir()
	unlock := Lock(filepath.Join(dir, "a.yaml"))
	defer unlock()
	done := make(chan struct{})
	go func() {
		Lock(filepath.Join(dir, "b.yaml"))()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("lock on a different path blocked")
	}
}

func TestUnlockIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.yaml")
	unlock := Lock(path)
	unlock()
	unlock() // must not panic or unlock someone else's hold
	held := Lock(path)
	defer held()
	if n := entries(); n != 1 {
		t.Errorf("entries = %d, want 1", n)
	}
}
