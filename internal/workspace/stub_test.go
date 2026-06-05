package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoDirName(t *testing.T) {
	cases := []struct {
		repo Repo
		want string
	}{
		{Repo{Identity: "github.com/docker/mcpruntime"}, "mcpruntime"},
		{Repo{Shortname: "gateway"}, "gateway"},
		{Repo{Identity: "github.com/foo/bar", Shortname: "bar-alias"}, "bar-alias"},
	}
	for _, tc := range cases {
		if got := tc.repo.DirName(); got != tc.want {
			t.Errorf("DirName(%+v) = %q, want %q", tc.repo, got, tc.want)
		}
	}
}

func TestStubCreateAndPath(t *testing.T) {
	root := t.TempDir()
	m := NewStub(root)
	ctx := context.Background()

	got, err := m.Create(ctx, "feat/x", []Repo{
		{Identity: "github.com/docker/gateway"},
		{Shortname: "deploy-manifests"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := filepath.Join(root, "feat/x")
	if got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	if info, err := os.Stat(got); err != nil || !info.IsDir() {
		t.Errorf("workspace dir missing or not a dir: %v", err)
	}

	// Path() must return the same location without re-creating anything.
	resolved, err := m.Path("feat/x")
	if err != nil || resolved != want {
		t.Errorf("Path = %q,%v, want %q", resolved, err, want)
	}

	// Breadcrumb file records what repos the orchestrator asked for.
	bs, err := os.ReadFile(filepath.Join(got, ".orch", "stub-repos.txt"))
	if err != nil {
		t.Fatalf("stub-repos.txt: %v", err)
	}
	body := string(bs)
	if !strings.Contains(body, "github.com/docker/gateway") || !strings.Contains(body, "deploy-manifests") {
		t.Errorf("stub-repos.txt missing entries:\n%s", body)
	}
}

func TestStubRejectsEmptyBranch(t *testing.T) {
	m := NewStub(t.TempDir())
	if _, err := m.Create(context.Background(), "", nil); err == nil {
		t.Error("Create with empty branch should error")
	}
	if _, err := m.Path(""); err == nil {
		t.Error("Path with empty branch should error")
	}
}

func TestStubRemoveIsIdempotent(t *testing.T) {
	root := t.TempDir()
	m := NewStub(root)
	ctx := context.Background()

	if _, err := m.Create(ctx, "feat/y", nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.Remove(ctx, "feat/y"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "feat/y")); !os.IsNotExist(err) {
		t.Errorf("workspace dir should be gone, got err=%v", err)
	}
	// Idempotent.
	if err := m.Remove(ctx, "feat/y"); err != nil {
		t.Errorf("second Remove: %v", err)
	}
}
