package pathlock

import (
	"path/filepath"
	"sync"
)

// entry is one path's mutex plus the number of goroutines holding or waiting
// for it, so the entry can be dropped from the table once nobody needs it.
type entry struct {
	mu   sync.Mutex
	refs int
}

var (
	tableMu sync.Mutex
	table   = map[string]*entry{}
)

// Lock blocks until the calling goroutine holds the write lock for path, and
// returns the function that releases it. Call the returned function exactly
// once, typically with defer.
func Lock(path string) (unlock func()) {
	key := Key(path)
	tableMu.Lock()
	e := table[key]
	if e == nil {
		e = &entry{}
		table[key] = e
	}
	e.refs++
	tableMu.Unlock()

	e.mu.Lock()
	var once sync.Once
	return func() {
		once.Do(func() {
			e.mu.Unlock()
			tableMu.Lock()
			e.refs--
			if e.refs == 0 {
				delete(table, key)
			}
			tableMu.Unlock()
		})
	}
}

// Key returns the lock key for path: its absolute, cleaned form, or the
// cleaned form alone when the working directory can't be determined.
func Key(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return filepath.Clean(path)
}

// entries reports how many paths currently have an entry in the table. Tests
// use it to check that released locks don't accumulate.
func entries() int {
	tableMu.Lock()
	defer tableMu.Unlock()
	return len(table)
}
