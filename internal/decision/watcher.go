package decision

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Pending is a parsed request together with the inbox file it came from.
type Pending struct {
	Request Request
	// Filename is the bare file name inside the inbox; pass it to
	// [ArchiveRequest] once the request is answered.
	Filename string
	// Path is the full path of the request file.
	Path string
}

// FileError is a failure to read or parse one inbox file.
type FileError struct {
	Filename string
	Path     string
	Err      error
}

func (e *FileError) Error() string { return fmt.Sprintf("%s: %v", e.Filename, e.Err) }
func (e *FileError) Unwrap() error { return e.Err }

// Scan loads every candidate request file currently in inboxDir (see
// [IsCandidateFile]); subdirectories such as `answered/` are skipped. Files
// that cannot be read or parsed are reported as [*FileError]s and left out
// of the result. The result is sorted with [SortPending]. A non-nil error
// list does not mean the scan failed: the valid requests are still returned.
func Scan(inboxDir string) ([]Pending, []error) {
	entries, err := os.ReadDir(inboxDir)
	if err != nil {
		return nil, []error{fmt.Errorf("scan inbox %s: %w", inboxDir, err)}
	}
	var (
		out  []Pending
		errs []error
	)
	for _, e := range entries {
		if !e.Type().IsRegular() || !IsCandidateFile(e.Name()) {
			continue
		}
		p, _, err := readPending(inboxDir, e.Name())
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // removed since ReadDir
			}
			errs = append(errs, err)
			continue
		}
		out = append(out, p)
	}
	SortPending(out)
	return out, errs
}

// SortPending orders requests by created_at (oldest first; requests without
// created_at sort after those with one), then by file name.
func SortPending(ps []Pending) {
	sort.SliceStable(ps, func(i, j int) bool {
		a, b := ps[i].Request.CreatedAt, ps[j].Request.CreatedAt
		switch {
		case a != nil && b != nil && !a.Equal(*b):
			return a.Before(*b)
		case a != nil && b == nil:
			return true
		case a == nil && b != nil:
			return false
		}
		return ps[i].Filename < ps[j].Filename
	})
}

// readPending reads and parses one inbox file, also returning its raw bytes.
// A missing file is returned as an error wrapping fs.ErrNotExist.
func readPending(inboxDir, name string) (Pending, []byte, error) {
	path := filepath.Join(inboxDir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		return Pending{}, nil, &FileError{Filename: name, Path: path, Err: err}
	}
	r, err := ParseRequest(data)
	if err != nil {
		return Pending{}, data, &FileError{Filename: name, Path: path, Err: err}
	}
	return Pending{Request: r, Filename: name, Path: path}, data, nil
}

// EventKind says what happened to an inbox file.
type EventKind int

const (
	// EventAdded: a valid request appeared (or became valid).
	EventAdded EventKind = iota + 1
	// EventUpdated: an already-reported request file changed.
	EventUpdated
	// EventRemoved: a request file left the inbox (deleted, renamed away
	// or archived).
	EventRemoved
	// EventError: a file could not be read or parsed, or the watcher
	// itself reported an error (Filename is then empty).
	EventError
)

func (k EventKind) String() string {
	switch k {
	case EventAdded:
		return "added"
	case EventUpdated:
		return "updated"
	case EventRemoved:
		return "removed"
	case EventError:
		return "error"
	}
	return fmt.Sprintf("EventKind(%d)", int(k))
}

// Event is one change seen by [Watch].
type Event struct {
	Kind     EventKind
	Filename string
	Path     string
	// Pending is set for EventAdded and EventUpdated.
	Pending *Pending
	// Err is set for EventError.
	Err error
}

// Tuning for Watch. Variables so tests can shorten them.
var (
	// watchDebounce is how long a file must be quiet after its last event
	// before it is parsed.
	watchDebounce = 100 * time.Millisecond
	// watchRetryDelay and watchRetries control how often a file that fails
	// to parse is re-read before an EventError is reported. Any new write
	// to the file restarts the cycle.
	watchRetryDelay = 150 * time.Millisecond
	watchRetries    = 3
)

// Watch watches inboxDir for request files being created, written, renamed
// in, renamed away or removed, and sends an [Event] for each change on the
// returned channel. Events for a file are debounced so a file written in
// several chunks is parsed once it is complete; a file that still fails to
// parse after a few retries yields an EventError, and a later write retries
// it again. Hidden, temp and non-JSON files and subdirectories are ignored.
//
// Watch does not report files already in the inbox; call [Scan] after Watch
// returns so no file is missed between the two. Files present when Watch
// starts are reported as EventUpdated if they later change.
//
// The channel is closed after ctx is cancelled.
func Watch(ctx context.Context, inboxDir string) (<-chan Event, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("watch inbox %s: %w", inboxDir, err)
	}
	if err := fw.Add(inboxDir); err != nil {
		fw.Close()
		return nil, fmt.Errorf("watch inbox %s: %w", inboxDir, err)
	}
	w := &inboxWatcher{
		dir:   inboxDir,
		fw:    fw,
		out:   make(chan Event, 16),
		files: make(map[string]*watchedFile),
	}
	// Seed files already present so later writes report EventUpdated and
	// removals report EventRemoved.
	if entries, err := os.ReadDir(inboxDir); err == nil {
		for _, e := range entries {
			if e.Type().IsRegular() && IsCandidateFile(e.Name()) {
				w.files[e.Name()] = &watchedFile{reported: true}
			}
		}
	}
	go w.run(ctx)
	return w.out, nil
}

type watchedFile struct {
	// reported: an EventAdded was sent (or the file pre-existed) and no
	// EventRemoved since.
	reported bool
	// errored: the last parse attempt sent an EventError.
	errored bool
	// last holds the bytes of the last reported version, to drop no-op
	// updates (e.g. chmod or touch).
	last []byte
	// due is when the file should next be parsed; zero if nothing pending.
	due      time.Time
	attempts int
}

type inboxWatcher struct {
	dir   string
	fw    *fsnotify.Watcher
	out   chan Event
	files map[string]*watchedFile
}

func (w *inboxWatcher) run(ctx context.Context) {
	defer close(w.out)
	defer w.fw.Close()

	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()

	for {
		w.resetTimer(timer)
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.fw.Events:
			if !ok {
				return
			}
			if !w.handleFS(ctx, ev) {
				return
			}
		case err, ok := <-w.fw.Errors:
			if !ok {
				return
			}
			if !w.send(ctx, Event{Kind: EventError, Err: fmt.Errorf("watch inbox %s: %w", w.dir, err)}) {
				return
			}
		case <-timer.C:
			if !w.processDue(ctx) {
				return
			}
		}
	}
}

// resetTimer arms timer for the earliest pending parse, or stops it.
func (w *inboxWatcher) resetTimer(timer *time.Timer) {
	var next time.Time
	for _, f := range w.files {
		if !f.due.IsZero() && (next.IsZero() || f.due.Before(next)) {
			next = f.due
		}
	}
	timer.Stop()
	if !next.IsZero() {
		timer.Reset(time.Until(next))
	}
}

func (w *inboxWatcher) handleFS(ctx context.Context, ev fsnotify.Event) bool {
	name := filepath.Base(ev.Name)
	if filepath.Dir(ev.Name) != filepath.Clean(w.dir) || !IsCandidateFile(name) {
		return true
	}
	switch {
	case ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename):
		// Rename is reported for the old name; the new name, if still in
		// the inbox, arrives as a separate Create.
		return w.removed(ctx, name)
	case ev.Has(fsnotify.Create) || ev.Has(fsnotify.Write):
		f := w.files[name]
		if f == nil {
			f = &watchedFile{}
			w.files[name] = f
		}
		f.due = time.Now().Add(watchDebounce)
		f.attempts = 0
	}
	return true
}

func (w *inboxWatcher) removed(ctx context.Context, name string) bool {
	f := w.files[name]
	if f == nil {
		return true
	}
	delete(w.files, name)
	if !f.reported && !f.errored {
		return true // never surfaced; nothing to retract
	}
	return w.send(ctx, Event{Kind: EventRemoved, Filename: name, Path: filepath.Join(w.dir, name)})
}

func (w *inboxWatcher) processDue(ctx context.Context) bool {
	now := time.Now()
	var due []string
	for name, f := range w.files {
		if !f.due.IsZero() && !f.due.After(now) {
			due = append(due, name)
		}
	}
	sort.Strings(due)
	for _, name := range due {
		if !w.parse(ctx, name) {
			return false
		}
	}
	return true
}

func (w *inboxWatcher) parse(ctx context.Context, name string) bool {
	f := w.files[name]
	f.due = time.Time{}
	p, data, err := readPending(w.dir, name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return w.removed(ctx, name)
		}
		f.attempts++
		if f.attempts <= watchRetries {
			// Possibly a partial write; try again shortly.
			f.due = time.Now().Add(watchRetryDelay)
			return true
		}
		f.attempts = 0
		if f.errored && bytes.Equal(data, f.last) {
			return true // same broken content already reported
		}
		f.errored = true
		f.last = data
		path := filepath.Join(w.dir, name)
		return w.send(ctx, Event{Kind: EventError, Filename: name, Path: path, Err: err})
	}
	f.attempts = 0
	if f.reported && !f.errored && f.last != nil && bytes.Equal(data, f.last) {
		return true // content unchanged
	}
	kind := EventAdded
	if f.reported {
		kind = EventUpdated
	}
	f.reported = true
	f.errored = false
	f.last = data
	return w.send(ctx, Event{Kind: kind, Filename: name, Path: p.Path, Pending: &p})
}

func (w *inboxWatcher) send(ctx context.Context, ev Event) bool {
	select {
	case w.out <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}
