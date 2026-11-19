package decision

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func init() {
	watchDebounce = 20 * time.Millisecond
	watchRetryDelay = 30 * time.Millisecond
}

func requestJSON(id, createdAt string) string {
	meta := ""
	if createdAt != "" {
		meta = fmt.Sprintf(`,"created_at":%q`, createdAt)
	}
	return fmt.Sprintf(`{"id":%q,"title":"T %s","summary":"s","choices":[{"id":"a","label":"A"}]%s}`, id, id, meta)
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScan(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "b.json", requestJSON("b", ""))
	writeFile(t, dir, "a.json", requestJSON("a", ""))
	writeFile(t, dir, "late.json", requestJSON("late", "2026-11-19T12:00:00Z"))
	writeFile(t, dir, "early.json", requestJSON("early", "2026-11-19T10:00:00Z"))
	writeFile(t, dir, "bad.json", `{"id": "bad",`)
	writeFile(t, dir, "invalid.json", `{"id":"x"}`)
	writeFile(t, dir, ".hidden.json", requestJSON("hidden", ""))
	writeFile(t, dir, "c.json.tmp", requestJSON("tmp", ""))
	writeFile(t, dir, "notes.txt", "hello")
	if err := os.Mkdir(filepath.Join(dir, AnsweredDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, AnsweredDirName), "old.json", requestJSON("old", ""))
	if err := os.Mkdir(filepath.Join(dir, "dir.json"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, errs := Scan(dir)
	var ids []string
	for _, p := range got {
		ids = append(ids, p.Request.ID)
		if p.Path != filepath.Join(dir, p.Filename) {
			t.Errorf("Path = %q, Filename = %q", p.Path, p.Filename)
		}
	}
	if want := "early,late,a,b"; strings.Join(ids, ",") != want {
		t.Fatalf("ids = %v, want %s", ids, want)
	}
	if len(errs) != 2 {
		t.Fatalf("errs = %v, want 2", errs)
	}
	var names []string
	for _, err := range errs {
		var fe *FileError
		if !errors.As(err, &fe) {
			t.Fatalf("error %v is not a *FileError", err)
		}
		names = append(names, fe.Filename)
	}
	if strings.Join(names, ",") != "bad.json,invalid.json" {
		t.Fatalf("error files = %v", names)
	}
}

func TestScanMissingDir(t *testing.T) {
	got, errs := Scan(filepath.Join(t.TempDir(), "nope"))
	if len(got) != 0 || len(errs) != 1 {
		t.Fatalf("Scan missing dir = %v, %v", got, errs)
	}
}

func startWatch(t *testing.T, dir string) <-chan Event {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := Watch(ctx, dir)
	if err != nil {
		cancel()
		t.Fatalf("Watch: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		deadline := time.After(2 * time.Second)
		for {
			select {
			case _, ok := <-ch:
				if !ok {
					return
				}
			case <-deadline:
				t.Error("watch channel not closed after cancel")
				return
			}
		}
	})
	return ch
}

func nextEvent(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("watch channel closed")
		}
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for watch event")
	}
	return Event{}
}

func expectNoEvent(t *testing.T, ch <-chan Event, d time.Duration) {
	t.Helper()
	select {
	case ev := <-ch:
		t.Fatalf("unexpected event %v %s: %v", ev.Kind, ev.Filename, ev.Err)
	case <-time.After(d):
	}
}

func TestWatchCreate(t *testing.T) {
	dir := t.TempDir()
	ch := startWatch(t, dir)
	writeFile(t, dir, "new.json", requestJSON("new", ""))
	ev := nextEvent(t, ch)
	if ev.Kind != EventAdded || ev.Filename != "new.json" || ev.Pending == nil || ev.Pending.Request.ID != "new" {
		t.Fatalf("event = %+v", ev)
	}
	expectNoEvent(t, ch, 200*time.Millisecond)
}

func TestWatchAtomicRename(t *testing.T) {
	dir := t.TempDir()
	ch := startWatch(t, dir)
	tmp := filepath.Join(dir, ".req.json.123.tmp")
	if err := os.WriteFile(tmp, []byte(requestJSON("req", "")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "req.json")); err != nil {
		t.Fatal(err)
	}
	ev := nextEvent(t, ch)
	if ev.Kind != EventAdded || ev.Filename != "req.json" || ev.Pending.Request.ID != "req" {
		t.Fatalf("event = %+v", ev)
	}
	expectNoEvent(t, ch, 200*time.Millisecond)
}

func TestWatchRenameFromOutside(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(t.TempDir(), "req.json")
	if err := os.WriteFile(src, []byte(requestJSON("req", "")), 0o644); err != nil {
		t.Fatal(err)
	}
	ch := startWatch(t, dir)
	if err := os.Rename(src, filepath.Join(dir, "req.json")); err != nil {
		t.Fatal(err)
	}
	ev := nextEvent(t, ch)
	if ev.Kind != EventAdded || ev.Pending.Request.ID != "req" {
		t.Fatalf("event = %+v", ev)
	}
}

func TestWatchChunkedWrite(t *testing.T) {
	dir := t.TempDir()
	ch := startWatch(t, dir)
	data := requestJSON("chunked", "")
	f, err := os.Create(filepath.Join(dir, "chunked.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	half := len(data) / 2
	if _, err := f.WriteString(data[:half]); err != nil {
		t.Fatal(err)
	}
	// Pause longer than the debounce but shorter than the retry budget.
	time.Sleep(50 * time.Millisecond)
	if _, err := f.WriteString(data[half:]); err != nil {
		t.Fatal(err)
	}
	ev := nextEvent(t, ch)
	if ev.Kind != EventAdded || ev.Pending == nil || ev.Pending.Request.ID != "chunked" {
		t.Fatalf("event = %+v (err %v)", ev, ev.Err)
	}
}

func TestWatchMalformed(t *testing.T) {
	dir := t.TempDir()
	ch := startWatch(t, dir)
	writeFile(t, dir, "bad.json", `{"id": "bad",`)
	ev := nextEvent(t, ch)
	if ev.Kind != EventError || ev.Filename != "bad.json" || ev.Err == nil {
		t.Fatalf("event = %+v", ev)
	}
	if !strings.Contains(ev.Err.Error(), "malformed") {
		t.Fatalf("err = %v", ev.Err)
	}
	expectNoEvent(t, ch, 300*time.Millisecond)

	// Fixing the file reports it as added.
	writeFile(t, dir, "bad.json", requestJSON("bad", ""))
	ev = nextEvent(t, ch)
	if ev.Kind != EventAdded || ev.Pending.Request.ID != "bad" {
		t.Fatalf("event after fix = %+v", ev)
	}
}

func TestWatchIgnoresTempFiles(t *testing.T) {
	dir := t.TempDir()
	ch := startWatch(t, dir)
	writeFile(t, dir, ".hidden.json", requestJSON("h", ""))
	writeFile(t, dir, "x.json.tmp", requestJSON("x", ""))
	writeFile(t, dir, "y.part", "garbage")
	writeFile(t, dir, "notes.txt", "garbage")
	if err := os.Mkdir(filepath.Join(dir, AnsweredDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, AnsweredDirName), "z.json", requestJSON("z", ""))
	expectNoEvent(t, ch, 300*time.Millisecond)
}

func TestWatchRemoveAndArchive(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "old.json", requestJSON("old", ""))
	ch := startWatch(t, dir)

	writeFile(t, dir, "new.json", requestJSON("new", ""))
	if ev := nextEvent(t, ch); ev.Kind != EventAdded || ev.Filename != "new.json" {
		t.Fatalf("event = %+v", ev)
	}
	if err := os.Remove(filepath.Join(dir, "new.json")); err != nil {
		t.Fatal(err)
	}
	if ev := nextEvent(t, ch); ev.Kind != EventRemoved || ev.Filename != "new.json" {
		t.Fatalf("event = %+v", ev)
	}

	// A file present before Watch started is reported when archived.
	if err := ArchiveRequest(dir, "old.json"); err != nil {
		t.Fatal(err)
	}
	if ev := nextEvent(t, ch); ev.Kind != EventRemoved || ev.Filename != "old.json" {
		t.Fatalf("event = %+v", ev)
	}
	expectNoEvent(t, ch, 200*time.Millisecond)
}

func TestWatchUpdate(t *testing.T) {
	dir := t.TempDir()
	ch := startWatch(t, dir)
	writeFile(t, dir, "r.json", requestJSON("r", ""))
	if ev := nextEvent(t, ch); ev.Kind != EventAdded {
		t.Fatalf("event = %+v", ev)
	}
	writeFile(t, dir, "r.json", requestJSON("r", "2026-11-19T10:00:00Z"))
	ev := nextEvent(t, ch)
	if ev.Kind != EventUpdated || ev.Pending.Request.CreatedAt == nil {
		t.Fatalf("event = %+v", ev)
	}
}

func TestWatchCancelCloses(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := Watch(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected closed channel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel not closed after cancel")
	}
}

func TestWatchMissingDir(t *testing.T) {
	if _, err := Watch(context.Background(), filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected error")
	}
}
