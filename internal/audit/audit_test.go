package audit

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLogFormat(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)
	l.now = func() time.Time { return time.Date(2026, 5, 12, 18, 4, 0, 0, time.UTC) }
	l.Log("project_updated", "path", "/x/.project.yaml", "status", "ready")
	got := buf.String()
	want := "2026-05-12T18:04:00Z project_updated path=/x/.project.yaml status=ready\n"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestQuoteValuesWithSpaces(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)
	l.Log("err", "msg", "no such file or directory")
	if !strings.Contains(buf.String(), `msg="no such file or directory"`) {
		t.Errorf("expected quoted value, got %q", buf.String())
	}
}

func TestConcurrentLogs(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); l.Log("evt", "i", "x") }()
	}
	wg.Wait()
	if got := strings.Count(buf.String(), "\n"); got != 50 {
		t.Errorf("got %d lines, want 50", got)
	}
}
