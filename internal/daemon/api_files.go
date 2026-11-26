package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"sync"
)

// fileCache parses files on demand and keeps each result until the file's
// mtime or size changes, the same stat signature the snapshot collector
// caches projects on. The API reads the task and project fields a snapshot
// leaves out through it, so a request re-parses only what changed. A cached
// value is shared by every caller and must not be modified: callers copy
// what they need into a resource and redact that.
type fileCache[T any] struct {
	parse func(path string, data []byte) (T, error)

	mu sync.Mutex
	m  map[string]cachedFile[T]
}

type cachedFile[T any] struct {
	sig  string
	val  T
	etag string
	err  error
}

func newFileCache[T any](parse func(path string, data []byte) (T, error)) *fileCache[T] {
	return &fileCache[T]{parse: parse, m: map[string]cachedFile[T]{}}
}

// get returns the parsed file at path and the etag of its bytes. A missing
// file returns the os error (errors.Is fs.ErrNotExist) and forgets the entry.
// A parse error is cached like a value, until the file changes.
func (c *fileCache[T]) get(path string) (T, string, error) {
	var zero T
	// Stat before reading: a write that lands in between leaves the cached
	// signature older than the content, so the next get re-reads, and never
	// the other way round.
	var b strings.Builder
	statSig(&b, path)
	sig := b.String()
	c.mu.Lock()
	if sig == ":missing" {
		delete(c.m, path)
		c.mu.Unlock()
		_, err := os.Stat(path)
		return zero, "", err
	}
	if e, ok := c.m[path]; ok && e.sig == sig {
		c.mu.Unlock()
		return e.val, e.etag, e.err
	}
	c.mu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		return zero, "", err
	}
	val, perr := c.parse(path, data)
	e := cachedFile[T]{sig: sig, val: val, etag: contentTag(data), err: perr}
	c.mu.Lock()
	c.m[path] = e
	c.mu.Unlock()
	return e.val, e.etag, e.err
}

// contentTag is the short hash of a file's bytes that a resource's etag is
// built from (§A.4.1: an etag is a hash of the file's bytes).
func contentTag(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:4])
}

// resourceETag renders a resource etag, quoted like an HTTP entity tag:
// "pj-9f1c2e7a" for a project, "tk-..." for a task, "in-..." for intake.
func resourceETag(prefix, tag string) string {
	if tag == "" {
		return ""
	}
	return `"` + prefix + "-" + tag + `"`
}
