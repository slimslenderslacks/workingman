package daemon

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/session"
)

// WolfSession describes one live wolf agent session so the inbound-message
// router can attach to its ACP conversation.
type WolfSession struct {
	// Key is the daemon's session-map key ("<project path>#wolf"), the same
	// SessionKey recorded in channels.ConversationTarget.
	Key string
	// ProjectPath is the .project.yaml the wolf serves.
	ProjectPath string
	// WorkStream is the human label of the project (see workStreamName).
	WorkStream string
	// ID is the launcher's name for the session — the ACP session directory
	// name, and channels.ConversationTarget.SessionID. It changes on every
	// relaunch.
	ID string
	// Dir is the session directory (<sessions-root>/<ID>); "" when the daemon
	// has no runner to resolve the sessions root with.
	Dir string
	// Attachable is false for a wolf the router cannot tune in to: one running
	// on the host under tmux (--wolf-host) rather than as an ACP session.
	Attachable bool
	StartedAt  time.Time
}

// WolfSessions returns the live wolf sessions, oldest first.
func (d *Daemon) WolfSessions() []WolfSession {
	d.sessionsMu.Lock()
	var out []WolfSession
	for key, e := range d.sessions {
		if e.kind != agent.WolfAgent {
			continue
		}
		projectPath := strings.TrimSuffix(key, "#wolf")
		out = append(out, WolfSession{
			Key:         key,
			ProjectPath: projectPath,
			WorkStream:  workStreamName(projectPath),
			ID:          e.sess.Name(),
			Attachable:  d.runner != nil && d.runner.UsesACP(agent.WolfAgent),
			StartedAt:   e.startedAt,
		})
	}
	d.sessionsMu.Unlock()

	for i := range out {
		if out[i].Attachable && d.runner != nil {
			if root, err := d.runner.ResolveSessionsRoot(); err == nil {
				out[i].Dir = session.Store{Root: root}.Dir(out[i].ID)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].Key < out[j].Key
		}
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out
}

// WolfSession returns the live wolf session tracked under key.
func (d *Daemon) WolfSession(key string) (WolfSession, bool) {
	for _, w := range d.WolfSessions() {
		if w.Key == key {
			return w, true
		}
	}
	return WolfSession{}, false
}

// StatusSummary is the one-line state summary the `/status` chat command
// shows, built from the daemon snapshot (so every string has been through
// audit.Redact).
func (d *Daemon) StatusSummary() string {
	return d.Snapshot().Summary()
}

// Summary renders the snapshot as one line: project statuses, live sessions
// and task counts.
func (s *Snapshot) Summary() string {
	projects := map[string]int{}
	tasks := map[string]int{}
	for _, p := range s.Projects {
		st := p.Status
		if p.LoadError != "" || st == "" {
			st = "unreadable"
		}
		projects[st]++
		for k, n := range p.TaskCounts {
			tasks[k] += n
		}
	}

	parts := []string{fmt.Sprintf("%d %s", len(s.Projects), plural(len(s.Projects), "project", "projects"))}
	if detail := countList(projects, nil); detail != "" {
		parts[0] += " (" + detail + ")"
	}

	kinds := map[string]int{}
	for _, ss := range s.Sessions {
		kinds[ss.Kind]++
	}
	live := fmt.Sprintf("%d live %s", len(s.Sessions), plural(len(s.Sessions), "session", "sessions"))
	if detail := countList(kinds, nil); detail != "" {
		live += " (" + detail + ")"
	}
	parts = append(parts, live)

	if detail := countList(tasks, taskStatusOrder); detail != "" {
		parts = append(parts, "tasks: "+detail)
	}
	if !s.LiveStateAvailable {
		parts = append(parts, "live state unavailable")
	}
	return strings.Join(parts, " · ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// countList renders "2 ready, 1 failed": in the given order when set, else
// alphabetically; zero counts are skipped.
func countList(counts map[string]int, order []string) string {
	keys := order
	if keys == nil {
		for k := range counts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
	}
	var out []string
	for _, k := range keys {
		if n := counts[k]; n > 0 {
			out = append(out, fmt.Sprintf("%d %s", n, k))
		}
	}
	return strings.Join(out, ", ")
}
