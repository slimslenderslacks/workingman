package postoffice

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Default poll intervals (§3.3, §5.2).
const (
	DefaultOutboxPoll = 2 * time.Second
	DefaultInboxPoll  = 5 * time.Second
)

// Watch reports changes to some of the store's directories on the returned
// channel, until ctx is done (the channel is then closed). dirs are store
// subdirectories (DirInbox, DirOutbox, DirDone, DirDeliveries); DirDone
// includes its day directories.
//
// Changes come from fsnotify and, as a fallback for missed or unsupported
// events, from polling each directory's listing every poll (zero means
// DefaultOutboxPoll). Each value is the store subdirectory that changed.
// Bursts are coalesced: at most one value per directory is pending, so a
// slow consumer sees "inbox changed", not every file. Consumers rescan the
// directory; a value is a hint, not a diff. Staging (.tmp) writes are
// ignored. When fsnotify is unavailable, Watch polls only.
func (s *Store) Watch(ctx context.Context, poll time.Duration, dirs ...string) <-chan string {
	if poll <= 0 {
		poll = DefaultOutboxPoll
	}
	w := &watcher{
		store:   s,
		dirs:    dirs,
		out:     make(chan string, len(dirs)),
		pending: map[string]bool{},
		snaps:   map[string]string{},
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		s.logf("postoffice: fsnotify unavailable, polling only: %v", err)
		fsw = nil
	}
	w.fsw = fsw
	for _, d := range dirs {
		w.addFS(d)
		w.snaps[d] = w.snapshot(d)
	}
	go w.run(ctx, poll)
	return w.out
}

type watcher struct {
	store *Store
	dirs  []string
	fsw   *fsnotify.Watcher
	out   chan string

	mu      sync.Mutex
	pending map[string]bool // dirs signalled but not yet delivered

	snaps map[string]string // poll snapshots, owned by run
}

// addFS adds dir (and, for done/, its day directories) to fsnotify.
func (w *watcher) addFS(dir string) {
	if w.fsw == nil {
		return
	}
	p := w.store.Dir(dir)
	_ = w.fsw.Add(p)
	if dir == DirDone {
		days, _ := w.store.doneDays()
		for _, d := range days {
			_ = w.fsw.Add(filepath.Join(p, d))
		}
	}
}

func (w *watcher) run(ctx context.Context, poll time.Duration) {
	defer close(w.out)
	if w.fsw != nil {
		defer w.fsw.Close()
	}
	var events <-chan fsnotify.Event
	var errs <-chan error
	if w.fsw != nil {
		events, errs = w.fsw.Events, w.fsw.Errors
	}
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if d, ok := w.classify(ev); ok {
				w.signal(d)
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			// Overflow and similar: the poll catches up, but signal every
			// directory now so nothing waits a full interval.
			w.store.logf("postoffice: fsnotify: %v", err)
			for _, d := range w.dirs {
				w.signal(d)
			}
		case <-tick.C:
			for _, d := range w.dirs {
				if snap := w.snapshot(d); snap != w.snaps[d] {
					w.snaps[d] = snap
					w.signal(d)
				}
			}
		}
		w.flush(ctx)
	}
}

// classify maps an fsnotify event to the watched store directory it
// belongs to, adding new done/ day directories to the watch.
func (w *watcher) classify(ev fsnotify.Event) (string, bool) {
	rel, err := filepath.Rel(w.store.Root, ev.Name)
	if err != nil {
		return "", false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 2 || strings.HasPrefix(parts[len(parts)-1], ".") {
		return "", false // the root itself, or a .tmp / .bad entry
	}
	for _, p := range parts[1:] {
		if strings.HasPrefix(p, ".") {
			return "", false
		}
	}
	top := parts[0]
	watched := false
	for _, d := range w.dirs {
		watched = watched || d == top
	}
	if !watched {
		return "", false
	}
	if top == DirDone && len(parts) == 2 && ev.Has(fsnotify.Create) && w.fsw != nil {
		if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
			_ = w.fsw.Add(ev.Name)
		}
	}
	return top, true
}

func (w *watcher) signal(dir string) {
	w.mu.Lock()
	w.pending[dir] = true
	w.mu.Unlock()
}

// flush delivers pending signals without blocking on a slow consumer; what
// cannot be delivered stays pending for the next loop turn.
func (w *watcher) flush(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, d := range w.dirs {
		if !w.pending[d] {
			continue
		}
		select {
		case w.out <- d:
			delete(w.pending, d)
		case <-ctx.Done():
			return
		default:
			// The consumer has unread values; this one stays pending and
			// goes out on a later turn (at the latest the next poll tick).
		}
	}
}

// snapshot is a cheap fingerprint of a directory listing: names, sizes and
// modification times (and those of done/ day directories).
func (w *watcher) snapshot(dir string) string {
	var b strings.Builder
	var walk func(p string, depth int)
	walk = func(p string, depth int) {
		entries, err := os.ReadDir(p)
		if err != nil {
			return
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			b.WriteString(e.Name())
			b.WriteByte(' ')
			b.WriteString(info.ModTime().Format(time.RFC3339Nano))
			b.WriteByte(' ')
			b.WriteString(strconv.FormatInt(info.Size(), 10))
			b.WriteByte('\n')
			if e.IsDir() && depth > 0 {
				walk(filepath.Join(p, e.Name()), depth-1)
			}
		}
	}
	depth := 0
	if dir == DirDone {
		depth = 1
	}
	walk(w.store.Dir(dir), depth)
	return b.String()
}

// Waiters wakes in-process goroutines blocked on a message id, such as
// post-office's wait_for_response calls, when that message is finalised.
// The zero value is ready to use.
type Waiters struct {
	mu sync.Mutex
	m  map[string]chan struct{}
}

// Wait returns a channel that is closed at the next Wake for id. Take the
// channel before checking the store, then select on it, so a finalisation
// between the check and the wait is not missed.
func (w *Waiters) Wait(id string) <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.m == nil {
		w.m = map[string]chan struct{}{}
	}
	ch, ok := w.m[id]
	if !ok {
		ch = make(chan struct{})
		w.m[id] = ch
	}
	return ch
}

// Wake releases every Wait on id.
func (w *Waiters) Wake(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if ch, ok := w.m[id]; ok {
		close(ch)
		delete(w.m, id)
	}
}

// WakeAll releases every Wait on any id (for example after a poll found
// changes it cannot attribute).
func (w *Waiters) WakeAll() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for id, ch := range w.m {
		close(ch)
		delete(w.m, id)
	}
}
