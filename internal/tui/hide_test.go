package tui

import (
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/slimslenderslacks/work/internal/project"
)

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// hideGalleryModel is a laid-out model holding three work streams, the middle
// one hidden — enough to tell "filtered out" apart from "list truncated".
func hideGalleryModel(t *testing.T) model {
	t.Helper()
	m := newModel(nil, nil, nil, &fakeAttacher{})
	m.width, m.height = 200, 40
	m.loaded = true
	m.projects = []ProjectView{
		{Name: "alpha", Path: "/alpha", Status: project.StatusIdle},
		{Name: "bravo", Path: "/bravo", Status: project.StatusIdle, Hidden: true},
		{Name: "charlie", Path: "/charlie", Status: project.StatusIdle},
	}
	m.projSel = "/alpha"
	return focusProjectsPane(t, m)
}

func TestProjectHideDefaultsFalse(t *testing.T) {
	path := writeProjectFile(t, t.TempDir(), "widget",
		"description: p\nbranch: b\nstatus: idle\nupdated_by: daemon\n")
	p, err := project.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.Hide {
		t.Error("hide = true on a project file that never set it; want false by default")
	}
	v, ok := loadProjectView(path)
	if !ok {
		t.Fatal("loadProjectView dropped the project")
	}
	if v.Hidden {
		t.Error("ProjectView.Hidden = true, want false by default")
	}
}

// TestRequestHideWritesAsDaemon is the crux of the flag's contract: hiding is
// display-only, so unlike `:cleanup`/`:review` it must NOT be written as the
// agent — a write the daemon acts on would re-route the work stream (and on a
// blocked one, re-summon the wolf) for the sake of a gallery toggle.
func TestRequestHideWritesAsDaemon(t *testing.T) {
	path := writeProjectFile(t, t.TempDir(), "widget",
		"description: p\nbranch: b\nstatus: blocked\nblocked_reason: stuck\nupdated_by: agent\n")

	name, err := requestHide(path)
	if err != nil {
		t.Fatalf("requestHide: %v", err)
	}
	if name != "widget" {
		t.Errorf("display name = %q, want widget", name)
	}

	p, err := project.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Hide {
		t.Error("hide = false, want true")
	}
	if p.UpdatedBy != project.WriterDaemon {
		t.Errorf("updated_by = %q, want daemon — hiding must not be routed as a project change", p.UpdatedBy)
	}
	// Nothing about how the work stream runs may change.
	if p.Status != project.StatusBlocked {
		t.Errorf("status = %q, want it left at blocked", p.Status)
	}
	if p.BlockedReason != "stuck" {
		t.Errorf("blocked_reason = %q, want it untouched", p.BlockedReason)
	}
}

func TestRequestShowClearsHide(t *testing.T) {
	path := writeProjectFile(t, t.TempDir(), "widget",
		"description: p\nbranch: b\nstatus: idle\nhide: true\nupdated_by: daemon\n")

	if _, err := requestShow(path); err != nil {
		t.Fatalf("requestShow: %v", err)
	}
	p, err := project.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.Hide {
		t.Error("hide = true after :show, want cleared")
	}
	if p.UpdatedBy != project.WriterDaemon {
		t.Errorf("updated_by = %q, want daemon", p.UpdatedBy)
	}
}

// TestHideOmitsKeyWhenUnset guards the `omitempty`: an unhidden project's file
// must not grow a `hide: false` line just because something round-tripped it.
func TestHideOmitsKeyWhenUnset(t *testing.T) {
	path := writeProjectFile(t, t.TempDir(), "widget",
		"description: p\nbranch: b\nstatus: idle\nhide: true\nupdated_by: daemon\n")
	if _, err := requestShow(path); err != nil {
		t.Fatalf("requestShow: %v", err)
	}
	data := readFileString(t, path)
	if strings.Contains(data, "hide:") {
		t.Errorf("project file still carries a hide key after :show:\n%s", data)
	}
}

func TestRequestHideOnAlreadyHiddenIsRefused(t *testing.T) {
	path := writeProjectFile(t, t.TempDir(), "widget",
		"description: p\nbranch: b\nstatus: idle\nhide: true\nupdated_by: daemon\n")
	_, err := requestHide(path)
	if err == nil {
		t.Fatal("requestHide on an already-hidden work stream should error")
	}
	if !strings.Contains(err.Error(), "already hidden") {
		t.Errorf("err = %q, want it to say the work stream is already hidden", err)
	}
}

func TestRequestShowOnVisibleProjectIsRefused(t *testing.T) {
	path := writeProjectFile(t, t.TempDir(), "widget",
		"description: p\nbranch: b\nstatus: idle\nupdated_by: daemon\n")
	_, err := requestShow(path)
	if err == nil {
		t.Fatal("requestShow on a work stream that isn't hidden should error")
	}
	if !strings.Contains(err.Error(), "not hidden") {
		t.Errorf("err = %q, want it to say the work stream isn't hidden", err)
	}
}

func TestRequestHideWithoutProjectErrors(t *testing.T) {
	if _, err := requestHide(""); err == nil {
		t.Error(`requestHide("") should error`)
	}
	if _, err := requestShow(""); err == nil {
		t.Error(`requestShow("") should error`)
	}
}

func TestCommandPickerHideSetsFlag(t *testing.T) {
	root := t.TempDir()
	m, projPath := selectProject(t, root, "widget")

	m, _ = runProjectCommand(t, m, "hide")

	if m.mode != modeNormal {
		t.Errorf("`hide` should not open a modal; mode = %v", m.mode)
	}
	if !strings.Contains(m.statusMsg, "hid widget") {
		t.Errorf("statusMsg = %q, want it to confirm the work stream was hidden", m.statusMsg)
	}
	if !strings.Contains(m.statusMsg, "tp") {
		t.Errorf("statusMsg = %q, want it to name the key that brings hidden streams back", m.statusMsg)
	}
	p, err := project.Load(projPath)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Hide {
		t.Error("hide = false after `:hide`, want true")
	}
}

// TestCommandPickerShowClearsFlag has to dispatch by key rather than by
// first-letter shortcut: "show" shares its "s" with `session`, which owns the
// shortcut (see projectCommands).
func TestCommandPickerShowClearsFlag(t *testing.T) {
	root := t.TempDir()
	m, projPath := selectProject(t, root, "widget")
	if _, err := requestHide(projPath); err != nil {
		t.Fatalf("requestHide: %v", err)
	}

	next, _ := m.dispatchProjectCommand("show")
	m = next.(model)

	if !strings.Contains(m.statusMsg, "showing widget") {
		t.Errorf("statusMsg = %q, want it to confirm the work stream is back", m.statusMsg)
	}
	p, err := project.Load(projPath)
	if err != nil {
		t.Fatal(err)
	}
	if p.Hide {
		t.Error("hide = true after `:show`, want cleared")
	}
}

func TestHideAndShowWithoutSelectionSurfaceErrors(t *testing.T) {
	for _, cmd := range []string{"hide", "show"} {
		m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
		m = focusProjectsPane(t, m)
		next, _ := m.dispatchProjectCommand(cmd)
		if got := next.(model).statusMsg; !strings.Contains(got, "no work stream") {
			t.Errorf("%s with no selection: statusMsg = %q, want it to explain nothing is selected", cmd, got)
		}
	}
}

// TestHiddenProjectsAreFilteredFromGallery is the user-visible half of the
// flag: a hidden work stream draws no card, while the streams around it are
// untouched.
func TestHiddenProjectsAreFilteredFromGallery(t *testing.T) {
	m := hideGalleryModel(t)

	view := m.View()
	if strings.Contains(view, "bravo") {
		t.Errorf("hidden work stream `bravo` should not be drawn; got:\n%s", view)
	}
	for _, name := range []string{"alpha", "charlie"} {
		if !strings.Contains(view, name) {
			t.Errorf("visible work stream %q should still be drawn; got:\n%s", name, view)
		}
	}
}

func TestTPTogglesHiddenProjects(t *testing.T) {
	m := hideGalleryModel(t)
	if m.showHidden {
		t.Fatal("showHidden should default to false — hidden work streams start hidden")
	}

	m = pressKey(m, "t")
	m = pressKey(m, "p")
	if !m.showHidden {
		t.Fatal("tp should have toggled showHidden on")
	}
	if m.pendingKey != "" {
		t.Errorf("pendingKey = %q after tp, want cleared", m.pendingKey)
	}
	if view := m.View(); !strings.Contains(view, "bravo") {
		t.Errorf("with the toggle on, the hidden work stream should be drawn; got:\n%s", view)
	}

	m = pressKey(m, "t")
	m = pressKey(m, "p")
	if m.showHidden {
		t.Error("second tp should have toggled showHidden back off")
	}
	if view := m.View(); strings.Contains(view, "bravo") {
		t.Errorf("with the toggle off again, the hidden work stream should be gone; got:\n%s", view)
	}
}

// TestTPMarksHiddenCards covers the affordance that makes `:show` usable: with
// the toggle on, a hidden card has to be distinguishable from the rest or
// there's no way to tell which one to run `:show` against.
func TestTPMarksHiddenCards(t *testing.T) {
	m := hideGalleryModel(t)
	m.showHidden = true
	view := m.View()
	if !strings.Contains(view, "⊘ bravo") {
		t.Errorf("a hidden card should be marked as hidden; got:\n%s", view)
	}
	if strings.Contains(view, "⊘ alpha") {
		t.Errorf("a visible card must not carry the hidden marker; got:\n%s", view)
	}
}

// TestTPToggleOffMovesSelectionOffHiddenProject covers the selection hazard of
// the toggle: the cursor can be sitting on a hidden card when it's switched
// off, and must land on a work stream that's still on screen rather than
// pointing at nothing.
func TestTPToggleOffMovesSelectionOffHiddenProject(t *testing.T) {
	m := hideGalleryModel(t)
	m.showHidden = true
	m.projSel = "/bravo"

	m = pressKey(m, "t")
	m = pressKey(m, "p")

	if m.showHidden {
		t.Fatal("tp should have toggled showHidden off")
	}
	if m.projSel == "/bravo" {
		t.Error("selection is still on the now-hidden work stream")
	}
	if m.projSel != "/alpha" {
		t.Errorf("projSel = %q, want it reconciled to the first visible work stream", m.projSel)
	}
}

// TestSelectionSkipsHiddenProjects makes sure j/k can't walk the cursor onto a
// card that isn't drawn — moving down from the first visible stream must reach
// the next visible one, not the hidden stream between them.
func TestSelectionSkipsHiddenProjects(t *testing.T) {
	m := hideGalleryModel(t)
	m = pressKey(m, "j")
	if m.projSel != "/charlie" {
		t.Errorf("projSel = %q, want /charlie — j must skip the hidden work stream", m.projSel)
	}
}

// TestProjectsSnapshotReconcileDropsHiddenSelection is the live-update path:
// hiding happens on disk, so the next scan is what actually removes the card,
// and that snapshot's reconcile has to move the cursor.
func TestProjectsSnapshotReconcileDropsHiddenSelection(t *testing.T) {
	m := hideGalleryModel(t)
	m.projCh = make(<-chan []ProjectView)
	m.projSel = "/charlie"

	next, _ := m.Update(projectsMsg{views: []ProjectView{
		{Name: "alpha", Path: "/alpha", Status: project.StatusIdle},
		{Name: "charlie", Path: "/charlie", Status: project.StatusIdle, Hidden: true},
	}})
	m = next.(model)

	if m.projSel != "/alpha" {
		t.Errorf("projSel = %q, want /alpha once the selected work stream became hidden", m.projSel)
	}
}

// TestGalleryExplainsAnEmptyPaneCausedByHiding keeps an all-hidden root from
// reading as "every work stream is gone".
func TestGalleryExplainsAnEmptyPaneCausedByHiding(t *testing.T) {
	m := newModel(nil, nil, nil, &fakeAttacher{})
	m.width, m.height = 200, 40
	m.loaded = true
	m.projects = []ProjectView{
		{Name: "alpha", Path: "/alpha", Status: project.StatusIdle, Hidden: true},
		{Name: "bravo", Path: "/bravo", Status: project.StatusIdle, Hidden: true},
	}
	m = focusProjectsPane(t, m)

	view := m.View()
	if !strings.Contains(view, "2 hidden") {
		t.Errorf("an empty-because-hidden gallery should report the hidden count; got:\n%s", view)
	}
	if !strings.Contains(view, "tp") {
		t.Errorf("an empty-because-hidden gallery should name the toggle key; got:\n%s", view)
	}
}

func TestFooterAdvertisesHiddenToggle(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 250, Height: 40})
	m = sized.(model)
	if view := m.View(); !strings.Contains(view, "tp: show hidden") {
		t.Errorf("footer should advertise the tp toggle; got:\n%s", view)
	}
	m.showHidden = true
	if view := m.View(); !strings.Contains(view, "tp: hide hidden") {
		t.Errorf("footer should flip the tp hint once hidden streams are showing; got:\n%s", view)
	}
}

// TestTChordFallsThroughForNonToggleKeys re-checks the chord's fall-through
// now that "p" is one of its completers: a key that isn't l/r/p must still be
// handled fresh rather than swallowed.
func TestTChordFallsThroughWithHiddenToggleAdded(t *testing.T) {
	m := hideGalleryModel(t)
	m = pressKey(m, "t")
	m = pressKey(m, "j")
	if m.showHidden {
		t.Error("`tj` must not toggle hidden work streams")
	}
	if m.projSel != "/charlie" {
		t.Errorf("projSel = %q, want the trailing j handled as a selection move", m.projSel)
	}
}

func TestProjectDetailFlagsHiddenWorkStream(t *testing.T) {
	v := ProjectView{Name: "bravo", Path: "/bravo", Status: project.StatusIdle, Hidden: true}
	body := renderProjectDetailBody(v, nil, 80)
	if !strings.Contains(body, "hidden from the gallery") {
		t.Errorf("detail pane should flag a hidden work stream; got:\n%s", body)
	}
	if !strings.Contains(body, ":show") {
		t.Errorf("detail pane should name the command that restores it; got:\n%s", body)
	}
}

func TestProjectViewEqualNoticesHiddenChange(t *testing.T) {
	a := ProjectView{Name: "x", Path: "/x"}
	b := a
	b.Hidden = true
	if projectViewEqual(a, b) {
		t.Error("projectViewEqual should notice a change to Hidden, or the gallery won't refresh when a stream is hidden")
	}
}
