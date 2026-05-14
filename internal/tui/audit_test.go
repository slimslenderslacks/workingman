package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadTailMissingFileReturnsNil(t *testing.T) {
	if got := readTail(filepath.Join(t.TempDir(), "nope.log"), 10); got != nil {
		t.Errorf("missing file should return nil, got %v", got)
	}
}

func TestReadTailReturnsLastNLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.log")
	if err := os.WriteFile(path, []byte("a\nb\nc\nd\ne\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := readTail(path, 3)
	if strings.Join(got, "|") != "c|d|e" {
		t.Errorf("got %v, want [c d e]", got)
	}
}

func TestReadTailHandlesShortFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.log")
	if err := os.WriteFile(path, []byte("only one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := readTail(path, 10)
	if strings.Join(got, "|") != "only one" {
		t.Errorf("got %v, want [only one]", got)
	}
}

func TestReadTailDropsPartialLeadingLine(t *testing.T) {
	// File larger than tailWindow so we seek into the middle of a line.
	dir := t.TempDir()
	path := filepath.Join(dir, "a.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// Write 40 KiB of "X" then a newline then three known lines.
	pad := strings.Repeat("X", 40*1024) + "\n"
	if _, err := f.WriteString(pad); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"first", "second", "third"} {
		if _, err := f.WriteString(line + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()

	got := readTail(path, 10)
	// The partial X-line at the start must be dropped; only the trailing
	// three intact lines should make it through.
	joined := strings.Join(got, "|")
	if !strings.HasSuffix(joined, "first|second|third") {
		t.Errorf("expected trailing lines, got %q", joined)
	}
	for _, line := range got {
		if strings.HasPrefix(line, "X") {
			t.Errorf("partial leading X-line not dropped: %q", line)
		}
	}
}

func TestTailAuditEmitsFirstSnapshotImmediately(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	if err := os.WriteFile(path, []byte("alpha\nbravo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := TailAudit(ctx, path, 50*time.Millisecond, 5)
	select {
	case lines := <-ch:
		if len(lines) != 2 || lines[0] != "alpha" || lines[1] != "bravo" {
			t.Errorf("first emission = %v, want [alpha bravo]", lines)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("no initial emission within 1s")
	}
}

func TestTailAuditEmitsOnChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	if err := os.WriteFile(path, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := TailAudit(ctx, path, 50*time.Millisecond, 5)
	<-ch // initial snapshot

	// Append a line; expect a second emission.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("second\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	select {
	case lines := <-ch:
		joined := strings.Join(lines, "|")
		if !strings.Contains(joined, "second") {
			t.Errorf("post-append snapshot = %v, want it to contain 'second'", lines)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("no follow-up emission after file change")
	}
}

func TestTailAuditClosesOnCtxCancel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	_ = os.WriteFile(path, []byte("x\n"), 0o644)

	ctx, cancel := context.WithCancel(context.Background())
	ch := TailAudit(ctx, path, 50*time.Millisecond, 5)
	<-ch
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			// One late emission can be in-flight; consume it then check the
			// next read sees the close.
			select {
			case _, ok2 := <-ch:
				if ok2 {
					t.Error("channel still open after cancel + drain")
				}
			case <-time.After(500 * time.Millisecond):
				t.Error("channel not closed within 500ms of cancel")
			}
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("channel not closed within 500ms of cancel")
	}
}
