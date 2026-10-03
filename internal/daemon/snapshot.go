package daemon

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/session"
	"github.com/slimslenderslacks/work/internal/task"
)

// SnapshotVersion is the schema version stamped into every Snapshot. Bump it
// on any breaking change to the JSON shape (renamed/removed/retyped fields);
// additive fields don't require a bump. Documented in docs/state-snapshot.md.
const SnapshotVersion = 1

// Snapshot source values.
const (
	// SnapshotSourceDaemon marks a snapshot built inside a running daemon: it
	// carries the in-memory state (live sessions, failure counters, ...).
	SnapshotSourceDaemon = "daemon"
	// SnapshotSourceOffline marks a snapshot built by reading the roots (and
	// the on-disk ACP session dirs and audit log) directly, with no daemon.
	SnapshotSourceOffline = "offline"
)

// Daemon lifecycle states reported in DaemonInfo.State.
const (
	DaemonStateRunning = "running"
	DaemonStateStopped = "stopped"
)

// defaultAuditEvents is how many trailing audit events a snapshot carries
// unless configured otherwise.
const defaultAuditEvents = 50

// maxDescriptionLen caps the project description copied into a snapshot; the
// full text lives in .project.yaml, which the reader can open itself.
const maxDescriptionLen = 400

// Snapshot is the JSON-serializable picture of the daemon's runtime state that
// an external agent — which can read the orch roots, the audit log and the ACP
// session dirs but cannot see the daemon's memory — consumes. The daemon
// rewrites it atomically to the --state-file; `orch status --json` prints the
// same document. See docs/state-snapshot.md for the field-by-field schema.
//
// Every string in a Snapshot has passed through audit.Redact.
type Snapshot struct {
	Version     int       `json:"version"`
	GeneratedAt time.Time `json:"generated_at"`
	// Source is SnapshotSourceDaemon or SnapshotSourceOffline.
	Source string `json:"source"`
	// LiveStateAvailable is true only for daemon-sourced snapshots. When false,
	// Sessions holds ACP sessions discovered on disk (not the daemon's live
	// tracking) and the per-project Live blocks are absent.
	LiveStateAvailable bool `json:"live_state_available"`
	// Notes carries human-readable caveats (e.g. why live state is missing).
	Notes []string `json:"notes,omitempty"`

	Daemon      *DaemonInfo       `json:"daemon,omitempty"`
	Roots       []string          `json:"roots"`
	Projects    []ProjectSnapshot `json:"projects"`
	Sessions    []SessionSnapshot `json:"sessions"`
	AuditEvents []AuditEvent      `json:"audit_events"`
}

// DaemonInfo describes the daemon process that produced (or last produced) a
// snapshot, including the flags that shape its behaviour.
type DaemonInfo struct {
	// State is DaemonStateRunning, or DaemonStateStopped for the final
	// snapshot a cleanly-exiting daemon leaves behind.
	State            string    `json:"state"`
	PID              int       `json:"pid"`
	StartedAt        time.Time `json:"started_at"`
	WorkspaceManager string    `json:"workspace_manager,omitempty"`
	AcpKit           string    `json:"acp_kit,omitempty"`
	AcpWrapper       string    `json:"acp_wrapper,omitempty"`
	SessionsRoot     string    `json:"sessions_root,omitempty"`
	TmuxSession      string    `json:"tmux_session,omitempty"`
	AuditLog         string    `json:"audit_log,omitempty"`
	StateFile        string    `json:"state_file,omitempty"`
	Headless         bool      `json:"headless"`
	Roots            []string  `json:"roots"`
}

// ProjectSnapshot is one work stream (one .project.yaml under a root).
type ProjectSnapshot struct {
	// WorkStream is the basename of the directory holding .project.yaml — the
	// identifier the TUI and sandbox names use.
	WorkStream    string `json:"work_stream"`
	Path          string `json:"path"`
	Status        string `json:"status"`
	BlockedReason string `json:"blocked_reason,omitempty"`
	Branch        string `json:"branch,omitempty"`
	Description   string `json:"description,omitempty"`
	// LoadError is set (and most other fields are empty) when .project.yaml
	// exists but could not be parsed.
	LoadError string `json:"load_error,omitempty"`

	Review     bool `json:"review"`
	ReviewNow  bool `json:"review_now"`
	Cleanup    bool `json:"cleanup"`
	Archive    bool `json:"archive"`
	Replan     bool `json:"replan"`
	WatchingPR bool `json:"watching_pr"`

	Cron       string `json:"cron,omitempty"`
	CronActive bool   `json:"cron_active"`

	PullRequests []PullRequestSummary `json:"pull_requests"`

	// TaskCounts has an entry for every task status (zero included) so a
	// reader never has to guess whether a missing key means zero.
	TaskCounts map[string]int `json:"task_counts"`
	TaskTotal  int            `json:"task_total"`
	Tasks      []TaskSnapshot `json:"tasks"`

	UpdatedAt time.Time `json:"updated_at,omitempty"`

	// Live is the daemon's in-memory view of this project; nil in an offline
	// snapshot.
	Live *ProjectLive `json:"live,omitempty"`
}

// PullRequestSummary is the review agent's recorded state of one PR.
type PullRequestSummary struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	URL    string `json:"url,omitempty"`
	State  string `json:"state,omitempty"`
}

// TaskSnapshot is one task of a project.
type TaskSnapshot struct {
	Name          string   `json:"name"`
	Status        string   `json:"status"`
	Attempts      int      `json:"attempts"`
	FailureReason string   `json:"failure_reason,omitempty"`
	BlockedReason string   `json:"blocked_reason,omitempty"`
	DependsOn     []string `json:"depends_on"`
}

// ProjectLive is the slice of a project's state that exists only in daemon
// memory.
type ProjectLive struct {
	// Sessions lists the agent kinds currently running for this project
	// (e.g. ["task", "wolf"]).
	Sessions []string `json:"sessions"`
	// WolfInFlight is true while the project's wolf agent is running.
	WolfInFlight bool `json:"wolf_in_flight"`
	// CleanupInFlight is true from the archive agent's dispatch until its
	// `cleanup: true` request flag has been cleared.
	CleanupInFlight bool `json:"cleanup_in_flight"`
	// ReviewAgentInFlight is true while a review agent run is active.
	ReviewAgentInFlight bool `json:"review_agent_in_flight"`
	// Failure counters (consecutive, reset on progress).
	PlanningFailures int `json:"planning_failures"`
	ProjectFailures  int `json:"project_failures"`
	ReviewFixCycles  int `json:"review_fix_cycles"`
	ReviewErrors     int `json:"review_errors"`
	// ReviewPoll is the PR poll state; nil when no poll is armed.
	ReviewPoll *ReviewPollInfo `json:"review_poll,omitempty"`
	// CronSchedule is the cron spec registered with the scheduler, if any.
	CronSchedule string `json:"cron_schedule,omitempty"`
}

// ReviewPollInfo is the state of a project's #review poll.
type ReviewPollInfo struct {
	// Schedule is the currently registered poll spec (e.g. "@every 15m").
	Schedule string `json:"schedule"`
	// BackoffIndex is the rung on the adaptive cadence ladder (0 = fastest).
	BackoffIndex int `json:"backoff_index"`
	// HasBaseline is true once a PR fingerprint has been recorded, i.e. a poll
	// has run and later quiet ticks can be recognised.
	HasBaseline bool `json:"has_baseline"`
}

// SessionSnapshot is one agent session.
type SessionSnapshot struct {
	// Key is the daemon's session-map key (the .project.yaml path, with a
	// "#wolf"/"#archive" suffix for those kinds). Empty for disk-sourced
	// sessions in an offline snapshot.
	Key         string `json:"key,omitempty"`
	Kind        string `json:"kind"`
	WorkStream  string `json:"work_stream,omitempty"`
	ProjectPath string `json:"project_path,omitempty"`
	Task        string `json:"task,omitempty"`
	// StartedAt is when the daemon began tracking the session (or, for a
	// disk-sourced session, the session dir's created_at).
	StartedAt   time.Time `json:"started_at"`
	Interactive bool      `json:"interactive"`
	// Source is "daemon" (live tracking) or "disk" (ACP session.json).
	Source      string `json:"source"`
	SandboxName string `json:"sandbox_name,omitempty"`
	// TmuxTarget is the tmux session name for non-ACP (legacy/interactive)
	// sessions.
	TmuxTarget string `json:"tmux_target,omitempty"`
	// ACP is set for sessions backed by an acp-wrapper.
	ACP *ACPInfo `json:"acp,omitempty"`
}

// ACPInfo locates an ACP session's on-disk presence.
type ACPInfo struct {
	SessionID  string `json:"session_id"`
	SessionDir string `json:"session_dir"`
	SocketPath string `json:"socket_path"`
	// Status is the session.json status ("starting", "running", ...) when the
	// record could be read.
	Status string `json:"status,omitempty"`
}

// AuditEvent is one parsed audit-log line.
type AuditEvent struct {
	Time   time.Time         `json:"time"`
	Event  string            `json:"event"`
	Fields map[string]string `json:"fields,omitempty"`
}

// ---------------------------------------------------------------------------
// Collection
// ---------------------------------------------------------------------------

// snapshotCollector walks roots and builds ProjectSnapshots, caching each
// project's result keyed by a cheap stat signature of its files so an idle
// tick re-parses nothing.
type snapshotCollector struct {
	mu    sync.Mutex
	cache map[string]cachedProject
}

type cachedProject struct {
	sig  string
	snap ProjectSnapshot
}

func newSnapshotCollector() *snapshotCollector {
	return &snapshotCollector{cache: map[string]cachedProject{}}
}

// projects returns a snapshot of every .project.yaml under roots, sorted by
// path. Projects whose files are unchanged since the last call are served from
// the cache (the returned values are deep-copied, so callers may attach Live).
func (c *snapshotCollector) projects(roots []string) []ProjectSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	seen := map[string]struct{}{}
	var out []ProjectSnapshot
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(p string, entry fs.DirEntry, err error) error {
			if err != nil {
				if entry != nil && entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if entry.IsDir() || entry.Name() != ".project.yaml" {
				return nil
			}
			if _, dup := seen[p]; dup {
				return nil
			}
			seen[p] = struct{}{}
			out = append(out, c.project(p))
			return nil
		})
	}
	for p := range c.cache {
		if _, ok := seen[p]; !ok {
			delete(c.cache, p) // project dir removed/archived
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func (c *snapshotCollector) project(path string) ProjectSnapshot {
	tasksDir := filepath.Join(filepath.Dir(path), "tasks")
	sig := projectSignature(path, tasksDir)
	if cp, ok := c.cache[path]; ok && cp.sig == sig {
		return cloneProjectSnapshot(cp.snap)
	}
	snap := loadProjectSnapshot(path, tasksDir)
	redactStrings(&snap)
	c.cache[path] = cachedProject{sig: sig, snap: snap}
	return cloneProjectSnapshot(snap)
}

// projectSignature fingerprints a project's inputs from stat data alone:
// mtime+size of .project.yaml and of every tasks/*.yaml.
func projectSignature(projectPath, tasksDir string) string {
	var b strings.Builder
	statSig(&b, projectPath)
	entries, err := os.ReadDir(tasksDir)
	if err != nil {
		return b.String()
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		b.WriteByte('|')
		b.WriteString(e.Name())
		statSig(&b, filepath.Join(tasksDir, e.Name()))
	}
	return b.String()
}

func statSig(b *strings.Builder, path string) {
	info, err := os.Stat(path)
	if err != nil {
		b.WriteString(":missing")
		return
	}
	b.WriteString(":")
	b.WriteString(info.ModTime().UTC().Format(time.RFC3339Nano))
	b.WriteString(":")
	b.WriteString(strconv.FormatInt(info.Size(), 10))
}

// allTaskStatuses lists every task status, so TaskCounts always has a key for
// each.
var allTaskStatuses = []task.Status{
	task.StatusReady, task.StatusRunning, task.StatusSuccess,
	task.StatusFailed, task.StatusBlocked, task.StatusCommitted,
}

func loadProjectSnapshot(path, tasksDir string) ProjectSnapshot {
	snap := ProjectSnapshot{
		WorkStream:   filepath.Base(filepath.Dir(path)),
		Path:         path,
		PullRequests: []PullRequestSummary{},
		TaskCounts:   map[string]int{},
		Tasks:        []TaskSnapshot{},
	}
	for _, s := range allTaskStatuses {
		snap.TaskCounts[string(s)] = 0
	}
	if info, err := os.Stat(path); err == nil {
		snap.UpdatedAt = info.ModTime().UTC()
	}

	p, err := project.Load(path)
	if err != nil {
		snap.LoadError = err.Error()
		return snap
	}
	snap.Status = string(p.Status)
	snap.BlockedReason = p.BlockedReason
	snap.Branch = p.Branch
	snap.Description = truncate(strings.TrimSpace(p.Description), maxDescriptionLen)
	snap.Review = p.Review
	snap.ReviewNow = p.ReviewNow
	snap.Cleanup = p.Cleanup
	snap.Archive = p.Archive
	snap.Replan = p.Replan
	snap.WatchingPR = p.Status == project.StatusIdle && p.WatchingPR()
	snap.Cron = p.Cron
	snap.CronActive = p.Cron != "" && !p.CronExpired()
	for _, pr := range p.PullRequests {
		snap.PullRequests = append(snap.PullRequests, PullRequestSummary{
			Repo: pr.Repo, Number: pr.Number, URL: pr.URL, State: pr.State,
		})
	}

	// Load tasks file-by-file rather than through taskgraph.Load: that loader
	// is strict (one bad file fails the whole graph), which is right for
	// dispatch but wrong for a status report. Mirrors the TUI's tasksFor.
	entries, err := os.ReadDir(tasksDir)
	if err != nil {
		return snap
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		t, err := task.Load(filepath.Join(tasksDir, e.Name()))
		if err != nil {
			continue
		}
		name := t.Name
		if name == "" {
			// A pending seed not yet named by planning.
			name = strings.TrimSuffix(e.Name(), ".yaml")
		}
		snap.TaskCounts[string(t.Status)]++
		snap.TaskTotal++
		deps := append([]string{}, t.DependsOn...)
		snap.Tasks = append(snap.Tasks, TaskSnapshot{
			Name:          name,
			Status:        string(t.Status),
			Attempts:      t.Attempts,
			FailureReason: t.FailureReason,
			BlockedReason: t.BlockedReason,
			DependsOn:     deps,
		})
	}
	sort.SliceStable(snap.Tasks, func(i, j int) bool { return snap.Tasks[i].Name < snap.Tasks[j].Name })
	return snap
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func cloneProjectSnapshot(p ProjectSnapshot) ProjectSnapshot {
	out := p
	out.PullRequests = append([]PullRequestSummary{}, p.PullRequests...)
	out.TaskCounts = make(map[string]int, len(p.TaskCounts))
	for k, v := range p.TaskCounts {
		out.TaskCounts[k] = v
	}
	out.Tasks = make([]TaskSnapshot, len(p.Tasks))
	for i, t := range p.Tasks {
		t.DependsOn = append([]string{}, t.DependsOn...)
		out.Tasks[i] = t
	}
	out.Live = nil
	return out
}

// auditEventsFromLines parses audit lines (oldest first) into events, redacting
// every value, and keeps the last n. Unparseable lines are skipped.
func auditEventsFromLines(lines []string, n int) []AuditEvent {
	events := []AuditEvent{}
	for _, ln := range lines {
		ev, ok := audit.ParseLine(ln)
		if !ok {
			continue
		}
		out := AuditEvent{Time: ev.Time.UTC(), Event: ev.Name}
		if len(ev.Fields) > 0 {
			out.Fields = ev.Fields
		}
		redactStrings(&out)
		events = append(events, out)
	}
	if n >= 0 && len(events) > n {
		events = events[len(events)-n:]
	}
	return events
}

// ---------------------------------------------------------------------------
// Offline snapshot
// ---------------------------------------------------------------------------

// OfflineOptions configures BuildOfflineSnapshot.
type OfflineOptions struct {
	// Roots are the directories to scan for .project.yaml files. Required.
	Roots []string
	// AuditLog is the audit log whose tail is reported; empty skips events.
	AuditLog string
	// SessionsRoot is the ACP sessions root to scan; empty uses the default
	// (~/.workingman/sessions).
	SessionsRoot string
	// AuditEvents is how many trailing audit events to include (default 50).
	AuditEvents int
}

// BuildOfflineSnapshot assembles a Snapshot without a running daemon, from
// what is on disk: the roots' project/task files, the audit log tail and the
// ACP session dirs. It reports LiveStateAvailable=false: the daemon's
// in-memory state (tracked sessions, wolf in flight, review poll state,
// failure counters) simply cannot be recovered this way.
func BuildOfflineSnapshot(opts OfflineOptions) *Snapshot {
	n := opts.AuditEvents
	if n <= 0 {
		n = defaultAuditEvents
	}
	snap := &Snapshot{
		Version:     SnapshotVersion,
		GeneratedAt: time.Now().UTC(),
		Source:      SnapshotSourceOffline,
		Roots:       append([]string{}, opts.Roots...),
		Notes: []string{
			"live daemon state is unavailable (read directly from disk): sessions below are ACP session dirs found on disk, and failure counters, wolf/review in-flight flags and review poll state are not reported",
		},
	}
	snap.Projects = newSnapshotCollector().projects(opts.Roots)
	snap.Sessions = diskSessions(opts.SessionsRoot, nil)
	if opts.AuditLog != "" {
		lines, err := audit.TailFile(opts.AuditLog, n)
		if err != nil {
			snap.Notes = append(snap.Notes, "audit log unreadable: "+err.Error())
		}
		snap.AuditEvents = auditEventsFromLines(lines, n)
	}
	if snap.AuditEvents == nil {
		snap.AuditEvents = []AuditEvent{}
	}
	redactStrings(snap)
	return snap
}

// diskSessions lists the live-looking ACP sessions recorded under the sessions
// root (status starting/running). skip, when non-nil, names session ids to
// leave out (those already reported from live tracking).
func diskSessions(root string, skip map[string]bool) []SessionSnapshot {
	out := []SessionSnapshot{}
	store, err := session.NewStore(root)
	if err != nil {
		return out
	}
	recs, err := store.List()
	if err != nil {
		return out
	}
	for _, rec := range recs {
		if rec.Status != session.StatusRunning && rec.Status != session.StatusStarting {
			continue
		}
		if skip[rec.ID] {
			continue
		}
		s := SessionSnapshot{
			Kind:        rec.Kind,
			ProjectPath: rec.ProjectPath,
			StartedAt:   rec.CreatedAt.UTC(),
			Source:      "disk",
			SandboxName: rec.SandboxName,
			ACP: &ACPInfo{
				SessionID:  rec.ID,
				SessionDir: store.Dir(rec.ID),
				SocketPath: socketOf(store, rec),
				Status:     string(rec.Status),
			},
		}
		if rec.ProjectPath != "" {
			s.WorkStream = filepath.Base(filepath.Dir(rec.ProjectPath))
		}
		s.Task = taskNameFor(rec.TaskPath)
		out = append(out, s)
	}
	return out
}

func socketOf(store session.Store, rec session.Session) string {
	if rec.SocketPath != "" {
		return rec.SocketPath
	}
	return store.SocketPath(rec.ID)
}

// ---------------------------------------------------------------------------
// Redaction
// ---------------------------------------------------------------------------

// redactStrings walks v (a pointer to a struct/slice/map/string tree) and runs
// audit.Redact over every string it holds, in place. It is the single choke
// point guaranteeing no snapshot string leaves the process unredacted,
// whichever field a secret ends up in.
func redactStrings(v any) {
	redactValue(reflect.ValueOf(v))
}

func redactValue(rv reflect.Value) {
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !rv.IsNil() {
			redactValue(rv.Elem())
		}
	case reflect.Struct:
		for i := 0; i < rv.NumField(); i++ {
			if rv.Type().Field(i).IsExported() {
				redactValue(rv.Field(i))
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < rv.Len(); i++ {
			redactValue(rv.Index(i))
		}
	case reflect.Map:
		if rv.IsNil() {
			return
		}
		for _, k := range rv.MapKeys() {
			val := rv.MapIndex(k)
			if val.Kind() == reflect.String {
				red := audit.Redact(val.String())
				if red != val.String() {
					rv.SetMapIndex(k, reflect.ValueOf(red).Convert(val.Type()))
				}
			}
		}
	case reflect.String:
		if rv.CanSet() {
			rv.SetString(audit.Redact(rv.String()))
		}
	}
}
