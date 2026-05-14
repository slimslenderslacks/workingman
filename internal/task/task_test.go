package task

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadExampleNoDeps(t *testing.T) {
	tk, err := Load("../../examples/tasks/01-add-healthz-handler.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if tk.Name != "add-healthz-handler" {
		t.Errorf("Name = %q", tk.Name)
	}
	if tk.Status != StatusReady {
		t.Errorf("Status = %q, want ready", tk.Status)
	}
	if len(tk.DependsOn) != 0 {
		t.Errorf("DependsOn = %v, want empty", tk.DependsOn)
	}
	if tk.Attempts != 0 {
		t.Errorf("Attempts = %d, want 0", tk.Attempts)
	}
}

func TestLoadExampleWithDeps(t *testing.T) {
	tk, err := Load("../../examples/tasks/02-add-readiness-probe.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(tk.DependsOn) != 1 || tk.DependsOn[0] != "add-healthz-handler" {
		t.Errorf("DependsOn = %v, want [add-healthz-handler]", tk.DependsOn)
	}
}

func TestRoundTrip(t *testing.T) {
	src, err := Load("../../examples/tasks/02-add-readiness-probe.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "t.yaml")
	if err := Save(dst, src); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded, err := Load(dst)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	// Path is set from the load path and intentionally not persisted, so
	// compare the rest of the struct after normalising it.
	srcCopy := *src
	srcCopy.Path = reloaded.Path
	if !reflect.DeepEqual(&srcCopy, reloaded) {
		t.Errorf("round-trip mismatch:\n src=%+v\n got=%+v", srcCopy, reloaded)
	}
	if reloaded.Path != dst {
		t.Errorf("Path = %q, want %q", reloaded.Path, dst)
	}
}

func TestInvalidStatusRejected(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "bad.yaml")
	if err := writeRaw(dst, "name: x\nstatus: notathing\n"); err != nil {
		t.Fatalf("writeRaw: %v", err)
	}
	if _, err := Load(dst); err == nil {
		t.Errorf("Load accepted invalid status")
	}
}
