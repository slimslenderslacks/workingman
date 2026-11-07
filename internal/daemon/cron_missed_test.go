package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/scheduler"
)

// missedFixture is a daemon with a controllable clock and a persisted cron
// state file, plus one hourly cron project on disk.
type missedFixture struct {
	d         *Daemon
	root      string
	statePath string
	projPath  string
	now       time.Time
}

func newMissedFixture(t *testing.T, mutate func(*project.Project)) *missedFixture {
	t.Helper()
	f := &missedFixture{
		root:      t.TempDir(),
		statePath: filepath.Join(t.TempDir(), "cron_state.json"),
		now:       time.Now().Truncate(time.Hour).Add(10 * time.Minute),
	}
	f.projPath = filepath.Join(f.root, "stream-a", "project.yaml")
	until := f.now.Add(240 * time.Hour)
	p := &project.Project{
		Description: "d", Branch: "b", Status: project.StatusIdle,
		Cron: "0 * * * *", CronUntil: &until,
		Repos: []project.Repo{{Org: "docker", Name: "gateway"}},
	}
	if mutate != nil {
		mutate(p)
	}
	if err := os.MkdirAll(filepath.Dir(f.projPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := project.SaveAs(f.projPath, p, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	f.d = f.newDaemon(t)
	return f
}

func (f *missedFixture) newDaemon(t *testing.T) *Daemon {
	t.Helper()
	d, err := New([]string{f.root}, audit.New(&safeBuf{}),
		WithScheduler(scheduler.New()), WithCronState(f.statePath))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { d.watcher.Close() })
	d.clock = func() time.Time { return f.now }
	return d
}

func TestMissedAtStartupCollapsesTicks(t *testing.T) {
	f := newMissedFixture(t, nil)
	lastFire := f.now.Add(-5*time.Hour - 10*time.Minute) // on the hour
	f.d.recordCronFire(f.projPath, lastFire)

	// Fresh daemon on the same state file = restart.
	d2 := f.newDaemon(t)
	d2.detectMissedAtStartup()

	got := d2.MissedSchedules()
	if len(got) != 1 {
		t.Fatalf("MissedSchedules = %+v, want one record", got)
	}
	m := got[0]
	wantRecent := f.now.Truncate(time.Hour)
	if m.MissedCount != 5 || !m.MostRecentMissed.Equal(wantRecent) || !m.LastFired.Equal(lastFire) {
		t.Errorf("record = %+v, want count 5, recent %v, last fired %v", m, wantRecent, lastFire)
	}
	if m.ProjectPath != f.projPath || m.WorkStream != "stream-a" {
		t.Errorf("identity = %q / %q", m.ProjectPath, m.WorkStream)
	}
}

func TestMissedAtStartupIsIdempotent(t *testing.T) {
	f := newMissedFixture(t, nil)
	f.d.recordCronFire(f.projPath, f.now.Add(-3*time.Hour-10*time.Minute))
	f.d.detectMissedAtStartup()
	f.d.detectMissedAtStartup()
	if got := f.d.MissedSchedules(); len(got) != 1 || got[0].MissedCount != 3 {
		t.Fatalf("MissedSchedules = %+v, want one record with count 3", got)
	}
}

func TestMissedNoBaselineNothingMissed(t *testing.T) {
	f := newMissedFixture(t, nil)
	f.d.detectMissedAtStartup() // first sight only seeds the baseline
	if got := f.d.MissedSchedules(); len(got) != 0 {
		t.Fatalf("MissedSchedules = %+v, want none", got)
	}
	f.now = f.now.Add(2*time.Hour + time.Minute)
	f.d.detectMissedAtStartup()
	if got := f.d.MissedSchedules(); len(got) != 1 || got[0].MissedCount != 2 {
		t.Fatalf("MissedSchedules = %+v, want count 2 since baseline", got)
	}
}

func TestMissedRespectsStopCondition(t *testing.T) {
	cases := map[string]func(*project.Project){
		"cron_until passed": func(p *project.Project) {
			past := time.Now().Add(-time.Hour)
			p.CronUntil = &past
		},
		"cron_max_runs reached": func(p *project.Project) {
			p.CronUntil = nil
			p.CronMaxRuns, p.CronRuns = 3, 3
		},
		"unbounded": func(p *project.Project) { p.CronUntil = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newMissedFixture(t, mutate)
			f.d.recordCronFire(f.projPath, f.now.Add(-6*time.Hour))
			f.d.detectMissedAtStartup()
			if got := f.d.MissedSchedules(); len(got) != 0 {
				t.Fatalf("MissedSchedules = %+v, want none", got)
			}
		})
	}
}

func TestMissedSkipsProjectsWithoutCron(t *testing.T) {
	f := newMissedFixture(t, func(p *project.Project) { p.Cron, p.CronUntil = "", nil })
	f.d.recordCronFire(f.projPath, f.now.Add(-6*time.Hour))
	f.d.detectMissedAtStartup()
	if got := f.d.MissedSchedules(); len(got) != 0 {
		t.Fatalf("MissedSchedules = %+v, want none", got)
	}
}

func TestFireClearsMissedAndAckDrops(t *testing.T) {
	f := newMissedFixture(t, nil)
	f.d.recordCronFire(f.projPath, f.now.Add(-4*time.Hour))
	f.d.detectMissedAtStartup()
	if len(f.d.MissedSchedules()) != 1 {
		t.Fatal("expected a missed record")
	}
	f.d.recordCronFire(f.projPath, f.now)
	if got := f.d.MissedSchedules(); len(got) != 0 {
		t.Fatalf("after a fire: %+v, want none", got)
	}

	f.now = f.now.Add(3 * time.Hour)
	f.d.detectMissedAtStartup()
	if len(f.d.MissedSchedules()) != 1 {
		t.Fatal("expected a new missed record")
	}
	f.d.AckMissedSchedule(f.projPath)
	if got := f.d.MissedSchedules(); len(got) != 0 {
		t.Fatalf("after ack: %+v, want none", got)
	}
}

func TestMissedStatePersistsAcrossRestart(t *testing.T) {
	f := newMissedFixture(t, nil)
	f.d.recordCronFire(f.projPath, f.now.Add(-2*time.Hour-10*time.Minute))
	f.d.detectMissedAtStartup()

	d2 := f.newDaemon(t) // loads the state file, no detection run
	got := d2.MissedSchedules()
	if len(got) != 1 || got[0].MissedCount != 2 {
		t.Fatalf("reloaded MissedSchedules = %+v, want count 2", got)
	}
}

func TestClockJumped(t *testing.T) {
	base := time.Now().Round(0)
	every := 30 * time.Second
	if clockJumped(base, base.Add(31*time.Second), every) {
		t.Error("a normal sample interval must not count as a jump")
	}
	if !clockJumped(base, base.Add(3*time.Hour), every) {
		t.Error("a 3h gap must count as a jump")
	}
}

func TestSleepGapRecordsMissedTicks(t *testing.T) {
	f := newMissedFixture(t, nil)
	f.d.trackCron(f.projPath)                    // baseline: f.now
	f.now = f.now.Add(4*time.Hour + time.Minute) // laptop slept through 4 ticks
	f.d.detectMissedTracked(f.now)

	got := f.d.MissedSchedules()
	if len(got) != 1 || got[0].MissedCount != 4 {
		t.Fatalf("MissedSchedules = %+v, want one record with count 4", got)
	}
}

func TestSleepWatchLoopDetectsJump(t *testing.T) {
	f := newMissedFixture(t, nil)
	f.d.sleepCheckEvery = 20 * time.Millisecond
	f.d.trackCron(f.projPath)

	// The clock reads normally for the loop's first sample, then leaps ahead.
	start := f.now
	var calls int
	f.d.clock = func() time.Time {
		calls++
		if calls <= 2 {
			return start
		}
		return start.Add(5*time.Hour + time.Minute)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.d.sleepWatchLoop(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := f.d.MissedSchedules(); len(got) == 1 {
			if got[0].MissedCount != 5 {
				t.Fatalf("count = %d, want 5", got[0].MissedCount)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("sleep watcher never recorded the missed ticks")
}
