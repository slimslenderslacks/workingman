package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// ReadSnapshot loads a snapshot file written by a daemon.
func ReadSnapshot(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse snapshot %s: %w", path, err)
	}
	return &s, nil
}

// WriteJSON writes the snapshot as indented JSON — the same document the
// daemon publishes to its state file.
func (s *Snapshot) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

// taskStatusOrder is the display order of task counts.
var taskStatusOrder = []string{"ready", "running", "success", "failed", "blocked", "committed"}

// WriteText renders the snapshot for humans and agents reading a terminal.
func (s *Snapshot) WriteText(w io.Writer) error {
	var err error
	p := func(format string, a ...any) {
		if err == nil {
			_, err = fmt.Fprintf(w, format, a...)
		}
	}

	p("orch status — generated %s (source: %s)\n", s.GeneratedAt.Format(time.RFC3339), s.Source)
	if d := s.Daemon; d != nil {
		up := ""
		if d.State == DaemonStateRunning && !d.StartedAt.IsZero() {
			up = ", up " + s.GeneratedAt.Sub(d.StartedAt).Round(time.Second).String()
		}
		p("daemon: %s pid=%d started=%s%s\n", d.State, d.PID, d.StartedAt.Format(time.RFC3339), up)
		p("  workspace-manager=%s acp-kit=%s acp-image=%s headless=%t\n", orDash(d.WorkspaceManager), orDash(d.AcpKit), orDash(d.AcpImage), d.Headless)
		p("  sessions-root=%s state-file=%s\n", orDash(d.SessionsRoot), orDash(d.StateFile))
	}
	p("roots: %s\n", strings.Join(s.Roots, ", "))
	for _, n := range s.Notes {
		p("note: %s\n", n)
	}

	// Sessions.
	label := "live sessions"
	if !s.LiveStateAvailable {
		label = "ACP sessions on disk (live session data unavailable)"
	}
	p("\n%s (%d):\n", label, len(s.Sessions))
	for _, ss := range s.Sessions {
		line := fmt.Sprintf("  %-8s %s", ss.Kind, orDash(ss.WorkStream))
		if ss.Task != "" {
			line += "/" + ss.Task
		}
		line += "  started " + ss.StartedAt.Format(time.RFC3339)
		if ss.ACP != nil {
			line += "  acp=" + ss.ACP.SessionID
			if ss.ACP.SocketPath != "" {
				line += " sock=" + ss.ACP.SocketPath
			}
		} else if ss.TabName != "" {
			line += "  tab=" + ss.TabName
		}
		p("%s\n", line)
	}

	// Projects.
	p("\nprojects (%d):\n", len(s.Projects))
	for _, pr := range s.Projects {
		status := pr.Status
		if pr.LoadError != "" {
			status = "LOAD ERROR"
		}
		p("  %s  [%s]", pr.WorkStream, orDash(status))
		if pr.Branch != "" {
			p("  branch=%s", pr.Branch)
		}
		var flags []string
		for _, f := range []struct {
			on   bool
			name string
		}{{pr.Review, "review"}, {pr.ReviewNow, "review_now"}, {pr.Cleanup, "cleanup"}, {pr.Archive, "archive"}, {pr.Replan, "replan"}, {pr.Hidden, "hidden"}, {pr.WatchingPR, "watching_pr"}, {pr.CronActive, "cron:" + pr.Cron}} {
			if f.on {
				flags = append(flags, f.name)
			}
		}
		if len(flags) > 0 {
			p("  flags=%s", strings.Join(flags, ","))
		}
		p("\n")
		if pr.LoadError != "" {
			p("    error: %s\n", pr.LoadError)
		}
		if pr.BlockedReason != "" {
			p("    blocked: %s\n", pr.BlockedReason)
		}
		for _, r := range pr.PullRequests {
			p("    PR %s#%d %s %s\n", r.Repo, r.Number, orDash(r.State), r.URL)
		}
		if counts := taskCountsLine(pr); counts != "" {
			p("    tasks: %s\n", counts)
		}
		if pr.MaxParallelTasks > 1 {
			p("    max_parallel_tasks: %d\n", pr.MaxParallelTasks)
		}
		if len(pr.CommitQueue) > 0 {
			p("    queue: %s\n", queueLine(pr))
		}
		if vc := pr.VerifyChecks; vc != nil {
			p("    verify checks: %d run, %d failed, %d skipped\n", vc.Run, vc.Failed, vc.Skipped)
		}
		if len(pr.Tasks) > 0 {
			tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
			for _, t := range pr.Tasks {
				extra := ""
				if len(t.DependsOn) > 0 {
					extra += " deps=" + strings.Join(t.DependsOn, ",")
				}
				if t.QueuePosition > 0 {
					extra += fmt.Sprintf(" queue=#%d", t.QueuePosition)
				}
				if len(t.WaitingOn) > 0 {
					extra += " waiting_on=" + strings.Join(t.WaitingOn, ",")
				}
				if in := t.Integration; in != nil {
					extra += " integration=" + integrationText(in)
				}
				if t.FailureReason != "" {
					extra += " failure=" + oneLine(t.FailureReason)
				}
				if t.BlockedReason != "" {
					extra += " blocked=" + oneLine(t.BlockedReason)
				}
				if t.Worktree != "" && t.Status != "committed" {
					extra += " worktree=" + t.Worktree
				}
				fmt.Fprintf(tw, "      - %s\t%s\tattempts=%d\t%s\n", t.Name, t.Status, t.Attempts, strings.TrimSpace(extra))
			}
			tw.Flush()
		}
		if l := pr.Live; l != nil {
			var parts []string
			if len(l.Sessions) > 0 {
				parts = append(parts, "sessions="+strings.Join(l.Sessions, ","))
			}
			if l.CommitReserved {
				parts = append(parts, "commit-reserved")
			}
			if l.WolfInFlight {
				parts = append(parts, "wolf-in-flight")
			}
			if l.CleanupInFlight {
				parts = append(parts, "cleanup-in-flight")
			}
			if l.ReviewAgentInFlight {
				parts = append(parts, "review-agent-in-flight")
			}
			for _, c := range []struct {
				n    int
				name string
			}{{l.PlanningFailures, "planning_failures"}, {l.ProjectFailures, "project_failures"}, {l.ReviewFixCycles, "review_fix_cycles"}, {l.ReviewErrors, "review_errors"}} {
				if c.n > 0 {
					parts = append(parts, fmt.Sprintf("%s=%d", c.name, c.n))
				}
			}
			if rp := l.ReviewPoll; rp != nil {
				parts = append(parts, fmt.Sprintf("review-poll=%s(backoff %d)", rp.Schedule, rp.BackoffIndex))
			}
			if len(parts) > 0 {
				p("    live: %s\n", strings.Join(parts, " "))
			}
		}
	}

	// Audit events.
	p("\nrecent audit events (%d):\n", len(s.AuditEvents))
	for _, ev := range s.AuditEvents {
		keys := make([]string, 0, len(ev.Fields))
		for k := range ev.Fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		for _, k := range keys {
			v := ev.Fields[k]
			if strings.ContainsAny(v, " \"") || v == "" {
				v = fmt.Sprintf("%q", v)
			}
			fmt.Fprintf(&b, " %s=%s", k, v)
		}
		p("  %s %s%s\n", ev.Time.Format(time.RFC3339), ev.Event, b.String())
	}
	return err
}

// taskCountsLine is the per-project task summary: the count of each status,
// with the running tasks named and `success` split into the task being
// integrated and the rest of the queue, e.g.
// "1 ready, 2 running (a, b), 1 integrating (c), 1 queued, 3 committed".
func taskCountsLine(pr ProjectSnapshot) string {
	integrating := ""
	if pr.Live != nil {
		integrating = pr.Live.Integrating
	}
	var running []string
	queuedSuccess, integratingSuccess := 0, 0
	for _, t := range pr.Tasks {
		switch t.Status {
		case "running":
			running = append(running, t.Name)
		case "success":
			if t.Name == integrating {
				integratingSuccess++
			} else {
				queuedSuccess++
			}
		}
	}
	var counts []string
	for _, st := range taskStatusOrder {
		n := pr.TaskCounts[st]
		switch {
		case st == "running" && n > 0 && len(running) == n:
			counts = append(counts, fmt.Sprintf("%d running (%s)", n, strings.Join(running, ", ")))
		case st == "success" && n > 0 && integratingSuccess+queuedSuccess == n:
			if integratingSuccess > 0 {
				counts = append(counts, fmt.Sprintf("%d integrating (%s)", integratingSuccess, integrating))
			}
			if queuedSuccess > 0 {
				counts = append(counts, fmt.Sprintf("%d queued", queuedSuccess))
			}
		case n > 0:
			counts = append(counts, fmt.Sprintf("%d %s", n, st))
		}
	}
	return strings.Join(counts, ", ")
}

// queueLine renders the commit queue head first, marking the task being
// integrated: "c (integrating), d".
func queueLine(pr ProjectSnapshot) string {
	integrating := ""
	if pr.Live != nil {
		integrating = pr.Live.Integrating
	}
	parts := make([]string, len(pr.CommitQueue))
	for i, name := range pr.CommitQueue {
		parts[i] = name
		if name == integrating {
			parts[i] += " (integrating)"
		}
	}
	return strings.Join(parts, ", ")
}

// integrationText is a task's integration report in one token-ish string:
// "conflict(gateway:a.go,b.go)" or "error(attempt 2)".
func integrationText(in *IntegrationSnapshot) string {
	var detail []string
	where := in.Repo
	if len(in.Files) > 0 {
		where += ":" + strings.Join(in.Files, ",")
	}
	if where != "" {
		detail = append(detail, where)
	}
	if in.Attempts > 0 {
		detail = append(detail, fmt.Sprintf("attempt %d", in.Attempts))
	}
	if len(detail) == 0 {
		return in.Result
	}
	return in.Result + "(" + strings.Join(detail, " ") + ")"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
