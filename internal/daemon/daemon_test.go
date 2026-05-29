package daemon

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/project"
)

// safeBuf is a thread-safe buffer for tests that race the daemon goroutine.
type safeBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// waitFor polls the buffer until want appears or 2s elapses. Returns the
// snapshot at the moment we either saw it or gave up, for use in error messages.
func waitFor(t *testing.T, buf *safeBuf, want string) (bool, string) {
	t.Helper()
	return waitForWithin(t, buf, want, 2*time.Second)
}

func waitForWithin(t *testing.T, buf *safeBuf, want string, dur time.Duration) (bool, string) {
	t.Helper()
	deadline := time.Now().Add(dur)
	for time.Now().Before(deadline) {
		s := buf.String()
		if strings.Contains(s, want) {
			return true, s
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false, buf.String()
}

// waitForCount waits until want appears at least n times in buf, or dur
// elapses. Used to step through orchestrated lifecycles where the same
// audit event (e.g. session_started) fires once per dispatched agent.
func waitForCount(t *testing.T, buf *safeBuf, want string, n int, dur time.Duration) (bool, string) {
	t.Helper()
	deadline := time.Now().Add(dur)
	for time.Now().Before(deadline) {
		s := buf.String()
		if strings.Count(s, want) >= n {
			return true, s
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false, buf.String()
}

func startDaemon(t *testing.T, root string) (*safeBuf, context.CancelFunc) {
	t.Helper()
	buf := &safeBuf{}
	a := audit.New(buf)
	d, err := New([]string{root}, a)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = d.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	// Give fsnotify a beat to install watches before the test writes files.
	if ok, snap := waitFor(t, buf, "watch_root"); !ok {
		t.Fatalf("daemon never logged watch_root: %q", snap)
	}
	return buf, cancel
}

func TestProjectAgentWriteIsObserved(t *testing.T) {
	root := t.TempDir()
	buf, _ := startDaemon(t, root)

	path := filepath.Join(root, ".project.yaml")
	p := &project.Project{
		Description: "test",
		Branch:      "feat/x",
		Status:      project.StatusReady,
	}
	if err := project.SaveAs(path, p, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}

	ok, snap := waitFor(t, buf, "project_updated")
	if !ok {
		t.Fatalf("no project_updated event seen.\naudit:\n%s", snap)
	}
	if !strings.Contains(snap, "status=ready") {
		t.Errorf("expected status=ready in audit, got: %s", snap)
	}
	if !strings.Contains(snap, "writer=agent") {
		t.Errorf("expected writer=agent in audit, got: %s", snap)
	}
}

func TestDaemonWriteIsIgnored(t *testing.T) {
	root := t.TempDir()
	buf, _ := startDaemon(t, root)

	path := filepath.Join(root, ".project.yaml")
	p := &project.Project{Branch: "x", Status: project.StatusReady}
	if err := project.Save(path, p); err != nil { // forces UpdatedBy=daemon
		t.Fatalf("Save: %v", err)
	}

	// Give fsnotify time to deliver — we expect *no* project_updated entry.
	time.Sleep(250 * time.Millisecond)
	if strings.Contains(buf.String(), "project_updated") {
		t.Errorf("daemon-written file should have been ignored.\naudit:\n%s", buf.String())
	}
}

func TestEmptyFileLogged(t *testing.T) {
	root := t.TempDir()
	buf, _ := startDaemon(t, root)

	path := filepath.Join(root, ".project.yaml")
	if err := writeFile(path, nil); err != nil {
		t.Fatalf("write empty: %v", err)
	}
	if ok, snap := waitFor(t, buf, "project_empty"); !ok {
		t.Fatalf("no project_empty event seen.\naudit:\n%s", snap)
	}
}

func TestNewDirectoryIsPickedUp(t *testing.T) {
	root := t.TempDir()
	buf, _ := startDaemon(t, root)

	// Create the new subtree first and wait until the daemon has actually
	// installed a watch on it. Writing the file before the kqueue watch is
	// registered races on macOS — the file change isn't delivered.
	sub := filepath.Join(root, "later")
	if err := mkdirAll(sub); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if ok, snap := waitFor(t, buf, "watch_added"); !ok {
		t.Fatalf("daemon never watched new dir.\naudit:\n%s", snap)
	}

	path := filepath.Join(sub, ".project.yaml")
	p := &project.Project{Branch: "x", Status: project.StatusReady}
	if err := project.SaveAs(path, p, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	if ok, snap := waitFor(t, buf, "project_updated"); !ok {
		t.Fatalf("daemon did not pick up file in new dir.\naudit:\n%s", snap)
	}
}

// TestNewDirWithFileIsPickedUp exercises the TUI's :new flow: mkdir + write
// an empty .project.yaml back-to-back. The directory watch is installed
// after the file already exists on disk, so without the post-watch scan the
// file's Create event is missed and the project agent never fires.
func TestNewDirWithFileIsPickedUp(t *testing.T) {
	root := t.TempDir()
	buf, _ := startDaemon(t, root)

	sub := filepath.Join(root, "newproj")
	if err := mkdirAll(sub); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(sub, ".project.yaml")
	if err := writeFile(path, nil); err != nil {
		t.Fatalf("writeFile: %v", err)
	}

	if ok, snap := waitFor(t, buf, "project_empty"); !ok {
		t.Fatalf("daemon did not observe empty .project.yaml in new dir.\naudit:\n%s", snap)
	}
}
