package audit

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSeqMonotonic(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)
	if got := l.LastSeq(); got != 0 {
		t.Fatalf("LastSeq before any line = %d, want 0", got)
	}
	l.Seed([]string{"2026-01-01T00:00:00Z old a=1", "2026-01-01T00:00:01Z old a=2"})
	for i := 0; i < 5; i++ {
		l.Log("e", "i", fmt.Sprint(i))
	}
	lines := l.RecentLines(100)
	if len(lines) != 7 {
		t.Fatalf("RecentLines len = %d, want 7", len(lines))
	}
	for i, ln := range lines {
		if ln.Seq != uint64(i+1) {
			t.Errorf("lines[%d].Seq = %d, want %d", i, ln.Seq, i+1)
		}
	}
	if got := l.LastSeq(); got != 7 {
		t.Errorf("LastSeq = %d, want 7", got)
	}
	// Recent and RecentLines agree on text.
	texts := l.Recent(100)
	for i := range lines {
		if texts[i] != lines[i].Text {
			t.Errorf("Recent[%d] = %q, RecentLines[%d].Text = %q", i, texts[i], i, lines[i].Text)
		}
	}
	if got := l.RecentLines(0); got != nil {
		t.Errorf("RecentLines(0) = %v, want nil", got)
	}
	if got := l.RecentLines(2); len(got) != 2 || got[0].Seq != 6 || got[1].Seq != 7 {
		t.Errorf("RecentLines(2) = %+v", got)
	}
}

func TestSeqNotWrittenToFile(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)
	l.now = func() time.Time { return time.Date(2026, 5, 12, 18, 4, 0, 0, time.UTC) }
	l.Log("a", "k", "v")
	l.Log("b")
	want := "2026-05-12T18:04:00Z a k=v\n2026-05-12T18:04:00Z b\n"
	if buf.String() != want {
		t.Errorf("file = %q, want %q", buf.String(), want)
	}
}

func TestSeqRingWrapAround(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)
	total := recentCap*2 + 37
	for i := 1; i <= total; i++ {
		l.Log("e", "i", fmt.Sprint(i))
	}
	lines := l.RecentLines(recentCap * 3)
	if len(lines) != recentCap {
		t.Fatalf("ring size = %d, want %d", len(lines), recentCap)
	}
	first := uint64(total - recentCap + 1)
	for i, ln := range lines {
		want := first + uint64(i)
		if ln.Seq != want {
			t.Fatalf("lines[%d].Seq = %d, want %d", i, ln.Seq, want)
		}
		if !strings.HasSuffix(ln.Text, fmt.Sprintf("i=%d", want)) {
			t.Fatalf("lines[%d] = %q does not match seq %d", i, ln.Text, want)
		}
	}
	if got := l.LastSeq(); got != uint64(total) {
		t.Errorf("LastSeq = %d, want %d", got, total)
	}

	// Resume from inside the ring.
	got, ok := l.Since(uint64(total - 3))
	if !ok || len(got) != 3 || got[0].Seq != uint64(total-2) || got[2].Seq != uint64(total) {
		t.Errorf("Since(total-3) = %+v ok=%v", got, ok)
	}
	// Resume from exactly one before the oldest held line: nothing lost.
	got, ok = l.Since(first - 1)
	if !ok || len(got) != recentCap || got[0].Seq != first {
		t.Errorf("Since(first-1): len=%d ok=%v", len(got), ok)
	}
	// Resume from a seq that has fallen out of the ring: gap reported.
	got, ok = l.Since(first - 2)
	if ok || len(got) != recentCap || got[0].Seq != first {
		t.Errorf("Since(first-2): len=%d ok=%v, want full ring and ok=false", len(got), ok)
	}
	// Up to date, or ahead.
	for _, s := range []uint64{uint64(total), uint64(total) + 10} {
		if got, ok := l.Since(s); !ok || got != nil {
			t.Errorf("Since(%d) = %+v ok=%v, want nil, true", s, got, ok)
		}
	}
}

func TestSinceEmpty(t *testing.T) {
	l := New(&bytes.Buffer{})
	if got, ok := l.Since(0); !ok || got != nil {
		t.Errorf("Since(0) on empty = %+v ok=%v", got, ok)
	}
	l.Log("e")
	if got, ok := l.Since(0); !ok || len(got) != 1 || got[0].Seq != 1 {
		t.Errorf("Since(0) = %+v ok=%v", got, ok)
	}
}

func TestSeqConcurrent(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)
	const writers, perWriter = 16, 100
	total := writers * perWriter

	var wg sync.WaitGroup
	stop := make(chan struct{})
	readerErr := make(chan string, 1)
	// A concurrent reader checks that every view of the ring is strictly
	// increasing by one and never goes backwards between reads.
	go func() {
		var last uint64
		for {
			select {
			case <-stop:
				close(readerErr)
				return
			default:
			}
			lines := l.RecentLines(recentCap)
			for i := 1; i < len(lines); i++ {
				if lines[i].Seq != lines[i-1].Seq+1 {
					readerErr <- fmt.Sprintf("non-contiguous seq %d -> %d", lines[i-1].Seq, lines[i].Seq)
					return
				}
			}
			if n := len(lines); n > 0 {
				if lines[n-1].Seq < last {
					readerErr <- fmt.Sprintf("seq went backwards %d -> %d", last, lines[n-1].Seq)
					return
				}
				last = lines[n-1].Seq
			}
		}
	}()
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				l.Log("e", "w", fmt.Sprint(w), "i", fmt.Sprint(i))
			}
		}(w)
	}
	wg.Wait()
	close(stop)
	if msg, ok := <-readerErr; ok {
		t.Fatal(msg)
	}

	if got := l.LastSeq(); got != uint64(total) {
		t.Errorf("LastSeq = %d, want %d", got, total)
	}
	if got := strings.Count(buf.String(), "\n"); got != total {
		t.Errorf("file lines = %d, want %d", got, total)
	}
	// The ring's order matches the file's order: seq k is the k-th line written.
	fileLines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	for _, ln := range l.RecentLines(recentCap) {
		if fileLines[ln.Seq-1] != ln.Text {
			t.Fatalf("seq %d: ring %q, file %q", ln.Seq, ln.Text, fileLines[ln.Seq-1])
		}
	}
}
