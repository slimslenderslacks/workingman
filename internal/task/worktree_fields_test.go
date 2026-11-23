package task

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestTouchesEmptySurvivesSave pins the absent-vs-empty distinction H1 relies
// on: an explicit `touches: []` ("touches nothing") must survive a Save, and an
// absent field must stay absent rather than becoming an empty list.
func TestTouchesEmptySurvivesSave(t *testing.T) {
	dir := t.TempDir()

	empty := filepath.Join(dir, "empty.yaml")
	if err := writeRaw(empty, "name: x\nstatus: ready\ntouches: []\n"); err != nil {
		t.Fatalf("writeRaw: %v", err)
	}
	tk, err := Load(empty)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if tk.Touches == nil || len(*tk.Touches) != 0 {
		t.Fatalf("Touches = %v, want non-nil empty", tk.Touches)
	}
	if tk.Exclusive() {
		t.Error("touches: [] must not be exclusive")
	}
	if err := Save(empty, tk); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, _ := os.ReadFile(empty)
	if !strings.Contains(string(data), "touches: []") {
		t.Errorf("saved yaml lost touches: []; got:\n%s", data)
	}
	again, err := Load(empty)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if again.Touches == nil || len(*again.Touches) != 0 {
		t.Errorf("after round-trip Touches = %v, want non-nil empty", again.Touches)
	}

	absent := filepath.Join(dir, "absent.yaml")
	if err := writeRaw(absent, "name: y\nstatus: ready\n"); err != nil {
		t.Fatalf("writeRaw: %v", err)
	}
	tk, err = Load(absent)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if tk.Touches != nil {
		t.Errorf("absent touches loaded as %v, want nil", *tk.Touches)
	}
	if !tk.Exclusive() {
		t.Error("a task with no hints must be exclusive")
	}
	if err := Save(absent, tk); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, _ = os.ReadFile(absent)
	if strings.Contains(string(data), "touches") {
		t.Errorf("absent touches should stay absent; got:\n%s", data)
	}
}

func TestExclusive(t *testing.T) {
	globs := []string{"workingman/internal/api/*.go"}
	none := []string{}
	cases := []struct {
		name string
		task Task
		want bool
	}{
		{"no hints", Task{}, true},
		{"empty touches", Task{Touches: &none}, false},
		{"touches", Task{Touches: &globs}, false},
		{"areas only", Task{Areas: []string{"go-deps"}}, false},
		{"empty areas, no touches", Task{Areas: []string{}}, true},
	}
	for _, c := range cases {
		if got := c.task.Exclusive(); got != c.want {
			t.Errorf("%s: Exclusive() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestHasTaskBranch(t *testing.T) {
	if (&Task{}).HasTaskBranch() {
		t.Error("legacy task reported a task branch")
	}
	if !(&Task{TaskBranch: "wm/main/x"}).HasTaskBranch() {
		t.Error("task with task_branch reported none")
	}
}

// TestWorktreeFieldsRoundTrip saves and reloads a task carrying every
// parallel-worktree field (D1) and checks each one survives intact.
func TestWorktreeFieldsRoundTrip(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "t.yaml")
	when := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	touches := []string{"workingman/internal/api/healthz*.go", "workingman/docs/api.md"}
	src := &Task{
		Name:        "add-healthz",
		Status:      StatusSuccess,
		Touches:     &touches,
		Areas:       []string{"go-deps"},
		Worktree:    "/ws/proj/.worktrees/add-healthz",
		TaskBranch:  "wm/proj/add-healthz",
		Base:        []Commit{{Repo: "workingman", Hash: "abc123"}},
		SucceededAt: &when,
		Integration: &Integration{
			Result:    "verify_failed",
			Repo:      "workingman",
			Files:     []string{"internal/api/router.go"},
			Onto:      []Commit{{Repo: "workingman", Hash: "def456"}},
			Output:    "FAIL ./internal/api",
			Published: []string{"other"},
			Landed:    []string{"fix-router"},
			Attempts:  2,
		},
		TouchesActual: []string{"workingman/go.mod"},
	}
	if err := Save(dst, src); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(dst)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Touches == nil || !reflect.DeepEqual(*got.Touches, touches) {
		t.Errorf("Touches = %v, want %v", got.Touches, touches)
	}
	if !reflect.DeepEqual(got.Areas, src.Areas) {
		t.Errorf("Areas = %v, want %v", got.Areas, src.Areas)
	}
	if got.Worktree != src.Worktree || got.TaskBranch != src.TaskBranch {
		t.Errorf("Worktree/TaskBranch = %q/%q, want %q/%q", got.Worktree, got.TaskBranch, src.Worktree, src.TaskBranch)
	}
	if !reflect.DeepEqual(got.Base, src.Base) {
		t.Errorf("Base = %+v, want %+v", got.Base, src.Base)
	}
	if got.SucceededAt == nil || !got.SucceededAt.Equal(when) {
		t.Errorf("SucceededAt = %v, want %v", got.SucceededAt, when)
	}
	if !reflect.DeepEqual(got.Integration, src.Integration) {
		t.Errorf("Integration = %+v, want %+v", got.Integration, src.Integration)
	}
	if !reflect.DeepEqual(got.TouchesActual, src.TouchesActual) {
		t.Errorf("TouchesActual = %v, want %v", got.TouchesActual, src.TouchesActual)
	}
}

// TestOlderTaskFileUnchanged checks a task file written before the worktree
// fields existed loads with every new field at its zero value and saves back
// without gaining any of their keys.
func TestOlderTaskFileUnchanged(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "old.yaml")
	old := "name: old\ndescription: d\ndepends_on: []\nstatus: committed\nattempts: 1\nfailure_reason: \"\"\nblocked_reason: \"\"\nmodel: default\n"
	if err := writeRaw(dst, old); err != nil {
		t.Fatalf("writeRaw: %v", err)
	}
	tk, err := Load(dst)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if tk.Touches != nil || tk.Areas != nil || tk.Worktree != "" || tk.TaskBranch != "" ||
		tk.Base != nil || tk.SucceededAt != nil || tk.Integration != nil || tk.TouchesActual != nil {
		t.Errorf("older file grew worktree fields: %+v", tk)
	}
	if !tk.Exclusive() || tk.HasTaskBranch() {
		t.Errorf("older file: Exclusive=%v HasTaskBranch=%v, want true/false", tk.Exclusive(), tk.HasTaskBranch())
	}
	if err := Save(dst, tk); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, _ := os.ReadFile(dst)
	for _, key := range []string{"touches", "areas:", "worktree:", "task_branch:", "base:", "succeeded_at:", "integration:"} {
		if strings.Contains(string(data), key) {
			t.Errorf("saved older file contains %q; got:\n%s", key, data)
		}
	}
}
