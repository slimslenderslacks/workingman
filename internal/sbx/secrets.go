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
	"slices"
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

// combined adapts a RunFunc (combined output) to an OutputFunc.
func combined(run RunFunc) OutputFunc {
	return func(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
		out, err := run(ctx, name, args...)
		return out, nil, err
	}
}

// secretList is the part of `sbx secret ls --json` we read.
type secretList struct {
	CustomSecrets []struct {
		Scope   string   `json:"scope"`
		Targets []string `json:"targets"`
	} `json:"custom_secrets"`
}

// DecodeSecretList parses `sbx secret ls --json` output, skipping anything
// sbx prints before the document: plain `WARN: ...` lines (seen when many
// sbx processes contend for its docker hub refresh lock) and JSON log lines
// that are not the secrets document. The document is the first JSON object,
// starting on its own line, with a "custom_secrets" or "secrets" key. Output
// without one is an error, never an empty list: a lone log line must not read
// as "no gateway secret".
func DecodeSecretList(out []byte) (secretList, error) {
	var data secretList
	for off := 0; off < len(out); {
		line := out[off:]
		next := len(out)
		if i := bytes.IndexByte(line, '\n'); i >= 0 {
			next = off + i + 1
		}
		if t := bytes.TrimLeft(line, " \t\r"); len(t) > 0 && t[0] == '{' {
			var doc map[string]json.RawMessage
			if json.NewDecoder(bytes.NewReader(t)).Decode(&doc) == nil {
				_, custom := doc["custom_secrets"]
				_, plain := doc["secrets"]
				if custom || plain {
					if raw, ok := doc["custom_secrets"]; ok {
						if err := json.Unmarshal(raw, &data.CustomSecrets); err != nil {
							return secretList{}, fmt.Errorf("decode custom_secrets: %w", err)
						}
					}
					return data, nil
				}
			}
		}
		off = next
	}
	return secretList{}, fmt.Errorf("no secrets document in output %q", truncate(out))
}

func truncate(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// gatewayScopes reports which of data's custom secrets targeting
// LLMGatewayHost apply to sandbox.
func gatewayScopes(data secretList, sandbox string) (global, scoped bool) {
	for _, s := range data.CustomSecrets {
		if !slices.Contains(s.Targets, LLMGatewayHost) {
			continue
		}
		switch s.Scope {
		case "global":
			global = true
		case sandbox:
			scoped = true
		}
	}
	return global, scoped
}

// SecretLs runs `sbx secret ls --json` robustly. When many sessions start at
// once, concurrent sbx processes contend for sbx's own docker hub refresh
// lock and a call can fail or print a warning in place of (or before) the
// document; SecretLs retries those with jittered exponential backoff and,
// when LockPath is set, serializes the invocation itself across processes on
// the host with an advisory file lock. The lock is held only while sbx runs,
// never across a backoff sleep, so nothing else about startup is serialized.
type SecretLs struct {
	Run      OutputFunc // nil: ExecOutput
	SbxPath  string     // "": "sbx"
	LockPath string     // "": no cross-process lock

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

// SecretLsLockPath is the lock file SecretLs callers share for the sessions
// root sessionsRoot: <state>/locks/sbx-secret-ls.lock, next to the gateways/
// and sandboxes/ state directories.
func SecretLsLockPath(sessionsRoot string) string {
	return filepath.Join(filepath.Dir(filepath.Clean(sessionsRoot)), "locks", "sbx-secret-ls.lock")
}

// GatewayScopes is GatewaySecretScopes with SecretLs's retries and lock.
// Only a failure on every attempt is an error; a document that lists no
// gateway secret is a successful (false, false).
func (q SecretLs) GatewayScopes(ctx context.Context, sandbox string) (global, scoped bool, err error) {
	data, err := q.List(ctx)
	if err != nil {
		return false, false, err
	}
	global, scoped = gatewayScopes(data, sandbox)
	return global, scoped, nil
}

// GatewayConfigured reports whether GatewayScopes finds either scope.
func (q SecretLs) GatewayConfigured(ctx context.Context, sandbox string) (bool, error) {
	global, scoped, err := q.GatewayScopes(ctx, sandbox)
	return global || scoped, err
}

// List runs `sbx secret ls --json` until it decodes, up to Attempts times.
func (q SecretLs) List(ctx context.Context) (secretList, error) {
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
			q.logf("sbx secret ls attempt %d/%d failed: %v; retrying in %s", i, attempts, err, d.Round(time.Millisecond))
			if serr := q.sleep(ctx, d); serr != nil {
				return secretList{}, fmt.Errorf("sbx secret ls: %w (last error: %v)", serr, err)
			}
			delay *= 2
		}
		var data secretList
		if data, err = q.once(ctx); err == nil {
			return data, nil
		}
		if ctx.Err() != nil {
			return secretList{}, fmt.Errorf("sbx secret ls: %w (last error: %v)", ctx.Err(), err)
		}
	}
	return secretList{}, fmt.Errorf("sbx secret ls failed after %d attempts: %w", attempts, err)
}

// once runs and decodes one `sbx secret ls --json`, under the lock.
func (q SecretLs) once(ctx context.Context) (secretList, error) {
	run := q.Run
	if run == nil {
		run = ExecOutput
	}
	bin := q.SbxPath
	if bin == "" {
		bin = "sbx"
	}
	unlock := q.lock(ctx)
	stdout, stderr, err := run(ctx, bin, "secret", "ls", "--json")
	unlock()
	if err != nil {
		return secretList{}, fmt.Errorf("%w: stdout %q, stderr %q", err, truncate(stdout), truncate(stderr))
	}
	data, err := DecodeSecretList(stdout)
	if err != nil {
		if s := truncate(stderr); s != "" {
			return secretList{}, fmt.Errorf("decode output: %w (stderr %q)", err, s)
		}
		return secretList{}, fmt.Errorf("decode output: %w", err)
	}
	return data, nil
}

// lock takes LockPath exclusively, polling so ctx and LockTimeout are
// honored. It never fails the call: if the lock cannot be had it logs why and
// returns a no-op unlock, and the caller runs sbx unlocked.
func (q SecretLs) lock(ctx context.Context) (unlock func()) {
	noop := func() {}
	if q.LockPath == "" {
		return noop
	}
	if err := os.MkdirAll(filepath.Dir(q.LockPath), 0o755); err != nil {
		q.logf("WARNING sbx secret ls lock: %v; proceeding unlocked", err)
		return noop
	}
	f, err := os.OpenFile(q.LockPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		q.logf("WARNING sbx secret ls lock: %v; proceeding unlocked", err)
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
			q.logf("WARNING sbx secret ls lock %s: %v; proceeding unlocked", q.LockPath, err)
			return noop
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			q.logf("WARNING sbx secret ls lock %s still held after %s; proceeding unlocked", q.LockPath, timeout)
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

func (q SecretLs) jitter(d time.Duration) time.Duration {
	if q.Jitter != nil {
		return q.Jitter(d)
	}
	return d/2 + rand.N(d)
}

func (q SecretLs) sleep(ctx context.Context, d time.Duration) error {
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

func (q SecretLs) logf(format string, args ...any) {
	if q.Logf != nil {
		q.Logf(format, args...)
	}
}
