package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/prompts"
	"github.com/slimslenderslacks/work/internal/runner"
)

// Sleepy-head launcher.
//
// When the daemon comes up and Daemon.MissedSchedules is non-empty, it opens an
// interactive sleepy-head agent in a terminal tab (the wolf's host launch path:
// agent.Kind.Interactive, no ACP) that reports the projects that are behind and,
// with the user's say-so, catches them up. A catch-up is ONE firing of the
// project's cron callback however many ticks were missed.
//
// The agent triggers a catch-up by dropping a marker file next to the project's
// project.yaml (`orch catchup <project.yaml>` does exactly that). The daemon
// already watches that directory, so it consumes the marker and fires the
// callback in-process — the CLI never needs to reach the daemon.

const (
	// CatchupMarker is the file, created beside a project.yaml, that asks the
	// daemon to fire that project's cron callback once immediately.
	CatchupMarker = "catchup-request"

	// sleepyHeadSessionKey is the session-map key of the (single) sleepy-head
	// agent. Like the workingman agent it belongs to no project.
	sleepyHeadSessionKey = "sleepy-head"
)

// RequestCatchup asks the running daemon to fire the cron callback of the
// project at projectPath (its project.yaml) once, immediately. It only writes
// the marker file; the daemon does the rest (see catchUpProject).
func RequestCatchup(projectPath string) error {
	if filepath.Base(projectPath) != project.FileName {
		return fmt.Errorf("%q is not a %s", projectPath, project.FileName)
	}
	if _, err := os.Stat(projectPath); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(filepath.Dir(projectPath), CatchupMarker), []byte("catchup\n"), 0o644)
}

// catchupMu serialises catch-ups so two requests for one project cannot both
// pass the "is it still behind" check.
var catchupMu sync.Mutex

// handleCatchup is the watcher handler for a catch-up marker. Consuming the
// marker (removing it) is the claim: a second event for the same file finds it
// gone and does nothing.
func (d *Daemon) handleCatchup(markerPath string) {
	if err := os.Remove(markerPath); err != nil {
		return // already consumed (the Create and Write events both arrive)
	}
	d.catchUpProject(filepath.Join(filepath.Dir(markerPath), project.FileName))
}

// catchUpProject fires projectPath's cron callback exactly once, but only while
// the project is still recorded as behind: that is what stops a repeated or
// stale request from firing twice, and from firing a project that has nothing
// missed. Going through onCronFired means the firing is stamped (clearing the
// missed record) and cron_max_runs is charged only if a cycle really started.
func (d *Daemon) catchUpProject(projectPath string) {
	p, err := project.Load(projectPath)
	if err != nil || p.Cron == "" {
		d.audit.Log("catchup_skipped", "path", projectPath, "reason", "not a cron project")
		return
	}
	if d.PowerPaused() {
		// onCronFired would refuse and count another miss; leave the record.
		d.audit.Log("catchup_skipped", "path", projectPath, "reason", "on battery")
		return
	}
	catchupMu.Lock()
	behind := false
	for _, m := range d.MissedSchedules() {
		if m.ProjectPath == projectPath {
			behind = true
		}
	}
	if behind {
		d.recordCronFire(projectPath, d.clock()) // claim it: clears the missed record
	}
	catchupMu.Unlock()
	if !behind {
		d.audit.Log("catchup_skipped", "path", projectPath, "reason", "nothing missed")
		return
	}
	d.audit.Log("catchup_fired", "path", projectPath, "spec", p.Cron)
	d.onCronFired(projectPath)
}

// catchupInstruction is the text the sleepy-head prompt embeds for step 3.
func catchupInstruction() string {
	bin := "orch"
	if exe, err := os.Executable(); err == nil {
		bin = exe
	}
	return fmt.Sprintf("run `%s catchup <project file>` once for the approved project (the project file path is "+
		"listed above). It fires that project's schedule exactly once; equivalently, create an empty file named "+
		"`%s` in the same directory as the project file.", bin, CatchupMarker)
}

// behindProjects renders the missed records for the prompt, loading each
// project for its current cron spec and status.
func behindProjects(missed []MissedSchedule) []prompts.BehindProject {
	out := make([]prompts.BehindProject, 0, len(missed))
	for _, m := range missed {
		b := prompts.BehindProject{
			Name:        m.WorkStream,
			ProjectPath: m.ProjectPath,
			MissedCount: m.MissedCount,
			MostRecent:  m.MostRecentMissed.Local().Format(time.RFC1123),
			Status:      "unknown",
		}
		if !m.LastFired.IsZero() {
			b.LastFired = m.LastFired.Local().Format(time.RFC1123)
		}
		if p, err := project.Load(m.ProjectPath); err == nil {
			b.Cron = p.Cron
			b.Status = string(p.Status)
		}
		out = append(out, b)
	}
	return out
}

// launchSleepyHead opens the sleepy-head agent if any schedule is behind. It
// reports whether one was launched. Never launches with nothing missed or
// while one is already running; a daemon with no runner (headless) just logs
// what is behind.
func (d *Daemon) launchSleepyHead() bool {
	missed := d.MissedSchedules()
	if len(missed) == 0 {
		return false
	}
	names := make([]string, len(missed))
	for i, m := range missed {
		names[i] = m.WorkStream
	}
	if d.runner == nil {
		d.audit.Log("sleepy_head_headless", "projects", strconv.Itoa(len(missed)), "behind", strings.Join(names, ","))
		return false
	}
	if d.PowerPaused() {
		d.audit.Log("sleepy_head_skipped", "reason", "on battery", "behind", strings.Join(names, ","))
		return false
	}
	if d.hasSession(sleepyHeadSessionKey) {
		d.audit.Log("session_skip_duplicate", "kind", agent.SleepyHeadAgent.String())
		return false
	}
	plan := runner.Plan{
		Kind:              agent.SleepyHeadAgent,
		WorkingDir:        d.roots[0],
		SessionName:       sleepyHeadSessionKey,
		BehindProjects:    behindProjects(missed),
		ReplanInstruction: catchupInstruction(),
	}
	started, _ := d.startSessionTracked(sleepyHeadSessionKey, plan, func(error) {
		d.audit.Log("sleepy_head_ended")
	})
	if !started {
		return false
	}
	d.audit.Log("sleepy_head_dispatch", "projects", strconv.Itoa(len(missed)), "behind", strings.Join(names, ","))
	d.announceSleepyHead(missed)
	return true
}

// announceSleepyHead tells the messaging channels (if enabled) that sleepy-head
// is waiting in a terminal tab. Asynchronous, like the wolf's messages.
func (d *Daemon) announceSleepyHead(missed []MissedSchedule) {
	c := d.channels
	if c == nil {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "😴 sleepy-head is running: %d project(s) missed their schedule", len(missed))
	for _, m := range missed {
		fmt.Fprintf(&b, "\n- %s (%d missed)", m.WorkStream, m.MissedCount)
	}
	b.WriteString("\nOpen the sleepy-head terminal tab to catch them up.")
	text := b.String()
	c.inflight.Add(1)
	go func() {
		defer c.inflight.Done()
		d.sendChannelMessage(text, "sleepy_head", "")
	}()
}
