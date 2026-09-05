package task

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
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

func TestCommitArtifactsRoundTrip(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "t.yaml")
	src := &Task{
		Name:        "x",
		Description: "do the thing",
		Status:      StatusCommitted,
		Summary:     "Renamed gwctl to cp and updated all callers.",
		Commits: []Commit{
			{Repo: "mcpruntime", Hash: "abc123"},
			{Repo: "sandboxes", Hash: "def456"},
		},
		CreatedFiles: []string{"/tmp/notes.md"},
	}
	if err := Save(dst, src); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(dst)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Summary != src.Summary {
		t.Errorf("Summary = %q, want %q", got.Summary, src.Summary)
	}
	if !reflect.DeepEqual(got.Commits, src.Commits) {
		t.Errorf("Commits = %+v, want %+v", got.Commits, src.Commits)
	}
	if !reflect.DeepEqual(got.CreatedFiles, src.CreatedFiles) {
		t.Errorf("CreatedFiles = %+v, want %+v", got.CreatedFiles, src.CreatedFiles)
	}
}

func TestCommitArtifactsOmittedWhenEmpty(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "t.yaml")
	if err := Save(dst, &Task{Name: "x", Status: StatusReady}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	for _, key := range []string{"summary:", "commits:", "created_files:"} {
		if strings.Contains(string(data), key) {
			t.Errorf("empty task yaml should not contain %q; got:\n%s", key, string(data))
		}
	}
}

func TestCompletedAtRoundTrip(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "t.yaml")
	when := time.Date(2026, 6, 21, 18, 49, 0, 0, time.UTC)
	if err := Save(dst, &Task{Name: "x", Status: StatusCommitted, CompletedAt: &when}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(dst)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.CompletedAt == nil || !got.CompletedAt.Equal(when) {
		t.Errorf("CompletedAt = %v, want %v", got.CompletedAt, when)
	}

	// A task that never completed must omit the field entirely.
	dst2 := filepath.Join(t.TempDir(), "t2.yaml")
	if err := Save(dst2, &Task{Name: "y", Status: StatusReady}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(dst2)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(data), "completed_at") {
		t.Errorf("nil CompletedAt should be omitted from yaml; got:\n%s", string(data))
	}
}

func TestLoadBackfillsModelDefault(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "nomodel.yaml")
	if err := writeRaw(dst, "name: x\nstatus: ready\n"); err != nil {
		t.Fatalf("writeRaw: %v", err)
	}
	tk, err := Load(dst)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if tk.Model != ModelDefault {
		t.Errorf("Model = %q, want %q (backfill when field is absent)", tk.Model, ModelDefault)
	}
}

func TestLoadPreservesExplicitModel(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "withmodel.yaml")
	if err := writeRaw(dst, "name: x\nstatus: ready\nmodel: haiku\n"); err != nil {
		t.Fatalf("writeRaw: %v", err)
	}
	tk, err := Load(dst)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if tk.Model != "haiku" {
		t.Errorf("Model = %q, want %q (explicit value must survive Load)", tk.Model, "haiku")
	}
}

func TestValidName(t *testing.T) {
	valid := []string{"a", "add-healthz-handler", "task2", "00-mystery-task", strings.Repeat("a", MaxNameLen)}
	for _, name := range valid {
		if !ValidName(name) {
			t.Errorf("ValidName(%q) = false, want true", name)
		}
	}
	invalid := []string{
		"",                                // blank
		"Add Healthz Handler",             // spaces, uppercase
		"add_healthz_handler",             // underscores
		"-leading-hyphen",                 // leading hyphen
		"trailing-hyphen-",                // trailing hyphen
		"double--hyphen",                  // doubled hyphen
		strings.Repeat("a", MaxNameLen+1), // over length
	}
	for _, name := range invalid {
		if ValidName(name) {
			t.Errorf("ValidName(%q) = true, want false", name)
		}
	}
}

func TestSlugify(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Add Healthz Handler!", "add-healthz-handler"},
		{"add_healthz_handler", "add-healthz-handler"},
		{"  leading and trailing  ", "leading-and-trailing"},
		{"", "task"},
		{"!!!", "task"},
		{strings.Repeat("a", MaxNameLen+10), strings.Repeat("a", MaxNameLen)},
	}
	for _, c := range cases {
		if got := Slugify(c.in); got != c.want {
			t.Errorf("Slugify(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Every non-empty input must slugify to something ValidName accepts.
	for _, c := range cases {
		if got := Slugify(c.in); !ValidName(got) {
			t.Errorf("Slugify(%q) = %q, which ValidName rejects", c.in, got)
		}
	}
}

// Save is the single choke point for writing a task file, so an over-length
// name must never reach disk. Save asserts the invariant (refuses with an error
// and writes nothing) rather than silently truncating, since truncating in
// isolation would break other tasks' depends_on. Repair-with-context is
// taskgraph.Load's job. An empty Name (the seed signal) and a valid name are
// both accepted.
func TestSaveRefusesOverlongName(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "long.yaml")
	src := &Task{Name: strings.Repeat("a", MaxNameLen+20), Status: StatusReady}
	if err := Save(dst, src); err == nil {
		t.Fatalf("Save accepted an over-length name")
	}
	// Nothing must have been written.
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("Save wrote a file for a refused name: stat err = %v", err)
	}

	// An empty seed name is allowed (planning fills it in later).
	seed := filepath.Join(t.TempDir(), "seed.yaml")
	if err := Save(seed, &Task{Status: StatusReady}); err != nil {
		t.Fatalf("Save rejected an empty seed name: %v", err)
	}

	// An already-valid name is written verbatim.
	dst2 := filepath.Join(t.TempDir(), "ok.yaml")
	if err := Save(dst2, &Task{Name: "add-healthz-handler", Status: StatusReady}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	ok, err := Load(dst2)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ok.Name != "add-healthz-handler" {
		t.Errorf("Save mangled valid name: got %q", ok.Name)
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
