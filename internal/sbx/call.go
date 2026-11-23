package sbx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// OutputFunc runs one command and returns its stdout and stderr separately,
// so a caller can parse stdout without the warnings sbx prints on stderr.
type OutputFunc func(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)

// ExecOutput is the production OutputFunc.
func ExecOutput(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// Combined adapts a RunFunc (combined output) to an OutputFunc, for test
// seams that script sbx as a RunFunc.
func Combined(run RunFunc) OutputFunc {
	return func(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
		out, err := run(ctx, name, args...)
		return out, nil, err
	}
}

// Client runs sbx commands whose stdout is a JSON document (`sbx ls --json`,
// `sbx secret ls --json`) robustly. When many sessions start at once,
// concurrent sbx processes contend for sbx's own docker hub refresh lock and a
// call can fail, or print a `WARN: ...` line (or a JSON log line) before or
// after the document on stdout. Client decodes stdout only, skipping such
// lines, retries a non-zero exit or an undecodable output with jittered
// exponential backoff and, when LockPath is set, serializes the invocation
// itself across processes on the host with an advisory file lock. The lock is
// held only while sbx runs, never across a backoff sleep, so nothing else
// about startup is serialized.
type Client struct {
	Run      OutputFunc // nil: ExecOutput
	SbxPath  string     // "": "sbx"
	LockPath string     // "": no cross-process lock (see LockPath, DefaultLockPath)

	Attempts int           // 0: 5
	Backoff  time.Duration // first retry delay, doubling each retry; 0: 200ms
	// LockTimeout bounds the wait for LockPath; past it the call proceeds
	// unlocked. 0: 30s.
	LockTimeout time.Duration

	// Sleep and Jitter are test seams. Sleep waits d or until ctx is done
	// (nil: a timer); Jitter randomizes a nominal delay (nil: d/2 + [0, d)).
	Sleep  func(ctx context.Context, d time.Duration) error
	Jitter func(d time.Duration) time.Duration
	// Logf reports each retry and lock problem (nil: discarded).
	Logf func(format string, args ...any)
}

// LockPath is the lock file every Client sharing the sessions root
// sessionsRoot uses: <state>/locks/sbx.lock, next to the gateways/ and
// sandboxes/ state directories. One lock covers every JSON-parsed sbx call,
// since they all contend for the same sbx-side lock.
func LockPath(sessionsRoot string) string {
	return filepath.Join(filepath.Dir(filepath.Clean(sessionsRoot)), "locks", "sbx.lock")
}

// DefaultLockPath is LockPath for the default sessions root
// (~/.workingman/sessions), i.e. ~/.workingman/locks/sbx.lock: the lock for
// callers that have no sessions root at hand. Without a home directory it
// falls back to workingman-sbx.lock in the temp dir.
func DefaultLockPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "workingman-sbx.lock")
	}
	return LockPath(filepath.Join(home, ".workingman", "sessions"))
}

// Call runs `sbx <args...>` until decode accepts its stdout, up to Attempts
// times. decode sees stdout only; stderr is quoted in errors. On exhaustion
// the error names the command ("sbx ls --json failed after 5 attempts: ...").
func (q Client) Call(ctx context.Context, decode func(stdout []byte) error, args ...string) error {
	name := "sbx " + strings.Join(args, " ")
	attempts := q.Attempts
	if attempts <= 0 {
		attempts = 5
	}
	delay := q.Backoff
	if delay <= 0 {
		delay = 200 * time.Millisecond
	}
	var err error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			d := q.jitter(delay)
			q.logf("%s attempt %d/%d failed: %v; retrying in %s", name, i, attempts, err, d.Round(time.Millisecond))
			if serr := q.sleep(ctx, d); serr != nil {
				return fmt.Errorf("%s: %w (last error: %v)", name, serr, err)
			}
			delay *= 2
		}
		if err = q.once(ctx, decode, args); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("%s: %w (last error: %v)", name, ctx.Err(), err)
		}
	}
	if attempts == 1 {
		return fmt.Errorf("%s: %w", name, err)
	}
	return fmt.Errorf("%s failed after %d attempts: %w", name, attempts, err)
}

// once runs and decodes one invocation, under the lock.
func (q Client) once(ctx context.Context, decode func([]byte) error, args []string) error {
	run := q.Run
	if run == nil {
		run = ExecOutput
	}
	bin := q.SbxPath
	if bin == "" {
		bin = "sbx"
	}
	unlock := q.lock(ctx)
	stdout, stderr, err := run(ctx, bin, args...)
	unlock()
	if err != nil {
		return fmt.Errorf("%w: stdout %q, stderr %q", err, truncate(stdout), truncate(stderr))
	}
	if err := decode(stdout); err != nil {
		if s := truncate(stderr); s != "" {
			return fmt.Errorf("decode output: %w (stderr %q)", err, s)
		}
		return fmt.Errorf("decode output: %w", err)
	}
	return nil
}

// firstJSON returns the first JSON value in out that starts its own line
// (after leading blanks) and that accept takes, decoding only that value: the
// lines before it (plain `WARN: ...` lines, JSON log lines accept rejects) and
// anything after it are ignored. A value may span lines (pretty-printed).
func firstJSON(out []byte, accept func(raw json.RawMessage) bool) (json.RawMessage, bool) {
	for off := 0; off < len(out); {
		line := out[off:]
		next := len(out)
		if i := bytes.IndexByte(line, '\n'); i >= 0 {
			next = off + i + 1
		}
		if t := bytes.TrimLeft(line, " \t\r"); len(t) > 0 && (t[0] == '{' || t[0] == '[') {
			var raw json.RawMessage
			if json.NewDecoder(bytes.NewReader(t)).Decode(&raw) == nil && accept(raw) {
				return raw, true
			}
		}
		off = next
	}
	return nil, false
}

// hasKey reports whether raw is a JSON object with any of keys.
func hasKey(raw json.RawMessage, keys ...string) bool {
	var doc map[string]json.RawMessage
	if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &doc) != nil {
		return false
	}
	for _, k := range keys {
		if _, ok := doc[k]; ok {
			return true
		}
	}
	return false
}

func truncate(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// lock takes LockPath exclusively, polling so ctx and LockTimeout are
// honored. It never fails the call: if the lock cannot be had it logs why and
// returns a no-op unlock, and the caller runs sbx unlocked.
func (q Client) lock(ctx context.Context) (unlock func()) {
	noop := func() {}
	if q.LockPath == "" {
		return noop
	}
	if err := os.MkdirAll(filepath.Dir(q.LockPath), 0o755); err != nil {
		q.logf("WARNING sbx lock: %v; proceeding unlocked", err)
		return noop
	}
	f, err := os.OpenFile(q.LockPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		q.logf("WARNING sbx lock: %v; proceeding unlocked", err)
		return noop
	}
	timeout := q.LockTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			q.logf("WARNING sbx lock %s: %v; proceeding unlocked", q.LockPath, err)
			return noop
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			q.logf("WARNING sbx lock %s still held after %s; proceeding unlocked", q.LockPath, timeout)
			return noop
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return noop // the run that follows fails on ctx itself
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (q Client) jitter(d time.Duration) time.Duration {
	if q.Jitter != nil {
		return q.Jitter(d)
	}
	return d/2 + rand.N(d)
}

func (q Client) sleep(ctx context.Context, d time.Duration) error {
	if q.Sleep != nil {
		return q.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (q Client) logf(format string, args ...any) {
	if q.Logf != nil {
		q.Logf(format, args...)
	}
}
