package bridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProc exits when told to, or immediately with a preset code.
type fakeProc struct {
	exit    chan struct{}
	code    int
	once    sync.Once
	stopped chan struct{}
}

func newFakeProc(code int, exitNow bool) *fakeProc {
	p := &fakeProc{exit: make(chan struct{}), code: code, stopped: make(chan struct{})}
	if exitNow {
		close(p.exit)
	}
	return p
}

func (p *fakeProc) Wait() (int, error) {
	<-p.exit
	if p.code != 0 {
		return p.code, errors.New("exit status")
	}
	return 0, nil
}

func (p *fakeProc) Stop() {
	p.once.Do(func() { close(p.stopped) })
	select {
	case <-p.exit:
	default:
		close(p.exit)
	}
}

// script hands out a scripted sequence of processes, then blocks-forever ones.
type script struct {
	mu     sync.Mutex
	procs  []*fakeProc
	starts int
	errs   map[int]error // start index -> error
}

func (s *script) start(ctx context.Context) (Process, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.starts
	s.starts++
	if err := s.errs[i]; err != nil {
		return nil, err
	}
	if i < len(s.procs) {
		return s.procs[i], nil
	}
	return newFakeProc(0, false), nil
}

func (s *script) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starts
}

type sleeps struct {
	mu sync.Mutex
	ds []time.Duration
	// ch, if set, receives each sleep so a test can sequence on it.
	ch chan time.Duration
}

func (s *sleeps) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.ds = append(s.ds, d)
	s.mu.Unlock()
	if s.ch != nil {
		select {
		case s.ch <- d:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return ctx.Err()
}

func (s *sleeps) got() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.ds...)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestBackoffDelaysDoubleAndCap(t *testing.T) {
	b := Backoff{Initial: time.Second, Max: 10 * time.Second}.withDefaults()
	var got []time.Duration
	for i := 0; i < 6; i++ {
		got = append(got, b.delay(i))
	}
	want := []time.Duration{1, 2, 4, 8, 10, 10}
	for i := range want {
		if got[i] != want[i]*time.Second {
			t.Fatalf("delay(%d) = %v, want %v (all: %v)", i, got[i], want[i]*time.Second, got)
		}
	}
}

func TestSupervisorRestartsWithGrowingBackoff(t *testing.T) {
	sc := &script{procs: []*fakeProc{newFakeProc(1, true), newFakeProc(1, true), newFakeProc(1, true)}}
	sl := &sleeps{}
	sup := &Supervisor{
		Start:   sc.start,
		Backoff: Backoff{Initial: time.Second, Max: 3 * time.Second, ResetAfter: time.Hour},
		Sleep:   sl.sleep,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	// Three crashes, then a process that stays up.
	waitFor(t, "fourth start", func() bool { return sc.count() == 4 && sup.Status().Running })
	got := sl.got()
	want := []time.Duration{time.Second, 2 * time.Second, 3 * time.Second}
	if len(got) != len(want) {
		t.Fatalf("sleeps = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sleeps = %v, want %v", got, want)
		}
	}
	if st := sup.Status(); st.Restarts != 3 || st.LastExit != 1 {
		t.Fatalf("status = %+v, want 3 restarts, last exit 1", st)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run after cancel = %v, want nil", err)
	}
	if sup.Status().Running {
		t.Fatal("still marked running after Run returned")
	}
}

func TestSupervisorBackoffResetsAfterHealthyRun(t *testing.T) {
	// Fake clock: the second process "ran" for a minute before dying.
	var mu sync.Mutex
	now := time.Unix(1000, 0)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }

	sc := &script{}
	sc.procs = []*fakeProc{newFakeProc(1, true), newFakeProc(1, true), newFakeProc(1, false), newFakeProc(1, true)}
	sl := &sleeps{}
	sup := &Supervisor{
		Start:   sc.start,
		Backoff: Backoff{Initial: time.Second, Max: time.Minute, ResetAfter: 30 * time.Second},
		Sleep:   sl.sleep,
		Now:     clock,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	waitFor(t, "third process running", func() bool { return sc.count() == 3 && sup.Status().Running })
	advance(time.Minute) // third process has been healthy for a minute
	sc.procs[2].Stop()   // ... then it dies
	waitFor(t, "fourth start", func() bool { return sc.count() >= 4 })
	cancel()
	<-done

	got := sl.got()
	// crash, crash (1s, 2s), long healthy run then crash: back to 1s.
	if len(got) < 3 || got[0] != time.Second || got[1] != 2*time.Second || got[2] != time.Second {
		t.Fatalf("sleeps = %v, want [1s 2s 1s ...]", got)
	}
}

func TestSupervisorStopsOnLoggedOut(t *testing.T) {
	sc := &script{procs: []*fakeProc{newFakeProc(ExitLoggedOut, true)}}
	sup := &Supervisor{Start: sc.start, Sleep: (&sleeps{}).sleep}
	err := sup.Run(context.Background())
	if !errors.Is(err, ErrLoggedOut) {
		t.Fatalf("Run = %v, want ErrLoggedOut", err)
	}
	if sc.count() != 1 {
		t.Fatalf("started %d times, want 1 (no restart after logout)", sc.count())
	}
	if !sup.Status().LoggedOut {
		t.Fatal("status.LoggedOut not set")
	}
}

func TestSupervisorRetriesStartErrors(t *testing.T) {
	sc := &script{errs: map[int]error{0: errors.New("spawn failed"), 1: errors.New("spawn failed")}}
	sl := &sleeps{}
	sup := &Supervisor{Start: sc.start, Backoff: Backoff{Initial: time.Second, Max: time.Minute}, Sleep: sl.sleep}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()
	waitFor(t, "third start", func() bool { return sc.count() == 3 && sup.Status().Running })
	cancel()
	<-done
	if got := sl.got(); len(got) != 2 || got[0] != time.Second || got[1] != 2*time.Second {
		t.Fatalf("sleeps = %v, want [1s 2s]", got)
	}
}

func TestSupervisorCancelStopsProcess(t *testing.T) {
	p := newFakeProc(0, false)
	sup := &Supervisor{Start: func(context.Context) (Process, error) { return p, nil }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()
	waitFor(t, "running", func() bool { return sup.Status().Running })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}
	select {
	case <-p.stopped:
	default:
		t.Fatal("process was not stopped on cancel")
	}
}

func TestSupervisorRestartSkipsBackoff(t *testing.T) {
	sc := &script{}
	sl := &sleeps{}
	sup := &Supervisor{Start: sc.start, Backoff: Backoff{Initial: time.Hour}, Sleep: sl.sleep}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()
	waitFor(t, "first run", func() bool { return sup.Status().Running })

	sup.Restart()
	waitFor(t, "second start", func() bool { return sc.count() == 2 && sup.Status().Running })
	cancel()
	<-done
	if got := sl.got(); len(got) != 0 {
		t.Fatalf("Restart slept %v; it should restart immediately", got)
	}
	if st := sup.Status(); st.Restarts != 1 {
		t.Fatalf("Restarts = %d, want 1", st.Restarts)
	}
}

func TestExecStarterCapturesOutputAndExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake node")
	}
	dir := t.TempDir()
	node := filepath.Join(dir, "node")
	// Echoes its args and the token env, then exits like a logged-out bridge.
	os.WriteFile(node, []byte("#!/bin/sh\necho \"args: $@ token=$WORKINGMAN_BRIDGE_TOKEN\"\necho oops >&2\nexit 78\n"), 0o755)
	logPath := filepath.Join(dir, "bridge.log")
	logf, err := OpenLog(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logf.Close()

	start := ExecStarter(Launch{Node: node, Dir: dir, Args: []string{"--port", "1234"}, Env: []string{"WORKINGMAN_BRIDGE_TOKEN=s3"}, Log: logf})
	sup := &Supervisor{Start: start}
	if err := sup.Run(context.Background()); !errors.Is(err, ErrLoggedOut) {
		t.Fatalf("Run = %v, want ErrLoggedOut", err)
	}
	data, _ := os.ReadFile(logPath)
	for _, want := range []string{"args: bridge.js --port 1234 token=s3", "oops", "--- bridge start"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("log missing %q:\n%s", want, data)
		}
	}
}

func TestExecStarterStopKillsProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake node")
	}
	dir := t.TempDir()
	node := filepath.Join(dir, "node")
	os.WriteFile(node, []byte("#!/bin/sh\nexec sleep 60\n"), 0o755)
	p, err := ExecStarter(Launch{Node: node, Dir: dir, StopGrace: time.Second})(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p.Stop()
	done := make(chan struct{})
	go func() { p.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("process survived Stop")
	}
}
