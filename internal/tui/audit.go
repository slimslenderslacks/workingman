package tui

import (
	"bytes"
	"context"
	"io"
	"os"
	"time"
)

// TailAudit polls path on interval and emits the last n lines whenever the
// snapshot changes. The first emission happens immediately. The channel
// closes when ctx is cancelled. interval <= 0 falls back to 250ms; n <= 0
// falls back to 8.
//
// Tail is best-effort: a missing file produces an empty snapshot rather
// than an error, and read errors during polling silently keep the previous
// snapshot. The TUI is a display surface, not the canonical audit reader —
// callers who need precision should read the file directly.
//
// The poller reads only the trailing window (tailWindow bytes), so growth
// of a very large audit log doesn't slow the TUI down.
func TailAudit(ctx context.Context, path string, interval time.Duration, n int) <-chan []string {
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	if n <= 0 {
		n = 8
	}
	out := make(chan []string)
	go func() {
		defer close(out)
		emit := func(lines []string) bool {
			select {
			case out <- lines:
				return true
			case <-ctx.Done():
				return false
			}
		}
		var prev []string
		first := readTail(path, n)
		prev = first
		if !emit(first) {
			return
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				curr := readTail(path, n)
				if linesEqual(prev, curr) {
					continue
				}
				prev = curr
				if !emit(curr) {
					return
				}
			}
		}
	}()
	return out
}

// tailWindow is the byte tail we read when scanning the file. 32 KiB easily
// holds the last several dozen audit lines but won't blow up memory if the
// log has grown to many megabytes.
const tailWindow int64 = 32 * 1024

func readTail(path string, n int) []string {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	size := st.Size()
	offset := int64(0)
	if size > tailWindow {
		offset = size - tailWindow
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	// If we seeked into the middle of a line, drop the partial leading line.
	if offset > 0 {
		if nl := bytes.IndexByte(data, '\n'); nl >= 0 {
			data = data[nl+1:]
		}
	}
	// Trim trailing newline so the final split entry isn't empty.
	data = bytes.TrimRight(data, "\n")
	if len(data) == 0 {
		return nil
	}
	all := bytes.Split(data, []byte{'\n'})
	if len(all) > n {
		all = all[len(all)-n:]
	}
	out := make([]string, len(all))
	for i, line := range all {
		out[i] = string(line)
	}
	return out
}

func linesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
