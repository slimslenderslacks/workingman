package session

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func sampleSession(id string, created time.Time) Session {
	return Session{
		ID:          id,
		SandboxName: "acp-" + id,
		Status:      StatusRunning,
		CreatedAt:   created,
		UpdatedAt:   created,
		SocketPath:  "/tmp/sessions/" + id + "/agent.sock",
		Workspaces:  []string{"/repo", "/orch"},
		Kit:         "/kits/acp",
		PromptCount: 3,
	}
}

func TestStorePaths(t *testing.T) {
	s := Store{Root: "/tmp/sessions"}
	if got, want := s.Dir("sess1"), "/tmp/sessions/sess1"; got != want {
		t.Errorf("Dir = %q, want %q", got, want)
	}
	if got, want := s.Path("sess1"), "/tmp/sessions/sess1/session.json"; got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
	if got, want := s.SocketPath("sess1"), "/tmp/sessions/sess1/agent.sock"; got != want {
		t.Errorf("SocketPath = %q, want %q", got, want)
	}
}

func TestNewStoreDefaults(t *testing.T) {
	t.Setenv("HOME", "/home/test")
	s, err := NewStore("")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	want := filepath.Join("/home/test", ".workingman", "sessions")
	if s.Root != want {
		t.Errorf("Root = %q, want %q", s.Root, want)
	}
}

func TestNewStoreMakesAbsolute(t *testing.T) {
	s, err := NewStore("relative/sessions")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if !filepath.IsAbs(s.Root) {
		t.Errorf("Root not absolute: %q", s.Root)
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	s := Store{Root: t.TempDir()}
	created := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	in := sampleSession("sess1", created)

	if err := s.Write(in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out, err := s.Read("sess1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round trip mismatch:\n in = %+v\nout = %+v", in, out)
	}
}

func TestWriteDefaultsSocketPath(t *testing.T) {
	s := Store{Root: t.TempDir()}
	in := sampleSession("sess1", time.Now())
	in.SocketPath = "" // unset -> Write should default it to the store's socket path
	if err := s.Write(in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out, err := s.Read("sess1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if want := s.SocketPath("sess1"); out.SocketPath != want {
		t.Errorf("SocketPath = %q, want %q", out.SocketPath, want)
	}
}

// TestWriteIsAtomic asserts that every byte sequence ever visible at the
// session.json path decodes to a complete record — never a truncated one — even
// while a writer overwrites it. The temp-then-rename strategy guarantees the
// path only ever points at a fully written file.
func TestWriteIsAtomic(t *testing.T) {
	s := Store{Root: t.TempDir()}
	created := time.Now()
	if err := s.Write(sampleSession("sess1", created)); err != nil {
		t.Fatalf("seed Write: %v", err)
	}

	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		var firstErr error
		for {
			select {
			case <-stop:
				done <- firstErr
				return
			default:
			}
			data, err := os.ReadFile(s.Path("sess1"))
			if err != nil {
				// A missing file would mean the rename briefly exposed a gap;
				// it must never happen.
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			var sess Session
			if err := json.Unmarshal(data, &sess); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}()

	for i := 0; i < 200; i++ {
		rec := sampleSession("sess1", created)
		rec.PromptCount = i
		if err := s.Write(rec); err != nil {
			close(stop)
			t.Fatalf("Write iter %d: %v", i, err)
		}
	}
	close(stop)
	if err := <-done; err != nil {
		t.Fatalf("reader observed a partial/missing session.json: %v", err)
	}
}

func TestWriteNoTempLeftBehind(t *testing.T) {
	s := Store{Root: t.TempDir()}
	if err := s.Write(sampleSession("sess1", time.Now())); err != nil {
		t.Fatalf("Write: %v", err)
	}
	entries, err := os.ReadDir(s.Dir("sess1"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != FileName {
			t.Errorf("unexpected leftover file %q in session dir", e.Name())
		}
	}
}

func TestReadMissingIsNotExist(t *testing.T) {
	s := Store{Root: t.TempDir()}
	_, err := s.Read("ghost")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Read missing: err = %v, want fs.ErrNotExist", err)
	}
}

func TestListDiscoversAndSorts(t *testing.T) {
	s := Store{Root: t.TempDir()}
	base := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	// Written out of order; List must return oldest-first.
	if err := s.Write(sampleSession("b", base.Add(2*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(sampleSession("a", base)); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(sampleSession("c", base.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}

	got, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var ids []string
	for _, sess := range got {
		ids = append(ids, sess.ID)
	}
	if want := []string{"a", "c", "b"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("List order = %v, want %v", ids, want)
	}
}

func TestListTieBreaksByID(t *testing.T) {
	s := Store{Root: t.TempDir()}
	same := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	for _, id := range []string{"zeta", "alpha", "mike"} {
		if err := s.Write(sampleSession(id, same)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var ids []string
	for _, sess := range got {
		ids = append(ids, sess.ID)
	}
	if want := []string{"alpha", "mike", "zeta"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("tie-break order = %v, want %v", ids, want)
	}
}

func TestListSkipsNonSessionDirs(t *testing.T) {
	s := Store{Root: t.TempDir()}
	if err := s.Write(sampleSession("good", time.Now())); err != nil {
		t.Fatal(err)
	}
	// A directory with no session.json (e.g. created but not yet populated).
	if err := os.MkdirAll(filepath.Join(s.Root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory with a corrupt session.json.
	corrupt := filepath.Join(s.Root, "broken")
	if err := os.MkdirAll(corrupt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corrupt, FileName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A stray file at the root (not a directory).
	if err := os.WriteFile(filepath.Join(s.Root, "stray"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].ID != "good" {
		t.Errorf("List = %v, want only the good session", got)
	}
}

func TestListMissingRootIsEmpty(t *testing.T) {
	s := Store{Root: filepath.Join(t.TempDir(), "does-not-exist")}
	got, err := s.List()
	if err != nil {
		t.Fatalf("List missing root: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List = %v, want empty", got)
	}
}

func TestRemove(t *testing.T) {
	s := Store{Root: t.TempDir()}
	if err := s.Write(sampleSession("sess1", time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("sess1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(s.Dir("sess1")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("session dir still present after Remove: %v", err)
	}
	// Removing again is a no-op, not an error.
	if err := s.Remove("sess1"); err != nil {
		t.Errorf("Remove (second) = %v, want nil", err)
	}
}

func TestInvalidIDRejected(t *testing.T) {
	s := Store{Root: t.TempDir()}
	for _, id := range []string{"", "  ", ".", "..", "a/b", `a\b`} {
		if err := s.Write(Session{ID: id}); err == nil {
			t.Errorf("Write(id=%q) = nil, want error", id)
		}
		if _, err := s.Read(id); err == nil {
			t.Errorf("Read(id=%q) = nil, want error", id)
		}
		if err := s.Remove(id); err == nil {
			t.Errorf("Remove(id=%q) = nil, want error", id)
		}
	}
}

func TestWrittenJSONIsIndentedWithTrailingNewline(t *testing.T) {
	s := Store{Root: t.TempDir()}
	if err := s.Write(sampleSession("sess1", time.Now())); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.Path("sess1"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "\n  ") {
		t.Errorf("expected indented JSON, got: %s", data)
	}
	if !strings.HasSuffix(string(data), "}\n") {
		t.Errorf("expected trailing newline, got: %q", string(data[len(data)-3:]))
	}
}
