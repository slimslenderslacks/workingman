package terminal

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
)

// fakeTerm is a Launcher that "opens a tab" by starting the tab's command as a
// background process — so the pid-recording wrapper really runs — and records
// every open and focus.
type fakeTerm struct {
	mu       sync.Mutex
	opened   []Tab
	focused  []TabRef
	focusErr error
	procs    []*exec.Cmd
	exited   []chan struct{}
	noStart  bool // accept the tab but never start its command
}

func (f *fakeTerm) Name() string { return "fake" }

func (f *fakeTerm) OpenTab(_ context.Context, tab Tab) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, tab)
	if !f.noStart {
		cmd := exec.Command(tab.Command[0], tab.Command[1:]...)
		cmd.Dir = tab.Dir
		if err := cmd.Start(); err != nil {
			return "", err
		}
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		f.procs = append(f.procs, cmd)
		f.exited = append(f.exited, done)
	}
	return "id-" + tab.Title, nil
}

func (f *fakeTerm) FocusTab(_ context.Context, ref TabRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.focused = append(f.focused, ref)
	return f.focusErr
}

func (f *fakeTerm) killAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.procs {
		_ = c.Process.Kill()
	}
	for _, done := range f.exited {
		<-done
	}
}

func newTabLauncher(t *testing.T, term *fakeTerm) *TabLauncher {
	t.Helper()
	stateDir := t.TempDir()
	// Registered after TempDir so it runs first: the tab commands must be gone
	// before their state dir is removed.
	t.Cleanup(term.killAll)
	return &TabLauncher{
		Resolve:      func() (Launcher, error) { return term, nil },
		StateDir:     stateDir,
		Env:          []string{"ORCH_TEST=1"},
		PollInterval: 10 * time.Millisecond,
		StartTimeout: 2 * time.Second,
	}
}

func waitForPid(t *testing.T, dir, name string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := readPid(dir, name); ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("command for %q never recorded its pid", name)
}

var sleepSpec = agent.Spec{Name: "wolf-feat/x", Title: "myproj", Workspace: "/", Command: []string{"sleep", "30"}}

func TestTabLauncherOpensTitledTab(t *testing.T) {
	term := &fakeTerm{}
	l := newTabLauncher(t, term)
	sess, err := l.Open(context.Background(), sleepSpec)
	if err != nil {
		t.Fatal(err)
	}
	if len(term.opened) != 1 {
		t.Fatalf("opened %d tabs, want 1", len(term.opened))
	}
	tab := term.opened[0]
	if tab.Title != "myproj" || tab.Dir != "/" {
		t.Errorf("tab = %+v, want title myproj in /", tab)
	}
	// The command is wrapped to record its pid and carry the forwarded env.
	joined := strings.Join(tab.Command, " ")
	if !strings.Contains(joined, "env ORCH_TEST=1 sleep 30") {
		t.Errorf("wrapped command = %q", joined)
	}
	if sess.Name() != "wolf-feat/x" || sess.Title() != "myproj" || sess.Reused() {
		t.Errorf("session = %q/%q reused=%v", sess.Name(), sess.Title(), sess.Reused())
	}
}

func TestTabLauncherTitleDefaultsToName(t *testing.T) {
	term := &fakeTerm{}
	l := newTabLauncher(t, term)
	spec := sleepSpec
	spec.Title = ""
	if _, err := l.Open(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if term.opened[0].Title != "wolf-feat/x" {
		t.Errorf("title = %q, want the session name", term.opened[0].Title)
	}
}

// The duplicate-tab path: a second Open for a session whose tab is still open
// must not open another tab; it focuses the existing one and reports it.
func TestTabLauncherFocusesAlreadyOpenTab(t *testing.T) {
	term := &fakeTerm{}
	l := newTabLauncher(t, term)
	if _, err := l.Open(context.Background(), sleepSpec); err != nil {
		t.Fatal(err)
	}
	waitForPid(t, l.StateDir, sleepSpec.Name)

	sess, err := l.Open(context.Background(), sleepSpec)
	var already *AlreadyOpenError
	if !errors.As(err, &already) {
		t.Fatalf("err = %v, want *AlreadyOpenError", err)
	}
	if already.Title != "myproj" || already.FocusErr != nil {
		t.Errorf("already = %+v", already)
	}
	if !strings.Contains(err.Error(), "already open") || !strings.Contains(err.Error(), "switched to it") {
		t.Errorf("message = %q", err)
	}
	if len(term.opened) != 1 {
		t.Errorf("opened %d tabs, want still 1", len(term.opened))
	}
	if len(term.focused) != 1 || term.focused[0] != (TabRef{ID: "id-myproj", Title: "myproj"}) {
		t.Errorf("focused = %+v, want the first tab's id", term.focused)
	}
	if sess == nil || !sess.Reused() {
		t.Errorf("want a reused session, got %+v", sess)
	}

	// Launch (the agent.Launcher path) adopts the open tab silently.
	if s, err := l.Launch(context.Background(), sleepSpec); err != nil || s == nil {
		t.Errorf("Launch on an open tab: %v, %v", s, err)
	}
	if len(term.opened) != 1 {
		t.Errorf("Launch opened a second tab")
	}
}

func TestTabLauncherAlreadyOpenReportsFocusFailure(t *testing.T) {
	term := &fakeTerm{focusErr: ErrFocusUnsupported}
	l := newTabLauncher(t, term)
	if _, err := l.Open(context.Background(), sleepSpec); err != nil {
		t.Fatal(err)
	}
	waitForPid(t, l.StateDir, sleepSpec.Name)
	_, err := l.Open(context.Background(), sleepSpec)
	var already *AlreadyOpenError
	if !errors.As(err, &already) || !errors.Is(already.FocusErr, ErrFocusUnsupported) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "could not switch") {
		t.Errorf("message = %q", err)
	}
}

// Within the start window a tab whose command hasn't started yet still counts
// as open, so two quick opens don't race into two tabs.
func TestTabLauncherPendingTabCountsAsOpen(t *testing.T) {
	term := &fakeTerm{noStart: true}
	l := newTabLauncher(t, term)
	if _, err := l.Open(context.Background(), sleepSpec); err != nil {
		t.Fatal(err)
	}
	var already *AlreadyOpenError
	if _, err := l.Open(context.Background(), sleepSpec); !errors.As(err, &already) {
		t.Fatalf("err = %v, want *AlreadyOpenError for a pending tab", err)
	}
}

func TestTabLauncherReopensAfterSessionEnds(t *testing.T) {
	term := &fakeTerm{}
	l := newTabLauncher(t, term)
	spec := sleepSpec
	spec.Command = []string{"true"}
	sess, err := l.Open(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sess.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if _, err := os.Stat(filepath.Join(l.StateDir, fileStem(spec.Name)+".json")); !os.IsNotExist(err) {
		t.Errorf("state file should be removed once the session ends: %v", err)
	}
	if _, err := l.Open(context.Background(), spec); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if len(term.opened) != 2 {
		t.Errorf("opened %d tabs, want 2", len(term.opened))
	}
}

func TestTabSessionCloseEndsCommand(t *testing.T) {
	term := &fakeTerm{}
	l := newTabLauncher(t, term)
	sess, err := l.Open(context.Background(), sleepSpec)
	if err != nil {
		t.Fatal(err)
	}
	waitForPid(t, l.StateDir, sleepSpec.Name)
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sess.Wait(ctx); err != nil {
		t.Fatalf("Wait after Close: %v", err)
	}
	_ = sess.Close() // idempotent
}

func TestTabSessionWaitFailsWhenCommandNeverStarts(t *testing.T) {
	term := &fakeTerm{noStart: true}
	l := newTabLauncher(t, term)
	l.StartTimeout = 50 * time.Millisecond
	sess, err := l.Open(context.Background(), sleepSpec)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Wait(context.Background()); err == nil || !strings.Contains(err.Error(), "never started") {
		t.Errorf("Wait err = %v", err)
	}
}

func TestTabLauncherFocusByName(t *testing.T) {
	term := &fakeTerm{}
	l := newTabLauncher(t, term)
	if err := l.Focus(context.Background(), "nope"); !errors.Is(err, ErrTabNotFound) {
		t.Errorf("focus unknown: err = %v, want ErrTabNotFound", err)
	}
	if _, err := l.Open(context.Background(), sleepSpec); err != nil {
		t.Fatal(err)
	}
	if err := l.Focus(context.Background(), sleepSpec.Name); err != nil {
		t.Fatal(err)
	}
	if len(term.focused) != 1 || term.focused[0].ID != "id-myproj" {
		t.Errorf("focused = %+v", term.focused)
	}
}

// With no resolvable terminal the error surfaces when the tab is needed.
func TestTabLauncherResolveErrorAtLaunch(t *testing.T) {
	l := &TabLauncher{
		Resolve:  func() (Launcher, error) { return nil, errors.New("don't know how to open a terminal tab here") },
		StateDir: t.TempDir(),
	}
	if _, err := l.Launch(context.Background(), sleepSpec); err == nil || !strings.Contains(err.Error(), "don't know how") {
		t.Errorf("err = %v", err)
	}
}

func TestTabLauncherValidatesSpec(t *testing.T) {
	l := newTabLauncher(t, &fakeTerm{})
	if _, err := l.Open(context.Background(), agent.Spec{Command: []string{"true"}}); err == nil {
		t.Error("expected an error without a name")
	}
	if _, err := l.Open(context.Background(), agent.Spec{Name: "x"}); err == nil {
		t.Error("expected an error without a command")
	}
}
