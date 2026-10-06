package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/policy"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/session"
)

// workingmanSessionKey is the session-map key of the workingman agent. Unlike
// every other agent it belongs to no project, so the key is not a project path
// (nor a "path#marker"): it is a fixed name, which splitSessionKey/ListSessions
// special-case so the agent is not mislabelled as a work stream.
const workingmanSessionKey = runner.WorkingmanAgentSession

// workingmanScratchName is the scratch directory's name, a sibling of the ACP
// sessions root and the state dir (see defaultWorkingmanDir).
const workingmanScratchName = "workingman-agent"

const (
	// defaultWorkingmanMinBackoff / defaultWorkingmanMaxBackoff bound the
	// exponential delay before the agent is relaunched after it exits or fails
	// to start: 5s, 10s, 20s, … capped at 5 minutes.
	defaultWorkingmanMinBackoff = 5 * time.Second
	defaultWorkingmanMaxBackoff = 5 * time.Minute
	// defaultWorkingmanStableAfter is how long a launch must survive for the
	// next failure to count as a fresh one (backoff restarts from the minimum)
	// rather than a continuation of a crash loop.
	defaultWorkingmanStableAfter = 10 * time.Minute
)

// WorkingmanAgentConfig tunes WithWorkingmanAgent. The zero value is the
// production configuration.
type WorkingmanAgentConfig struct {
	// Dir is the agent's writable scratch directory — the one writable mount of
	// its sandbox, holding its .orch/ handoff files. Default:
	// <sessions-root>/../workingman-agent. It must lie outside every watched
	// root (New rejects it otherwise).
	Dir string
	// ExtraReadOnlyMounts are additional host paths mounted read-only next to
	// the orch roots, the state snapshot dir, the audit-log dir and the ACP
	// sessions root, which are always mounted.
	ExtraReadOnlyMounts []string
	// AuditLog is the audit log file the agent may read; its directory is
	// mounted read-only. Defaults to the audit log recorded by WithRuntimeInfo.
	AuditLog string
	// Policies are sbx policy rules applied to its sandbox right after creation
	// (e.g. a filesystem deny for paths outside Dir, where sbx supports it).
	// Read-only access is enforced by the mounts themselves, not by these.
	Policies []policy.Rule

	// MinBackoff / MaxBackoff / StableAfter override the restart backoff (see
	// the defaults above); non-positive values keep the defaults. Tests shrink
	// them.
	MinBackoff  time.Duration
	MaxBackoff  time.Duration
	StableAfter time.Duration
}

// workingmanAgent is the daemon's supervision state for the workingman agent.
type workingmanAgent struct {
	cfg WorkingmanAgentConfig
	dir string // resolved scratch directory

	// ended is poked (non-blocking) whenever the tracked session ends; the
	// supervisor wakes on it. Buffered so a poke that lands before the
	// supervisor is waiting is not lost.
	ended chan struct{}
}

func (w *workingmanAgent) signalEnded() {
	select {
	case w.ended <- struct{}{}:
	default:
	}
}

// WithWorkingmanAgent makes the daemon run the workingman agent: a persistent,
// read-only observer in its own sandbox that answers questions about the orch
// state (see agents.md §8). It is started when Run begins, relaunched with
// exponential backoff whenever it exits, and stopped on shutdown. A failing
// launch is audit-logged and retried; it never delays or blocks project
// dispatch. Needs a runner with an ACP launcher — without one it is skipped and
// audit-logged (workingman_agent_unavailable).
func WithWorkingmanAgent(cfg WorkingmanAgentConfig) Option {
	return func(d *Daemon) {
		d.workingman = &workingmanAgent{cfg: cfg, ended: make(chan struct{}, 1)}
	}
}

// WorkingmanAgentSession describes the live workingman agent's ACP session so
// another component (the inbound-message router) can attach to it.
type WorkingmanAgentSession struct {
	// ID is the ACP session id (the directory name under the sessions root).
	ID string
	// Dir is the session directory: <sessions-root>/<ID>.
	Dir string
	// SocketPath is the session's agent.sock, which acpchat.Attach connects to.
	SocketPath string
	// SandboxName is the sbx sandbox the agent runs in.
	SandboxName string
	// StartedAt is when the daemon began tracking this launch.
	StartedAt time.Time
}

// WorkingmanAgentSession returns the live workingman agent's session, or false
// when it is not running (not enabled, between restarts, or its launch failed).
// The session's socket may not be accepting connections yet right after a
// (re)launch — session.json's status becomes "running" once it is.
func (d *Daemon) WorkingmanAgentSession() (WorkingmanAgentSession, bool) {
	if d.workingman == nil {
		return WorkingmanAgentSession{}, false
	}
	d.sessionsMu.Lock()
	e, ok := d.sessions[workingmanSessionKey]
	d.sessionsMu.Unlock()
	if !ok {
		return WorkingmanAgentSession{}, false
	}
	out := WorkingmanAgentSession{
		ID:          e.sess.Name(),
		SandboxName: runner.WorkingmanAgentSandbox,
		StartedAt:   e.startedAt,
	}
	if d.runner != nil {
		if root, err := d.runner.ResolveSessionsRoot(); err == nil {
			store := session.Store{Root: root}
			out.Dir = store.Dir(out.ID)
			out.SocketPath = store.SocketPath(out.ID)
			if rec, err := store.Read(out.ID); err == nil {
				if rec.SocketPath != "" {
					out.SocketPath = rec.SocketPath
				}
				if rec.SandboxName != "" {
					out.SandboxName = rec.SandboxName
				}
			}
		}
	}
	return out, true
}

// defaultWorkingmanDir is the scratch directory used when the config names none:
// a sibling of the sessions root, away from the orch roots the daemon watches
// and from the read-only state dir.
func defaultWorkingmanDir(sessionsRoot string) string {
	return filepath.Join(filepath.Dir(sessionsRoot), workingmanScratchName)
}

// resolveWorkingman fills in the scratch directory and checks it against the
// watched roots; called from New once the options are applied. A runner-less
// daemon (observation-only) cannot run the agent, so there is nothing to
// resolve — Run audit-logs that.
func (d *Daemon) resolveWorkingman() error {
	w := d.workingman
	if w == nil || d.runner == nil {
		return nil
	}
	dir := w.cfg.Dir
	if dir == "" {
		root, err := d.runner.ResolveSessionsRoot()
		if err != nil {
			return fmt.Errorf("workingman agent: %w", err)
		}
		dir = defaultWorkingmanDir(root)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("workingman agent dir %q: %w", dir, err)
	}
	for _, r := range d.roots {
		rabs, err := filepath.Abs(r)
		if err != nil {
			continue
		}
		if within(abs, rabs) || within(rabs, abs) {
			return fmt.Errorf("workingman agent dir %s overlaps watched root %s: the agent's scratch writes would feed the daemon's own file watcher (and a root must stay read-only); pick a dir outside every --root", abs, rabs)
		}
	}
	w.dir = abs
	return nil
}

// workingmanPlan builds the launch Plan: the scratch dir as the one writable
// mount, and everything the agent observes mounted read-only.
func (d *Daemon) workingmanPlan() (runner.Plan, error) {
	w := d.workingman
	sessionsRoot, err := d.runner.ResolveSessionsRoot()
	if err != nil {
		return runner.Plan{}, err
	}
	obs := runner.Observe{
		Roots:        absAll(d.roots),
		SnapshotFile: d.snapshot.path,
		AuditLog:     w.cfg.AuditLog,
		SessionsRoot: sessionsRoot,
	}
	if obs.AuditLog == "" {
		obs.AuditLog = d.snapshot.info.AuditLog
	}
	var ro []string
	ro = append(ro, obs.Roots...)
	if obs.SnapshotFile != "" {
		// The snapshot is replaced by rename, so the whole directory is mounted:
		// a file bind-mount would keep pointing at the replaced inode.
		ro = append(ro, filepath.Dir(obs.SnapshotFile))
	}
	if obs.AuditLog != "" {
		ro = append(ro, filepath.Dir(obs.AuditLog))
	}
	ro = append(ro, sessionsRoot)
	ro = append(ro, w.cfg.ExtraReadOnlyMounts...)
	return runner.Plan{
		Kind:           agent.WorkingmanAgent,
		WorkingDir:     w.dir,
		SessionName:    runner.WorkingmanAgentSession,
		Persistent:     true,
		ReadOnlyMounts: ro,
		Observe:        obs,
		Policies:       w.cfg.Policies,
	}, nil
}

func absAll(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		out = append(out, p)
	}
	return out
}

// workingmanBackoff is the delay before relaunch number failures (1-based):
// lo, 2·lo, 4·lo, … capped at hi.
func workingmanBackoff(failures int, lo, hi time.Duration) time.Duration {
	if failures < 1 {
		failures = 1
	}
	d := lo
	for i := 1; i < failures; i++ {
		d *= 2
		if d >= hi || d <= 0 {
			return hi
		}
	}
	if d > hi {
		return hi
	}
	return d
}

// superviseWorkingmanAgent keeps the workingman agent running until ctx is
// cancelled. It runs on its own goroutine (started from Run), so nothing it
// does — a slow Start, a launch that keeps failing, a crash loop — can delay
// project dispatch.
//
// Each pass: if no session is tracked under the agent's key (a daemon restart
// that adopted a still-live one skips this), start one; then wait for it to end;
// then back off exponentially (cfg.MinBackoff doubling to cfg.MaxBackoff) before
// the next pass. A launch that survived cfg.StableAfter resets the backoff, so a
// healthy agent that dies once a day is relaunched promptly while a crash loop
// is throttled to one attempt per MaxBackoff.
func (d *Daemon) superviseWorkingmanAgent(ctx context.Context) {
	w := d.workingman
	lo, hi, stable := w.cfg.MinBackoff, w.cfg.MaxBackoff, w.cfg.StableAfter
	if lo <= 0 {
		lo = defaultWorkingmanMinBackoff
	}
	if hi <= 0 {
		hi = defaultWorkingmanMaxBackoff
	}
	if hi < lo {
		hi = lo
	}
	if stable <= 0 {
		stable = defaultWorkingmanStableAfter
	}

	failures := 0
	for {
		if ctx.Err() != nil {
			return
		}
		launchedAt := time.Now()
		if !d.hasSession(workingmanSessionKey) {
			if err := d.startWorkingmanAgent(); err != nil {
				d.audit.Log("workingman_agent_start_error", "err", err.Error())
				// A failed launch is the same failure class as an early exit.
				failures++
				if !d.sleepCtx(ctx, d.workingmanRetryDelay(failures, lo, hi)) {
					return
				}
				continue
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-w.ended:
		}
		if d.hasSession(workingmanSessionKey) {
			continue // spurious wake-up: a stale poke while the session is live
		}
		if time.Since(launchedAt) >= stable {
			failures = 0
		}
		failures++
		if !d.sleepCtx(ctx, d.workingmanRetryDelay(failures, lo, hi)) {
			return
		}
	}
}

// workingmanRetryDelay logs and returns the backoff before the next launch.
func (d *Daemon) workingmanRetryDelay(failures int, lo, hi time.Duration) time.Duration {
	delay := workingmanBackoff(failures, lo, hi)
	d.audit.Log("workingman_agent_restart_scheduled",
		"failures", fmt.Sprintf("%d", failures),
		"delay", delay.String(),
	)
	return delay
}

// sleepCtx waits for dur or ctx, reporting whether the full duration elapsed.
func (d *Daemon) sleepCtx(ctx context.Context, dur time.Duration) bool {
	t := time.NewTimer(dur)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// startWorkingmanAgent launches one workingman agent and tracks it. It returns
// an error only for a launch that did not happen; a session that is already
// tracked (lost a race with reconcile) counts as running.
func (d *Daemon) startWorkingmanAgent() error {
	w := d.workingman
	plan, err := d.workingmanPlan()
	if err != nil {
		return err
	}
	started, err := d.startSessionTracked(workingmanSessionKey, plan, func(waitErr error) {
		fields := []string{}
		if waitErr != nil {
			fields = append(fields, "err", waitErr.Error())
		}
		d.audit.Log("workingman_agent_ended", fields...)
		w.signalEnded()
	})
	if err != nil {
		return err
	}
	if started {
		d.audit.Log("workingman_agent_started",
			"session", d.sessionName(workingmanSessionKey),
			"sandbox", runner.WorkingmanAgentSandbox,
			"dir", w.dir,
		)
	}
	return nil
}

// beginWorkingmanAgent starts supervision from Run, or audit-logs why it cannot.
func (d *Daemon) beginWorkingmanAgent(ctx context.Context) {
	if d.workingman == nil {
		return
	}
	if d.runner == nil || !d.runner.UsesACP(agent.WorkingmanAgent) {
		d.audit.Log("workingman_agent_unavailable", "reason", "needs a runner with an ACP launcher (--acp-kit)")
		return
	}
	d.audit.Log("workingman_agent_enabled", "dir", d.workingman.dir)
	go d.superviseWorkingmanAgent(ctx)
}
