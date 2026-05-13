package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestApplyWritesContextAndInstructions(t *testing.T) {
	ws := t.TempDir()
	ctx := Context{
		Kind:        "project",
		Workspace:   ws,
		ProjectPath: filepath.Join(ws, ".project.yaml"),
	}
	if err := Apply(ws, ctx, "do the thing\n", nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(ws, ".orch", "context.yaml"))
	if err != nil {
		t.Fatalf("read context.yaml: %v", err)
	}
	var got Context
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Kind != "project" || got.Workspace != ws {
		t.Errorf("context round-trip mismatch: %+v", got)
	}
	// Empty fields are omitted, keeping context.yaml readable.
	if strings.Contains(string(data), "task_path") {
		t.Errorf("expected omitempty to drop task_path: %s", data)
	}

	instr, err := os.ReadFile(filepath.Join(ws, ".orch", "instructions.md"))
	if err != nil {
		t.Fatalf("read instructions.md: %v", err)
	}
	if string(instr) != "do the thing\n" {
		t.Errorf("instructions = %q", instr)
	}
}

func TestApplyInstallsSkills(t *testing.T) {
	ws := t.TempDir()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("# my skill"), 0o644); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(src, "scripts"), 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "scripts", "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("seed nested: %v", err)
	}

	err := Apply(ws, Context{Kind: "task", Workspace: ws}, "x", []Skill{
		{Name: "my-skill", Source: src},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	skillDir := filepath.Join(ws, ".claude", "skills", "my-skill")
	if data, err := os.ReadFile(filepath.Join(skillDir, "SKILL.md")); err != nil || string(data) != "# my skill" {
		t.Errorf("top-level file not copied: %v / %q", err, data)
	}
	if data, err := os.ReadFile(filepath.Join(skillDir, "scripts", "run.sh")); err != nil || string(data) != "#!/bin/sh\n" {
		t.Errorf("nested file not copied: %v / %q", err, data)
	}
}

func TestApplyRejectsEmptySkill(t *testing.T) {
	ws := t.TempDir()
	err := Apply(ws, Context{Kind: "x", Workspace: ws}, "x", []Skill{{}})
	if err == nil {
		t.Error("expected error for skill with empty Name and Source")
	}
}
