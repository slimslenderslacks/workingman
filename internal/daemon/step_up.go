package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/slimslenderslacks/work/internal/acpchat"
	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/policy"
	"github.com/slimslenderslacks/work/internal/postoffice"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/sbx"
	"github.com/slimslenderslacks/work/internal/session"
)

// Step-Up agent (docs/messaging-design.md §8).
//
// Step-Up is the always-on receiver of other agents' step-up requests ("I need
// network access to proxy.golang.org"). It is a post-office recipient: it takes
// requests with next_message, asks the operator with send_request and answers
// with reply. Its only MCP server is post-office, on a static gateway.
//
// It is supervised exactly like the workingman agent (superviseAgent), with two
// differences:
//
//   - its mounts: no orch roots at all; the snapshot, audit-log and sessions
//     directories read-only, and its request directory (StepUpDir) — requests/
//     writable, results/ read-only;
//   - its turns: nobody types into it, so the daemon drives it (driveStepUp).
//     The driver attaches to the session and sends a short prompt whenever
//     post-office holds work for it: a message it has not received yet, an
//     approval request of its own that has closed, or (as a reminder) a
//     request it holds but has not answered. Each turn drains the inbox and
//     ends, so the agent never sits in one endless turn.
//
// The daemon executor (step_up_exec.go) applies the changes it asks for in its
// request directory, after checking each one; it runs next to the driver.

const (
	// stepUpSessionKey is the session-map key of the Step-Up agent. Like the
	// workingman agent it belongs to no project.
	stepUpSessionKey = runner.StepUpSession

	// stepUpScratchName is the agent's scratch directory, a sibling of the
	// sessions root (like workingman-agent/).
	stepUpScratchName = "step-up-agent"

	// stepUpDirName is the request directory beside the sessions root
	// (~/.workingman/step-up, §8.3).
	stepUpDirName = "step-up"

	// StepUpRequestsDir and StepUpResultsDir are the request directory's
	// subdirectories: requests/ is written by the agent, results/ by the daemon.
	StepUpRequestsDir = "requests"
	StepUpResultsDir  = "results"
)

// Driver timing defaults.
const (
	// defaultStepUpPoll is how often the driver re-reads the post-office store
	// when no change notification arrives.
	defaultStepUpPoll = 10 * time.Second
	// defaultStepUpReminder is how often the agent is prompted again about
	// work it already knows of (a held request, a message it did not take).
	defaultStepUpReminder = 10 * time.Minute
	// defaultStepUpMinGap is the least time between two prompts.
	defaultStepUpMinGap = 5 * time.Second
	// defaultStepUpTurnTimeout bounds one prompted turn.
	defaultStepUpTurnTimeout = 30 * time.Minute
	// stepUpAttachRetry is the pause before attaching again after the session
	// went away or an attach failed.
	stepUpAttachRetry = 5 * time.Second
	// stepUpDiscoverTimeout is how long an attach waits for another client
	// (the TUI) to create the ACP session before creating one itself.
	stepUpDiscoverTimeout = 20 * time.Second
)

// StepUpPrompt is the turn the driver sends. It is self-contained, so it also
// serves as the opening prompt of a session nobody else has prompted.
const StepUpPrompt = "Read .orch/instructions.md and .orch/context.yaml if you have not yet in this conversation. " +
	"Then serve your post-office inbox as they describe: take every waiting message with next_message " +
	"(timeout_seconds: 0) until it returns status: empty, check your pending approval requests with " +
	"check_response, read the results of your change requests, reply where you can, and end your turn. " +
	"You will be prompted again when there is more."

// StepUpAgentConfig tunes WithStepUpAgent. The zero value is the production
// configuration.
type StepUpAgentConfig struct {
	// Dir is the agent's writable scratch directory, holding its .orch/
	// handoff files. Default: <sessions-root>/../step-up-agent. It must lie
	// outside every watched root.
	Dir string
	// StepUpDir is the request directory (§8.3). Default:
	// <sessions-root>/../step-up. It must lie outside every watched root.
	StepUpDir string
	// PostOfficeDir is the post-office store the driver reads. Default:
	// postoffice.DefaultDir(<sessions-root>).
	PostOfficeDir string
	// AuditLog is the audit log file the agent may read; its directory is
	// mounted read-only. Defaults to the audit log recorded by WithRuntimeInfo.
	AuditLog string
	// Policies are sbx policy rules applied to its sandbox right after creation.
	Policies []policy.Rule

	// MinBackoff / MaxBackoff / StableAfter override the restart backoff (the
	// workingman agent's defaults); non-positive values keep the defaults.
	MinBackoff  time.Duration
	MaxBackoff  time.Duration
	StableAfter time.Duration

	// Poll, Reminder, MinGap and TurnTimeout override the driver's timing (see
	// the defaults above); non-positive values keep the defaults.
	Poll        time.Duration
	Reminder    time.Duration
	MinGap      time.Duration
	TurnTimeout time.Duration

	// Executor settings (step_up_exec.go). Home is the operator's home
	// directory, protected from filesystem rules (default os.UserHomeDir).
	// SbxPath is the sbx binary (default the runner's, else "sbx"). Sbx runs
	// it; nil means exec (a test seam, as are Now and ExecPoll, the fallback
	// poll of requests/, default 2s).
	Home     string
	SbxPath  string
	Sbx      sbx.RunFunc
	Now      func() time.Time
	ExecPoll time.Duration

	// Attach joins the agent's session; nil means acpchat.Attach. Test seam.
	Attach func(ctx context.Context, ref string, opts acpchat.Options) (StepUpConversation, error)
}

// StepUpConversation is what the driver needs of an attached session;
// *acpchat.Conversation implements it.
type StepUpConversation interface {
	Ask(ctx context.Context, text string, opts ...acpchat.AskOption) (string, error)
	Events() <-chan acpchat.Event
	Created() bool
	Close() error
}

// stepUpAgent is the daemon's supervision state for the Step-Up agent.
type stepUpAgent struct {
	cfg       StepUpAgentConfig
	dir       string // resolved scratch directory
	stepUpDir string // resolved request directory
	poDir     string // resolved post-office store

	// ended is poked (non-blocking) whenever the tracked session ends, and
	// started whenever one is tracked (the driver attaches to it).
	ended   chan struct{}
	started chan struct{}
}

func (s *stepUpAgent) signalEnded() { poke(s.ended) }

func poke(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// WithStepUpAgent makes the daemon run the Step-Up agent (see agents.md §9):
// started when Run begins, relaunched with exponential backoff whenever it
// exits, prompted whenever post-office holds work for it, and stopped on
// shutdown. Needs a runner with an ACP launcher — without one it is skipped and
// audit-logged (step_up_agent_unavailable).
func WithStepUpAgent(cfg StepUpAgentConfig) Option {
	return func(d *Daemon) {
		d.stepUp = &stepUpAgent{cfg: cfg, ended: make(chan struct{}, 1), started: make(chan struct{}, 1)}
	}
}

// StepUpAgentSession describes the live Step-Up agent's ACP session.
type StepUpAgentSession struct {
	ID          string
	Dir         string
	SocketPath  string
	SandboxName string
	StartedAt   time.Time
}

// StepUpAgentSession returns the live Step-Up agent's session, or false when
// it is not running.
func (d *Daemon) StepUpAgentSession() (StepUpAgentSession, bool) {
	if d.stepUp == nil {
		return StepUpAgentSession{}, false
	}
	d.sessionsMu.Lock()
	e, ok := d.sessions[stepUpSessionKey]
	d.sessionsMu.Unlock()
	if !ok {
		return StepUpAgentSession{}, false
	}
	out := StepUpAgentSession{
		ID:          e.sess.Name(),
		SandboxName: runner.StepUpSandbox,
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

// resolveStepUp fills in the directories and checks them against the watched
// roots; called from New once the options are applied.
func (d *Daemon) resolveStepUp() error {
	s := d.stepUp
	if s == nil || d.runner == nil {
		return nil
	}
	root, err := d.runner.ResolveSessionsRoot()
	if err != nil {
		return fmt.Errorf("step-up agent: %w", err)
	}
	base := filepath.Dir(filepath.Clean(root))
	dirs := []struct {
		name string
		val  string
		def  string
		out  *string
	}{
		{"dir", s.cfg.Dir, filepath.Join(base, stepUpScratchName), &s.dir},
		{"request dir", s.cfg.StepUpDir, filepath.Join(base, stepUpDirName), &s.stepUpDir},
		{"post-office dir", s.cfg.PostOfficeDir, postoffice.DefaultDir(root), &s.poDir},
	}
	for _, x := range dirs {
		p := x.val
		if p == "" {
			p = x.def
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return fmt.Errorf("step-up agent %s %q: %w", x.name, p, err)
		}
		for _, r := range d.roots {
			rabs, err := filepath.Abs(r)
			if err != nil {
				continue
			}
			if within(abs, rabs) || within(rabs, abs) {
				return fmt.Errorf("step-up agent %s %s overlaps watched root %s: its writes would feed the daemon's own file watcher; pick a dir outside every --root", x.name, abs, rabs)
			}
		}
		*x.out = abs
	}
	return nil
}

// stepUpPlan builds the launch Plan: the scratch dir and requests/ writable;
// results/, the snapshot dir, the audit-log dir and the sessions root
// read-only; no orch roots; post-office as the only, static, MCP server.
func (d *Daemon) stepUpPlan() (runner.Plan, error) {
	s := d.stepUp
	sessionsRoot, err := d.runner.ResolveSessionsRoot()
	if err != nil {
		return runner.Plan{}, err
	}
	requests := filepath.Join(s.stepUpDir, StepUpRequestsDir)
	results := filepath.Join(s.stepUpDir, StepUpResultsDir)
	for _, dir := range []string{requests, results} {
		// Mounted, so they must exist before the sandbox is created.
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return runner.Plan{}, fmt.Errorf("step-up agent: %w", err)
		}
	}
	obs := runner.Observe{
		SnapshotFile: d.snapshot.path,
		AuditLog:     s.cfg.AuditLog,
		SessionsRoot: sessionsRoot,
		StepUpDir:    s.stepUpDir,
	}
	if obs.AuditLog == "" {
		obs.AuditLog = d.snapshot.info.AuditLog
	}
	var ro []string
	if obs.SnapshotFile != "" {
		// Replaced by rename, so the directory is mounted (see workingmanPlan).
		ro = append(ro, filepath.Dir(obs.SnapshotFile))
	}
	if obs.AuditLog != "" {
		ro = append(ro, filepath.Dir(obs.AuditLog))
	}
	ro = append(ro, sessionsRoot, results)
	return runner.Plan{
		Kind:           agent.StepUpAgent,
		WorkingDir:     s.dir,
		SessionName:    runner.StepUpSession,
		Persistent:     true,
		StaticMCPs:     runner.StepUpMCPs,
		WritableMounts: []string{requests},
		ReadOnlyMounts: ro,
		Observe:        obs,
		Policies:       s.cfg.Policies,
	}, nil
}

// startStepUpAgent launches one Step-Up agent and tracks it.
func (d *Daemon) startStepUpAgent() error {
	s := d.stepUp
	plan, err := d.stepUpPlan()
	if err != nil {
		return err
	}
	started, err := d.startSessionTracked(stepUpSessionKey, plan, func(waitErr error) {
		fields := []string{}
		if waitErr != nil {
			fields = append(fields, "err", waitErr.Error())
		}
		d.audit.Log("step_up_agent_ended", fields...)
		s.signalEnded()
	})
	if err != nil {
		return err
	}
	poke(s.started)
	if started {
		d.audit.Log("step_up_agent_started",
			"session", d.sessionName(stepUpSessionKey),
			"sandbox", runner.StepUpSandbox,
			"dir", s.dir,
			"step_up_dir", s.stepUpDir,
			"mcp_servers", strings.Join(d.runner.MCPServersFor(plan), ","),
			"mcp_dynamic", strconv.FormatBool(d.runner.MCPGatewayDynamic(agent.StepUpAgent)),
		)
	}
	return nil
}

// beginStepUpAgent starts supervision and the driver from Run, or audit-logs
// why it cannot.
func (d *Daemon) beginStepUpAgent(ctx context.Context) {
	s := d.stepUp
	if s == nil {
		return
	}
	if d.runner == nil || !d.runner.UsesACP(agent.StepUpAgent) {
		d.audit.Log("step_up_agent_unavailable", "reason", "needs a runner with an ACP launcher (--acp-kit)")
		return
	}
	if strings.TrimSpace(d.runner.MCPGatewayURL) == "" {
		// Without a gateway the agent has no post-office, so nothing to do.
		d.audit.Log("step_up_agent_unavailable", "reason", "needs an MCP gateway (--mcp-gateway-url) to reach post-office")
		return
	}
	d.audit.Log("step_up_agent_enabled", "dir", s.dir, "step_up_dir", s.stepUpDir, "post_office_dir", s.poDir)
	if exec, err := d.newStepUpExecutor(); err != nil {
		d.audit.Log("step_up_exec_unavailable", "err", err.Error())
	} else {
		go exec.loop(ctx, s.cfg.ExecPoll)
	}
	go d.superviseAgent(ctx, agentSupervision{
		key:         stepUpSessionKey,
		event:       "step_up_agent",
		start:       d.startStepUpAgent,
		ended:       s.ended,
		minBackoff:  s.cfg.MinBackoff,
		maxBackoff:  s.cfg.MaxBackoff,
		stableAfter: s.cfg.StableAfter,
	})
	go d.driveStepUp(ctx)
}

// --- driver ------------------------------------------------------------------

// stepUpWork is what post-office holds for the agent.
type stepUpWork struct {
	unreceived []string // open, addressed to step-up, not taken yet
	held       []string // open, addressed to step-up, taken but not answered
	// lastClosed is the newest of the agent's own requests (its approval
	// requests to the operator) to have closed: its id and closing time.
	lastClosed string
}

func (w stepUpWork) pending() bool { return len(w.unreceived) > 0 || len(w.held) > 0 }

// stepUpSeen is the work the agent has been prompted about.
type stepUpSeen struct {
	ids    map[string]bool
	closed string
}

// news reports whether w holds something the agent has not been prompted
// about: a message it has not taken, or a newly closed request of its own.
// Work going away (a message taken, a request answered) is not news.
func (s stepUpSeen) news(w stepUpWork) bool {
	for _, id := range w.unreceived {
		if !s.ids[id] {
			return true
		}
	}
	return w.lastClosed != "" && w.lastClosed != s.closed
}

func (s *stepUpSeen) mark(w stepUpWork) {
	if s.ids == nil {
		s.ids = map[string]bool{}
	}
	for _, id := range w.unreceived {
		s.ids[id] = true
	}
	s.closed = w.lastClosed
}

// stepUpWorkIn reads the store. Expired messages are left out: post-office
// closes them, and they need nothing from the agent.
func stepUpWorkIn(store *postoffice.Store, now time.Time) (stepUpWork, error) {
	var w stepUpWork
	msgs, err := store.ListOpen()
	if err != nil {
		return w, err
	}
	for _, m := range msgs {
		if m.Recipient != runner.StepUpSandbox || m.Expired(now) {
			continue
		}
		if _, claimed, _ := store.Claimed(m.ID); claimed {
			continue
		}
		if m.ReceivedAt == nil {
			w.unreceived = append(w.unreceived, m.ID)
		} else {
			w.held = append(w.held, m.ID)
		}
	}
	done, err := store.ListDone(now.Add(-24 * time.Hour))
	if err != nil {
		return w, err
	}
	var last postoffice.DoneRecord
	for _, r := range done {
		if r.From.Sandbox != runner.StepUpSandbox || r.Kind != postoffice.KindRequest {
			continue
		}
		if last.ID == "" || r.Resolution.At.After(last.Resolution.At) {
			last = r
		}
	}
	if last.ID != "" {
		w.lastClosed = last.ID + "@" + last.Resolution.At.Format(time.RFC3339Nano)
	}
	return w, nil
}

// driveStepUp gives the Step-Up agent its turns until ctx is done: it attaches
// to whichever session is live, and re-attaches after the session goes away.
func (d *Daemon) driveStepUp(ctx context.Context) {
	s := d.stepUp
	store := &postoffice.Store{Root: s.poDir}
	for ctx.Err() == nil {
		sess, ok := d.StepUpAgentSession()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-s.started:
			case <-time.After(stepUpAttachRetry):
			}
			continue
		}
		if err := d.driveStepUpSession(ctx, store, sess); err != nil && ctx.Err() == nil {
			d.audit.Log("step_up_driver_detached", "session", sess.ID, "err", err.Error())
		}
		if !d.sleepCtx(ctx, stepUpAttachRetry) {
			return
		}
	}
}

// driveStepUpSession attaches to one session and prompts it whenever there is
// work, until the conversation ends.
func (d *Daemon) driveStepUpSession(ctx context.Context, store *postoffice.Store, sess StepUpAgentSession) error {
	s := d.stepUp
	attach := s.cfg.Attach
	if attach == nil {
		attach = func(ctx context.Context, ref string, opts acpchat.Options) (StepUpConversation, error) {
			return acpchat.Attach(ctx, ref, opts)
		}
	}
	ref := sess.Dir
	if ref == "" {
		ref = sess.ID
	}
	root, _ := d.runner.ResolveSessionsRoot()
	conv, err := attach(ctx, ref, acpchat.Options{
		Root: root,
		// The agent's own session: create it when no TUI does, so a headless
		// daemon still gets a working Step-Up.
		CreateIfMissing: true,
		DiscoverTimeout: stepUpDiscoverTimeout,
		// bypassPermissions is set at session/new; nothing should ask.
		OnPermission: func(context.Context, acpchat.PermissionRequest) acpchat.PermissionDecision {
			return acpchat.PermissionDecision{}
		},
	})
	if err != nil {
		return err
	}
	defer conv.Close()
	d.audit.Log("step_up_driver_attached", "session", sess.ID, "created", strconv.FormatBool(conv.Created()))

	dur := func(v, def time.Duration) time.Duration {
		if v > 0 {
			return v
		}
		return def
	}
	poll := dur(s.cfg.Poll, defaultStepUpPoll)
	reminder := dur(s.cfg.Reminder, defaultStepUpReminder)
	minGap := dur(s.cfg.MinGap, defaultStepUpMinGap)
	turnTimeout := dur(s.cfg.TurnTimeout, defaultStepUpTurnTimeout)

	events := conv.Events()
	changes := store.Watch(ctx, poll, postoffice.DirInbox, postoffice.DirDone)
	tick := time.NewTicker(poll)
	defer tick.Stop()

	// foreignBusy is true while another client (the TUI's opening prompt, a
	// human typing in its tab) has a turn running: prompting then would queue
	// behind it at best.
	foreignBusy := false
	var seen stepUpSeen
	var lastPrompt time.Time
	prime := conv.Created() // nobody else will give this session its opening prompt

	for ctx.Err() == nil {
		if foreignBusy {
			prime = false // the other client's turn reads the instructions
		}
		if !foreignBusy && time.Since(lastPrompt) >= minGap {
			w, err := stepUpWorkIn(store, time.Now())
			if err != nil {
				d.audit.Log("step_up_driver_error", "err", err.Error())
			}
			due := prime ||
				(err == nil && seen.news(w)) ||
				(err == nil && w.pending() && time.Since(lastPrompt) >= reminder)
			if due {
				// Marked before the turn: what arrives during it, and is not
				// taken in it, is still news afterwards.
				seen.mark(w)
				lastPrompt, prime = time.Now(), false
				d.audit.Log("step_up_prompted",
					"unreceived", strconv.Itoa(len(w.unreceived)),
					"held", strconv.Itoa(len(w.held)))
				tctx, cancel := context.WithTimeout(ctx, turnTimeout)
				_, err := conv.Ask(tctx, StepUpPrompt)
				cancel()
				switch {
				case errors.Is(err, acpchat.ErrSessionGone), errors.Is(err, acpchat.ErrClosed):
					return err
				case err != nil && ctx.Err() == nil:
					d.audit.Log("step_up_turn_error", "err", err.Error())
				}
				continue // look again at once: the work may have changed during the turn
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-events:
			if !ok {
				return acpchat.ErrClosed
			}
			switch {
			case ev.Kind == acpchat.EventGone:
				return ev.Err
			case ev.Own:
			case ev.Kind == acpchat.EventText:
				foreignBusy = true
			case ev.Kind == acpchat.EventTurnEnd:
				foreignBusy = false
			case ev.Kind == acpchat.EventSessionChanged, ev.Kind == acpchat.EventReconnecting:
				// A fresh ACP session (its creator gives it the opening prompt),
				// or a dropped socket that may have cost us the turn's end.
				foreignBusy = false
			}
		case <-changes:
		case <-tick.C:
		}
	}
	return nil
}
