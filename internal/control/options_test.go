package control

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/slimslenderslacks/work/internal/project"
)

// A Check that refuses leaves the file untouched and its error reaches the
// caller, for every project-file action.
func TestCheckRefusalWritesNothing(t *testing.T) {
	stale := errors.New("stale etag")
	refuseAll := Check(func([]byte) error { return stale })
	for name, tc := range map[string]struct {
		yaml string
		run  func(path string) error
	}{
		"Stop":       {"description: d\nstatus: working\n", func(p string) error { _, err := Stop(p, refuseAll); return err }},
		"Start":      {"description: d\nstatus: stopped\nstopped_from: idle\n", func(p string) error { _, err := Start(p, refuseAll); return err }},
		"SummonWolf": {"description: d\nstatus: idle\n", func(p string) error { _, err := SummonWolf(p, refuseAll); return err }},
		"Review":     {"description: d\nstatus: idle\n", func(p string) error { _, _, err := Review(p, refuseAll); return err }},
		"Cleanup":    {"description: d\nstatus: idle\n", func(p string) error { _, err := Cleanup(p, refuseAll); return err }},
		"Hide":       {"description: d\nstatus: idle\n", func(p string) error { _, err := Hide(p, refuseAll); return err }},
		"Show":       {"description: d\nstatus: idle\nhide: true\n", func(p string) error { _, err := Show(p, refuseAll); return err }},
	} {
		path := writeYAML(t, t.TempDir(), "widget", tc.yaml)
		before := readFile(t, path)
		if err := tc.run(path); !errors.Is(err, stale) {
			t.Errorf("%s: err = %v, want the check's error", name, err)
		}
		if after := readFile(t, path); after != before {
			t.Errorf("%s: a refused check rewrote the file:\n%s", name, after)
		}
	}
}

// The check sees exactly the bytes the action then reads.
func TestCheckSeesTheFile(t *testing.T) {
	path := writeYAML(t, t.TempDir(), "widget", "description: d\nstatus: working\n")
	var seen []byte
	if _, err := Stop(path, Check(func(b []byte) error { seen = b; return nil })); err != nil {
		t.Fatal(err)
	}
	if string(seen) != "description: d\nstatus: working\n" {
		t.Errorf("check saw %q", seen)
	}
	if p := load(t, path); p.Status != project.StatusStopped {
		t.Errorf("status = %q, want stopped", p.Status)
	}
}

// Refusals of the project-file actions are ErrState, so the API answers them
// with 409 invalid_state.
func TestRefusalsAreErrState(t *testing.T) {
	root := t.TempDir()
	stopped := writeYAML(t, root, "a", "description: d\nstatus: stopped\n")
	working := writeYAML(t, root, "b", "description: d\nstatus: working\n")
	archived := writeYAML(t, root, "c", "description: d\nstatus: idle\narchive: true\n")
	for name, err := range map[string]error{
		"Stop":    second(Stop(stopped)),
		"Start":   second(Start(working)),
		"Review":  third(Review(working)),
		"Cleanup": second(Cleanup(archived)),
		"Show":    second(Show(working)),
	} {
		if !errors.Is(err, ErrState) {
			t.Errorf("%s: err = %v, want ErrState", name, err)
		}
	}
}

func second(_ string, err error) error        { return err }
func third(_ string, _ bool, err error) error { return err }

func TestReportNoop(t *testing.T) {
	root := t.TempDir()
	for name, tc := range map[string]struct {
		yaml string
		run  func(path string, opts ...Option) error
	}{
		"Hide":    {"description: d\nstatus: idle\nhide: true\n", func(p string, o ...Option) error { _, err := Hide(p, o...); return err }},
		"Show":    {"description: d\nstatus: idle\n", func(p string, o ...Option) error { _, err := Show(p, o...); return err }},
		"Cleanup": {"description: d\nstatus: idle\ncleanup: true\n", func(p string, o ...Option) error { _, err := Cleanup(p, o...); return err }},
		"Review":  {"description: d\nstatus: idle\nreview: true\nreview_now: true\n", func(p string, o ...Option) error { _, _, err := Review(p, o...); return err }},
	} {
		path := writeYAML(t, root, "w-"+name, tc.yaml)
		before := readFile(t, path)
		var noop bool
		if err := tc.run(path, ReportNoop(&noop)); err != nil || !noop {
			t.Errorf("%s: err = %v, noop = %v; want nil, true", name, err, noop)
		}
		if after := readFile(t, path); after != before {
			t.Errorf("%s: a no-op rewrote the file:\n%s", name, after)
		}
	}
	// A real change is not a no-op.
	path := writeYAML(t, root, "fresh", "description: d\nstatus: idle\n")
	var noop bool
	if _, err := Cleanup(path, ReportNoop(&noop)); err != nil || noop || !load(t, path).Cleanup {
		t.Errorf("Cleanup on a fresh project: err=%v noop=%v", err, noop)
	}
	// Without ReportNoop, Hide still refuses a hidden project (the TUI).
	hidden := writeYAML(t, root, "hidden", "description: d\nhide: true\n")
	if _, err := Hide(hidden); err == nil {
		t.Error("Hide on a hidden project without ReportNoop: no refusal")
	}
}

func TestSummonWolfReason(t *testing.T) {
	root := t.TempDir()
	path := writeYAML(t, root, "widget", "description: d\nstatus: working\n")
	if _, err := SummonWolf(path, WolfReason("  the API tests hang  ")); err != nil {
		t.Fatal(err)
	}
	if p := load(t, path); p.BlockedReason != "the API tests hang" || p.Status != project.StatusBlocked {
		t.Errorf("blocked_reason/status = %q/%q", p.BlockedReason, p.Status)
	}
	// An existing diagnosis still wins over the given reason.
	kept := writeYAML(t, root, "kept", "description: d\nstatus: blocked\nblocked_reason: task foo crashed\n")
	if _, err := SummonWolf(kept, WolfReason(SummonReasonGUI)); err != nil {
		t.Fatal(err)
	}
	if p := load(t, kept); p.BlockedReason != "task foo crashed" {
		t.Errorf("blocked_reason = %q, want the original kept", p.BlockedReason)
	}
}

func TestRequestCatchup(t *testing.T) {
	root := t.TempDir()
	cron := writeYAML(t, root, "nightly", "description: d\nstatus: idle\ncron: \"0 3 * * *\"\ncron_max_runs: 5\n")
	path, noop, err := RequestCatchup(cron)
	if err != nil || noop {
		t.Fatalf("RequestCatchup: %v noop=%v", err, noop)
	}
	if path != filepath.Join(filepath.Dir(cron), CatchupMarker) {
		t.Errorf("path = %q", path)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "catchup\n" {
		t.Errorf("marker = %q, %v", b, err)
	}
	// Still waiting for the daemon: a no-op, not a second marker.
	if _, noop, err := RequestCatchup(cron); err != nil || !noop {
		t.Errorf("second RequestCatchup: %v noop=%v, want a no-op", err, noop)
	}
	// No temp files left beside the project.
	entries, _ := os.ReadDir(filepath.Dir(cron))
	for _, e := range entries {
		if e.Name() != project.FileName && e.Name() != CatchupMarker {
			t.Errorf("stray file %s", e.Name())
		}
	}

	plain := writeYAML(t, root, "plain", "description: d\nstatus: idle\n")
	if _, _, err := RequestCatchup(plain); !errors.Is(err, ErrState) {
		t.Errorf("no cron: err = %v, want ErrState", err)
	}
	if _, _, err := RequestCatchup(filepath.Join(root, "gone", project.FileName)); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: err = %v, want ErrNotFound", err)
	}
	if _, _, err := RequestCatchup(filepath.Join(root, "plain", "other.yaml")); !errors.Is(err, ErrInvalid) {
		t.Errorf("not a project.yaml: err = %v, want ErrInvalid", err)
	}
}
