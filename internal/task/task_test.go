package task

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadExampleNoDeps(t *testing.T) {
	tk, err := Load("../../examples/tasks/01-add-healthz-handler.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if tk.Name != "add-healthz-handler" {
		t.Errorf("Name = %q", tk.Name)
	}
	if tk.Status != StatusReady {
		t.Errorf("Status = %q, want ready", tk.Status)
	}
	if len(tk.DependsOn) != 0 {
		t.Errorf("DependsOn = %v, want empty", tk.DependsOn)
	}
	if tk.Attempts != 0 {
		t.Errorf("Attempts = %d, want 0", tk.Attempts)
	}
}

func TestLoadExampleWithDeps(t *testing.T) {
	tk, err := Load("../../examples/tasks/02-add-readiness-probe.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(tk.DependsOn) != 1 || tk.DependsOn[0] != "add-healthz-handler" {
		t.Errorf("DependsOn = %v, want [add-healthz-handler]", tk.DependsOn)
	}
}

func TestRoundTrip(t *testing.T) {
	src, err := Load("../../examples/tasks/02-add-readiness-probe.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "t.yaml")
	if err := Save(dst, src); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded, err := Load(dst)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	// Path is set from the load path and intentionally not persisted, so
	// compare the rest of the struct after normalising it.
	srcCopy := *src
	srcCopy.Path = reloaded.Path
	if !reflect.DeepEqual(&srcCopy, reloaded) {
		t.Errorf("round-trip mismatch:\n src=%+v\n got=%+v", srcCopy, reloaded)
	}
	if reloaded.Path != dst {
		t.Errorf("Path = %q, want %q", reloaded.Path, dst)
	}
}

func TestCommitArtifactsRoundTrip(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "t.yaml")
	src := &Task{
		Name:        "x",
		Description: "do the thing",
		Status:      StatusCommitted,
		Summary:     "Renamed gwctl to cp and updated all callers.",
		Commits: []Commit{
			{Repo: "mcpruntime", Hash: "abc123"},
			{Repo: "sandboxes", Hash: "def456"},
		},
		CreatedFiles: []string{"/tmp/notes.md"},
	}
	if err := Save(dst, src); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(dst)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Summary != src.Summary {
		t.Errorf("Summary = %q, want %q", got.Summary, src.Summary)
	}
	if !reflect.DeepEqual(got.Commits, src.Commits) {
		t.Errorf("Commits = %+v, want %+v", got.Commits, src.Commits)
	}
	if !reflect.DeepEqual(got.CreatedFiles, src.CreatedFiles) {
		t.Errorf("CreatedFiles = %+v, want %+v", got.CreatedFiles, src.CreatedFiles)
	}
}

func TestCommitArtifactsOmittedWhenEmpty(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "t.yaml")
	if err := Save(dst, &Task{Name: "x", Status: StatusReady}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	for _, key := range []string{"summary:", "commits:", "created_files:"} {
		if strings.Contains(string(data), key) {
			t.Errorf("empty task yaml should not contain %q; got:\n%s", key, string(data))
		}
	}
}

func TestCompletedAtRoundTrip(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "t.yaml")
	when := time.Date(2026, 6, 21, 18, 49, 0, 0, time.UTC)
	if err := Save(dst, &Task{Name: "x", Status: StatusCommitted, CompletedAt: &when}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(dst)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.CompletedAt == nil || !got.CompletedAt.Equal(when) {
		t.Errorf("CompletedAt = %v, want %v", got.CompletedAt, when)
	}

	// A task that never completed must omit the field entirely.
	dst2 := filepath.Join(t.TempDir(), "t2.yaml")
	if err := Save(dst2, &Task{Name: "y", Status: StatusReady}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(dst2)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(data), "completed_at") {
		t.Errorf("nil CompletedAt should be omitted from yaml; got:\n%s", string(data))
	}
}

func TestLoadBackfillsModelDefault(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "nomodel.yaml")
	if err := writeRaw(dst, "name: x\nstatus: ready\n"); err != nil {
		t.Fatalf("writeRaw: %v", err)
	}
	tk, err := Load(dst)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if tk.Model != ModelDefault {
		t.Errorf("Model = %q, want %q (backfill when field is absent)", tk.Model, ModelDefault)
	}
}

func TestLoadPreservesExplicitModel(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "withmodel.yaml")
	if err := writeRaw(dst, "name: x\nstatus: ready\nmodel: haiku\n"); err != nil {
		t.Fatalf("writeRaw: %v", err)
	}
	tk, err := Load(dst)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if tk.Model != "haiku" {
		t.Errorf("Model = %q, want %q (explicit value must survive Load)", tk.Model, "haiku")
	}
}

func TestInvalidStatusRejected(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "bad.yaml")
	if err := writeRaw(dst, "name: x\nstatus: notathing\n"); err != nil {
		t.Fatalf("writeRaw: %v", err)
	}
	if _, err := Load(dst); err == nil {
		t.Errorf("Load accepted invalid status")
	}
}
