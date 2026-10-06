package tui

import (
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/project"
)

func mergedModel(t *testing.T, status project.Status) model {
	t.Helper()
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	m.projects = []ProjectView{{Name: "p", Path: "/x/p/.project.yaml", Status: status}}
	m.projSel = "/x/p/.project.yaml"
	return focusProjectsPane(t, m)
}

func TestRenderStatusMergedDistinctFromIdle(t *testing.T) {
	if statusMerged.GetForeground() == statusDone.GetForeground() {
		t.Error("merged should use a colour distinct from idle")
	}
	if got := renderStatus("merged"); !strings.Contains(got, "merged") {
		t.Errorf("renderStatus(merged) = %q, want label", got)
	}
}

func TestCommandPickerHighlightsCleanupWhenMerged(t *testing.T) {
	m := openCommandPicker(t, mergedModel(t, project.StatusMerged))
	if want := projectCommandIndex("cleanup"); m.cmdPickerIdx != want {
		t.Errorf("idx = %d, want cleanup row %d", m.cmdPickerIdx, want)
	}
	if !strings.Contains(m.renderCommandPickerModal(), "ready to :cleanup/archive") {
		t.Error("picker should show the merged cleanup hint")
	}
}

func TestCommandPickerNoCleanupHintForOtherStatuses(t *testing.T) {
	m := openCommandPicker(t, mergedModel(t, project.StatusIdle))
	if m.cmdPickerIdx != 0 {
		t.Errorf("idx = %d, want 0", m.cmdPickerIdx)
	}
	if strings.Contains(m.renderCommandPickerModal(), "ready to :cleanup/archive") {
		t.Error("hint should only appear for merged projects")
	}
}
