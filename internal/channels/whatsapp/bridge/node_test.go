package bridge

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestParseNodeVersion(t *testing.T) {
	for in, want := range map[string][3]int{"v22.22.1\n": {22, 22, 1}, "18.0.0": {18, 0, 0}, "v20.5.0-nightly": {20, 5, 0}} {
		ma, mi, pa, err := ParseNodeVersion(in)
		if err != nil || [3]int{ma, mi, pa} != want {
			t.Errorf("ParseNodeVersion(%q) = %d.%d.%d, %v; want %v", in, ma, mi, pa, err, want)
		}
	}
	if _, _, _, err := ParseNodeVersion("not node"); err == nil {
		t.Error("want error for garbage")
	}
}

// fakeNode writes an executable shell script that prints version for --version.
func fakeNode(t *testing.T, dir, version string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake node")
	}
	p := filepath.Join(dir, "node")
	script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo " + version + "; exit 0; fi\nexit 0\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFindNode(t *testing.T) {
	dir := t.TempDir()
	node := fakeNode(t, dir, "v20.11.0")
	got, err := FindNode(node)
	if err != nil || got != node {
		t.Fatalf("FindNode = %q, %v", got, err)
	}

	old := t.TempDir()
	_, err = FindNode(fakeNode(t, old, "v16.20.2"))
	if !errors.Is(err, ErrNodeUnavailable) {
		t.Fatalf("old node: err = %v, want ErrNodeUnavailable", err)
	}

	_, err = FindNode(filepath.Join(dir, "does-not-exist"))
	if !errors.Is(err, ErrNodeUnavailable) {
		t.Fatalf("missing node: err = %v, want ErrNodeUnavailable", err)
	}
}

func TestFindNodeOnPath(t *testing.T) {
	dir := t.TempDir()
	node := fakeNode(t, dir, "v18.0.0")
	t.Setenv("PATH", dir)
	got, err := FindNode("")
	if err != nil || got != node {
		t.Fatalf("FindNode(\"\") = %q, %v", got, err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := FindNode(""); !errors.Is(err, ErrNodeUnavailable) {
		t.Fatalf("empty PATH: err = %v", err)
	}
}
