package daemon

import (
	"context"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/slimslenderslacks/work/internal/power"
)

// Power gate: while the host is on battery the daemon stands down.
//
// Design: pause-and-resume, not exit-and-restart. Exiting would need a
// power-aware launchd KeepAlive and would tear down the watcher, the TUI and
// the channels on every unplug; pausing keeps the process, its state and its
// running sessions intact and costs one cheap `pmset` poll per interval.
//
// While paused:
//   - dispatchProject is a no-op that remembers the project (powerDeferred), so
//     no new planning/task/commit/review agent starts. Sessions already running
//     are left alone.
//   - a cron firing launches nothing and is recorded as a missed tick
//     (recordMissedTick), so the sleepy-head launcher reports it exactly like a
//     tick lost to sleep.
//
// On the next poll that sees AC, every deferred project is re-dispatched.

// defaultPowerCheckEvery is how often the power source is polled.
const defaultPowerCheckEvery = 30 * time.Second

// WithPowerSource enables the AC-power gate: while src reports battery the
// daemon pauses dispatching new work and cron firings. every <= 0 keeps the
// default poll period.
func WithPowerSource(src power.Source, every time.Duration) Option {
	return func(d *Daemon) {
		d.power = src
		if every > 0 {
			d.powerCheckEvery = every
		}
	}
}

// PowerPaused reports whether dispatching is currently paused for battery.
func (d *Daemon) PowerPaused() bool {
	d.powerMu.Lock()
	defer d.powerMu.Unlock()
	return d.powerPaused
}

// deferForPower reports whether new work must wait, remembering path so it is
// re-dispatched on resume.
func (d *Daemon) deferForPower(path string) bool {
	d.powerMu.Lock()
	defer d.powerMu.Unlock()
	if !d.powerPaused {
		return false
	}
	if d.powerDeferred == nil {
		d.powerDeferred = map[string]bool{}
	}
	d.powerDeferred[path] = true
	return true
}

// checkPower polls the source once and applies a pause/resume transition. A
// failed reading keeps the previous state.
func (d *Daemon) checkPower(ctx context.Context) {
	if d.power == nil {
		return
	}
	onBattery, err := d.power.OnBattery(ctx)
	if err != nil {
		d.audit.Log("power_check_error", "err", err.Error())
		return
	}
	d.powerMu.Lock()
	was := d.powerPaused
	d.powerPaused = onBattery
	var resume []string
	if was && !onBattery {
		for p := range d.powerDeferred {
			resume = append(resume, p)
		}
		d.powerDeferred = nil
	}
	d.powerMu.Unlock()

	switch {
	case onBattery && !was:
		d.audit.Log("power_paused", "reason", "on battery")
	case !onBattery && was:
		sort.Strings(resume)
		d.audit.Log("power_resumed", "deferred", strconv.Itoa(len(resume)))
		revisit := d.revisitProject
		if d.powerRevisit != nil {
			revisit = d.powerRevisit
		}
		for _, p := range resume {
			go revisit(p)
		}
	}
}

// powerWatchLoop polls until ctx is done.
func (d *Daemon) powerWatchLoop(ctx context.Context) {
	t := time.NewTicker(d.powerCheckEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.checkPower(ctx)
		}
	}
}

// recordMissedTick folds one cron firing skipped for battery into the
// project's missed record. The tick time becomes the counting baseline, so a
// later evaluateMissed does not count it again.
func (d *Daemon) recordMissedTick(projectPath string, at time.Time) {
	s := d.cronHistory
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entryLocked(projectPath)
	if e.Missed == nil {
		e.Missed = &MissedSchedule{
			ProjectPath: projectPath,
			WorkStream:  filepath.Base(filepath.Dir(projectPath)),
			LastFired:   e.LastFired,
		}
	}
	e.Missed.MissedCount++
	e.Missed.MostRecentMissed = at
	if err := s.saveLocked(); err != nil {
		d.audit.Log("cron_state_save_error", "path", s.path, "err", err.Error())
	}
}
