package project

import (
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
)

func TestBlockedSessionPath(t *testing.T) {
	got := BlockedSessionPath("/orch/myproj/.project.yaml")
	want := "/orch/myproj/blocked-session.yaml"
	if got != want {
		t.Errorf("BlockedSessionPath = %q, want %q", got, want)
	}
}

func TestLoadBlockedSessionMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocked-session.yaml")
	_, err := LoadBlockedSession(path)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("LoadBlockedSession missing file: err = %v, want fs.ErrNotExist", err)
	}
}

// TestSaveAndLoadBlockedSessionRoundTrip covers the write/read-back deliverable:
// a wolf session's diagnosis, once saved, comes back byte-for-byte on the next
// invocation.
func TestSaveAndLoadBlockedSessionRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocked-session.yaml")
	src := &BlockedSession{
		BlockedReason: "task \"add-healthz\" failed after 3 attempts",
		Summary:       "1. Fix the missing import in handler.go\n2. Re-run the task",
		Attempted:     []string{"reverted the last commit", "checked the sandbox logs"},
	}
	if err := SaveBlockedSessionAs(path, src, WriterAgent); err != nil {
		t.Fatalf("SaveBlockedSessionAs: %v", err)
	}

	got, err := LoadBlockedSession(path)
	if err != nil {
		t.Fatalf("LoadBlockedSession: %v", err)
	}
	if got.BlockedReason != src.BlockedReason {
		t.Errorf("BlockedReason = %q, want %q", got.BlockedReason, src.BlockedReason)
	}
	if got.Summary != src.Summary {
		t.Errorf("Summary = %q, want %q", got.Summary, src.Summary)
	}
	if len(got.Attempted) != len(src.Attempted) {
		t.Fatalf("Attempted = %+v, want %+v", got.Attempted, src.Attempted)
	}
	for i := range src.Attempted {
		if got.Attempted[i] != src.Attempted[i] {
			t.Errorf("Attempted[%d] = %q, want %q", i, got.Attempted[i], src.Attempted[i])
		}
	}
	if got.UpdatedBy != WriterAgent {
		t.Errorf("UpdatedBy = %q, want %q", got.UpdatedBy, WriterAgent)
	}
}

func TestSaveBlockedSessionAsRejectsInvalidWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocked-session.yaml")
	err := SaveBlockedSessionAs(path, &BlockedSession{}, Writer("bogus"))
	if err == nil {
		t.Fatal("expected error for invalid writer, got nil")
	}
}
