package project

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/pathlock"
)

// TestUpdateConcurrentWritersLoseNothing is the lost-update race the per-path
// lock closes: many in-process writers each load, increment and save the same
// project.yaml. Without the lock some increments are overwritten. Run with
// -race.
func TestUpdateConcurrentWritersLoseNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := Save(path, &Project{Description: "d", Status: StatusIdle}); err != nil {
		t.Fatal(err)
	}
	const writers, rounds = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, writers*rounds)
	for i := range writers {
		by := WriterDaemon
		if i%2 == 1 {
			by = WriterAgent
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				errs <- Update(path, by, func(p *Project) bool {
					p.CronRuns++
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
	p, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.CronRuns != writers*rounds {
		t.Errorf("CronRuns = %d, want %d (lost updates)", p.CronRuns, writers*rounds)
	}
	if p.Description != "d" || p.Status != StatusIdle {
		t.Errorf("other fields clobbered: %+v", p)
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*.tmp*"))
	if len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
}

// TestSaveAsWaitsForPathLock checks that SaveAs takes the same lock an
// in-process read-modify-write holds, so a plain save can't land in the middle
// of one.
func TestSaveAsWaitsForPathLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := Save(path, &Project{Description: "before"}); err != nil {
		t.Fatal(err)
	}
	unlock := pathlock.Lock(path)
	done := make(chan error, 1)
	go func() { done <- SaveAs(path, &Project{Description: "after"}, WriterAgent) }()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("SaveAs returned (%v) while the path lock was held", err)
	case <-time.After(50 * time.Millisecond):
	}
	if p, _ := Load(path); p.Description != "before" {
		t.Fatalf("file written while locked: description %q", p.Description)
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	p, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.Description != "after" || p.UpdatedBy != WriterAgent {
		t.Errorf("got %q by %q, want after by agent", p.Description, p.UpdatedBy)
	}
}

func TestUpdateStampsWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := Save(path, &Project{Description: "d"}); err != nil {
		t.Fatal(err)
	}
	if err := Update(path, WriterAgent, func(p *Project) bool {
		p.Cleanup = true
		return true
	}); err != nil {
		t.Fatal(err)
	}
	p, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Cleanup || p.UpdatedBy != WriterAgent {
		t.Errorf("Cleanup=%v UpdatedBy=%q, want true/agent", p.Cleanup, p.UpdatedBy)
	}
}

func TestUpdateNoChangeDoesNotWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := Save(path, &Project{Description: "d"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := Update(path, WriterAgent, func(*Project) bool { return false }); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) || !fi.ModTime().Equal(old) {
		t.Errorf("file rewritten although fn reported no change")
	}
}

func TestUpdateErrors(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing", FileName)
	called := false
	err := Update(missing, WriterDaemon, func(*Project) bool { called = true; return true })
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file: err = %v, want fs.ErrNotExist", err)
	}
	if called {
		t.Error("fn called for a missing file")
	}
	if err := Update(filepath.Join(dir, FileName), Writer("bogus"), func(*Project) bool { return true }); err == nil {
		t.Error("invalid writer: want error")
	}
}

func TestUpdateIfCheckSeesBytesAndCanRefuse(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := Save(path, &Project{Description: "d", Status: StatusIdle}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	refused := errors.New("stale")
	ran := false
	err = UpdateIf(path, WriterAgent, func(data []byte) error {
		if string(data) != string(before) {
			t.Errorf("check saw %q, want the file's bytes %q", data, before)
		}
		return refused
	}, func(p *Project) bool {
		ran = true
		p.Cleanup = true
		return true
	})
	if !errors.Is(err, refused) || ran {
		t.Fatalf("UpdateIf = %v (fn ran: %v), want the check's error and no fn", err, ran)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Errorf("a refused check rewrote the file: %q", after)
	}
	if err := UpdateIf(path, WriterAgent, func([]byte) error { return nil }, func(p *Project) bool {
		p.Cleanup = true
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if p, _ := Load(path); !p.Cleanup || p.UpdatedBy != WriterAgent {
		t.Errorf("after a passing check: %+v", p)
	}
}
