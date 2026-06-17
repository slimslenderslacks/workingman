package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/notify"
	"github.com/slimslenderslacks/work/internal/project"
)

// newPlanningTestDaemon builds a runner-less daemon suitable for exercising
// afterPlanningSession directly. With no runner the block path's wolf launch
// is a no-op, so the test observes the blocking decision in isolation. ctx is
// assigned (Run normally does this) so backoffPlanning has a channel to select
// on; tests that want to skip the backoff pass an already-cancelled ctx.
func newPlanningTestDaemon(t *testing.T, ctx context.Context) (*Daemon, *safeBuf, *notify.Recorder) {
	t.Helper()
	buf := &safeBuf{}
	a := audit.New(buf)
	rec := &notify.Recorder{}
	d, err := New([]string{t.TempDir()}, a, WithNotifier(rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.ctx = ctx
	return d, buf, rec
}

func writeReadyProject(t *testing.T, root string) string {
	t.Helper()
	projectPath := filepath.Join(root, ".project.yaml")
	if err := project.SaveAs(projectPath, &project.Project{
		Description: "test",
		Branch:      "feat/planning",
		Status:      project.StatusReady,
		Repos:       []project.Repo{{Org: "docker", Name: "gateway"}},
	}, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	return projectPath
}

// TestPlanningHardFailureBlocksImmediately is the regression for the acp-kit
// crash loop: when a planning session ends with a non-nil wait error (the
// acp-wrapper exited non-zero because the sandbox could not be created) and
// the project is still status:ready, the daemon must block the project at once
// rather than relaunch. It must not consume the retry budget or emit a retry.
func TestPlanningHardFailureBlocksImmediately(t *testing.T) {
	root := t.TempDir()
	projectPath := writeReadyProject(t, root)
	d, buf, rec := newPlanningTestDaemon(t, context.Background())

	d.afterPlanningSession(projectPath, errors.New("sbx create: path does not exist"))

	reloaded, err := project.Load(projectPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Status != project.StatusBlocked {
		t.Fatalf("project status = %q, want blocked", reloaded.Status)
	}
	if reloaded.UpdatedBy != project.WriterDaemon {
		t.Errorf("updated_by = %q, want daemon", reloaded.UpdatedBy)
	}
	if !strings.Contains(reloaded.BlockedReason, "sbx create") {
		t.Errorf("blocked reason should carry the wait error, got %q", reloaded.BlockedReason)
	}

	audit := buf.String()
	if !strings.Contains(audit, "project_blocked") {
		t.Errorf("expected project_blocked in audit:\n%s", audit)
	}
	if strings.Contains(audit, "planning_retry") {
		t.Errorf("hard failure must not retry, but saw planning_retry:\n%s", audit)
	}
	if len(rec.Calls()) == 0 {
		t.Errorf("expected a block notification")
	}
}

// TestPlanningNoProgressExhaustsRetriesAndBlocks covers the soft case: the
// planning agent exits cleanly but leaves the project in status:ready. The
// daemon retries up to maxPlanningRetries, then blocks. A cancelled ctx makes
// backoffPlanning return immediately so the test doesn't sleep.
func TestPlanningNoProgressExhaustsRetriesAndBlocks(t *testing.T) {
	root := t.TempDir()
	projectPath := writeReadyProject(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // skip the real backoff sleep
	d, buf, _ := newPlanningTestDaemon(t, ctx)

	for i := 0; i < maxPlanningRetries; i++ {
		d.afterPlanningSession(projectPath, nil)
	}

	reloaded, err := project.Load(projectPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Status != project.StatusBlocked {
		t.Fatalf("project status = %q, want blocked after %d attempts", reloaded.Status, maxPlanningRetries)
	}
	audit := buf.String()
	if got := strings.Count(audit, "planning_retry"); got != maxPlanningRetries-1 {
		t.Errorf("planning_retry count = %d, want %d:\n%s", got, maxPlanningRetries-1, audit)
	}
	if !strings.Contains(audit, "project_blocked") {
		t.Errorf("expected project_blocked in audit:\n%s", audit)
	}
}

// TestPlanningProgressResetsCounter verifies that a productive cycle (project
// advanced off status:ready) clears the failure counter so earlier
// non-productive cycles don't accumulate toward a spurious block.
func TestPlanningProgressResetsCounter(t *testing.T) {
	root := t.TempDir()
	projectPath := writeReadyProject(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d, _, _ := newPlanningTestDaemon(t, ctx)

	// Two non-productive cycles bump the counter.
	d.afterPlanningSession(projectPath, nil)
	d.afterPlanningSession(projectPath, nil)
	if n := d.planningFailures[projectPath]; n != 2 {
		t.Fatalf("failure count = %d, want 2", n)
	}

	// Planning advances the project: counter must reset.
	advanced, err := project.Load(projectPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	advanced.Status = project.StatusWorking
	if err := project.SaveAs(projectPath, advanced, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	d.afterPlanningSession(projectPath, nil)

	if n := d.planningFailures[projectPath]; n != 0 {
		t.Errorf("failure count after progress = %d, want 0", n)
	}
}
