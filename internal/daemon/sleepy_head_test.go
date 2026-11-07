package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
)

// specLauncher records every launched spec.
type specLauncher struct {
	mu    sync.Mutex
	specs []agent.Spec
}

func (l *specLauncher) Launch(_ context.Context, spec agent.Spec) (agent.Session, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.specs = append(l.specs, spec)
	return newStubSession(spec.Name), nil
}

func (l *specLauncher) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.specs)
}

// behindFixture is a missedFixture whose project is behind and whose daemon
// has a recording runner.
func newBehindFixture(t *testing.T, mutate func(*project.Project)) (*missedFixture, *specLauncher) {
	t.Helper()
	f := newMissedFixture(t, mutate)
	f.d.recordCronFire(f.projPath, f.now.Add(-5*time.Hour-10*time.Minute))
	f.d.detectMissedAtStartup()
	if len(f.d.MissedSchedules()) != 1 {
		t.Fatalf("fixture not behind: %+v", f.d.MissedSchedules())
	}
	l := &specLauncher{}
	f.d.runner = &runner.Runner{
		Launcher: l,
		Command:  func(agent.Kind, string) []string { return []string{"true"} },
	}
	return f, l
}

func TestSleepyHeadNotLaunchedWhenNothingMissed(t *testing.T) {
	f := newMissedFixture(t, nil)
	l := &specLauncher{}
	f.d.runner = &runner.Runner{Launcher: l, Command: func(agent.Kind, string) []string { return []string{"true"} }}
	if f.d.launchSleepyHead() || l.count() != 0 {
		t.Fatalf("launched with nothing missed (count %d)", l.count())
	}
}

func TestSleepyHeadLaunchesOnceWithPrompt(t *testing.T) {
	f, l := newBehindFixture(t, nil)
	if !f.d.launchSleepyHead() {
		t.Fatal("expected launch")
	}
	if l.count() != 1 || l.specs[0].Kind != agent.SleepyHeadAgent {
		t.Fatalf("specs = %+v", l.specs)
	}
	b, err := os.ReadFile(filepath.Join(f.root, ".orch", "instructions.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"stream-a", f.projPath, "0 * * * *", "missed ticks:   5", "catchup"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("instructions missing %q:\n%s", want, b)
		}
	}
	if !f.d.hasSession(sleepyHeadSessionKey) {
		t.Error("sleepy-head session not tracked")
	}
	// A second launch while one is open is a no-op.
	if f.d.launchSleepyHead() || l.count() != 1 {
		t.Errorf("second launch not deduped (count %d)", l.count())
	}
	infos := f.d.ListSessions()
	if len(infos) != 1 || infos[0].Project != "sleepy-head" || !infos[0].Interactive {
		t.Errorf("ListSessions = %+v", infos)
	}
}

func TestSleepyHeadHeadlessOnlyLogs(t *testing.T) {
	f := newMissedFixture(t, nil)
	buf := &safeBuf{}
	f.d.audit = audit.New(buf)
	f.d.recordCronFire(f.projPath, f.now.Add(-5*time.Hour-10*time.Minute))
	f.d.detectMissedAtStartup()
	if f.d.launchSleepyHead() || f.d.hasSession(sleepyHeadSessionKey) {
		t.Fatal("launched without a runner")
	}
	if !strings.Contains(buf.String(), "sleepy_head_headless") || !strings.Contains(buf.String(), "stream-a") {
		t.Errorf("audit missing headless event:\n%s", buf.String())
	}
}

func TestSleepyHeadSkippedOnBattery(t *testing.T) {
	f, l := newBehindFixture(t, nil)
	f.d.powerPaused = true
	if f.d.launchSleepyHead() || l.count() != 0 {
		t.Fatal("launched while power-paused")
	}
}

func TestCatchupFiresOnceAndClearsMissed(t *testing.T) {
	f, _ := newBehindFixture(t, func(p *project.Project) {
		p.CronUntil = nil
		p.CronMaxRuns = 3
	})
	f.d.catchUpProject(f.projPath)

	if got := f.d.MissedSchedules(); len(got) != 0 {
		t.Errorf("missed record not cleared: %+v", got)
	}
	p, err := project.Load(f.projPath)
	if err != nil {
		t.Fatal(err)
	}
	if p.CronRuns != 1 || p.Status != project.StatusReady || !p.Replan {
		t.Errorf("after catch-up: runs=%d status=%s replan=%v, want 1/ready/true", p.CronRuns, p.Status, p.Replan)
	}
	// Repeating it must not fire again: nothing is missed any more.
	f.d.catchUpProject(f.projPath)
	p, _ = project.Load(f.projPath)
	if p.CronRuns != 1 {
		t.Errorf("double fire: cron_runs = %d, want 1", p.CronRuns)
	}
}

func TestCatchupNotChargedWhenProjectBusy(t *testing.T) {
	f, _ := newBehindFixture(t, func(p *project.Project) {
		p.CronUntil = nil
		p.CronMaxRuns = 3
		p.Status = project.StatusBlocked
	})
	f.d.catchUpProject(f.projPath)
	p, _ := project.Load(f.projPath)
	if p.CronRuns != 0 {
		t.Errorf("cron_runs = %d, want 0 for a firing that started no cycle", p.CronRuns)
	}
	if len(f.d.MissedSchedules()) != 0 {
		t.Error("missed record should still clear (the schedule fired)")
	}
}

func TestCatchupSkipsProjectNotBehind(t *testing.T) {
	f := newMissedFixture(t, func(p *project.Project) { p.CronUntil = nil; p.CronMaxRuns = 3 })
	f.d.catchUpProject(f.projPath)
	p, _ := project.Load(f.projPath)
	if p.CronRuns != 0 || p.Status != project.StatusIdle {
		t.Errorf("fired a project with nothing missed: runs=%d status=%s", p.CronRuns, p.Status)
	}
}

func TestCatchupSkippedOnBattery(t *testing.T) {
	f, _ := newBehindFixture(t, nil)
	f.d.powerPaused = true
	f.d.catchUpProject(f.projPath)
	if len(f.d.MissedSchedules()) != 1 {
		t.Error("battery catch-up must leave the missed record")
	}
}

func TestRequestCatchupMarkerIsConsumedOnce(t *testing.T) {
	f, _ := newBehindFixture(t, func(p *project.Project) { p.CronUntil = nil; p.CronMaxRuns = 3 })
	if err := RequestCatchup(f.projPath); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(filepath.Dir(f.projPath), CatchupMarker)
	h := f.d.handlerFor(marker)
	if h == nil {
		t.Fatal("no handler for the catch-up marker")
	}
	h(marker) // the Create event
	h(marker) // the Write event for the same file
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("marker not consumed: %v", err)
	}
	p, _ := project.Load(f.projPath)
	if p.CronRuns != 1 {
		t.Errorf("cron_runs = %d, want exactly 1", p.CronRuns)
	}
}

func TestRequestCatchupRejectsNonProject(t *testing.T) {
	if err := RequestCatchup(filepath.Join(t.TempDir(), "nope.txt")); err == nil {
		t.Error("expected error for a non project.yaml path")
	}
	if err := RequestCatchup(filepath.Join(t.TempDir(), "project.yaml")); err == nil {
		t.Error("expected error for a missing project")
	}
}

func TestSleepyHeadChannelAnnouncement(t *testing.T) {
	h := newChannelHarness(t)
	h.d.roots = []string{filepath.Dir(filepath.Dir(h.proj))}
	h.d.recordMissedTick(h.proj, time.Now())
	if !h.d.launchSleepyHead() {
		t.Fatal("expected launch")
	}
	eventually(t, "sleepy-head message", func() bool { return len(h.fake.Sent()) == 1 })
	msg := h.fake.Sent()[0]
	if msg.Topic != wolfTopic || !strings.Contains(msg.Text, "myproj (1 missed)") {
		t.Errorf("message = %+v", msg)
	}
}
