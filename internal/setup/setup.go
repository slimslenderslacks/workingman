// Package setup prepares a workspace for an agent: writes the structured
// context file the agent reads at startup, writes the rendered instructions,
// and installs any task-specific skills into the workspace's .claude/skills/
// directory.
//
// Every file setup produces lives under one of two top-level directories
// inside the workspace:
//
//	.orch/                        # orchestrator-owned state
//	  context.yaml                # structured data (paths, branch, kind, ...)
//	  instructions.md             # rendered prompt text
//	.claude/skills/<name>/        # one directory per installed skill
package setup

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Context is the structured side of the agent handoff. Fields are tagged so
// the YAML on disk reads naturally; omitempty keeps role-irrelevant fields
// from cluttering the file.
type Context struct {
	Kind        string   `yaml:"kind"`
	Workspace   string   `yaml:"workspace"`
	Branch      string   `yaml:"branch,omitempty"`
	ProjectPath string   `yaml:"project_path,omitempty"`
	TasksDir    string   `yaml:"tasks_dir,omitempty"`
	TaskPath    string   `yaml:"task_path,omitempty"`
	TaskName    string   `yaml:"task_name,omitempty"`
	FailedTasks []string `yaml:"failed_tasks,omitempty"`
}

// Skill is a directory of files copied verbatim into the workspace's
// .claude/skills/<Name>/ tree. We do not interpret the contents — that's
// claude's job once it boots.
type Skill struct {
	Name   string // destination subdirectory name
	Source string // absolute path to source dir
}

// Apply writes every artifact into workspace. Workspace must already exist.
// Apply is not transactional: a partial failure may leave some files in place.
// The orchestrator treats workspaces as disposable on error.
func Apply(workspace string, ctx Context, instructions string, skills []Skill) error {
	orchDir := filepath.Join(workspace, ".orch")
	if err := os.MkdirAll(orchDir, 0o755); err != nil {
		return fmt.Errorf("setup: mkdir .orch: %w", err)
	}
	ctxBytes, err := yaml.Marshal(&ctx)
	if err != nil {
		return fmt.Errorf("setup: marshal context: %w", err)
	}
	if err := os.WriteFile(filepath.Join(orchDir, "context.yaml"), ctxBytes, 0o644); err != nil {
		return fmt.Errorf("setup: write context.yaml: %w", err)
	}
	if err := os.WriteFile(filepath.Join(orchDir, "instructions.md"), []byte(instructions), 0o644); err != nil {
		return fmt.Errorf("setup: write instructions.md: %w", err)
	}
	for _, sk := range skills {
		if err := installSkill(workspace, sk); err != nil {
			return err
		}
	}
	return nil
}

func installSkill(workspace string, sk Skill) error {
	if sk.Name == "" || sk.Source == "" {
		return fmt.Errorf("setup: skill must have Name and Source (got %+v)", sk)
	}
	dst := filepath.Join(workspace, ".claude", "skills", sk.Name)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("setup: mkdir skill %s: %w", sk.Name, err)
	}
	return copyTree(sk.Source, dst)
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
