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
		ProjectPath: filepath.Join(ws, "project.yaml"),
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

// A task's hints reach .orch/context.yaml, and an explicit `touches: []`
// is written rather than dropped (docs/parallel-task-worktrees.md H1).
func TestApplyWritesHints(t *testing.T) {
	ws := t.TempDir()
	touches := []string{"gateway/internal/api/**"}
	ctx := Context{Kind: "task", Workspace: ws, Touches: &touches, Areas: []string{"go-deps"}}
	if err := Apply(ws, ctx, "x", nil); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(ws, ".orch", "context.yaml"))
	for _, want := range []string{"touches:\n    - gateway/internal/api/**\n", "areas:\n    - go-deps\n"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("context.yaml missing %q:\n%s", want, data)
		}
	}

	empty := []string{}
	ws = t.TempDir()
	if err := Apply(ws, Context{Kind: "task", Workspace: ws, Touches: &empty}, "x", nil); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(ws, ".orch", "context.yaml"))
	if !strings.Contains(string(data), "touches: []\n") {
		t.Errorf("an explicit touches: [] must be kept:\n%s", data)
	}

	ws = t.TempDir()
	plan := Context{Kind: "planning", Workspace: ws, MaxParallelTasks: 3, Repos: []string{"gateway", "docs"}}
	if err := Apply(ws, plan, "x", nil); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(ws, ".orch", "context.yaml"))
	for _, want := range []string{"max_parallel_tasks: 3\n", "repos:\n    - gateway\n    - docs\n"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("planning context.yaml missing %q:\n%s", want, data)
		}
	}
	if strings.Contains(string(data), "touches") {
		t.Errorf("no hints means no touches key:\n%s", data)
	}
}
