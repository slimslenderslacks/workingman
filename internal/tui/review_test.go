package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/project"
)

func TestCommandPickerReviewStartsReviewing(t *testing.T) {
	root := t.TempDir()
	m, projPath := selectProject(t, root, "widget") // seeded status:done

	m, _ = runProjectCommand(t, m, "review")

	if m.mode != modeNormal {
		t.Errorf("`review` should not open a modal; mode = %v", m.mode)
	}
	if !strings.Contains(m.statusMsg, "PR") {
		t.Errorf("statusMsg = %q, want it to confirm PR watching", m.statusMsg)
	}

	p, err := project.Load(projPath)
	if err != nil {
		t.Fatalf("loading project: %v", err)
	}
	if p.Status != project.StatusReviewing {
		t.Errorf("project status = %q, want reviewing", p.Status)
	}
	// Written as agent so the daemon acts on it rather than filtering its own write.
	if p.UpdatedBy != project.WriterAgent {
		t.Errorf("project updated_by = %q, want agent", p.UpdatedBy)
	}
}

func TestCommandPickerReviewWithoutSelectionSurfacesError(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	m = focusProjectsPane(t, m)
	m, _ = runProjectCommand(t, m, "review")

	if !strings.Contains(m.statusMsg, "no work stream") {
		t.Errorf("statusMsg = %q, want it to explain no work stream is selected", m.statusMsg)
	}
}

// TestRequestReviewRefusesNonDone pins the gate: review is the manual entry for
// a finished work stream. Starting it on a working project would interrupt live
// task dispatch, so it must be refused with an explanatory error.
func TestRequestReviewRefusesNonDone(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "widget")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".project.yaml")
	yaml := "description: p\nbranch: b\nstatus: working\nupdated_by: daemon\n"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, _, err := requestReview(path); err == nil {
		t.Errorf("requestReview on a working project should error")
	}
	// And it must not have changed the status.
	p, err := project.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != project.StatusWorking {
		t.Errorf("status = %q, want working left untouched", p.Status)
	}
}

func TestRequestReviewWithoutProjectErrors(t *testing.T) {
	if _, _, err := requestReview(""); err == nil {
		t.Errorf("requestReview(\"\") should error")
	}
}

// TestRequestReviewKicksReviewingProject pins the overload: `:review` on a
// project already in the loop must NOT re-flip status (it's already reviewing)
// but instead set the one-shot ReviewNow request the daemon acts on, and report
// kicked=true so the caller can say "running now" rather than "watching".
func TestRequestReviewKicksReviewingProject(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "widget")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".project.yaml")
	yaml := "description: p\nbranch: b\nstatus: reviewing\nupdated_by: daemon\n"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	_, kicked, err := requestReview(path)
	if err != nil {
		t.Fatalf("requestReview on a reviewing project: %v", err)
	}
	if !kicked {
		t.Errorf("kicked = false, want true for an already-reviewing project")
	}

	p, err := project.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != project.StatusReviewing {
		t.Errorf("status = %q, want reviewing left untouched", p.Status)
	}
	if !p.ReviewNow {
		t.Errorf("ReviewNow = false, want the request flag set")
	}
	// Written as agent so the daemon sees it (its own daemon writes are filtered).
	if p.UpdatedBy != project.WriterAgent {
		t.Errorf("updated_by = %q, want agent", p.UpdatedBy)
	}
}
