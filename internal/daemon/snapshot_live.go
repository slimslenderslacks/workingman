package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/session"
)

const (
	// snapshotTick is how often the snapshot loop re-evaluates state. The
	// collector's stat-signature cache keeps an idle tick down to a directory
	// walk plus stats, and nothing is written unless content changed.
	snapshotTick = 2 * time.Second
	// snapshotDebounce coalesces a burst of triggers (a session ending usually
	// comes with several audit lines and file writes) into one write.
	snapshotDebounce = 150 * time.Millisecond
	// snapshotHeartbeat is the longest an unchanged snapshot goes unwritten, so
	// generated_at doubles as a liveness signal for the daemon.
	snapshotHeartbeat = 15 * time.Second
)

// RuntimeInfo is the daemon-flag context recorded in each snapshot.
type RuntimeInfo struct {
	WorkspaceManager string
	AcpKit           string
	AcpWrapper       string
	SessionsRoot     string
	TmuxSession      string
	AuditLog         string
	Headless         bool
}

// snapshotState is the daemon's snapshot-publishing state. A zero value (path
// == "") means publishing is disabled.
type snapshotState struct {
	path        string
	auditEvents int
	info        RuntimeInfo
	startedAt   time.Time
	pid         int
	collector   *snapshotCollector

	kick chan struct{} // coalesced "state changed" trigger

	mu       sync.Mutex // serialises build+write
	lastHash string
	lastAt   time.Time
	lastErr  string // last write error logged, to avoid repeating it every tick
	stopped  bool   // final (stopped) snapshot written; no further writes
}

// WithRuntimeInfo records the daemon's flags for inclusion in snapshots.
func WithRuntimeInfo(info RuntimeInfo) Option {
	return func(d *Daemon) { d.snapshot.info = info }
}

// WithStateFile enables state-snapshot publishing to path (written atomically,
// refreshed on a ticker and on state changes). New rejects a path inside any
// watched root: a write there would feed the daemon's own fsnotify watcher.
// auditEvents is how many trailing audit events to include (<=0 → default).
func WithStateFile(path string, auditEvents int) Option {
	return func(d *Daemon) {
		d.snapshot.path = path
		d.snapshot.auditEvents = auditEvents
	}
}

// StateFile returns the configured snapshot path ("" when disabled).
func (d *Daemon) StateFile() string { return d.snapshot.path }

// DefaultStateFile is where the daemon publishes its snapshot when the user
// doesn't say: <sessions-root>/../state/snapshot.json — beside the ACP session
// dirs and well away from the orch roots the daemon watches.
func DefaultStateFile(sessionsRoot string) (string, error) {
	store, err := session.NewStore(sessionsRoot)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(store.Root), "state", "snapshot.json"), nil
}

// validateStateFile checks that a snapshot at path can't be seen by the
// watcher on roots: the file and its temp siblings (same directory) must lie
// outside every root's tree.
func validateStateFile(path string, roots []string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("state file %q: %w", path, err)
	}
	for _, r := range roots {
		rabs, err := filepath.Abs(r)
		if err != nil {
			continue
		}
		if within(abs, rabs) {
			return fmt.Errorf("state file %s overlaps watched root %s: writing it would trigger the daemon's own file watcher; pick a --state-file outside every --root", abs, rabs)
		}
	}
	return nil
}

// within reports whether p is root or lies beneath it.
func within(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// markSnapshotDirty asks the snapshot loop for a prompt (debounced) refresh.
// Cheap and non-blocking; a no-op when publishing is disabled.
func (d *Daemon) markSnapshotDirty() {
	if d.snapshot.path == "" {
		return
	}
	select {
	case d.snapshot.kick <- struct{}{}:
	default: // a refresh is already pending
	}
}

// snapshotLoop publishes snapshots until ctx is done: on every tick, and
// shortly after each markSnapshotDirty. Writes are skipped when nothing but
// generated_at would change, except for the heartbeat.
func (d *Daemon) snapshotLoop(ctx context.Context) {
	t := time.NewTicker(snapshotTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-d.snapshot.kick:
			// Debounce: let the burst settle, then fold any further kicks in.
			select {
			case <-time.After(snapshotDebounce):
			case <-ctx.Done():
				return
			}
			select {
			case <-d.snapshot.kick:
			default:
			}
		}
		if err := d.writeSnapshot(false); err != nil {
			d.logSnapshotError(err)
		}
	}
}

// logSnapshotError audits a failed write once per distinct message, so a
// persistently unwritable state file doesn't fill the audit log every tick.
func (d *Daemon) logSnapshotError(err error) {
	d.snapshot.mu.Lock()
	repeat := d.snapshot.lastErr == err.Error()
	d.snapshot.lastErr = err.Error()
	d.snapshot.mu.Unlock()
	if repeat {
		return
	}
	// The audit logger's OnLog hook only kicks the loop (debounced, and the
	// content hash then matches), so this cannot recurse into a write storm.
	d.audit.Log("state_snapshot_error", "path", d.snapshot.path, "err", err.Error())
}

// writeSnapshot builds the current snapshot and atomically writes it to the
// state file. Unless force is set it skips the write when the content (ignoring
// generated_at) matches the last write and the heartbeat hasn't elapsed.
func (d *Daemon) writeSnapshot(force bool) error {
	if d.snapshot.path == "" {
		return nil
	}
	d.snapshot.mu.Lock()
	defer d.snapshot.mu.Unlock()
	if d.snapshot.stopped {
		return nil
	}

	snap := d.Snapshot()
	return d.publishLocked(snap, force)
}

func (d *Daemon) publishLocked(snap *Snapshot, force bool) error {
	generated := snap.GeneratedAt
	snap.GeneratedAt = time.Time{}
	body, err := json.Marshal(snap)
	snap.GeneratedAt = generated
	if err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	sumHex := hex.EncodeToString(sum[:])
	if !force && sumHex == d.snapshot.lastHash && time.Since(d.snapshot.lastAt) < snapshotHeartbeat {
		return nil
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := WriteFileAtomic(d.snapshot.path, data); err != nil {
		return err
	}
	d.snapshot.lastHash = sumHex
	d.snapshot.lastAt = time.Now()
	d.snapshot.lastErr = ""
	return nil
}

// writeFinalSnapshot publishes the state a cleanly exiting daemon leaves
// behind: marked stopped, with disk-discovered ACP sessions (detached ones keep
// running) instead of live tracking.
func (d *Daemon) writeFinalSnapshot() {
	if d.snapshot.path == "" {
		return
	}
	d.snapshot.mu.Lock()
	defer d.snapshot.mu.Unlock()
	d.snapshot.stopped = true
	snap := d.buildSnapshot(false)
	snap.Daemon.State = DaemonStateStopped
	snap.Notes = append(snap.Notes, "daemon has stopped; ACP sessions listed were still running on disk when it exited")
	if err := d.publishLocked(snap, true); err != nil {
		d.audit.Log("state_snapshot_error", "path", d.snapshot.path, "err", err.Error())
	}
}

// WriteFileAtomic writes data to path via a temp file in the same directory
// followed by a rename, so a concurrent reader sees either the previous
// complete file or the new one, never a partial write. The directory is
// created if needed. The temp file is removed on failure.
func WriteFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("state: create dir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("state: create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("state: write %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return fmt.Errorf("state: chmod %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("state: sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("state: close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("state: rename %s -> %s: %w", tmpName, path, err)
	}
	return nil
}

// Snapshot returns the daemon's current state — the roots' projects and tasks
// plus everything held only in memory. It is safe to call concurrently and
// does not touch the state file.
func (d *Daemon) Snapshot() *Snapshot { return d.buildSnapshot(true) }

// buildSnapshot assembles a Snapshot. With live=true it includes the daemon's
// in-memory session tracking and counters; with live=false it falls back to
// disk-discovered ACP sessions and omits the live blocks.
func (d *Daemon) buildSnapshot(live bool) *Snapshot {
	n := d.snapshot.auditEvents
	if n <= 0 {
		n = defaultAuditEvents
	}
	snap := &Snapshot{
		Version:     SnapshotVersion,
		GeneratedAt: time.Now().UTC(),
		Source:      SnapshotSourceDaemon,
		Roots:       append([]string{}, d.roots...),
		Daemon:      d.daemonInfo(),
	}

	snap.Projects = d.snapshot.collector.projects(d.roots)

	if live {
		snap.LiveStateAvailable = true
		snap.Sessions = d.liveSessionSnapshots()
		for i := range snap.Projects {
			snap.Projects[i].Live = d.projectLive(snap.Projects[i].Path, snap.Sessions)
		}
		// Sessions the daemon doesn't track but whose ACP dir is on disk are
		// not added: the daemon reconciles those into its own tracking at
		// startup, so a live snapshot's list is the authoritative one.
	} else {
		snap.Notes = append(snap.Notes, "live in-memory state not included")
		snap.Sessions = diskSessions(d.sessionsRootOrEmpty(), nil)
	}

	snap.AuditEvents = auditEventsFromLines(d.audit.Recent(n), n)
	redactStrings(snap)
	return snap
}

func (d *Daemon) sessionsRootOrEmpty() string {
	if d.runner != nil {
		if root, err := d.runner.ResolveSessionsRoot(); err == nil {
			return root
		}
	}
	return d.snapshot.info.SessionsRoot
}

func (d *Daemon) daemonInfo() *DaemonInfo {
	info := d.snapshot.info
	root := info.SessionsRoot
	if root == "" {
		root = d.sessionsRootOrEmpty()
	}
	return &DaemonInfo{
		State:            DaemonStateRunning,
		PID:              d.snapshot.pid,
		StartedAt:        d.snapshot.startedAt.UTC(),
		WorkspaceManager: info.WorkspaceManager,
		AcpKit:           info.AcpKit,
		AcpWrapper:       info.AcpWrapper,
		SessionsRoot:     root,
		TmuxSession:      info.TmuxSession,
		AuditLog:         info.AuditLog,
		StateFile:        d.snapshot.path,
		Headless:         info.Headless,
		Roots:            append([]string{}, d.roots...),
	}
}

// splitSessionKey separates a session-map key into its project path and the
// agent-slot marker ("" for the shared slot, "wolf", "archive").
func splitSessionKey(key string) (projectPath, marker string) {
	if i := strings.LastIndexByte(key, '#'); i >= 0 {
		return key[:i], key[i+1:]
	}
	return key, ""
}

// liveSessionSnapshots renders every session the daemon is tracking.
func (d *Daemon) liveSessionSnapshots() []SessionSnapshot {
	var sessionsRoot string
	if d.runner != nil {
		sessionsRoot, _ = d.runner.ResolveSessionsRoot()
	}

	d.sessionsMu.Lock()
	out := make([]SessionSnapshot, 0, len(d.sessions))
	for key, entry := range d.sessions {
		projectPath, _ := splitSessionKey(key)
		s := SessionSnapshot{
			Key:         key,
			Kind:        entry.kind.String(),
			WorkStream:  filepath.Base(filepath.Dir(projectPath)),
			ProjectPath: projectPath,
			Task:        entry.taskName,
			StartedAt:   entry.startedAt.UTC(),
			Interactive: entry.kind.Interactive(),
			Source:      "daemon",
		}
		if d.runner != nil && d.runner.UsesACP(entry.kind) {
			id := entry.sess.Name()
			s.SandboxName = runner.ACPSandboxNameFor(entry.kind, projectPath, entry.taskName)
			s.ACP = &ACPInfo{SessionID: id}
			if sessionsRoot != "" {
				store := session.Store{Root: sessionsRoot}
				s.ACP.SessionDir = store.Dir(id)
				s.ACP.SocketPath = store.SocketPath(id)
			}
		} else {
			s.TmuxTarget = entry.sess.Name()
		}
		out = append(out, s)
	}
	d.sessionsMu.Unlock()

	// Fill in the recorded ACP status (and the sandbox the wrapper actually
	// used) outside the lock: it reads session.json from disk.
	for i := range out {
		a := out[i].ACP
		if a == nil || sessionsRoot == "" {
			continue
		}
		if rec, err := (session.Store{Root: sessionsRoot}).Read(a.SessionID); err == nil {
			a.Status = string(rec.Status)
			if rec.SocketPath != "" {
				a.SocketPath = rec.SocketPath
			}
			if rec.SandboxName != "" {
				out[i].SandboxName = rec.SandboxName
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

// projectLive gathers the in-memory state for one project.
func (d *Daemon) projectLive(projectPath string, sessions []SessionSnapshot) *ProjectLive {
	live := &ProjectLive{Sessions: []string{}}
	for _, s := range sessions {
		if s.ProjectPath != projectPath {
			continue
		}
		live.Sessions = append(live.Sessions, s.Kind)
		switch s.Kind {
		case agent.WolfAgent.String():
			live.WolfInFlight = true
		case agent.ReviewAgent.String():
			live.ReviewAgentInFlight = true
		}
	}
	sort.Strings(live.Sessions)

	d.planningMu.Lock()
	live.PlanningFailures = d.planningFailures[projectPath]
	live.ProjectFailures = d.projectFailures[projectPath]
	d.planningMu.Unlock()

	d.cleanupMu.Lock()
	live.CleanupInFlight = d.cleanupInFlight[projectPath]
	d.cleanupMu.Unlock()

	d.reviewMu.Lock()
	live.ReviewFixCycles = d.reviewFixCycles[projectPath]
	live.ReviewErrors = d.reviewErrors[projectPath]
	idx := d.reviewBackoff[projectPath]
	_, hasBaseline := d.reviewFinger[projectPath]
	d.reviewMu.Unlock()

	if d.scheduler != nil {
		if spec := d.scheduler.Spec(reviewPollKey(projectPath)); spec != "" {
			live.ReviewPoll = &ReviewPollInfo{Schedule: spec, BackoffIndex: idx, HasBaseline: hasBaseline}
		}
		live.CronSchedule = d.scheduler.Spec(projectPath)
	}
	return live
}
