package daemon

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/slimslenderslacks/work/internal/project"
)

// handleIntakeFile is how a new task gets queued for planning: a human (or a
// script) drops a markdown file into `<project>/intake/`, and this gets a
// planning agent onto it right away, whatever the project is doing:
//
//   - working — the status is left alone and the project is dispatched
//     directly; dispatchProject's working branch sees intake no planner has
//     been handed yet and launches one alongside the running tasks (planning
//     has its own session slot — see launchPlanningAgent). Flipping a working
//     project to ready is avoided on purpose: it would take the project out of
//     the task dispatch loop's status for the length of the planning run.
//   - anything else (idle, merged, blocked, ready) — flip to status:ready
//     (clearing any blocked_reason), which routes to planning through the
//     daemon's normal status:ready handling.
//
// If a planner is already running, the launch is a dedup no-op and the new
// file is picked up by a follow-up run once the current one ends (see
// afterPlanningSession) — never by a second concurrent planner.
//
// The planning agent itself reads the pending intake files directly — see
// launchPlanningAgent, which scans the intake/ dir at launch time and forwards
// the list via runner.Plan.IntakeFiles — and is responsible for renaming each
// one to `*.processed` once it has created tasks from it, so handlerFor's
// intake/*.md match never fires on it again (see planning.tmpl).
func (d *Daemon) handleIntakeFile(path string) {
	projectDir := filepath.Dir(filepath.Dir(path)) // .../<project>/intake/<file> -> .../<project>
	projectPath := filepath.Join(projectDir, project.FileName)
	if _, err := os.Stat(projectPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			d.audit.Log("intake_no_project", "path", path)
			return
		}
		d.audit.Log("project_load_error", "path", projectPath, "err", err.Error())
		return
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Already renamed away by a planning agent (or a human cleaning
			// up) racing this event — nothing to do.
			return
		}
		d.audit.Log("intake_read_error", "path", path, "err", err.Error())
		return
	}
	if strings.TrimSpace(string(data)) == "" {
		// Leave it alone rather than triggering a planning cycle over
		// nothing: an empty file is more likely mid-write or a mistake a
		// human will fix than a real request.
		d.audit.Log("intake_empty", "path", path)
		return
	}

	p, err := project.Load(projectPath)
	if err != nil {
		d.audit.Log("project_load_error", "path", projectPath, "err", err.Error())
		return
	}
	if d.planningInFlight(projectPath) {
		// A planner is already running and will not see this file. No status
		// write (it would race the planner's own project.yaml write) and no
		// second planner: afterPlanningSession dispatches the project when the
		// run ends, and a pending file nobody was handed then gets its
		// follow-up run (working → launchPlanningAgent, ready → the retry,
		// idle → seed_replan).
		d.audit.Log("intake_queued", "path", path, "project", projectPath, "deferred", "planning_in_flight")
		return
	}
	if p.Status == project.StatusWorking {
		d.audit.Log("intake_queued", "path", path, "project", projectPath, "status", string(p.Status))
		d.revisitProject(projectPath)
		return
	}
	// Flip to ready (same as the old `:task` command did): this drives the
	// daemon's normal status:ready -> planning routing, and clearing
	// blocked_reason means a queued intake request also un-sticks a project a
	// human previously had to intervene on.
	p.Status = project.StatusReady
	p.BlockedReason = ""
	if err := project.SaveAs(projectPath, p, project.WriterAgent); err != nil {
		d.audit.Log("project_save_error", "path", projectPath, "err", err.Error())
		return
	}
	d.audit.Log("intake_queued", "path", path, "project", projectPath)
}

// pendingIntakeFiles returns the absolute paths of every non-blank *.md file
// sitting in root's intake/ directory, sorted for a stable, deterministic
// prompt. A missing or empty intake/ dir returns nil. Blank files are
// skipped for the same reason handleIntakeFile leaves them untouched rather
// than flipping the project to ready: an empty file reads as mid-write or a
// mistake, not a real request, and both this scan and the orphan-recovery
// check in dispatchProject (hasPendingIntake) must agree with that or a
// blank file left in intake/ would spuriously re-arm planning on a resting
// project.
//
// This is a plain directory scan — there is no daemon-side bookkeeping of
// "which files are pending"; the files themselves, not renamed to
// *.processed yet, are the durable record.
func pendingIntakeFiles(root string) []string {
	entries, err := os.ReadDir(filepath.Join(root, "intake"))
	if err != nil {
		return nil
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" {
			continue
		}
		path := filepath.Join(root, "intake", e.Name())
		data, err := os.ReadFile(path)
		if err != nil || strings.TrimSpace(string(data)) == "" {
			continue
		}
		files = append(files, path)
	}
	sort.Strings(files)
	return files
}

// hasPendingIntake reports whether projectPath's intake/ directory holds any
// unprocessed *.md files. Used alongside hasPendingSeed to re-arm planning
// for a resting (idle/merged) project whose intake-triggered status flip was
// clobbered (e.g. by a review agent's own status write) or whose planning run
// left a file unprocessed.
func hasPendingIntake(projectPath string) bool {
	return len(pendingIntakeFiles(filepath.Dir(projectPath))) > 0
}
