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
		p("  workspace-manager=%s acp-kit=%s headless=%t\n", orDash(d.WorkspaceManager), orDash(d.AcpKit), d.Headless)
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
		} else if ss.TmuxTarget != "" {
			line += "  tmux=" + ss.TmuxTarget
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
		}{{pr.Review, "review"}, {pr.ReviewNow, "review_now"}, {pr.Cleanup, "cleanup"}, {pr.Archive, "archive"}, {pr.Replan, "replan"}, {pr.WatchingPR, "watching_pr"}, {pr.CronActive, "cron:" + pr.Cron}} {
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
		var counts []string
		for _, st := range taskStatusOrder {
			if n := pr.TaskCounts[st]; n > 0 {
				counts = append(counts, fmt.Sprintf("%d %s", n, st))
			}
		}
		if len(counts) > 0 {
			p("    tasks: %s\n", strings.Join(counts, ", "))
		}
		if len(pr.Tasks) > 0 {
			tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
			for _, t := range pr.Tasks {
				extra := ""
				if len(t.DependsOn) > 0 {
					extra += " deps=" + strings.Join(t.DependsOn, ",")
				}
				if t.FailureReason != "" {
					extra += " failure=" + oneLine(t.FailureReason)
				}
				if t.BlockedReason != "" {
					extra += " blocked=" + oneLine(t.BlockedReason)
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

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
