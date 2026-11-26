package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ReadToken returns the token stored at path. A missing file wraps
// fs.ErrNotExist; an empty one is an error.
func ReadToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("api: read token: %w", err)
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", fmt.Errorf("api: token file %s is empty; run `orch api token --rotate`", path)
	}
	return tok, nil
}

// EnsureToken returns the token at path, creating a new random one (32
// bytes, base64url, mode 0600, in a 0700 directory) when there is none or
// when rotate is set. created reports whether a new token was written.
func EnsureToken(path string, rotate bool) (tok string, created bool, err error) {
	if !rotate {
		tok, err := ReadToken(path)
		if err == nil {
			return tok, false, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", false, err
		}
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", false, fmt.Errorf("api: %w", err)
	}
	tok = base64.RawURLEncoding.EncodeToString(b)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", false, fmt.Errorf("api: %w", err)
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return "", false, fmt.Errorf("api: %w", err)
	}
	_, err = f.WriteString(tok + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
		return "", false, fmt.Errorf("api: write token: %w", err)
	}
	return tok, true, nil
}

// tokenFile checks bearer tokens against the token file. It re-reads the
// file when its size or mtime changes, so `orch api token --rotate` takes
// effect on the next request.
type tokenFile struct {
	path string

	mu    sync.Mutex
	tok   []byte
	size  int64
	mtime time.Time
}

// current returns the token, reloading it if the file changed. On a read
// error it returns nil, which matches no request: a broken token file fails
// closed.
func (t *tokenFile) current() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	fi, err := os.Stat(t.path)
	if err != nil {
		t.tok = nil
		return nil
	}
	if t.tok != nil && fi.Size() == t.size && fi.ModTime().Equal(t.mtime) {
		return t.tok
	}
	tok, err := ReadToken(t.path)
	if err != nil {
		t.tok = nil
		return nil
	}
	t.tok, t.size, t.mtime = []byte(tok), fi.Size(), fi.ModTime()
	return t.tok
}

// check reports whether the Authorization header carries the token.
func (t *tokenFile) check(header string) bool {
	tok := t.current()
	if len(tok) == 0 {
		return false
	}
	got, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), tok) == 1
}
