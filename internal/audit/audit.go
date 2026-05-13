package audit

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// Logger appends timestamped key/value lines to an io.Writer. The writer is
// not closed by the Logger — callers own the file/buffer lifetime.
type Logger struct {
	mu  sync.Mutex
	w   io.Writer
	now func() time.Time
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
	l.mu.Lock()
	defer l.mu.Unlock()
	ts := l.now().UTC().Format(time.RFC3339)
	fmt.Fprintf(l.w, "%s %s", ts, event)
	for i := 0; i+1 < len(kv); i += 2 {
		fmt.Fprintf(l.w, " %s=%s", kv[i], quoteIfNeeded(kv[i+1]))
	}
	fmt.Fprintln(l.w)
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
