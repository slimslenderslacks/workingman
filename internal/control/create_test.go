package control

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestValidProjectName(t *testing.T) {
	for _, ok := range []string{"gui-wireframes", "a", "x1-y2", strings.Repeat("a", 63)} {
		if err := ValidProjectName(ok); err != nil {
			t.Errorf("ValidProjectName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "..", ".worktrees", "a/b", "../x", "Has-Caps", "a_b", "-a", "a-", "a--b", "with space", strings.Repeat("a", 64)} {
		err := ValidProjectName(bad)
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("ValidProjectName(%q) = %v, want ErrInvalid", bad, err)
		}
	}
}

func TestProjectNameWarning(t *testing.T) {
	if w := ProjectNameWarning("gui"); w != "" {
		t.Errorf("short name warns: %q", w)
	}
	if w := ProjectNameWarning(strings.Repeat("a", 60)); !strings.Contains(w, "will be shortened") {
		t.Errorf("long name: %q, want a shortening warning", w)
	}
	if w := ProjectNameWarning(strings.Repeat("a", 45)); !strings.Contains(w, "characters for task names") {
		t.Errorf("tight name: %q, want a task-name budget warning", w)
	}
}

func TestNewProjectWritesSeed(t *testing.T) {
	root := t.TempDir()
	path, err := NewProject(root, "gui-wireframes", "  Draw SVG wireframes\n\n")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "gui-wireframes", "project.md"); path != want {
		t.Errorf("path = %s, want %s", path, want)
	}
	if got := readFile(t, path); got != "Draw SVG wireframes\n" {
		t.Errorf("project.md = %q", got)
	}
	// Only project.md: no project.yaml (the daemon writes it), no temp file.
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("project dir holds %d files, want 1: %v", len(entries), entries)
	}
}

func TestNewProjectRefusals(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "taken"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		root, name, desc string
		want             error
	}{
		{root, "taken", "x", ErrExists},
		{root, "Bad Name", "x", ErrInvalid},
		{root, "../escape", "x", ErrInvalid},
		{root, "fine", "   \n", ErrInvalid},
		{filepath.Join(root, "missing"), "fine", "x", ErrNotFound},
	} {
		if _, err := NewProject(tc.root, tc.name, tc.desc); !errors.Is(err, tc.want) {
			t.Errorf("NewProject(%q, %q): %v, want %v", tc.root, tc.name, err, tc.want)
		}
	}
	// A refusal leaves nothing behind.
	if _, err := os.Stat(filepath.Join(root, "fine")); !os.IsNotExist(err) {
		t.Errorf("a refused create left a directory: %v", err)
	}
}

func TestNewProjectConcurrentCreatesOneWins(t *testing.T) {
	root := t.TempDir()
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = NewProject(root, "race", "desc")
		}()
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case !errors.Is(err, ErrExists):
			t.Errorf("loser: %v, want ErrExists", err)
		}
	}
	if won != 1 {
		t.Errorf("%d creates won, want 1", won)
	}
}

func intakeProject(t *testing.T) string {
	t.Helper()
	return writeYAML(t, t.TempDir(), "widget", "status: working\n")
}

func TestQueueIntakeWritesPendingFile(t *testing.T) {
	path := intakeProject(t)
	got, err := QueueIntake(path, "add-card", "Add a sandbox card")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(filepath.Dir(path), "intake", "add-card.md"); got != want {
		t.Errorf("path = %s, want %s", got, want)
	}
	if body := readFile(t, got); body != "Add a sandbox card\n" {
		t.Errorf("body = %q", body)
	}
	// Nothing but the request is left in intake/: in particular no temp file
	// ending in .md, which the daemon would take for a request.
	entries, _ := os.ReadDir(IntakeDir(path))
	if len(entries) != 1 {
		t.Errorf("intake/ holds %v, want only add-card.md", entries)
	}
}

func TestQueueIntakeRefusals(t *testing.T) {
	path := intakeProject(t)
	if _, err := QueueIntake(path, "dup", "first"); err != nil {
		t.Fatal(err)
	}
	dir := IntakeDir(path)
	if err := os.WriteFile(filepath.Join(dir, "done.processed"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gone.withdrawn"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path, name, body string
		want             error
	}{
		{path, "dup", "second", ErrExists},
		{path, "done", "again", ErrExists},
		{path, "gone", "again", ErrExists},
		{path, "x.md", "body", ErrInvalid},
		{path, "../escape", "body", ErrInvalid},
		{path, "", "body", ErrInvalid},
		{path, "blank", " \n\t", ErrInvalid},
		{filepath.Join(t.TempDir(), "nope", "project.yaml"), "fine", "body", ErrNotFound},
	} {
		if _, err := QueueIntake(tc.path, tc.name, tc.body); !errors.Is(err, tc.want) {
			t.Errorf("QueueIntake(%q, %q): %v, want %v", tc.name, tc.body, err, tc.want)
		}
	}
	for status, want := range map[string]string{"stopped": "is stopped", "": "has no status yet"} {
		p := writeYAML(t, t.TempDir(), "widget", "description: x\nstatus: "+status+"\n")
		_, err := QueueIntake(p, "req", "body")
		if !errors.Is(err, ErrState) || !strings.Contains(err.Error(), want) {
			t.Errorf("status %q: %v, want ErrState %q", status, err, want)
		}
		if _, err := os.Stat(IntakeDir(p)); !os.IsNotExist(err) {
			t.Errorf("status %q: a refused queue made intake/", status)
		}
	}
	if body := readFile(t, filepath.Join(dir, "dup.md")); body != "first\n" {
		t.Errorf("a refused queue overwrote the request: %q", body)
	}
}

func TestDefaultIntakeName(t *testing.T) {
	now := time.Date(2026, 11, 26, 21, 57, 0, 0, time.UTC)
	n := DefaultIntakeName("gui", now)
	if !strings.HasPrefix(n, "gui-20261126-215700-") || len(n) != len("gui-20261126-215700-")+4 {
		t.Errorf("DefaultIntakeName = %q", n)
	}
	if err := ValidIntakeName(n); err != nil {
		t.Errorf("default name is not valid: %v", err)
	}
}

func TestWithdrawIntake(t *testing.T) {
	path := intakeProject(t)
	pending, err := QueueIntake(path, "oops", "never mind")
	if err != nil {
		t.Fatal(err)
	}
	// A failing check refuses and leaves the request pending.
	stale := errors.New("stale")
	if _, _, err := WithdrawIntake(path, "oops", func([]byte) error { return stale }); !errors.Is(err, stale) {
		t.Fatalf("check refusal: %v", err)
	}
	if _, err := os.Stat(pending); err != nil {
		t.Fatalf("refused withdraw moved the file: %v", err)
	}
	var seen string
	got, noop, err := WithdrawIntake(path, "oops", func(b []byte) error { seen = string(b); return nil })
	if err != nil || noop {
		t.Fatalf("withdraw: %v noop=%v", err, noop)
	}
	if seen != "never mind\n" {
		t.Errorf("check saw %q", seen)
	}
	if want := filepath.Join(IntakeDir(path), "oops.withdrawn"); got != want {
		t.Errorf("path = %s, want %s", got, want)
	}
	if _, err := os.Stat(pending); !os.IsNotExist(err) {
		t.Errorf("pending file still there: %v", err)
	}
	// Again: a no-op.
	if _, noop, err := WithdrawIntake(path, "oops", nil); err != nil || !noop {
		t.Errorf("second withdraw: noop=%v err=%v, want a no-op", noop, err)
	}
}

func TestWithdrawIntakeRefusals(t *testing.T) {
	path := intakeProject(t)
	dir := IntakeDir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "taken.processed"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := WithdrawIntake(path, "taken", nil); !errors.Is(err, ErrProcessed) {
		t.Errorf("processed: %v, want ErrProcessed", err)
	}
	if _, _, err := WithdrawIntake(path, "missing", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v, want ErrNotFound", err)
	}
	if _, _, err := WithdrawIntake(path, "../project", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("bad name: %v, want ErrNotFound", err)
	}
	// A pending file beside an old .withdrawn of the same name is not
	// clobbered.
	for _, f := range []string{"both.md", "both.withdrawn"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(f+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := WithdrawIntake(path, "both", nil); !errors.Is(err, ErrExists) {
		t.Errorf("both: %v, want ErrExists", err)
	}
	if got := readFile(t, filepath.Join(dir, "both.withdrawn")); got != "both.withdrawn\n" {
		t.Errorf("old .withdrawn overwritten: %q", got)
	}
}
