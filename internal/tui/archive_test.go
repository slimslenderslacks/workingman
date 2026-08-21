package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/slimslenderslacks/work/internal/project"
)

// fakeWspRemover stands in for the real wsp manager so archive tests never
// shell out. It records every branch it was asked to remove and can be told to
// fail, which is how the partial-failure path is exercised.
//
// root, when set, is the stub workspace root: Path resolves a branch to
// <root>/<branch>, the same shape workspace.StubManager uses, so tests can put a
// real `.orch` directory on disk and watch it go. Left empty, Path fails and the
// `.orch` cleanup is skipped.
type fakeWspRemover struct {
	root     string
	branches []string
	err      error
	// removeHook runs inside Remove, before the recorded result, so a test can
	// observe the state of the workspace at the moment wsp is asked to tear it
	// down — that is how the `.orch`-first ordering is pinned down.
	removeHook func(branch string)
}

func (f *fakeWspRemover) Path(branch string) (string, error) {
	if f.root == "" || branch == "" {
		return "", errors.New("no workspace root")
	}
	return filepath.Join(f.root, branch), nil
}

func (f *fakeWspRemover) Remove(ctx context.Context, branch string) error {
	if f.removeHook != nil {
		f.removeHook(branch)
	}
	f.branches = append(f.branches, branch)
	return f.err
}

// writeProject drops a .project.yaml for a work stream named name under root
// and returns its path. archive mirrors the cleanup agent's `archive: true`
// marker, which `:archive` requires.
func writeProject(t *testing.T, root, name, branch string, archive bool) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".project.yaml")
	p := &project.Project{
		Description: name,
		Branch:      branch,
		Status:      project.StatusReady,
		Archive:     archive,
	}
	if err := project.SaveAs(path, p, project.WriterAgent); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestArchiveProjectMovesTree(t *testing.T) {
	root := t.TempDir()
	projPath := writeProject(t, root, "weather-tui", "weather-tui", true)
	projDir := filepath.Dir(projPath)
	if err := os.MkdirAll(filepath.Join(projDir, "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "tasks", "a.yaml"), []byte("name: a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wsp := &fakeWspRemover{}

	if err := archiveProject(root, projPath, wsp); err != nil {
		t.Fatalf("archiveProject: %v", err)
	}

	// Original is gone from the workspace root.
	if _, err := os.Stat(projDir); !os.IsNotExist(err) {
		t.Errorf("original project dir still present (err=%v)", err)
	}
	// The whole tree landed under <root>.backup/<name>.
	dest := filepath.Join(root+".backup", "weather-tui")
	if _, err := os.Stat(filepath.Join(dest, ".project.yaml")); err != nil {
		t.Errorf("archived .project.yaml missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "tasks", "a.yaml")); err != nil {
		t.Errorf("archived task file missing: %v", err)
	}
	// The wsp workspace went with it, keyed on the project's branch.
	if len(wsp.branches) != 1 || wsp.branches[0] != "weather-tui" {
		t.Errorf("wsp removals = %v, want [weather-tui]", wsp.branches)
	}
}

// writeWorkspace lays out a stub wsp workspace for branch under root: a repo
// directory plus the orchestrator's `.orch` control directory, populated the way
// a dispatched agent leaves it. Returns the workspace root.
func writeWorkspace(t *testing.T, root, branch string) string {
	t.Helper()
	dir := filepath.Join(root, branch)
	if err := os.MkdirAll(filepath.Join(dir, ".orch"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".orch", "instructions.md"), []byte("do the thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "widget"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The workspace's `.orch` directory goes before wsp is asked to remove the
// workspace, so wsp never has to step over it — and nothing else in the
// workspace is touched by that cleanup.
func TestArchiveProjectRemovesOrchBeforeWorkspaceRemoval(t *testing.T) {
	root := t.TempDir()
	stubRoot := t.TempDir()
	projPath := writeProject(t, root, "weather-tui", "feat/weather", true)
	wspDir := writeWorkspace(t, stubRoot, "feat/weather")

	var orchAtRemoval, repoAtRemoval bool
	wsp := &fakeWspRemover{root: stubRoot}
	wsp.removeHook = func(string) {
		_, orchErr := os.Stat(filepath.Join(wspDir, ".orch"))
		orchAtRemoval = orchErr == nil
		_, repoErr := os.Stat(filepath.Join(wspDir, "widget"))
		repoAtRemoval = repoErr == nil
	}

	if err := archiveProject(root, projPath, wsp); err != nil {
		t.Fatalf("archiveProject: %v", err)
	}

	if orchAtRemoval {
		t.Error(".orch still present when wsp was asked to remove the workspace; it must go first")
	}
	if !repoAtRemoval {
		t.Error(".orch cleanup removed more than .orch — the repo dir was gone before wsp ran")
	}
	if _, err := os.Stat(filepath.Join(wspDir, ".orch")); !os.IsNotExist(err) {
		t.Errorf(".orch survived the archive (err=%v)", err)
	}
	if len(wsp.branches) != 1 || wsp.branches[0] != "feat/weather" {
		t.Errorf("wsp removals = %v, want [feat/weather]", wsp.branches)
	}
	if _, err := os.Stat(filepath.Join(root+".backup", "weather-tui", ".project.yaml")); err != nil {
		t.Errorf("archived project missing: %v", err)
	}
}

// A workspace with no `.orch` directory is not an error — the archive carries on
// to the workspace removal and the move.
func TestArchiveProjectSkipsOrchRemovalWhenAbsent(t *testing.T) {
	root := t.TempDir()
	stubRoot := t.TempDir()
	projPath := writeProject(t, root, "bare", "bare", true)
	if err := os.MkdirAll(filepath.Join(stubRoot, "bare", "widget"), 0o755); err != nil {
		t.Fatal(err)
	}
	wsp := &fakeWspRemover{root: stubRoot}

	if err := archiveProject(root, projPath, wsp); err != nil {
		t.Fatalf("archiveProject: %v", err)
	}
	if len(wsp.branches) != 1 || wsp.branches[0] != "bare" {
		t.Errorf("wsp removals = %v, want [bare]", wsp.branches)
	}
	if _, err := os.Stat(filepath.Join(root+".backup", "bare", ".project.yaml")); err != nil {
		t.Errorf("archived project missing: %v", err)
	}
}

// A branch wsp cannot resolve to a path leaves nothing to clean: the archive
// still proceeds (the workspace removal that follows is a no-op for a workspace
// wsp doesn't know about).
func TestArchiveProjectProceedsWhenWorkspacePathUnresolved(t *testing.T) {
	root := t.TempDir()
	projPath := writeProject(t, root, "unknown", "unknown", true)
	wsp := &fakeWspRemover{} // no root: Path fails

	if err := archiveProject(root, projPath, wsp); err != nil {
		t.Fatalf("archiveProject: %v", err)
	}
	if len(wsp.branches) != 1 || wsp.branches[0] != "unknown" {
		t.Errorf("wsp removals = %v, want [unknown]", wsp.branches)
	}
	if _, err := os.Stat(filepath.Join(root+".backup", "unknown", ".project.yaml")); err != nil {
		t.Errorf("archived project missing: %v", err)
	}
}

// rootPathRemover resolves every branch to the filesystem root. A wsp answer
// that absurd must be refused rather than acted on — `/.orch` is not ours to
// delete — and the refusal aborts the archive before anything moves.
type rootPathRemover struct{ fakeWspRemover }

func (r *rootPathRemover) Path(string) (string, error) { return "/", nil }

func TestArchiveProjectRefusesOrchAtFilesystemRoot(t *testing.T) {
	root := t.TempDir()
	projPath := writeProject(t, root, "absurd", "absurd", true)
	wsp := &rootPathRemover{}

	err := archiveProject(root, projPath, wsp)
	if err == nil {
		t.Fatal("expected archiveProject to refuse a workspace root of /")
	}
	if !strings.Contains(err.Error(), ".orch") {
		t.Errorf("error should name the .orch cleanup; got %q", err)
	}
	if _, statErr := os.Stat(projPath); statErr != nil {
		t.Errorf("project must be left in place: %v", statErr)
	}
	if len(wsp.branches) != 0 {
		t.Errorf("workspace was removed anyway: %v", wsp.branches)
	}
	if _, statErr := os.Stat(filepath.Join(root+".backup", "absurd")); !os.IsNotExist(statErr) {
		t.Errorf("project should not have moved (err=%v)", statErr)
	}
}

func TestArchiveProjectRefusesWithoutArchiveFlag(t *testing.T) {
	root := t.TempDir()
	projPath := writeProject(t, root, "messy", "messy", false)
	wsp := &fakeWspRemover{}

	err := archiveProject(root, projPath, wsp)
	if err == nil {
		t.Fatal("expected archiveProject to refuse a project without archive: true")
	}
	if !strings.Contains(err.Error(), "cleaned up") || !strings.Contains(err.Error(), ":cleanup") {
		t.Errorf("refusal should warn about cleanup and point at :cleanup; got %q", err)
	}
	// Nothing moved and no workspace was touched.
	if _, statErr := os.Stat(projPath); statErr != nil {
		t.Errorf("project should be intact after refused archive: %v", statErr)
	}
	if _, statErr := os.Stat(root + ".backup"); !os.IsNotExist(statErr) {
		t.Errorf("backup root should not exist after refused archive (err=%v)", statErr)
	}
	if len(wsp.branches) != 0 {
		t.Errorf("refused archive removed workspaces %v, want none", wsp.branches)
	}
}

func TestArchiveProjectRefusesUnloadableProject(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "broken")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	projPath := filepath.Join(dir, ".project.yaml")
	if err := os.WriteFile(projPath, []byte("description: [unterminated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wsp := &fakeWspRemover{}

	if err := archiveProject(root, projPath, wsp); err == nil {
		t.Error("expected archiveProject to refuse a project file it cannot load")
	}
	if _, err := os.Stat(projPath); err != nil {
		t.Errorf("project should be intact after refused archive: %v", err)
	}
	if len(wsp.branches) != 0 {
		t.Errorf("refused archive removed workspaces %v, want none", wsp.branches)
	}
}

func TestArchiveProjectSkipsWorkspaceRemovalWithoutBranch(t *testing.T) {
	root := t.TempDir()
	projPath := writeProject(t, root, "no-branch", "", true)
	wsp := &fakeWspRemover{}

	if err := archiveProject(root, projPath, wsp); err != nil {
		t.Fatalf("archiveProject: %v", err)
	}
	if len(wsp.branches) != 0 {
		t.Errorf("branchless project asked wsp to remove %v, want none", wsp.branches)
	}
	if _, err := os.Stat(filepath.Join(root+".backup", "no-branch", ".project.yaml")); err != nil {
		t.Errorf("project should still have been archived: %v", err)
	}
}

// A failed workspace removal aborts before the move so the project is never
// lost — the caller reports the failure instead.
func TestArchiveProjectAbortsWhenWorkspaceRemovalFails(t *testing.T) {
	root := t.TempDir()
	projPath := writeProject(t, root, "stuck", "stuck", true)
	wsp := &fakeWspRemover{err: errors.New("wsp rm: boom")}

	err := archiveProject(root, projPath, wsp)
	if err == nil {
		t.Fatal("expected archiveProject to fail when workspace removal fails")
	}
	if !strings.Contains(err.Error(), "wsp rm: boom") {
		t.Errorf("error should report the removal failure; got %q", err)
	}
	if _, statErr := os.Stat(projPath); statErr != nil {
		t.Errorf("project must be left in place when removal fails: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(root+".backup", "stuck")); !os.IsNotExist(statErr) {
		t.Errorf("project should not have moved (err=%v)", statErr)
	}
}

func TestArchiveProjectRefusesExistingArchive(t *testing.T) {
	root := t.TempDir()
	projPath := writeProject(t, root, "dup", "dup", true)
	// A prior archive of the same name already exists.
	prior := filepath.Join(root+".backup", "dup")
	if err := os.MkdirAll(prior, 0o755); err != nil {
		t.Fatal(err)
	}
	wsp := &fakeWspRemover{}

	if err := archiveProject(root, projPath, wsp); err == nil {
		t.Errorf("expected archiveProject to refuse clobbering an existing backup")
	}
	// Original must be left intact when the archive is refused.
	if _, err := os.Stat(projPath); err != nil {
		t.Errorf("original should be intact after refused archive: %v", err)
	}
	// The clobber check runs before the workspace is torn down.
	if len(wsp.branches) != 0 {
		t.Errorf("refused archive removed workspaces %v, want none", wsp.branches)
	}
}

func TestArchiveProjectRejectsOutsideRoot(t *testing.T) {
	root := t.TempDir()
	// A cleaned-up project that is not a direct child of root.
	elsewhere := filepath.Join(t.TempDir(), "nested")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	projPath := writeProject(t, elsewhere, "outside", "outside", true)
	wsp := &fakeWspRemover{}

	if err := archiveProject(root, projPath, wsp); err == nil {
		t.Errorf("expected archiveProject to reject a path outside the root")
	}
	if len(wsp.branches) != 0 {
		t.Errorf("rejected archive removed workspaces %v, want none", wsp.branches)
	}
}

func TestArchiveCommandOpensConfirmModal(t *testing.T) {
	root := t.TempDir()
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	m.projectRoot = root
	m.wspRemover = &fakeWspRemover{}
	m.projSel = writeProject(t, root, "alpha", "alpha", true)
	m = focusProjectsPane(t, m)

	m, _ = runProjectCommand(t, m, "archive")

	if m.mode != modeConfirmArchive {
		t.Fatalf("after picking `archive`, mode = %v, want modeConfirmArchive", m.mode)
	}
	if m.archiveTarget != m.projSel {
		t.Errorf("archiveTarget = %q, want %q", m.archiveTarget, m.projSel)
	}

	// n cancels without touching disk.
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	m = step.(model)
	if m.mode != modeNormal {
		t.Errorf("after n, mode = %v, want modeNormal", m.mode)
	}
	if _, err := os.Stat(m.projSel); err != nil {
		t.Errorf("project should be untouched after cancel: %v", err)
	}
}

// The guard fires before the confirm modal opens: a work stream that hasn't
// been cleaned up never gets a dialog, just a warning on the status line.
func TestArchiveCommandRefusesUncleanedProject(t *testing.T) {
	root := t.TempDir()
	wsp := &fakeWspRemover{}
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	m.projectRoot = root
	m.wspRemover = wsp
	m.projSel = writeProject(t, root, "messy", "messy", false)
	m = focusProjectsPane(t, m)

	m, _ = runProjectCommand(t, m, "archive")

	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal (no confirm modal for an unclean work stream)", m.mode)
	}
	if m.archiveTarget != "" {
		t.Errorf("archiveTarget = %q, want empty", m.archiveTarget)
	}
	if !strings.Contains(m.statusMsg, "cleaned up") || !strings.Contains(m.statusMsg, ":cleanup") {
		t.Errorf("statusMsg = %q, want a warning naming :cleanup", m.statusMsg)
	}
	if _, err := os.Stat(m.projSel); err != nil {
		t.Errorf("nothing should have moved: %v", err)
	}
	if _, err := os.Stat(root + ".backup"); !os.IsNotExist(err) {
		t.Errorf("backup root should not exist (err=%v)", err)
	}
	if len(wsp.branches) != 0 {
		t.Errorf("refused archive removed workspaces %v, want none", wsp.branches)
	}
}

// End-to-end through the modal: `y` on a cleaned-up work stream archives it and
// removes the wsp workspace for the project's branch.
func TestArchiveConfirmArchivesAndRemovesWorkspace(t *testing.T) {
	root := t.TempDir()
	wsp := &fakeWspRemover{}
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	m.projectRoot = root
	m.wspRemover = wsp
	m.projSel = writeProject(t, root, "beta", "beta-branch", true)
	m = focusProjectsPane(t, m)

	m, _ = runProjectCommand(t, m, "archive")
	if m.mode != modeConfirmArchive {
		t.Fatalf("mode = %v, want modeConfirmArchive", m.mode)
	}
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	m = step.(model)

	if m.mode != modeNormal {
		t.Errorf("after y, mode = %v, want modeNormal", m.mode)
	}
	if !strings.Contains(m.statusMsg, "archived beta") {
		t.Errorf("statusMsg = %q, want an archived confirmation", m.statusMsg)
	}
	if _, err := os.Stat(filepath.Join(root, "beta")); !os.IsNotExist(err) {
		t.Errorf("project dir still in the workspace root (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(root+".backup", "beta", ".project.yaml")); err != nil {
		t.Errorf("archived project missing: %v", err)
	}
	if len(wsp.branches) != 1 || wsp.branches[0] != "beta-branch" {
		t.Errorf("wsp removals = %v, want [beta-branch]", wsp.branches)
	}
}
