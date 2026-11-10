package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/project"
)

func loadProj(t *testing.T, path string) *project.Project {
	t.Helper()
	p, err := project.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type fakePower struct {
	mu      sync.Mutex
	battery bool
	err     error
}

func (f *fakePower) set(b bool) { f.mu.Lock(); f.battery = b; f.mu.Unlock() }
func (f *fakePower) OnBattery(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.battery, f.err
}

func TestPowerGatePausesAndResumes(t *testing.T) {
	f := newMissedFixture(t, nil)
	fp := &fakePower{battery: true}
	WithPowerSource(fp, 0)(f.d)
	ctx := context.Background()
	revisited := make(chan string, 1)
	f.d.powerRevisit = func(p string) { revisited <- p }

	f.d.checkPower(ctx)
	if !f.d.PowerPaused() {
		t.Fatal("want paused on battery")
	}
	if !f.d.deferForPower(f.projPath) {
		t.Fatal("dispatch should be deferred on battery")
	}

	fp.set(false)
	f.d.checkPower(ctx)
	if f.d.PowerPaused() {
		t.Fatal("want resumed on AC")
	}
	if f.d.deferForPower(f.projPath) {
		t.Fatal("dispatch should proceed on AC")
	}
	select {
	case p := <-revisited:
		if p != f.projPath {
			t.Errorf("revisited %q", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deferred project not re-dispatched on resume")
	}
	if len(f.d.powerDeferred) != 0 {
		t.Errorf("deferred set not drained: %v", f.d.powerDeferred)
	}
}

func TestPowerCheckErrorKeepsState(t *testing.T) {
	f := newMissedFixture(t, nil)
	fp := &fakePower{battery: true}
	WithPowerSource(fp, 0)(f.d)
	f.d.checkPower(context.Background())
	fp.mu.Lock()
	fp.err = context.DeadlineExceeded
	fp.battery = false
	fp.mu.Unlock()
	f.d.checkPower(context.Background())
	if !f.d.PowerPaused() {
		t.Fatal("a failed reading must not resume")
	}
}

func TestCronFiringOnBatteryIsRecordedMissed(t *testing.T) {
	f := newMissedFixture(t, nil)
	WithPowerSource(&fakePower{battery: true}, 0)(f.d)
	f.d.checkPower(context.Background())
	f.d.recordCronFire(f.projPath, f.now.Add(-time.Hour))

	f.d.onCronFired(f.projPath)
	f.d.onCronFired(f.projPath)

	got := f.d.MissedSchedules()
	if len(got) != 1 || got[0].MissedCount != 2 || !got[0].MostRecentMissed.Equal(f.now) {
		t.Fatalf("MissedSchedules = %+v, want one record, count 2", got)
	}
	// A later sleep re-evaluation must not recount the same ticks.
	f.d.evaluateMissed(f.projPath, loadProj(t, f.projPath), f.now)
	if got := f.d.MissedSchedules(); got[0].MissedCount != 2 {
		t.Errorf("recounted: %+v", got)
	}
}

func TestPowerGateNoopWithoutSource(t *testing.T) {
	f := newMissedFixture(t, nil)
	f.d.checkPower(context.Background())
	if f.d.PowerPaused() || f.d.deferForPower("x") {
		t.Fatal("no source = never paused")
	}
}
