package agent

import (
	"os"
	"strings"
	"testing"
)

func TestAugmentSearchPathAppendsMissingDirs(t *testing.T) {
	orig := os.Getenv("PATH")
	t.Cleanup(func() { _ = os.Setenv("PATH", orig) })

	dir1 := t.TempDir()
	dir2 := t.TempDir()
	_ = os.Setenv("PATH", dir1)

	got := AugmentSearchPath([]string{dir2, dir1 /* dup */})
	if !strings.Contains(got, dir1) || !strings.Contains(got, dir2) {
		t.Errorf("PATH should contain both dirs; got %q", got)
	}
	// dir1 must appear exactly once (no dup).
	if strings.Count(got, dir1) != 1 {
		t.Errorf("dir1 duplicated in PATH: %q", got)
	}
	// dir2 must appear after dir1 (we append, not prepend).
	if strings.Index(got, dir2) < strings.Index(got, dir1) {
		t.Errorf("expected dir2 appended after dir1; got %q", got)
	}
}

func TestAugmentSearchPathSkipsNonExistent(t *testing.T) {
	orig := os.Getenv("PATH")
	t.Cleanup(func() { _ = os.Setenv("PATH", orig) })

	_ = os.Setenv("PATH", "/bin")
	got := AugmentSearchPath([]string{"/totally/not/a/real/dir/12345"})
	if strings.Contains(got, "/totally/not/a/real/dir/12345") {
		t.Errorf("PATH should not contain nonexistent dir; got %q", got)
	}
}

func TestResolveTmuxFromCommonPath(t *testing.T) {
	// Best-effort smoke test: if tmux is installed anywhere ResolveTmux
	// knows about, it should find it. CI without tmux skips.
	path, err := ResolveTmux()
	if err != nil {
		t.Skip("tmux not installed in any known location")
	}
	if path == "" {
		t.Error("ResolveTmux returned empty path with no error")
	}
}
