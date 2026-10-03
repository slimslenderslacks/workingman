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

	recent []string // ring of the last recentCap lines, oldest first
	onLog  func()
}

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
	return append([]string(nil), l.recent[len(l.recent)-n:]...)
}

func (l *Logger) push(line string) {
	if len(l.recent) >= recentCap {
		copy(l.recent, l.recent[1:])
		l.recent = l.recent[:len(l.recent)-1]
	}
	l.recent = append(l.recent, line)
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
