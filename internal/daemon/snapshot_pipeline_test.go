package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/audit"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files under testdata/")

// writePipelineRoot lays out one work stream, "pipe", whose tasks sit at every
// stage of the integration pipeline (docs/parallel-task-worktrees.md D4):
//
//	base       committed, its worktree cleaned up
//	head       success, the oldest: the queue head, being integrated
//	next       success, queued behind head
//	conflicted ready again after a conflict, with an integration report
//	dependent  ready, depends on next (at success), so waiting_on it
//	legacy     ready, no hints and no worktree fields
func writePipelineRoot(t *testing.T) (root, projectPath string) {
	t.Helper()
	root = t.TempDir()
	ws := filepath.Join(root, "pipe")
	wt := func(name string) string { return "/ws/pipe-branch/.worktrees/" + name }
	files := map[string]string{
		"project.yaml": `description: pipeline
repos:
  - org: acme
    name: gateway
branch: pipe-branch
status: working
max_parallel_tasks: 2
verify: go test ./...
updated_by: agent
`,
		"tasks/base.yaml": `name: base
description: base
status: committed
touches: []
completed_at: 2026-11-19T09:00:00Z
commits:
  - repo: gateway
    hash: aaaa
`,
		"tasks/head.yaml": `name: head
description: head
status: success
touches: ["gateway/cmd/**"]
worktree: ` + wt("head") + `
task_branch: wm/pipe-branch/head
base:
  - repo: gateway
    hash: aaaa
succeeded_at: 2026-11-19T10:00:00Z
`,
		"tasks/next.yaml": `name: next
description: next
status: success
areas: [go-deps]
worktree: ` + wt("next") + `
task_branch: wm/pipe-branch/next
base:
  - repo: gateway
    hash: aaaa
succeeded_at: 2026-11-19T10:05:00Z
`,
		"tasks/conflicted.yaml": `name: conflicted
description: conflicted
status: ready
attempts: 1
touches: ["gateway/internal/**"]
worktree: ` + wt("conflicted") + `
task_branch: wm/pipe-branch/conflicted
base:
  - repo: gateway
    hash: aaaa
integration:
  result: conflict
  repo: gateway
  files: [internal/a.go, internal/b.go]
  onto:
    - repo: gateway
      hash: bbbb
  landed: [base]
`,
		"tasks/dependent.yaml": `name: dependent
description: dependent
status: ready
depends_on: [next]
touches: ["gateway/docs/**"]
`,
		"tasks/legacy.yaml": `name: legacy
description: legacy
status: ready
`,
	}
	for rel, body := range files {
		p := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root, filepath.Join(ws, "project.yaml")
}

// pipelineSnapshot is a live snapshot of writePipelineRoot with head's commit
// agent running and holding the commit reservation, and one build check
// counted of each kind.
func pipelineSnapshot(t *testing.T) (*Snapshot, string, string) {
	t.Helper()
	root, projectPath := writePipelineRoot(t)
	d, err := newDaemon(t, []string{root}, audit.New(&safeBuf{}),
		WithStateFile(filepath.Join(t.TempDir(), "state", "snapshot.json"), 5))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d.ctx = ctx
	t.Cleanup(func() { _ = d.watcher.Close() })

	d.trackSession(projectPath, newStubSession("orch-commit-pipe"), agent.CommitAgent, "head", nil)
	d.beginCommit(projectPath)
	d.countVerifyCheck(projectPath, verifyCheckRun)
	d.countVerifyCheck(projectPath, verifyCheckFailed)
	d.countVerifyCheck(projectPath, verifyCheckSkipped)
	return d.Snapshot(), root, projectPath
}

// The project block of a snapshot with queued, integrating and conflicted
// tasks matches testdata/snapshot_pipeline.golden.json (go test -update
// rewrites it).
func TestSnapshotPipelineGolden(t *testing.T) {
	s, root, _ := pipelineSnapshot(t)
	if s.Version != 1 {
		t.Errorf("version = %d; phase 3 fields are additive, it stays 1", s.Version)
	}
	pr := findProject(t, s, "pipe")
	pr.UpdatedAt = time.Time{} // the file's mtime
	got, err := json.MarshalIndent(pr, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(bytes.ReplaceAll(got, []byte(root), []byte("<root>")), '\n')

	golden := filepath.Join("testdata", "snapshot_pipeline.golden.json")
	if *updateGolden {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/daemon -run Golden -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("project snapshot differs from %s:\n--- got\n%s\n--- want\n%s", golden, got, want)
	}
}

// orch status prints the pipeline: running/integrating/queued counts, the
// queue, the build check counts, per-task queue and integration notes, and the
// commit reservation.
func TestSnapshotTextPipeline(t *testing.T) {
	s, _, _ := pipelineSnapshot(t)
	var sb strings.Builder
	if err := s.WriteText(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		"tasks: 3 ready, 1 integrating (head), 1 queued, 1 committed",
		"max_parallel_tasks: 2",
		"queue: head (integrating), next",
		"verify checks: 2 run, 1 failed, 1 skipped",
		"queue=#1",
		"queue=#2",
		"waiting_on=next",
		"integration=conflict(gateway:internal/a.go,internal/b.go)",
		"worktree=/ws/pipe-branch/.worktrees/conflicted",
		"commit-reserved",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "worktree=/ws/pipe-branch/.worktrees/base") {
		t.Errorf("a committed task's worktree is not listed:\n%s", out)
	}
}

// An offline snapshot has no live block and no build check counters, but the
// queue and the per-task fields come from disk and are all there.
func TestOfflineSnapshotPipelineFields(t *testing.T) {
	root, _ := writePipelineRoot(t)
	s := BuildOfflineSnapshot(OfflineOptions{Roots: []string{root}, SessionsRoot: t.TempDir()})
	pr := findProject(t, s, "pipe")
	if pr.Live != nil || pr.VerifyChecks != nil {
		t.Errorf("offline: live = %+v, verify_checks = %+v", pr.Live, pr.VerifyChecks)
	}
	if strings.Join(pr.CommitQueue, ",") != "head,next" || pr.MaxParallelTasks != 2 {
		t.Errorf("commit_queue = %v, max_parallel_tasks = %d", pr.CommitQueue, pr.MaxParallelTasks)
	}
	var sb strings.Builder
	if err := s.WriteText(&sb); err != nil {
		t.Fatal(err)
	}
	// Without live state nothing is known to be integrating.
	if out := sb.String(); !strings.Contains(out, "2 queued") || !strings.Contains(out, "queue: head, next\n") {
		t.Errorf("offline text:\n%s", out)
	}
}
