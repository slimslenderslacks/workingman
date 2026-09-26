package tui

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/task"
)

// maxRecentTransitions caps how many audit-log lines the "Recent" section
// shows, so a long-lived project's history doesn't crowd out the task graph
// above it.
const maxRecentTransitions = 8

// renderProjectDetail draws the optional right column ("tr", hidden by
// default): a curated dashboard for the currently-selected project — its
// status and what's watching it, its task graph, and its most recent
// audit-log activity. Unlike the raw YAML this pane used to show, everything
// here is derived from data the TUI already has (ProjectView/TaskView from
// ScanProjects, and the audit tail); the underlying files under
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
	body := renderProjectDetailBody(v, m.auditLines, innerWidth)
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
// (archive/cron/watching-PR/blocked), then the task graph, then recent audit
// activity. Sections are joined and handed to clampLines by the caller, so
// on a short pane the LEAST essential section (recent activity) is what gets
// cut off first, never the header.
func renderProjectDetailBody(v ProjectView, auditLines []string, width int) string {
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
	sections = append(sections, renderProjectDetailTasks(v, width))
	if recent := renderProjectDetailRecent(recentProjectTransitions(auditLines, v, maxRecentTransitions), width); len(recent) > 0 {
		sections = append(sections, recent)
	}

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
// project that aren't obvious from its bare status: it's cleaned up and
// archivable, it wakes itself up on a schedule, it's watching a pull
// request, or it's blocked and why. Any, all, or none may apply, so the
// caller only inserts this section when it isn't empty.
func renderProjectDetailBadges(v ProjectView, width int) []string {
	var lines []string
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
		style := taskStatusStyle(t.Status)
		row := fmt.Sprintf("%s %s  %s", taskStatusGlyph(t.Status), t.Name, t.Status)
		lines = append(lines, style.Render(truncate(row, width)))
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
func taskStatusGlyph(s task.Status) string {
	switch s {
	case task.StatusReady:
		return "○"
	case task.StatusRunning:
		return "●"
	case task.StatusSuccess, task.StatusCommitted:
		return "✓"
	case task.StatusFailed:
		return "✗"
	case task.StatusBlocked:
		return "⚠"
	default:
		return "?"
	}
}

// renderProjectDetailRecent renders the "Recent" section from an
// already-filtered, already-capped list of audit lines (see
// recentProjectTransitions), oldest first — so newest reads at the bottom,
// the same direction the Audit pane itself tails.
func renderProjectDetailRecent(lines []string, width int) []string {
	if len(lines) == 0 {
		return nil
	}
	out := []string{paneTitleStyle.Render(truncate("Recent", width))}
	for _, l := range lines {
		out = append(out, dimStyle.Render(truncate(compactAuditLine(l), width)))
	}
	return out
}

// recentProjectTransitions returns the audit lines that plausibly belong to
// project v: those whose path= value falls inside the project's own
// directory (covers .project.yaml, tasks/*.yaml, intake/*.md, and
// last-planned.yaml events) or whose task= value names one of the project's
// own tasks (covers task-scoped events, which the daemon logs without a
// path= at all — see internal/daemon/dispatch_lifecycle.go's task_observed
// etc.). Returned oldest-first, capped to the most recent `limit` matches,
// since that's what a human wants to see first.
func recentProjectTransitions(auditLines []string, v ProjectView, limit int) []string {
	if v.Path == "" {
		return nil
	}
	// Require a trailing "/" so a project named "widget" doesn't also match
	// a sibling "widget-2" whose directory happens to share the prefix.
	pathNeedle := "path=" + filepath.Dir(v.Path) + "/"
	taskNames := make(map[string]bool, len(v.Tasks))
	for _, t := range v.Tasks {
		if t.Name != "" {
			taskNames[t.Name] = true
		}
	}

	var matched []string
	for _, line := range auditLines {
		if strings.Contains(line, pathNeedle) || auditLineMentionsTask(line, taskNames) {
			matched = append(matched, line)
		}
	}
	if limit > 0 && len(matched) > limit {
		matched = matched[len(matched)-limit:]
	}
	return matched
}

// auditLineMentionsTask reports whether line carries a task=<name> field
// naming one of taskNames. Audit lines are space-separated key=value pairs
// (see internal/audit), and task names are kebab-case slugs with no spaces,
// so the value runs to the next space or end of line.
func auditLineMentionsTask(line string, taskNames map[string]bool) bool {
	idx := strings.Index(line, "task=")
	if idx < 0 {
		return false
	}
	rest := line[idx+len("task="):]
	if sp := strings.IndexByte(rest, ' '); sp >= 0 {
		rest = rest[:sp]
	}
	return taskNames[rest]
}

// compactAuditLine reformats a raw audit line for the detail pane: the
// timestamp shortens to local HH:MM:SS (the date is implicit — this is
// "recent" activity), and any path= field is dropped since every line here
// already belongs to the one project the pane is showing. task=, status=,
// and every other field are kept as-is.
func compactAuditLine(raw string) string {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return raw
	}
	ts := fields[0]
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		ts = t.Local().Format("15:04:05")
	}
	kept := []string{ts}
	for _, f := range fields[1:] {
		if strings.HasPrefix(f, "path=") {
			continue
		}
		kept = append(kept, f)
	}
	return strings.Join(kept, " ")
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
