package task

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/pathlock"
)

// TestUpdateConcurrentWritersLoseNothing: many in-process writers each load,
// bump and save the same task file. The per-path lock held across Update keeps
// every increment. Run with -race.
func TestUpdateConcurrentWritersLoseNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.yaml")
	if err := Save(path, &Task{Name: "t", Status: StatusReady}); err != nil {
		t.Fatal(err)
	}
	const writers, rounds = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, writers*rounds)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				errs <- Update(path, func(t *Task) bool {
					t.Attempts++
					return true
				})
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempts != writers*rounds {
		t.Errorf("Attempts = %d, want %d (lost updates)", got.Attempts, writers*rounds)
	}
	if got.Name != "t" || got.Status != StatusReady {
		t.Errorf("other fields clobbered: name %q status %q", got.Name, got.Status)
	}
}

func TestSaveWaitsForPathLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.yaml")
	if err := Save(path, &Task{Name: "before", Status: StatusReady}); err != nil {
		t.Fatal(err)
	}
	unlock := pathlock.Lock(path)
	done := make(chan error, 1)
	go func() { done <- Save(path, &Task{Name: "after", Status: StatusReady}) }()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("Save returned (%v) while the path lock was held", err)
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "after" {
		t.Errorf("Name = %q, want after", got.Name)
	}
}
