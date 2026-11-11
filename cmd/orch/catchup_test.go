package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/slimslenderslacks/work/internal/daemon"
)

func TestRunCatchupWritesMarker(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "project.yaml")
	if err := os.WriteFile(proj, []byte("description: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := runCatchup([]string{proj}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(dir, daemon.CatchupMarker)); err != nil {
		t.Errorf("marker not written: %v", err)
	}
}

func TestRunCatchupUsageAndErrors(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runCatchup(nil, &out, &errb); code != 2 {
		t.Errorf("no args: exit %d, want 2", code)
	}
	if code := runCatchup([]string{filepath.Join(t.TempDir(), "project.yaml")}, &out, &errb); code != 1 {
		t.Errorf("missing project: exit %d, want 1", code)
	}
}
