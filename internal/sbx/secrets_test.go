package sbx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const globalSecret = `{"secrets":[],"custom_secrets":[{"scope":"global","targets":["llm.es.atko.services"]}]}`

func TestDecodeSecretListSkipsLeadingNoise(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want bool
	}{
		{"plain", globalSecret, true},
		{"plain warn line", "WARN: docker hub refresh lock held by another process: context deadline exceeded\n" + globalSecret, true},
		{"json log line", `{"level":"warn","msg":"docker hub refresh lock held by another process: context deadline exceeded"}` + "\n" + globalSecret, true},
		{"several lines", "WARN: one\n\n{\"level\":\"warn\",\"msg\":\"two\"}\n  " + globalSecret + "\n", true},
		{"pretty printed", "WARN: x\n{\n  \"custom_secrets\": [\n    {\"scope\": \"global\", \"targets\": [\"llm.es.atko.services\"]}\n  ]\n}\n", true},
		{"empty document", "WARN: x\n" + `{"secrets":[],"custom_secrets":[]}`, false},
		{"only secrets key", `{"secrets":[]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := DecodeSecretList([]byte(tc.out))
			if err != nil {
				t.Fatal(err)
			}
			if global, _ := gatewayScopes(data, "s"); global != tc.want {
				t.Errorf("global = %v, want %v", global, tc.want)
			}
		})
	}
	for _, bad := range []string{
		"",
		"WARN: docker hub refresh lock held by another process: context deadline exceeded",
		`{"level":"warn","msg":"x"}`,
		`{}`,
		"WARN: x\n" + `{"custom_secrets":`,
	} {
		if _, err := DecodeSecretList([]byte(bad)); err == nil {
			t.Errorf("DecodeSecretList(%q) succeeded, want an error", bad)
		}
	}
}

// script returns an OutputFunc answering successive calls from results,
// repeating the last one, and counts the calls.
type result struct {
	stdout, stderr string
	err            error
}

func script(calls *int32, results ...result) OutputFunc {
	return func(context.Context, string, ...string) ([]byte, []byte, error) {
		i := int(atomic.AddInt32(calls, 1)) - 1
		r := results[min(i, len(results)-1)]
		return []byte(r.stdout), []byte(r.stderr), r.err
	}
}

func TestSecretLsRetriesTransientFailures(t *testing.T) {
	var calls int32
	var slept []time.Duration
	var logs []string
	q := SecretLs{
		Run: script(&calls,
			result{stderr: "WARN: lock", err: errors.New("exit status 1")},
			result{stdout: "WARN: docker hub refresh lock held by another process"},
			result{stdout: globalSecret},
		),
		Sleep:  func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil },
		Jitter: func(d time.Duration) time.Duration { return d },
		Logf:   func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
	}
	ok, err := q.GatewayConfigured(context.Background(), "s")
	if err != nil || !ok {
		t.Fatalf("GatewayConfigured = %v, %v; want true", ok, err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
	if want := []time.Duration{200 * time.Millisecond, 400 * time.Millisecond}; fmt.Sprint(slept) != fmt.Sprint(want) {
		t.Errorf("slept %v, want %v", slept, want)
	}
	if len(logs) != 2 || !strings.Contains(logs[0], "WARN: lock") {
		t.Errorf("logs = %q, want one per retry quoting the failure", logs)
	}
}

func TestSecretLsExhaustion(t *testing.T) {
	var calls int32
	var slept []time.Duration
	q := SecretLs{
		Run:    script(&calls, result{stdout: "", stderr: "WARN: docker hub refresh lock held", err: errors.New("exit status 1")}),
		Sleep:  func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil },
		Jitter: func(d time.Duration) time.Duration { return d },
	}
	_, _, err := q.GatewayScopes(context.Background(), "s")
	if err == nil || !strings.Contains(err.Error(), "sbx secret ls failed after 5 attempts") || !strings.Contains(err.Error(), "docker hub refresh lock held") {
		t.Fatalf("err = %v, want the exhaustion error quoting stderr", err)
	}
	if calls != 5 {
		t.Errorf("calls = %d, want 5", calls)
	}
	if want := "[200ms 400ms 800ms 1.6s]"; fmt.Sprint(slept) != want {
		t.Errorf("slept %v, want %s", slept, want)
	}
}

func TestSecretLsHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls int32
	q := SecretLs{
		Run:   script(&calls, result{stdout: "WARN: x"}),
		Sleep: func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() },
	}
	if _, err := q.List(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no attempt after cancellation)", calls)
	}
}

func TestSecretLsDefaultJitter(t *testing.T) {
	q := SecretLs{}
	for range 100 {
		if d := q.jitter(200 * time.Millisecond); d < 100*time.Millisecond || d >= 300*time.Millisecond {
			t.Fatalf("jitter(200ms) = %s, want within [100ms, 300ms)", d)
		}
	}
}

// Concurrent callers sharing a lock file never run sbx at the same time.
func TestSecretLsLockSerializesGoroutines(t *testing.T) {
	lock := SecretLsLockPath(filepath.Join(t.TempDir(), "state", "sessions"))
	var inside, overlaps, calls int32
	run := func(context.Context, string, ...string) ([]byte, []byte, error) {
		atomic.AddInt32(&calls, 1)
		if atomic.AddInt32(&inside, 1) > 1 {
			atomic.AddInt32(&overlaps, 1)
		}
		time.Sleep(5 * time.Millisecond)
		atomic.AddInt32(&inside, -1)
		return []byte(globalSecret), nil, nil
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			q := SecretLs{Run: run, LockPath: lock}
			if ok, err := q.GatewayConfigured(context.Background(), "s"); err != nil || !ok {
				t.Errorf("GatewayConfigured = %v, %v", ok, err)
			}
		})
	}
	wg.Wait()
	if calls != 10 || overlaps != 0 {
		t.Errorf("calls = %d, overlaps = %d; want 10 calls, none overlapping", calls, overlaps)
	}
}

// The same across processes: helper subprocesses contend on one lock file
// and each records its [start, end] inside the lock; no two intervals overlap.
func TestSecretLsLockSerializesProcesses(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "locks", "sbx-secret-ls.lock")
	const n = 6
	cmds := make([]*exec.Cmd, n)
	for i := range cmds {
		cmd := exec.Command(os.Args[0], "-test.run=^TestSecretLsLockHelper$")
		cmd.Env = append(os.Environ(), "SECRET_LS_LOCK_HELPER="+lock, "SECRET_LS_LOCK_OUT="+filepath.Join(dir, fmt.Sprintf("out-%d", i)))
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds[i] = cmd
	}
	type span struct{ start, end int64 }
	var spans []span
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("helper %d: %v", i, err)
		}
		data, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("out-%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		var s span
		if _, err := fmt.Sscan(string(data), &s.start, &s.end); err != nil {
			t.Fatalf("helper %d output %q: %v", i, data, err)
		}
		spans = append(spans, s)
	}
	for i := range spans {
		for j := i + 1; j < len(spans); j++ {
			if spans[i].start < spans[j].end && spans[j].start < spans[i].end {
				t.Errorf("helpers %d %v and %d %v ran sbx secret ls concurrently", i, spans[i], j, spans[j])
			}
		}
	}
}

func TestSecretLsLockHelper(t *testing.T) {
	lock := os.Getenv("SECRET_LS_LOCK_HELPER")
	if lock == "" {
		t.Skip("subprocess helper for TestSecretLsLockSerializesProcesses")
	}
	var start, end int64
	q := SecretLs{LockPath: lock, Run: func(context.Context, string, ...string) ([]byte, []byte, error) {
		start = time.Now().UnixNano()
		time.Sleep(20 * time.Millisecond)
		end = time.Now().UnixNano()
		return []byte(globalSecret), nil, nil
	}}
	if _, err := q.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("SECRET_LS_LOCK_OUT"), fmt.Appendf(nil, "%d %d", start, end), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A lock that cannot be taken never fails the call: it runs unlocked.
func TestSecretLsLockFailureProceedsUnlocked(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var logs []string
	var calls int32
	q := SecretLs{
		Run:      script(&calls, result{stdout: globalSecret}),
		LockPath: filepath.Join(file, "locks", "x.lock"),
		Logf:     func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
	}
	if ok, err := q.GatewayConfigured(context.Background(), "s"); err != nil || !ok {
		t.Fatalf("GatewayConfigured = %v, %v; want true", ok, err)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "proceeding unlocked") {
		t.Errorf("logs = %q, want one 'proceeding unlocked' warning", logs)
	}
}

// The lock is never held across a backoff sleep, and one held past
// LockTimeout is skipped rather than waited on forever.
func TestSecretLsLockTimeoutAndNotHeldWhileSleeping(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "x.lock")
	other := SecretLs{LockPath: lock}
	var calls int32
	q := SecretLs{
		Run:      script(&calls, result{stdout: "WARN: x"}, result{stdout: globalSecret}),
		LockPath: lock,
		Sleep: func(context.Context, time.Duration) error {
			other.lockOrFail(t)() // another process can take it meanwhile
			return nil
		},
	}
	if _, err := q.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}

	unlock := other.lockOrFail(t)
	defer unlock()
	var logs []string
	q = SecretLs{Run: script(new(int32), result{stdout: globalSecret}), LockPath: lock, LockTimeout: 50 * time.Millisecond,
		Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }}
	if _, err := q.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "still held") {
		t.Errorf("logs = %q, want one 'still held' warning", logs)
	}
}

// lockOrFail takes q's lock, failing the test if it falls back to unlocked.
func (q SecretLs) lockOrFail(t *testing.T) func() {
	t.Helper()
	failed := false
	q.Logf = func(string, ...any) { failed = true }
	q.LockTimeout = time.Second
	unlock := q.lock(context.Background())
	if failed {
		t.Fatal("could not take the lock: is it held across a sleep?")
	}
	return unlock
}
