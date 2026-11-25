package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/project"
)

// writeYAML drops raw project.yaml content for a work stream named name under
// root and returns its path.
func writeYAML(t *testing.T, root, name, yaml string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "project.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func load(t *testing.T, path string) *project.Project {
	t.Helper()
	p, err := project.Load(path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	return p
}

// assertRefused checks that err is a refusal mentioning want and that the
// file was left byte-for-byte as it was.
func assertRefused(t *testing.T, err error, want, path, before string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want refusal containing %q, got nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err, want)
	}
	if after := readFile(t, path); after != before {
		t.Errorf("refused action rewrote the file:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestDisplayName(t *testing.T) {
	if got := DisplayName("/orch/widget/project.yaml"); got != "widget" {
		t.Errorf("DisplayName = %q, want widget", got)
	}
	if got := DisplayName(""); got != "(none)" {
		t.Errorf("DisplayName(\"\") = %q, want (none)", got)
	}
}

func TestEmptyPathIsRefused(t *testing.T) {
	for name, fn := range map[string]func(string) (string, error){
		"Stop":       Stop,
		"Start":      Start,
		"SummonWolf": SummonWolf,
		"Cleanup":    Cleanup,
		"Hide":       Hide,
		"Show":       Show,
		"Review": func(p string) (string, error) {
			n, _, err := Review(p)
			return n, err
		},
	} {
		if _, err := fn(""); err == nil || !strings.Contains(err.Error(), "selected") {
			t.Errorf("%s(\"\") error = %v, want a no-selection refusal", name, err)
		}
	}
}

func TestMissingFileIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gone", "project.yaml")
	if _, err := Stop(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stop on missing file: err = %v, want ErrNotExist", err)
	}
	if _, err := LoadArchivable(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("LoadArchivable on missing file: err = %v, want ErrNotExist", err)
	}
}

func TestStopRecordsStatusAndWritesAsAgent(t *testing.T) {
	path := writeYAML(t, t.TempDir(), "widget", "description: d\nstatus: working\nupdated_by: daemon\n")
	name, err := Stop(path)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if name != "widget" {
		t.Errorf("name = %q, want widget", name)
	}
	p := load(t, path)
	if p.Status != project.StatusStopped || p.StoppedFrom != project.StatusWorking {
		t.Errorf("status/stopped_from = %q/%q, want stopped/working", p.Status, p.StoppedFrom)
	}
	if p.UpdatedBy != project.WriterAgent {
		t.Errorf("updated_by = %q, want agent", p.UpdatedBy)
	}
}

func TestStopRefusals(t *testing.T) {
	root := t.TempDir()
	stopped := writeYAML(t, root, "a", "description: d\nstatus: stopped\nstopped_from: idle\n")
	before := readFile(t, stopped)
	_, err := Stop(stopped)
	assertRefused(t, err, "a is already stopped", stopped, before)

	empty := writeYAML(t, root, "b", "")
	_, err = Stop(empty)
	assertRefused(t, err, "b has no status yet to stop", empty, "")
}

func TestStartRestoresStoppedFrom(t *testing.T) {
	path := writeYAML(t, t.TempDir(), "widget", "description: d\nstatus: stopped\nstopped_from: blocked\n")
	if _, err := Start(path); err != nil {
		t.Fatalf("Start: %v", err)
	}
	p := load(t, path)
	if p.Status != project.StatusBlocked || p.StoppedFrom != "" {
		t.Errorf("status/stopped_from = %q/%q, want blocked/empty", p.Status, p.StoppedFrom)
	}
	if p.UpdatedBy != project.WriterAgent {
		t.Errorf("updated_by = %q, want agent", p.UpdatedBy)
	}
}

func TestStartWithoutStoppedFromFallsBackToIdle(t *testing.T) {
	path := writeYAML(t, t.TempDir(), "widget", "description: d\nstatus: stopped\n")
	if _, err := Start(path); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if p := load(t, path); p.Status != project.StatusIdle {
		t.Errorf("status = %q, want idle", p.Status)
	}
}

func TestStartRefusesWhenNotStopped(t *testing.T) {
	path := writeYAML(t, t.TempDir(), "widget", "description: d\nstatus: working\n")
	before := readFile(t, path)
	_, err := Start(path)
	assertRefused(t, err, `widget is not stopped (status "working")`, path, before)
}

func TestSummonWolfBlocksWithReason(t *testing.T) {
	path := writeYAML(t, t.TempDir(), "widget", "description: d\nstatus: idle\nupdated_by: daemon\n")
	if _, err := SummonWolf(path); err != nil {
		t.Fatalf("SummonWolf: %v", err)
	}
	p := load(t, path)
	if p.Status != project.StatusBlocked {
		t.Errorf("status = %q, want blocked", p.Status)
	}
	if p.BlockedReason != SummonReason {
		t.Errorf("blocked_reason = %q, want SummonReason", p.BlockedReason)
	}
	if p.UpdatedBy != project.WriterAgent {
		t.Errorf("updated_by = %q, want agent", p.UpdatedBy)
	}
}

func TestSummonWolfKeepsExistingReason(t *testing.T) {
	path := writeYAML(t, t.TempDir(), "widget", "description: d\nstatus: blocked\nblocked_reason: task foo crashed\n")
	if _, err := SummonWolf(path); err != nil {
		t.Fatalf("SummonWolf: %v", err)
	}
	if p := load(t, path); p.BlockedReason != "task foo crashed" {
		t.Errorf("blocked_reason = %q, want the original diagnosis kept", p.BlockedReason)
	}
}

func TestReviewStartsLoopOnIdle(t *testing.T) {
	path := writeYAML(t, t.TempDir(), "widget", "description: d\nstatus: idle\n")
	name, kicked, err := Review(path)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if name != "widget" || kicked {
		t.Errorf("name/kicked = %q/%v, want widget/false", name, kicked)
	}
	p := load(t, path)
	if !p.Review || p.ReviewNow {
		t.Errorf("review/review_now = %v/%v, want true/false", p.Review, p.ReviewNow)
	}
	if p.UpdatedBy != project.WriterAgent {
		t.Errorf("updated_by = %q, want agent", p.UpdatedBy)
	}
}

func TestReviewKicksWhenWatching(t *testing.T) {
	path := writeYAML(t, t.TempDir(), "widget", "description: d\nstatus: idle\nreview: true\n")
	if !load(t, path).WatchingPR() {
		t.Fatal("fixture should be watching a PR")
	}
	_, kicked, err := Review(path)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if !kicked {
		t.Error("kicked = false, want true for a project already watching")
	}
	if p := load(t, path); !p.ReviewNow {
		t.Error("review_now = false, want true")
	}
}

func TestReviewRefusesNonIdle(t *testing.T) {
	path := writeYAML(t, t.TempDir(), "widget", "description: d\nstatus: working\n")
	before := readFile(t, path)
	_, _, err := Review(path)
	assertRefused(t, err, `review starts on an idle work stream or kicks one already watching a PR (this one is "working")`, path, before)
}

func TestCleanupSetsFlag(t *testing.T) {
	path := writeYAML(t, t.TempDir(), "widget", "description: d\nstatus: idle\n")
	if _, err := Cleanup(path); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	p := load(t, path)
	if !p.Cleanup {
		t.Error("cleanup = false, want true")
	}
	if p.Status != project.StatusIdle {
		t.Errorf("status = %q, want it left at idle", p.Status)
	}
	if p.UpdatedBy != project.WriterAgent {
		t.Errorf("updated_by = %q, want agent", p.UpdatedBy)
	}
}

func TestCleanupRefusesArchived(t *testing.T) {
	path := writeYAML(t, t.TempDir(), "widget", "description: d\nstatus: idle\narchive: true\n")
	before := readFile(t, path)
	_, err := Cleanup(path)
	assertRefused(t, err, "widget is already cleaned up — run :archive", path, before)
}

func TestHideAndShowWriteAsDaemon(t *testing.T) {
	path := writeYAML(t, t.TempDir(), "widget", "description: d\nstatus: blocked\nupdated_by: agent\n")
	if _, err := Hide(path); err != nil {
		t.Fatalf("Hide: %v", err)
	}
	p := load(t, path)
	if !p.Hide || p.UpdatedBy != project.WriterDaemon {
		t.Errorf("hide/updated_by = %v/%q, want true/daemon", p.Hide, p.UpdatedBy)
	}
	if p.Status != project.StatusBlocked {
		t.Errorf("status = %q, want it untouched", p.Status)
	}
	if _, err := Show(path); err != nil {
		t.Fatalf("Show: %v", err)
	}
	p = load(t, path)
	if p.Hide || p.UpdatedBy != project.WriterDaemon {
		t.Errorf("hide/updated_by = %v/%q, want false/daemon", p.Hide, p.UpdatedBy)
	}
}

func TestHideShowRefuseNoop(t *testing.T) {
	root := t.TempDir()
	hidden := writeYAML(t, root, "a", "description: d\nstatus: idle\nhide: true\n")
	before := readFile(t, hidden)
	_, err := Hide(hidden)
	assertRefused(t, err, "a is already hidden", hidden, before)

	shown := writeYAML(t, root, "b", "description: d\nstatus: idle\n")
	before = readFile(t, shown)
	_, err = Show(shown)
	assertRefused(t, err, "b is not hidden", shown, before)
}

// fakeWsp records the branches Archive asks it to remove. Path resolves to
// <root>/<branch> when root is set and fails otherwise.
type fakeWsp struct {
	root    string
	removed []string
	forced  []bool
	err     error
	sawOrch bool // whether .orch still existed when Remove ran
}

func (f *fakeWsp) Path(branch string) (string, error) {
	if f.root == "" {
		return "", errors.New("no workspace root")
	}
	return filepath.Join(f.root, branch), nil
}

func (f *fakeWsp) Remove(_ context.Context, branch string, force bool) error {
	if f.root != "" {
		_, err := os.Stat(filepath.Join(f.root, branch, ".orch"))
		f.sawOrch = err == nil
	}
	f.removed = append(f.removed, branch)
	f.forced = append(f.forced, force)
	return f.err
}

func TestBackupRoot(t *testing.T) {
	if got := BackupRoot("/home/u/orch/"); got != "/home/u/orch.backup" {
		t.Errorf("BackupRoot = %q", got)
	}
	if got := BackupRoot(""); got != "" {
		t.Errorf("BackupRoot(\"\") = %q, want empty", got)
	}
}

func TestLoadArchivableRefusesUncleaned(t *testing.T) {
	path := writeYAML(t, t.TempDir(), "widget", "description: d\nstatus: idle\n")
	if _, err := LoadArchivable(path); err == nil || err.Error() != "widget has not been cleaned up — run :cleanup first" {
		t.Errorf("err = %v, want the :cleanup-first refusal", err)
	}
	if _, err := LoadArchivable(""); err == nil || err.Error() != "no work stream selected" {
		t.Errorf("err = %v, want no-selection refusal", err)
	}
}

func TestArchiveMovesProjectAndRemovesWorkspace(t *testing.T) {
	root := t.TempDir()
	path := writeYAML(t, root, "widget", "description: d\nbranch: widget-br\nstatus: idle\narchive: true\n")
	stub := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stub, "widget-br", ".orch"), 0o755); err != nil {
		t.Fatal(err)
	}
	wsp := &fakeWsp{root: stub}

	if err := Archive(root, path, wsp); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Errorf("project dir still in root (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(root+".backup", "widget", "project.yaml")); err != nil {
		t.Errorf("archived project.yaml missing: %v", err)
	}
	if len(wsp.removed) != 1 || wsp.removed[0] != "widget-br" || !wsp.forced[0] {
		t.Errorf("wsp removals = %v forced=%v, want [widget-br] forced", wsp.removed, wsp.forced)
	}
	if wsp.sawOrch {
		t.Error(".orch should be removed before the workspace")
	}
}

func TestArchiveWithoutWorkspaceManager(t *testing.T) {
	root := t.TempDir()
	path := writeYAML(t, root, "widget", "description: d\nbranch: b\nstatus: idle\narchive: true\n")
	if err := Archive(root, path, nil); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root+".backup", "widget")); err != nil {
		t.Errorf("archived dir missing: %v", err)
	}
}

func TestArchiveRefusals(t *testing.T) {
	root := t.TempDir()
	archivable := "description: d\nbranch: b\nstatus: idle\narchive: true\n"

	if err := Archive("", "/x/project.yaml", nil); err == nil || err.Error() != "no orch root configured" {
		t.Errorf("no root: err = %v", err)
	}
	if err := Archive(root, "", nil); err == nil || err.Error() != "no project selected" {
		t.Errorf("no project: err = %v", err)
	}

	uncleaned := writeYAML(t, root, "raw", "description: d\nstatus: idle\n")
	if err := Archive(root, uncleaned, nil); err == nil || !strings.Contains(err.Error(), "run :cleanup first") {
		t.Errorf("uncleaned: err = %v", err)
	}

	outside := writeYAML(t, t.TempDir(), "elsewhere", archivable)
	if err := Archive(root, outside, nil); err == nil || !strings.Contains(err.Error(), "is not a project under") {
		t.Errorf("outside root: err = %v", err)
	}

	dup := writeYAML(t, root, "dup", archivable)
	if err := os.MkdirAll(filepath.Join(root+".backup", "dup"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Archive(root, dup, nil); err == nil || !strings.Contains(err.Error(), "already archived at") {
		t.Errorf("existing backup: err = %v", err)
	}

	failing := writeYAML(t, root, "failing", archivable)
	wsp := &fakeWsp{err: errors.New("wsp wedged")}
	err := Archive(root, failing, wsp)
	if err == nil || !strings.Contains(err.Error(), "workspace b not removed (wsp wedged); failing left in place") {
		t.Errorf("wsp failure: err = %v", err)
	}
	for _, p := range []string{uncleaned, outside, dup, failing} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s should be left in place after a refusal: %v", p, err)
		}
	}
}
