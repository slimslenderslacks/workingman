package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/project"
)

// TestIntakeFileFlipsProjectReady is the primary intake bootstrap path:
// dropping a markdown file into an already-watched project's intake/ dir
// flips the project back to status:ready (clearing any blocked_reason) so
// the daemon relaunches the planning agent — the same effect the old `:task`
// TUI command had, but with no on-disk task-seed conversion: the planning
// agent reads the intake file itself (see the runner_test.go-level test for
// that half of the contract).
func TestIntakeFileFlipsProjectReady(t *testing.T) {
	root := t.TempDir()
	buf, _ := startDaemon(t, root)

	sub := filepath.Join(root, "widget")
	if err := mkdirAll(sub); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	projectPath := filepath.Join(sub, ".project.yaml")
	existing := &project.Project{
		Description: "ship the widget", Branch: "feat/widget",
		Status: project.StatusBlocked, BlockedReason: "waiting on something unrelated",
	}
	if err := project.SaveAs(projectPath, existing, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	// Let the project's own observation (project_updated, plus whatever the
	// blocked-status switch does) fully settle on its own event chain before
	// the intake dir even exists — otherwise creating intake/ races that
	// chain's in-flight write to the same .project.yaml on a separate
	// directory-keyed goroutine (see dispatchEvent's per-directory chaining).
	if ok, snap := waitFor(t, buf, "project_updated"); !ok {
		t.Fatalf("project never observed.\naudit:\n%s", snap)
	}
	if err := mkdirAll(filepath.Join(sub, "intake")); err != nil {
		t.Fatalf("mkdir intake: %v", err)
	}
	if ok, snap := waitFor(t, buf, "watch_added"); !ok {
		t.Fatalf("daemon never watched the intake dir.\naudit:\n%s", snap)
	}

	mdPath := filepath.Join(sub, "intake", "add-healthz.md")
	if err := writeFile(mdPath, []byte("add a /healthz endpoint\n")); err != nil {
		t.Fatalf("writeFile: %v", err)
	}

	if ok, snap := waitFor(t, buf, "intake_queued"); !ok {
		t.Fatalf("daemon never queued the intake file.\naudit:\n%s", snap)
	}

	p, err := project.Load(projectPath)
	if err != nil {
		t.Fatalf("Load project: %v", err)
	}
	if p.Status != project.StatusReady {
		t.Errorf("status = %q, want ready", p.Status)
	}
	if p.BlockedReason != "" {
		t.Errorf("blocked_reason = %q, want cleared", p.BlockedReason)
	}

	// No seed conversion: nothing is written to tasks/, and the intake file
	// itself is left in place — the planning agent reads and renames it.
	if _, err := os.Stat(filepath.Join(sub, "tasks")); err == nil {
		t.Errorf("tasks dir should not have been created by handleIntakeFile")
	}
	if _, err := os.Stat(mdPath); err != nil {
		t.Errorf("intake file should still exist: %v", err)
	}
}

// TestIntakeFileInNewDirIsPickedUp mirrors TestNewDirWithFileIsPickedUp for
// the intake trigger: mkdir + write project.yaml + mkdir intake + write the
// markdown file, all before the daemon's watch on the new subtree is
// installed. Without maybeWatchNewDir's post-watch scan the file's Create
// event would be missed.
func TestIntakeFileInNewDirIsPickedUp(t *testing.T) {
	root := t.TempDir()
	buf, _ := startDaemon(t, root)

	sub := filepath.Join(root, "gadget")
	if err := mkdirAll(filepath.Join(sub, "intake")); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	projectPath := filepath.Join(sub, ".project.yaml")
	if err := project.SaveAs(projectPath, &project.Project{
		Description: "x", Branch: "b", Status: project.StatusIdle,
	}, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	if err := writeFile(filepath.Join(sub, "intake", "more-work.md"), []byte("do more work\n")); err != nil {
		t.Fatalf("writeFile: %v", err)
	}

	if ok, snap := waitFor(t, buf, "intake_queued"); !ok {
		t.Fatalf("daemon did not bootstrap the intake file in a new dir.\naudit:\n%s", snap)
	}
}

// TestIntakeFileWithoutProjectIsIgnored guards against an intake/ dir that
// doesn't sit next to a .project.yaml — there is no project to queue work
// against, so the file must be left alone rather than crashing or creating
// a project out of thin air (that's project.md's job, not intake's).
func TestIntakeFileWithoutProjectIsIgnored(t *testing.T) {
	root := t.TempDir()
	buf, _ := startDaemon(t, root)

	sub := filepath.Join(root, "orphan", "intake")
	if err := mkdirAll(sub); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mdPath := filepath.Join(sub, "work.md")
	if err := writeFile(mdPath, []byte("do some work\n")); err != nil {
		t.Fatalf("writeFile: %v", err)
	}

	if ok, snap := waitFor(t, buf, "intake_no_project"); !ok {
		t.Fatalf("expected intake_no_project in audit.\naudit:\n%s", snap)
	}
	if _, err := os.Stat(mdPath); err != nil {
		t.Errorf("intake file should be left untouched: %v", err)
	}
}

// TestIntakeFileEmptyIsIgnored guards against a blank intake file triggering
// a planning cycle with nothing for it to read.
func TestIntakeFileEmptyIsIgnored(t *testing.T) {
	root := t.TempDir()
	buf, _ := startDaemon(t, root)

	sub := filepath.Join(root, "widget")
	if err := mkdirAll(filepath.Join(sub, "intake")); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	projectPath := filepath.Join(sub, ".project.yaml")
	if err := project.SaveAs(projectPath, &project.Project{
		Description: "x", Branch: "b", Status: project.StatusIdle,
	}, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	if err := writeFile(filepath.Join(sub, "intake", "blank.md"), []byte("   \n")); err != nil {
		t.Fatalf("writeFile: %v", err)
	}

	if ok, snap := waitFor(t, buf, "intake_empty"); !ok {
		t.Fatalf("blank intake file should be logged and skipped.\naudit:\n%s", snap)
	}
	reloaded, err := project.Load(projectPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reloaded.Status != project.StatusIdle {
		t.Errorf("status = %q, want untouched (idle)", reloaded.Status)
	}
}

// TestPendingIntakeFilesScansAndSorts is a direct unit test of the helper
// launchProjectRootAgent uses to populate runner.Plan.IntakeFiles: it must
// find every *.md file in intake/, ignore non-markdown and subdirectories,
// and return them in a stable (sorted) order so the rendered prompt doesn't
// shuffle between runs.
func TestPendingIntakeFilesScansAndSorts(t *testing.T) {
	root := t.TempDir()
	intakeDir := filepath.Join(root, "intake")
	if err := mkdirAll(intakeDir); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := writeFile(filepath.Join(intakeDir, "z-later.md"), []byte("z\n")); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	if err := writeFile(filepath.Join(intakeDir, "a-first.md"), []byte("a\n")); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	if err := writeFile(filepath.Join(intakeDir, "already-done.processed"), []byte("done\n")); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	if err := mkdirAll(filepath.Join(intakeDir, "not-a-file.md")); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	got := pendingIntakeFiles(root)
	want := []string{
		filepath.Join(intakeDir, "a-first.md"),
		filepath.Join(intakeDir, "z-later.md"),
	}
	if len(got) != len(want) {
		t.Fatalf("pendingIntakeFiles = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pendingIntakeFiles[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestPendingIntakeFilesNoIntakeDir guards the common case (no intake/ dir at
// all) returning an empty, non-panicking result.
func TestPendingIntakeFilesNoIntakeDir(t *testing.T) {
	if got := pendingIntakeFiles(t.TempDir()); got != nil {
		t.Errorf("pendingIntakeFiles with no intake dir = %v, want nil", got)
	}
}

// TestHasPendingIntakeReArmsRestingProject covers the orphan-recovery half of
// the contract: an idle project with an un-renamed intake/*.md file left over
// — its status flip skipped or clobbered while another agent held the
// project slot — must be re-armed back to ready the same way a stranded
// task-graph seed already is. Covers both idle sub-cases (see
// Project.WatchingPR): plain idle, and idle-while-watching a PR — the
// re-arm applies the same either way, exactly as it did for the two
// separate statuses (done/reviewing) this collapsed from.
func TestHasPendingIntakeReArmsRestingProject(t *testing.T) {
	cases := []struct {
		name string
		p    project.Project
	}{
		{"idle", project.Project{}},
		{"idle watching a PR", project.Project{Review: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			buf := &safeBuf{}
			d, err := New([]string{root}, audit.New(buf))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			projectPath := filepath.Join(root, ".project.yaml")
			if err := mkdirAll(filepath.Join(root, "intake")); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := writeFile(filepath.Join(root, "intake", "stranded.md"), []byte("stranded request\n")); err != nil {
				t.Fatalf("writeFile: %v", err)
			}

			p := tc.p
			p.Description = "x"
			p.Branch = "b"
			p.Status = project.StatusIdle
			p.Repos = []project.Repo{{Org: "docker", Name: "gateway"}}
			d.dispatchProject(projectPath, &p)

			got, err := project.Load(projectPath)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got.Status != project.StatusReady {
				t.Errorf("status = %q, want ready (a pending intake file must re-arm planning)", got.Status)
			}
			if !strings.Contains(buf.String(), "seed_replan") {
				t.Errorf("expected seed_replan in audit:\n%s", buf.String())
			}
		})
	}
}
