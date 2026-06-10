package project

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
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

func TestCreatedAtRoundTripAndOmitEmpty(t *testing.T) {
	// Zero/missing CreatedAt must not produce a `created_at:` line so the
	// daemon's "stamp the first time we see it set" gate stays meaningful.
	noStamp := filepath.Join(t.TempDir(), "a.yaml")
	if err := SaveAs(noStamp, &Project{
		Description: "x", Branch: "feat/y", Status: StatusReady,
	}, WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	raw, _ := os.ReadFile(noStamp)
	if strings.Contains(string(raw), "created_at") {
		t.Errorf("nil CreatedAt should not emit created_at line; got:\n%s", string(raw))
	}

	// Setting it round-trips: stored, loaded, equal to the second.
	withStamp := filepath.Join(t.TempDir(), "b.yaml")
	now := time.Date(2026, 6, 10, 14, 0, 0, 0, time.UTC)
	if err := SaveAs(withStamp, &Project{
		Description: "x", Branch: "feat/y", Status: StatusReady, CreatedAt: &now,
	}, WriterDaemon); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	reloaded, err := Load(withStamp)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reloaded.CreatedAt == nil || !reloaded.CreatedAt.Equal(now) {
		t.Errorf("CreatedAt = %v, want %v", reloaded.CreatedAt, now)
	}
}

func TestBaseBranchRoundTrip(t *testing.T) {
	dst := filepath.Join(t.TempDir(), ".project.yaml")
	src := &Project{
		Description: "x",
		Branch:      "feat/y",
		Status:      StatusReady,
		Repos: []Repo{
			{Org: "docker", Name: "mcpruntime", BaseBranch: "mcp-kit-hooks"},
			{Org: "docker", Name: "sandboxes"}, // no BaseBranch → omitted
		},
	}
	if err := SaveAs(dst, src, WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	reloaded, err := Load(dst)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(reloaded.Repos) != 2 {
		t.Fatalf("want 2 repos, got %d", len(reloaded.Repos))
	}
	if reloaded.Repos[0].BaseBranch != "mcp-kit-hooks" {
		t.Errorf("repo[0].BaseBranch = %q, want mcp-kit-hooks", reloaded.Repos[0].BaseBranch)
	}
	if reloaded.Repos[1].BaseBranch != "" {
		t.Errorf("repo[1].BaseBranch = %q, want empty", reloaded.Repos[1].BaseBranch)
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
