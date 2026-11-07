package daemon

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/scheduler"
)

// Sleep-aware cron.
//
// robfig/cron keeps no history: a tick that elapses while the daemon is down or
// the machine is asleep is simply gone, and after a wake the library fires at
// most one late tick. To let a launcher tell the user "you missed these", the
// daemon persists when each project's schedule last fired, and — without ever
// firing the missed runs itself — collapses the ticks that went by unobserved
// into one MissedSchedule per project. Two things feed it:
//
//   - startup: before scheduler.Start, every cron project's ticks between its
//     last fire and now are counted (detectMissedAtStartup);
//   - sleep while running: a loop notices a wall-clock gap between its own
//     ticks (laptop lid closed) and does the same count (sleepWatchLoop).
//
// A project that fires again afterwards has been re-evaluated, so its record
// is dropped; the record describes schedules still owed attention.

const (
	// cronSleepCheckEvery is how often the sleep watcher samples the wall
	// clock. A gap of more than twice this between samples means the process
	// was suspended (the loop itself never blocks for long).
	cronSleepCheckEvery = 30 * time.Second
	// maxMissedTicks bounds the tick count per evaluation so a dense schedule
	// measured over a long outage cannot spin.
	maxMissedTicks = 100000
)

// MissedSchedule records the scheduled ticks of one project's cron that passed
// without the daemon observing them (daemon down, or host asleep). Multiple
// missed ticks are collapsed into this single record; nothing is fired for them.
type MissedSchedule struct {
	ProjectPath string `json:"project_path"`
	// WorkStream is the project's human label (its directory name).
	WorkStream string `json:"work_stream"`
	// LastFired is when the schedule last fired. Zero when it never has; the
	// count then runs from when the daemon first saw the schedule.
	LastFired time.Time `json:"last_fired,omitempty"`
	// MissedCount is how many ticks were missed (a lower bound if it hit
	// maxMissedTicks).
	MissedCount int `json:"missed_count"`
	// MostRecentMissed is the latest missed tick.
	MostRecentMissed time.Time `json:"most_recent_missed"`
}

// cronEntry is the persisted per-project record.
type cronEntry struct {
	LastFired time.Time `json:"last_fired,omitempty"`
	// Observed is when the daemon first tracked the schedule: the baseline
	// before the first fire, so a brand-new schedule doesn't "miss" ticks from
	// before it existed.
	Observed time.Time       `json:"observed,omitempty"`
	Missed   *MissedSchedule `json:"missed,omitempty"`
}

type cronStateFile struct {
	Projects map[string]*cronEntry `json:"projects"`
}

// cronState is the daemon's schedule-history store. With an empty path it is
// memory-only (history lost on restart).
type cronState struct {
	path string
	mu   sync.Mutex
	data cronStateFile
}

func loadCronState(path string) (*cronState, error) {
	s := &cronState{path: path, data: cronStateFile{Projects: map[string]*cronEntry{}}}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return s, err
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		s.data = cronStateFile{Projects: map[string]*cronEntry{}}
		return s, &fs.PathError{Op: "parse", Path: path, Err: err}
	}
	if s.data.Projects == nil {
		s.data.Projects = map[string]*cronEntry{}
	}
	return s, nil
}

// saveLocked writes atomically; callers hold s.mu.
func (s *cronState) saveLocked() error {
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(&s.data, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *cronState) entryLocked(path string) *cronEntry {
	e := s.data.Projects[path]
	if e == nil {
		e = &cronEntry{}
		s.data.Projects[path] = e
	}
	return e
}

// WithCronState persists per-project cron history (last fire, missed ticks) at
// path so it survives daemon restarts. Like the snapshot state file it must lie
// outside every watched root; New rejects an overlap. Without this option the
// history is kept in memory only.
func WithCronState(path string) Option {
	return func(d *Daemon) { d.cronStatePath = path }
}

// WithSleepCheckInterval overrides how often the wall-clock sleep watcher
// samples. Non-positive keeps the default. Mainly for tests.
func WithSleepCheckInterval(every time.Duration) Option {
	return func(d *Daemon) {
		if every > 0 {
			d.sleepCheckEvery = every
		}
	}
}

// MissedSchedules returns the collapsed missed-schedule records, ordered by
// project path. It is safe to call from any goroutine, and intended for the
// sleepy-head launcher.
func (d *Daemon) MissedSchedules() []MissedSchedule {
	s := d.cronHistory
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []MissedSchedule
	for _, e := range s.data.Projects {
		if e.Missed != nil {
			out = append(out, *e.Missed)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProjectPath < out[j].ProjectPath })
	return out
}

// AckMissedSchedule drops the missed record for projectPath (the launcher has
// surfaced it). No-op when there is none.
func (d *Daemon) AckMissedSchedule(projectPath string) {
	s := d.cronHistory
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.data.Projects[projectPath]; e != nil && e.Missed != nil {
		e.Missed = nil
		_ = s.saveLocked()
	}
}

// recordCronFire stamps a firing: it is the new baseline, and any missed record
// is dropped since the project is being re-evaluated now.
func (d *Daemon) recordCronFire(projectPath string, at time.Time) {
	s := d.cronHistory
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entryLocked(projectPath)
	e.LastFired = at
	e.Missed = nil
	if err := s.saveLocked(); err != nil {
		d.audit.Log("cron_state_save_error", "path", s.path, "err", err.Error())
	}
}

// trackCron makes sure a registered schedule has a baseline, so ticks from
// before the daemon knew about it are never reported as missed.
func (d *Daemon) trackCron(projectPath string) {
	s := d.cronHistory
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entryLocked(projectPath)
	if e.Observed.IsZero() && e.LastFired.IsZero() {
		e.Observed = d.clock()
		if err := s.saveLocked(); err != nil {
			d.audit.Log("cron_state_save_error", "path", s.path, "err", err.Error())
		}
	}
}

// untrackCron forgets a schedule that has ended or been removed.
func (d *Daemon) untrackCron(projectPath string) {
	s := d.cronHistory
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data.Projects[projectPath]; ok {
		delete(s.data.Projects, projectPath)
		_ = s.saveLocked()
	}
}

// evaluateMissed counts the ticks of p's schedule in (baseline, now] and folds
// them into the project's missed record. Ticks already recorded are not counted
// twice, so it is safe to run repeatedly. Reports whether anything new was found.
func (d *Daemon) evaluateMissed(projectPath string, p *project.Project, now time.Time) bool {
	s := d.cronHistory
	if s == nil || p.Cron == "" || p.CronUnbounded() || p.CronStopReason() != "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entryLocked(projectPath)
	base := e.LastFired
	if e.Observed.After(base) {
		base = e.Observed
	}
	if base.IsZero() {
		// First sight: nothing to compare against.
		e.Observed = now
		_ = s.saveLocked()
		return false
	}
	if e.Missed != nil && e.Missed.MostRecentMissed.After(base) {
		base = e.Missed.MostRecentMissed
	}
	n, last, err := scheduler.TicksBetween(p.Cron, base, now, maxMissedTicks)
	if err != nil {
		d.audit.Log("cron_missed_error", "path", projectPath, "spec", p.Cron, "err", err.Error())
		return false
	}
	if n == 0 {
		return false
	}
	if e.Missed == nil {
		e.Missed = &MissedSchedule{
			ProjectPath: projectPath,
			WorkStream:  filepath.Base(filepath.Dir(projectPath)),
			LastFired:   e.LastFired,
		}
	}
	e.Missed.MissedCount += n
	e.Missed.MostRecentMissed = last
	if err := s.saveLocked(); err != nil {
		d.audit.Log("cron_state_save_error", "path", s.path, "err", err.Error())
	}
	d.audit.Log("cron_missed", "path", projectPath, "spec", p.Cron,
		"missed", strconv.Itoa(e.Missed.MissedCount),
		"most_recent", e.Missed.MostRecentMissed.UTC().Format(time.RFC3339))
	return true
}

// detectMissedAtStartup runs before scheduler.Start: it evaluates every cron
// project under the roots against its persisted history. Nothing is fired.
func (d *Daemon) detectMissedAtStartup() {
	if d.cronHistory == nil {
		return
	}
	now := d.clock()
	for _, root := range d.roots {
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || filepath.Base(path) != project.FileName {
				return nil
			}
			if p, lerr := project.Load(path); lerr == nil && p.Cron != "" {
				d.evaluateMissed(path, p, now)
			}
			return nil
		})
	}
}

// detectMissedTracked re-evaluates every tracked schedule after a suspected
// sleep, reloading each project file for its current spec and stop condition.
func (d *Daemon) detectMissedTracked(now time.Time) {
	s := d.cronHistory
	if s == nil {
		return
	}
	s.mu.Lock()
	paths := make([]string, 0, len(s.data.Projects))
	for path := range s.data.Projects {
		paths = append(paths, path)
	}
	s.mu.Unlock()
	for _, path := range paths {
		if p, err := project.Load(path); err == nil {
			d.evaluateMissed(path, p, now)
		}
	}
}

// clockJumped reports whether the wall clock advanced by far more than a
// sampling interval between two samples — the signature of a suspended host.
// Both times should have their monotonic reading stripped (Round(0)): the
// monotonic clock may not advance during sleep, which is the point.
func clockJumped(prev, now time.Time, every time.Duration) bool {
	return now.Sub(prev) > 2*every
}

// sleepWatchLoop samples the wall clock and, on a jump, records the ticks the
// sleep swallowed. Runs until ctx is done.
func (d *Daemon) sleepWatchLoop(ctx context.Context) {
	every := d.sleepCheckEvery
	t := time.NewTicker(every)
	defer t.Stop()
	prev := d.clock().Round(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := d.clock().Round(0)
			if clockJumped(prev, now, every) {
				d.audit.Log("clock_jump", "gap", now.Sub(prev).Round(time.Second).String())
				d.detectMissedTracked(now)
			}
			prev = now
		}
	}
}
