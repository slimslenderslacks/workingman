package audit

import (
	"bytes"
	"fmt"
	"io"
	"sync"
	"time"
)

// recentCap bounds the in-memory tail of audit lines the Logger keeps for
// Recent. It only has to cover the largest "last N events" a state snapshot
// asks for; the full history lives in the audit file itself.
const recentCap = 500

// Logger appends timestamped key/value lines to an io.Writer. The writer is
// not closed by the Logger — callers own the file/buffer lifetime.
//
// It also keeps the most recent lines in memory (see Recent) so the daemon's
// state snapshot can report "what just happened" without re-reading the file.
type Logger struct {
	mu  sync.Mutex
	w   io.Writer
	now func() time.Time

	recent []Line // ring of the last recentCap lines, oldest first
	seq    uint64 // seq of the most recently pushed line; 0 before any
	onLog  func()
}

// Line is one entry of the in-memory tail: the audit line as written (without
// the trailing newline) and its sequence number. Seq is assigned by the Logger
// when the line enters the tail — 1 for the first line, then +1 for every
// logged or seeded line — so it is monotonically increasing within one Logger
// (one daemon process). It is not written to the audit file.
type Line struct {
	Seq  uint64
	Text string
}

// New returns a Logger that appends to w.
func New(w io.Writer) *Logger {
	return &Logger{w: w, now: time.Now}
}

// Log writes one line of the form:
//
//	2026-05-12T18:04:00Z event key=val key=val
//
// Values containing spaces are quoted.
func (l *Logger) Log(event string, kv ...string) {
	var b bytes.Buffer
	l.mu.Lock()
	ts := l.now().UTC().Format(time.RFC3339)
	fmt.Fprintf(&b, "%s %s", ts, event)
	for i := 0; i+1 < len(kv); i += 2 {
		fmt.Fprintf(&b, " %s=%s", kv[i], quoteIfNeeded(kv[i+1]))
	}
	b.WriteByte('\n')
	_, _ = l.w.Write(b.Bytes())
	l.push(b.String()[:b.Len()-1])
	cb := l.onLog
	l.mu.Unlock()
	if cb != nil {
		cb()
	}
}

// OnLog registers fn to be called (outside the Logger's lock) after every
// logged line. The daemon uses it as a cheap "something happened" signal to
// schedule a state-snapshot refresh; fn must not block and must not log.
func (l *Logger) OnLog(fn func()) {
	l.mu.Lock()
	l.onLog = fn
	l.mu.Unlock()
}

// Seed pre-loads the in-memory tail with lines that were already in the audit
// file before this Logger started (oldest first), so Recent is useful right
// after a daemon restart. It does not write to the underlying writer.
func (l *Logger) Seed(lines []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, ln := range lines {
		l.push(ln)
	}
}

// Recent returns up to n of the most recent audit lines, oldest first, without
// the trailing newline. n <= 0 returns nil.
func (l *Logger) Recent(n int) []string {
	if n <= 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if n > len(l.recent) {
		n = len(l.recent)
	}
	out := make([]string, n)
	for i, ln := range l.recent[len(l.recent)-n:] {
		out[i] = ln.Text
	}
	return out
}

// RecentLines is Recent with each line's sequence number. n <= 0 returns nil.
func (l *Logger) RecentLines(n int) []Line {
	if n <= 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if n > len(l.recent) {
		n = len(l.recent)
	}
	return append([]Line(nil), l.recent[len(l.recent)-n:]...)
}

// Since returns the lines in the in-memory tail with Seq > seq, oldest first,
// so a consumer that has seen up to seq can resume. ok is false when lines
// after seq have already fallen out of the ring (seq is older than the tail
// can cover); the returned lines are then everything still held, and the
// caller has to fall back to the file or a full refresh. A seq at or beyond
// LastSeq returns no lines and ok true.
func (l *Logger) Since(seq uint64) (lines []Line, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.recent) == 0 {
		return nil, seq >= l.seq
	}
	first := l.recent[0].Seq
	if seq+1 < first {
		return append([]Line(nil), l.recent...), false
	}
	if seq >= l.seq {
		return nil, true
	}
	return append([]Line(nil), l.recent[seq+1-first:]...), true
}

// LastSeq returns the sequence number of the most recent line, or 0 if
// nothing has been logged or seeded yet.
func (l *Logger) LastSeq() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seq
}

func (l *Logger) push(line string) {
	if len(l.recent) >= recentCap {
		copy(l.recent, l.recent[1:])
		l.recent = l.recent[:len(l.recent)-1]
	}
	l.seq++
	l.recent = append(l.recent, Line{Seq: l.seq, Text: line})
}

func quoteIfNeeded(s string) string {
	for _, r := range s {
		if r == ' ' || r == '"' {
			return fmt.Sprintf("%q", s)
		}
	}
	if s == "" {
		return `""`
	}
	return s
}
