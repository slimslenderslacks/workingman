package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaxParallelClamps(t *testing.T) {
	cases := []struct{ set, want int }{
		{0, 1}, {-3, 1}, {1, 1}, {2, 2}, {8, 8},
	}
	for _, c := range cases {
		if got := (&Project{MaxParallelTasks: c.set}).MaxParallel(); got != c.want {
			t.Errorf("MaxParallelTasks %d: MaxParallel() = %d, want %d", c.set, got, c.want)
		}
	}
}

func TestParallelFieldsRoundTrip(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "project.yaml")
	src := &Project{
		Description: "x", Branch: "b", Status: StatusWorking,
		MaxParallelTasks:   3,
		Verify:             "go build ./...",
		ConflictResolution: ConflictResolutionAgent,
	}
	if err := Save(dst, src); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(dst)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.MaxParallelTasks != 3 || got.Verify != src.Verify || got.ConflictResolution != src.ConflictResolution {
		t.Errorf("round-trip = %+v, want fields of %+v", got, src)
	}
	if got.MaxParallel() != 3 || got.EffectiveConflictResolution() != ConflictResolutionAgent {
		t.Errorf("MaxParallel/EffectiveConflictResolution = %d/%q", got.MaxParallel(), got.EffectiveConflictResolution())
	}
	if w := got.Warnings(); len(w) != 0 {
		t.Errorf("Warnings() = %v, want none", w)
	}
}

// TestOlderProjectFileUnchanged checks a project.yaml written before the
// parallel fields existed loads with the legacy behavior and re-saves without
// gaining their keys.
func TestOlderProjectFileUnchanged(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "project.yaml")
	old := "description: x\nrepos:\n  - org: o\n    name: r\nbranch: b\nstatus: working\nupdated_by: agent\n"
	if err := os.WriteFile(dst, []byte(old), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	p, err := Load(dst)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.MaxParallel() != 1 || p.Verify != "" || p.EffectiveConflictResolution() != ConflictResolutionRerun {
		t.Errorf("older file: MaxParallel=%d Verify=%q conflict=%q", p.MaxParallel(), p.Verify, p.EffectiveConflictResolution())
	}
	if w := p.Warnings(); len(w) != 0 {
		t.Errorf("Warnings() = %v, want none", w)
	}
	if err := SaveAs(dst, p, WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	raw, _ := os.ReadFile(dst)
	for _, key := range []string{"max_parallel_tasks", "verify", "conflict_resolution"} {
		if strings.Contains(string(raw), key) {
			t.Errorf("re-saved older file contains %q; got:\n%s", key, raw)
		}
	}
}

func TestUnknownConflictResolutionWarns(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "project.yaml")
	if err := os.WriteFile(dst, []byte("description: x\nbranch: b\nstatus: working\nconflict_resolution: yolo\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	p, err := Load(dst)
	if err != nil {
		t.Fatalf("Load must not fail on an unknown conflict_resolution: %v", err)
	}
	if got := p.EffectiveConflictResolution(); got != ConflictResolutionRerun {
		t.Errorf("EffectiveConflictResolution() = %q, want fallback %q", got, ConflictResolutionRerun)
	}
	w := p.Warnings()
	if len(w) != 1 || !strings.Contains(w[0], `"yolo"`) {
		t.Errorf("Warnings() = %v, want one naming %q", w, "yolo")
	}
	for _, ok := range []string{"", ConflictResolutionRerun, ConflictResolutionAgent} {
		if w := (&Project{ConflictResolution: ok}).Warnings(); len(w) != 0 {
			t.Errorf("conflict_resolution %q warned: %v", ok, w)
		}
	}
}
