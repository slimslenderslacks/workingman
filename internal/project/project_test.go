package project

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadExample(t *testing.T) {
	p, err := Load("../../examples/.project.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Branch != "feat/healthz-probe" {
		t.Errorf("Branch = %q, want feat/healthz-probe", p.Branch)
	}
	if p.Status != StatusReady {
		t.Errorf("Status = %q, want ready", p.Status)
	}
	if p.UpdatedBy != WriterAgent {
		t.Errorf("UpdatedBy = %q, want agent", p.UpdatedBy)
	}
	if len(p.Repos) != 2 || p.Repos[0].Name != "gateway" {
		t.Errorf("Repos = %+v, want [gateway, deploy-manifests]", p.Repos)
	}
	if p.Cron != "*/15 * * * *" {
		t.Errorf("Cron = %q", p.Cron)
	}
}

func TestSaveForcesDaemonWriter(t *testing.T) {
	src, err := Load("../../examples/.project.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if src.UpdatedBy != WriterAgent {
		t.Fatalf("precondition: example must be writer=agent, got %q", src.UpdatedBy)
	}
	dst := filepath.Join(t.TempDir(), ".project.yaml")
	if err := Save(dst, src); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded, err := Load(dst)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.UpdatedBy != WriterDaemon {
		t.Errorf("UpdatedBy after Save = %q, want daemon", reloaded.UpdatedBy)
	}
	// Save must not mutate the source struct.
	if src.UpdatedBy != WriterAgent {
		t.Errorf("Save mutated source UpdatedBy: %q", src.UpdatedBy)
	}
	reloaded.UpdatedBy = src.UpdatedBy
	if !reflect.DeepEqual(src, reloaded) {
		t.Errorf("round-trip mismatch:\n src=%+v\n got=%+v", src, reloaded)
	}
}

func TestSaveAsAgent(t *testing.T) {
	src, _ := Load("../../examples/.project.yaml")
	dst := filepath.Join(t.TempDir(), ".project.yaml")
	if err := SaveAs(dst, src, WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	reloaded, _ := Load(dst)
	if reloaded.UpdatedBy != WriterAgent {
		t.Errorf("UpdatedBy = %q, want agent", reloaded.UpdatedBy)
	}
}

func TestEmptyFile(t *testing.T) {
	dst := filepath.Join(t.TempDir(), ".project.yaml")
	if err := Save(dst, &Project{}); err != nil {
		// Save of an empty Project writes a non-empty YAML; create the
		// true-empty case by truncating.
		t.Fatalf("Save empty: %v", err)
	}
	// Now create a truly empty file.
	empty := filepath.Join(t.TempDir(), ".project.yaml")
	if err := writeEmpty(empty); err != nil {
		t.Fatalf("writeEmpty: %v", err)
	}
	p, err := Load(empty)
	if err != nil {
		t.Fatalf("Load empty: %v", err)
	}
	if !p.Empty() {
		t.Errorf("Empty() = false for zero-byte file: %+v", p)
	}
}
