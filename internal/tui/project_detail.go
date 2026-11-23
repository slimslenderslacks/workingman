package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/task"
)

// renderProjectDetail draws the optional right column ("tr", hidden by
// default): a curated dashboard for the currently-selected project — its
// status and what's watching it, and its task graph. Unlike the raw YAML this
// pane used to show, everything here is derived from data the TUI already has
// (ProjectView/TaskView from ScanProjects); the underlying files under
// ~/orch/<project> are always a terminal away, so this pane summarizes
// rather than dumps file content.
//
// `height` is the total rows the pane should occupy; the border consumes 2
// of those, the rest hold the dashboard body.
func (m model) renderProjectDetail(width, height int) string {
	bs := m.borderStyle(paneProjectDetail)
	base := bs.Width(width - bs.GetHorizontalBorderSize())
	innerHeight := height - base.GetVerticalFrameSize()
	if innerHeight < 0 {
		innerHeight = 0
	}
	style := base.Height(innerHeight).MaxWidth(width)
	innerWidth := width - style.GetHorizontalFrameSize()
	if innerWidth < 0 {
		innerWidth = 0
	}

	v, ok := m.selectedProject()
	if !ok {
		return style.Render(clampLines(dimStyle.Render("(none)"), innerHeight))
	}
	body := renderProjectDetailBody(v, innerWidth)
	return style.Render(clampLines(body, innerHeight))
}

// selectedProject resolves m.projSel to its ProjectView, mirroring
// selectedProjectTasks.
func (m model) selectedProject() (ProjectView, bool) {
	if m.projSel == "" {
		return ProjectView{}, false
	}
	for _, p := range m.projects {
		if p.Path == m.projSel {
			return p, true
		}
	}
	return ProjectView{}, false
}

// renderProjectDetailBody composes the panel's full content, most important
// information first: header/status, then anything actively worth flagging
// (archive/cron/watching-PR/blocked), then the task graph. Sections are joined
// and handed to clampLines by the caller, so on a short pane the LEAST
// essential section (the task graph) is what gets cut off first, never the
// header.
func renderProjectDetailBody(v ProjectView, width int) string {
	if v.LoadErr != "" {
		var lines []string
		for _, l := range wrapDisplayWidth("parse error: "+v.LoadErr, width) {
			lines = append(lines, statusErrStyle.Render(l))
		}
		return strings.Join(lines, "\n")
	}

	sections := [][]string{renderProjectDetailHeader(v, width)}
	if badges := renderProjectDetailBadges(v, width); len(badges) > 0 {
		sections = append(sections, badges)
	}
	if line := renderProjectDetailParallel(v, width); line != "" {
		sections = append(sections, []string{line})
	}
	sections = append(sections, renderProjectDetailTasks(v, width))

	var lines []string
	for i, s := range sections {
		if i > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, s...)
	}
	return strings.Join(lines, "\n")
}

// renderProjectDetailHeader is the panel's first section: name, status +
// branch, which repos the project spans (existing and, separately, any not
// yet created), and how long ago it was created.
func renderProjectDetailHeader(v ProjectView, width int) []string {
	lines := []string{cardNameStyle.Render(truncate(v.Name, width))}

	statusLine := renderStatus(string(v.Status))
	if v.Branch != "" {
		statusLine = truncate(statusLine+"  "+dimStyle.Render(v.Branch), width)
	}
	lines = append(lines, statusLine)

	if len(v.Repos) > 0 {
		lines = append(lines, dimStyle.Render(truncate("repos: "+strings.Join(repoNames(v.Repos), ", "), width)))
	}
	if len(v.NewRepos) > 0 {
		// Distinguished from Repos: these don't exist yet — the daemon
		// creates them on first workspace use — so lumping them into the
		// same "repos:" line would misreport the project as already
		// spanning a repo it hasn't touched.
		lines = append(lines, dimStyle.Render(truncate("new repos: "+strings.Join(repoNames(v.NewRepos), ", "), width)))
	}

	if !v.CreatedAt.IsZero() {
		age := "created " + sessionAge(v.CreatedAt, time.Now()) + " ago"
		lines = append(lines, dimStyle.Render(truncate(age, width)))
	}
	return lines
}

// repoNames formats each repo as "org/name", the same short form prShortLinks
// and the project card grid already use for pull requests.
func repoNames(repos []project.Repo) []string {
	names := make([]string, len(repos))
	for i, r := range repos {
		names[i] = r.Org + "/" + r.Name
	}
	return names
}

// renderProjectDetailBadges surfaces the "interesting things" about a
// project that aren't obvious from its bare status: it's hidden from the
// gallery, it's cleaned up and
// archivable, it wakes itself up on a schedule, it's watching a pull
// request, or it's blocked and why. Any, all, or none may apply, so the
// caller only inserts this section when it isn't empty.
func renderProjectDetailBadges(v ProjectView, width int) []string {
	var lines []string
	if v.Hidden {
		lines = append(lines, dimStyle.Render(truncate("⊘ hidden from the gallery — :show to restore", width)))
	}
	if v.Archive {
		lines = append(lines, dimStyle.Render(truncate("✓ cleaned up — ready for :archive", width)))
	}
	if v.CronActive {
		lines = append(lines, dimStyle.Render(truncate("cron: "+v.Cron, width)))
	}
	if v.WatchingPR {
		open := openPullRequests(v.PullRequests)
		if len(open) == 0 {
			lines = append(lines, dimStyle.Render(truncate("watching PR: expected", width)))
		} else {
			lines = append(lines, dimStyle.Render(truncate("watching PR:", width)))
			for _, pr := range open {
				url := prURL(pr)
				// Show the actual URL text rather than the compact "org/name#123"
				// form the project card uses: a terminal that doesn't support OSC 8
				// hyperlinks (many don't) still auto-linkifies plain URL text, so
				// this reads — and works — as a clickable link either way. Wrapped
				// in hyperlink() too for terminals that do support OSC 8. Uses
				// ansi.Truncate, not truncate, since it won't cut mid-escape and
				// leave the terminal thinking a hyperlink is still open.
				line := "  " + hyperlink(url, url)
				lines = append(lines, dimStyle.Render(ansi.Truncate(line, width, "…")))
			}
		}
	}
	if v.BlockedReason != "" {
		for _, l := range wrapDisplayWidth("blocked: "+v.BlockedReason, width) {
			lines = append(lines, statusErrStyle.Render(l))
		}
	}
	return lines
}

// renderProjectDetailParallel is the detail pane's view of the pipeline,
// shown when the project allows more than one task at a time or anything is
// waiting to integrate: "parallel: 2 (running a, b · integrating c · queued
// d)". Empty otherwise, so a serial project with nothing queued looks as it
// always did.
func renderProjectDetailParallel(v ProjectView, width int) string {
	var running, queued []string
	for _, t := range v.Tasks {
		if t.Status == task.StatusRunning {
			running = append(running, t.Name)
		}
	}
	sort.Strings(running)
	for _, name := range v.CommitQueue {
		if name != v.Integrating {
			queued = append(queued, name)
		}
	}
	limit := v.MaxParallel
	if limit < 1 {
		limit = 1
	}
	if limit <= 1 && len(v.CommitQueue) == 0 {
		return ""
	}
	var parts []string
	if len(running) > 0 {
		parts = append(parts, "running "+strings.Join(running, ", "))
	}
	if v.Integrating != "" {
		parts = append(parts, "integrating "+v.Integrating)
	}
	if len(queued) > 0 {
		parts = append(parts, "queued "+strings.Join(queued, ", "))
	}
	line := fmt.Sprintf("parallel: %d", limit)
	if len(parts) > 0 {
		line += " (" + strings.Join(parts, " · ") + ")"
	}
	return dimStyle.Render(truncate(line, width))
}

// renderProjectDetailTasks renders the task graph: one line per task with a
// status glyph and colored label, followed by an indented dependency line
// when it has any, and an indented line naming the repos its commits landed
// in when it has committed — a readable substitute for a boxes-and-arrows
// DAG diagram, in the same run order the Tasks pane itself uses.
func renderProjectDetailTasks(v ProjectView, width int) []string {
	lines := []string{paneTitleStyle.Render(truncate(fmt.Sprintf("Tasks (%d)", len(v.Tasks)), width))}
	if len(v.Tasks) == 0 {
		lines = append(lines, dimStyle.Render("no tasks yet"))
		return lines
	}
	for _, t := range v.Tasks {
		style := taskViewStyle(t)
		row := fmt.Sprintf("%s %s  %s", taskViewGlyph(t, v.Integrating), t.Name, t.Status)
		if hint := taskRowHint(t, v.Integrating); hint != "" {
			row += "  · " + hint
		}
		lines = append(lines, style.Render(truncate(row, width)))
		if t.Worktree != "" && (t.Status == task.StatusFailed || t.Status == task.StatusBlocked) {
			lines = append(lines, dimStyle.Render(truncate("    worktree: "+t.Worktree, width)))
		}
		if len(t.DependsOn) > 0 {
			dep := "    ← " + strings.Join(t.DependsOn, ", ")
			lines = append(lines, dimStyle.Render(truncate(dep, width)))
		}
		if len(t.Commits) > 0 {
			repos := make([]string, len(t.Commits))
			for i, c := range t.Commits {
				repos[i] = c.Repo
			}
			commit := "    ⎇ " + strings.Join(repos, ", ")
			lines = append(lines, dimStyle.Render(truncate(commit, width)))
		}
	}
	return lines
}

// taskStatusGlyph returns a compact symbol for a task's status, mirroring
// the vocabulary sessionStatusGlyph established for sessions: a single
// glyph that reads at a glance and pairs with taskStatusStyle's color.
// success is queued for integration (⧗), not done: only committed gets ✓.
// taskViewGlyph refines it with what only the task view knows.
func taskStatusGlyph(s task.Status) string {
	switch s {
	case task.StatusReady:
		return "○"
	case task.StatusRunning:
		return "●"
	case task.StatusSuccess:
		return "⧗"
	case task.StatusCommitted:
		return "✓"
	case task.StatusFailed:
		return "✗"
	case task.StatusBlocked:
		return "⚠"
	default:
		return "?"
	}
}

// taskViewGlyph is taskStatusGlyph for a task in context
// (docs/parallel-task-worktrees.md D5): a success task the project's commit
// agent is landing (integrating names it) shows ⇡ rather than the queued ⧗,
// and a ready task carrying an integration report, a conflict re-run waiting
// for a slot, shows ↻.
func taskViewGlyph(t TaskView, integrating string) string {
	switch {
	case t.Status == task.StatusSuccess && integrating != "" && t.Name == integrating:
		return "⇡"
	case t.Status == task.StatusReady && t.Integration != nil:
		return "↻"
	}
	return taskStatusGlyph(t.Status)
}

// taskViewStyle is the colour for taskViewGlyph: a conflict re-run waiting
// for a slot is in the running colour, as it is already part-way through the
// pipeline; everything else follows its status.
func taskViewStyle(t TaskView) lipgloss.Style {
	if t.Status == task.StatusReady && t.Integration != nil {
		return statusRunning
	}
	return taskStatusStyle(t.Status)
}

// wrapDisplayWidth hard-wraps each newline-separated line of s to width
// display columns. Empty source lines become a single empty output line so
// intentional blank spacing is preserved. The wrap is character-based rather
// than word-aware — free-form text here (descriptions, blocked reasons)
// often contains hyphens, slashes, and other characters word-aware wrap
// would treat as breakpoints, and a simple display-width wrap is easier to
// reason about.
func wrapDisplayWidth(s string, width int) []string {
	if width <= 0 {
		return strings.Split(s, "\n")
	}
	srcLines := strings.Split(s, "\n")
	out := make([]string, 0, len(srcLines))
	for _, line := range srcLines {
		if line == "" {
			out = append(out, "")
			continue
		}
		var cur strings.Builder
		curW := 0
		for _, r := range line {
			rw := lipgloss.Width(string(r))
			if rw == 0 {
				rw = 1
			}
			if curW+rw > width && cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
				curW = 0
			}
			cur.WriteRune(r)
			curW += rw
		}
		if cur.Len() > 0 {
			out = append(out, cur.String())
		}
	}
	return out
}
