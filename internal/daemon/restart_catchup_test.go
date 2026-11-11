package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
)

// TestDaemonDownThenRestartCatchUp walks the whole sleep-aware flow: a daemon
// fires its schedule, goes down for several cron periods, and a new daemon
// started on the same state file launches sleepy-head once with a correct
// report, after which catch-up fires each approved project exactly once.
func TestDaemonDownThenRestartCatchUp(t *testing.T) {
	f := newMissedFixture(t, func(p *project.Project) { p.CronUntil = nil; p.CronMaxRuns = 10 })

	// A second hourly project beside stream-a, with its own history.
	projB := filepath.Join(f.root, "stream-b", "project.yaml")
	if err := os.MkdirAll(filepath.Dir(projB), 0o755); err != nil {
		t.Fatal(err)
	}
	pb, err := project.Load(f.projPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := project.SaveAs(projB, pb, project.WriterAgent); err != nil {
		t.Fatal(err)
	}

	// Both schedules last fired 5h10m ago, then the daemon went down.
	lastFire := f.now.Add(-5*time.Hour - 10*time.Minute)
	f.d.recordCronFire(f.projPath, lastFire)
	f.d.recordCronFire(projB, lastFire)

	// Restart: a fresh daemon over the same persisted state.
	d := f.newDaemon(t)
	l := &specLauncher{}
	d.runner = &runner.Runner{Launcher: l, Command: func(agent.Kind, string) []string { return []string{"true"} }}
	d.detectMissedAtStartup()

	// The hourly ticks in (last fire, now] are collapsed into one record each.
	missed := d.MissedSchedules()
	if len(missed) != 2 {
		t.Fatalf("missed = %+v, want 2 records", missed)
	}
	for _, m := range missed {
		if m.MissedCount != 5 {
			t.Errorf("%s: MissedCount = %d, want 5", m.WorkStream, m.MissedCount)
		}
		if want := f.now.Truncate(time.Hour); !m.MostRecentMissed.Equal(want) {
			t.Errorf("%s: MostRecentMissed = %v, want %v", m.WorkStream, m.MostRecentMissed, want)
		}
	}
	// Nothing was fired by detection alone.
	for _, path := range []string{f.projPath, projB} {
		if p, _ := project.Load(path); p.CronRuns != 0 || p.Status != project.StatusIdle {
			t.Errorf("%s fired during detection: runs=%d status=%s", path, p.CronRuns, p.Status)
		}
	}

	// Exactly one sleepy-head, however many projects are behind, and a repeat
	// launch while it is open is a no-op.
	if !d.launchSleepyHead() {
		t.Fatal("sleepy-head not launched")
	}
	if d.launchSleepyHead() || l.count() != 1 || l.specs[0].Kind != agent.SleepyHeadAgent {
		t.Fatalf("launches = %d (%+v), want exactly one sleepy-head", l.count(), l.specs)
	}
	b, err := os.ReadFile(filepath.Join(f.root, ".orch", "instructions.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"stream-a", "stream-b", f.projPath, projB, "missed ticks:   5"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("report missing %q:\n%s", want, b)
		}
	}

	// The user approves both; each is caught up once, and repeated requests
	// (the marker's Create and Write events, or a retry) do nothing more.
	for _, path := range []string{f.projPath, projB} {
		d.catchUpProject(path)
		d.catchUpProject(path)
	}
	for _, path := range []string{f.projPath, projB} {
		p, err := project.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if p.CronRuns != 1 || p.Status != project.StatusReady || !p.Replan {
			t.Errorf("%s: runs=%d status=%s replan=%v, want 1/ready/true", path, p.CronRuns, p.Status, p.Replan)
		}
	}
	if got := d.MissedSchedules(); len(got) != 0 {
		t.Errorf("missed records remain after catch-up: %+v", got)
	}

	// Another restart finds nothing behind: no second sleepy-head.
	d2 := f.newDaemon(t)
	d2.runner = d.runner
	d2.detectMissedAtStartup()
	if len(d2.MissedSchedules()) != 0 || d2.launchSleepyHead() {
		t.Error("second restart still reports missed schedules")
	}
}
