package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
	log       *safeBuf
}

func newMissedFixture(t *testing.T, mutate func(*project.Project)) *missedFixture {
	t.Helper()
	f := &missedFixture{
		root:      t.TempDir(),
		statePath: filepath.Join(t.TempDir(), "cron_state.json"),
		now:       time.Now().Truncate(time.Hour).Add(10 * time.Minute),
		log:       &safeBuf{},
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
	d, err := newDaemon(t, []string{f.root}, audit.New(f.log),
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

	// The headless daemon (no runner) catches up directly: one run, charged
	// once, and the missed record is claimed.
	if !f.waitCronRuns(t, 1) {
		t.Fatal("sleep watcher never caught up the missed ticks")
	}
	if got := f.d.MissedSchedules(); len(got) != 0 {
		t.Fatalf("MissedSchedules = %+v, want none after catch-up", got)
	}
}

func (f *missedFixture) cronRuns(t *testing.T) int {
	t.Helper()
	p, err := project.Load(f.projPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return p.CronRuns
}

func (f *missedFixture) waitCronRuns(t *testing.T, want int) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.cronRuns(t) == want {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// A clock jump past a slot while running yields exactly one catch-up run, a
// stale timer fire for that slot afterwards is suppressed, and the next slot's
// on-time fire still runs.
func TestCatchUpAfterWakeThenStaleTimerFireDedupes(t *testing.T) {
	f := newMissedFixture(t, nil)
	f.d.trackCron(f.projPath)

	f.now = f.now.Add(time.Hour) // slot at +50m passed while asleep
	if !f.d.detectMissedTracked(f.now) {
		t.Fatal("detectMissedTracked found nothing")
	}
	f.d.catchUpAfterWake()
	if got := f.cronRuns(t); got != 1 {
		t.Fatalf("cron_runs after catch-up = %d, want 1", got)
	}

	if n := strings.Count(f.log.String(), "catchup_fired"); n != 1 {
		t.Fatalf("catchup_fired logged %d times, want 1", n)
	}

	f.d.onCronFired(f.projPath) // the late timer fire for the same slot
	if got := f.cronRuns(t); got != 1 {
		t.Fatalf("cron_runs after stale timer fire = %d, want 1", got)
	}
	f.d.catchUpAfterWake() // nothing left behind: no second run either
	if got := f.cronRuns(t); got != 1 {
		t.Fatalf("cron_runs after repeat catch-up = %d, want 1", got)
	}
	if n := strings.Count(f.log.String(), "catchup_fired"); n != 1 {
		t.Fatalf("catchup_fired logged %d times after stale fire, want 1", n)
	}

	f.now = f.now.Add(time.Hour) // next slot, on time
	f.d.onCronFired(f.projPath)
	if got := f.cronRuns(t); got != 2 {
		t.Fatalf("cron_runs after on-time fire = %d, want 2", got)
	}
}

// Catch-up goes through the same idle gate as a timer fire: a blocked project
// is not re-armed and is not charged a run.
func TestCatchUpAfterWakeSkipsNonIdle(t *testing.T) {
	f := newMissedFixture(t, func(p *project.Project) {
		p.Status = project.StatusBlocked
		p.BlockedReason = "needs a human"
	})
	f.d.trackCron(f.projPath)
	f.now = f.now.Add(time.Hour)
	f.d.detectMissedTracked(f.now)
	f.d.catchUpAfterWake()
	p, err := project.Load(f.projPath)
	if err != nil {
		t.Fatal(err)
	}
	if p.CronRuns != 0 || p.Status != project.StatusBlocked {
		t.Fatalf("blocked project: cron_runs=%d status=%s, want 0 / blocked", p.CronRuns, p.Status)
	}
}

// cron_max_runs is charged once for the catch-up; the stale timer fire adds
// nothing, and the budget then stops any further run.
func TestCatchUpAfterWakeHonoursMaxRuns(t *testing.T) {
	f := newMissedFixture(t, func(p *project.Project) {
		p.CronUntil = nil
		p.CronMaxRuns = 1
	})
	f.d.trackCron(f.projPath)

	f.now = f.now.Add(time.Hour)
	f.d.detectMissedTracked(f.now)
	f.d.catchUpAfterWake()
	if got := f.cronRuns(t); got != 1 {
		t.Fatalf("cron_runs after catch-up = %d, want 1", got)
	}
	f.d.onCronFired(f.projPath) // stale timer fire
	f.now = f.now.Add(time.Hour)
	f.d.onCronFired(f.projPath) // next slot, but the budget is spent
	if got := f.cronRuns(t); got != 1 {
		t.Fatalf("cron_runs past cron_max_runs = %d, want 1", got)
	}

	// With the budget spent a later sleep is not counted as missed, so nothing
	// is caught up.
	f.now = f.now.Add(5 * time.Hour)
	if f.d.detectMissedTracked(f.now) {
		t.Fatal("missed ticks recorded for a project past cron_max_runs")
	}
	if n := strings.Count(f.log.String(), "catchup_fired"); n != 1 {
		t.Fatalf("catchup_fired logged %d times, want 1", n)
	}
}

// A schedule whose cron_until has passed owes no catch-up: the sleep is not
// counted as missed and nothing is fired.
func TestCatchUpAfterWakeHonoursCronUntil(t *testing.T) {
	// cron_until is judged on the real clock, so it is already behind us.
	f := newMissedFixture(t, func(p *project.Project) {
		past := time.Now().Add(-time.Minute)
		p.CronUntil = &past
	})
	f.d.trackCron(f.projPath)

	f.now = f.now.Add(3 * time.Hour)
	if f.d.detectMissedTracked(f.now) {
		t.Fatal("missed ticks recorded after cron_until")
	}
	f.d.catchUpAfterWake()
	if got := f.cronRuns(t); got != 0 {
		t.Fatalf("cron_runs after cron_until = %d, want 0", got)
	}
	if strings.Contains(f.log.String(), "catchup_fired") {
		t.Fatal("catchup_fired logged for an expired schedule")
	}
}
