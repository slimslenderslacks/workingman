package audit

import (
	"bufio"
	"errors"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"
)

// Event is one parsed audit line.
type Event struct {
	Time   time.Time
	Name   string
	Fields map[string]string
}

// ParseLine parses a line written by Logger.Log. ok is false for a line that
// doesn't start with an RFC3339 timestamp followed by an event name — such
// lines (stray output, a torn write) are skipped by callers rather than
// reported as events.
func ParseLine(line string) (Event, bool) {
	line = strings.TrimRight(line, "\r\n")
	tsEnd := strings.IndexByte(line, ' ')
	if tsEnd <= 0 {
		return Event{}, false
	}
	ts, err := time.Parse(time.RFC3339, line[:tsEnd])
	if err != nil {
		return Event{}, false
	}
	rest := line[tsEnd+1:]
	name := rest
	if i := strings.IndexByte(rest, ' '); i >= 0 {
		name, rest = rest[:i], rest[i+1:]
	} else {
		rest = ""
	}
	if name == "" {
		return Event{}, false
	}
	ev := Event{Time: ts, Name: name, Fields: map[string]string{}}
	for rest != "" {
		rest = strings.TrimLeft(rest, " ")
		eq := strings.IndexByte(rest, '=')
		if eq <= 0 {
			break
		}
		key := rest[:eq]
		rest = rest[eq+1:]
		var val string
		if strings.HasPrefix(rest, `"`) {
			end := quotedEnd(rest)
			if end < 0 {
				val, rest = rest, ""
			} else {
				if u, err := strconv.Unquote(rest[:end]); err == nil {
					val = u
				} else {
					val = rest[:end]
				}
				rest = rest[end:]
			}
		} else if sp := strings.IndexByte(rest, ' '); sp >= 0 {
			val, rest = rest[:sp], rest[sp:]
		} else {
			val, rest = rest, ""
		}
		ev.Fields[key] = val
	}
	return ev, true
}

// quotedEnd returns the index just past the closing quote of the Go-quoted
// string at the start of s, or -1 if it is unterminated.
func quotedEnd(s string) int {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return -1
}

// tailChunk is how far back from the end of the file TailFile reads at a time.
const tailChunk = 64 * 1024

// TailFile returns up to n of the last lines of the file at path (oldest
// first, no trailing newline). A missing file is not an error — it yields no
// lines — since a daemon that hasn't logged yet has simply nothing to report.
// Only the tail of the file is read, so it stays cheap on a large audit log.
func TailFile(path string, n int) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}

	// Read backwards in chunks until we've seen n newlines (plus one more to
	// know the first line is whole) or hit the start of the file.
	size := info.Size()
	var buf []byte
	pos := size
	for pos > 0 && countNewlines(buf) <= n {
		step := int64(tailChunk)
		if step > pos {
			step = pos
		}
		pos -= step
		chunk := make([]byte, step)
		if _, err := f.ReadAt(chunk, pos); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		buf = append(chunk, buf...)
	}

	var lines []string
	sc := bufio.NewScanner(strings.NewReader(string(buf)))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	// When we stopped mid-file the first line is probably a fragment.
	if pos > 0 && len(lines) > 0 {
		lines = lines[1:]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

func countNewlines(b []byte) int {
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	return n
}
