package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/slimslenderslacks/work/internal/acpclient"
	"github.com/slimslenderslacks/work/internal/session"
)

// acpwatch.go is the live wiring behind the ACP tab view: it discovers sessions
// by polling the on-disk session.Store, dials each one's agent.sock over ACP,
// drives the opening prompt, and streams the lifecycle/output events back to the
// TUI as acpTabEvents. The pure tab state in acptabs.go consumes those events;
// keeping discovery + transport here means the model never touches a socket.

// acpEventKind tags an acpTabEvent so the model knows which mutation to apply.
type acpEventKind int

const (
	// acpTabAdded creates a tab — a session just appeared and is being watched.
	acpTabAdded acpEventKind = iota
	// acpTabPrompt records a prompt the TUI sent to the session.
	acpTabPrompt
	// acpTabStream carries one acpclient.Event (lifecycle transition or streamed
	// assistant chunk) for a session.
	acpTabStream
	// acpTabRemoved drops a tab — the session's directory is gone (cleaned up).
	acpTabRemoved
)

// acpTabEvent is one mutation to the tab view, delivered over the watcher's
// channel and handled in model.Update. Only the fields relevant to kind are set.
type acpTabEvent struct {
	kind  acpEventKind
	id    string
	title string
	text  string          // acpTabPrompt: the prompt text
	ev    acpclient.Event // acpTabStream: the lifecycle/stream event
}

// acpConn is the subset of *acpclient.Client the watcher drives. It is an
// interface so tests can substitute an in-memory fake without a real socket.
type acpConn interface {
	Connect(ctx context.Context, cwd string) error
	Prompt(ctx context.Context, text string) (stopReason string, err error)
	Events() <-chan acpclient.Event
	Close() error
}

// acpDialer opens a connection to a session's agent.sock. Production uses
// realDialer (acpclient.Dial); tests inject a fake.
type acpDialer func(ctx context.Context, socketPath string) (acpConn, error)

// sandboxProbe reports whether the named sbx sandbox still exists. It is how the
// watcher verifies a discovered session's backing sandbox is alive before
// trusting its socket: a crashed wrapper can leave an agent.sock behind that
// still accepts a connection (or hangs without ever answering) even though
// nothing is bridging it to a live agent, and there is no per-dial timeout to
// rescue a watcher that blocks on such a socket. A definitive "gone" lets the
// watcher reap the stale session without dialing at all.
//
// The boolean is only meaningful when err is nil: alive=false with a nil error
// means the sandbox is authoritatively absent. A non-nil error means the probe
// itself could not run (sbx missing, transient failure) and is inconclusive —
// the caller must NOT treat the session as dead on that basis and falls back to
// letting the connection's own behavior decide.
type sandboxProbe func(ctx context.Context, sandboxName string) (alive bool, err error)

// sbxSandboxProbe is the production sandboxProbe. It lists the sandboxes via
// `sbx ls --json` (the same stable read interface the wrapper and runner use)
// and reports whether one named sandboxName exists. A query or decode failure is
// returned as an error so the caller treats it as inconclusive rather than as
// "gone".
func sbxSandboxProbe(ctx context.Context, sandboxName string) (bool, error) {
	out, err := exec.CommandContext(ctx, "sbx", "ls", "--json").Output()
	if err != nil {
		return false, err
	}
	var data struct {
		Sandboxes []struct {
			Name string `json:"name"`
		} `json:"sandboxes"`
	}
	if err := json.Unmarshal(out, &data); err != nil {
		return false, err
	}
	for _, s := range data.Sandboxes {
		if s.Name == sandboxName {
			return true, nil
		}
	}
	return false, nil
}

// realDialer is the production dialer. It wraps acpclient.Dial so a dial failure
// yields a genuinely nil interface (returning a typed-nil *Client would make the
// interface non-nil and defeat the caller's err check).
func realDialer(ctx context.Context, socketPath string) (acpConn, error) {
	c, err := acpclient.Dial(ctx, socketPath)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// defaultACPPrompt is the opening turn the TUI sends to a freshly-connected ACP
// session — the same instruction the legacy tmux path baked into claude's argv.
// The sandboxed agent reads .orch/instructions.md + context.yaml from its mounted
// workspace and proceeds. Recording it as a prompt entry is what makes the tab's
// "prompts sent" half real.
const defaultACPPrompt = "Read .orch/instructions.md and .orch/context.yaml, then follow the instructions."

// WatchACPSessions starts a background watcher over the session store rooted at
// root and returns a channel of tab mutations for the TUI to consume. The
// channel is closed when ctx is cancelled. interval <= 0 defaults to 500ms.
func WatchACPSessions(ctx context.Context, root string, interval time.Duration) <-chan acpTabEvent {
	return watchACPSessions(ctx, root, interval, realDialer, sbxSandboxProbe, defaultACPPrompt)
}

// watchACPSessions is the testable core of WatchACPSessions, taking the dialer
// and opening prompt as parameters.
//
// It polls the store on interval. A session in StatusRunning that isn't already
// being watched gets a per-session goroutine (watchOneACPSession). A session
// that has disappeared from the listing — its directory cleaned up — has its
// watcher cancelled and an acpTabRemoved emitted so the tab goes away.
func watchACPSessions(ctx context.Context, root string, interval time.Duration, dial acpDialer, probe sandboxProbe, prompt string) <-chan acpTabEvent {
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	out := make(chan acpTabEvent)
	go func() {
		defer close(out)
		store, err := session.NewStore(root)
		if err != nil {
			return
		}

		active := map[string]context.CancelFunc{}
		var wg sync.WaitGroup
		defer func() {
			for _, cancel := range active {
				cancel()
			}
			wg.Wait()
		}()

		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			sessions, _ := store.List()
			seen := make(map[string]bool, len(sessions))
			for _, s := range sessions {
				seen[s.ID] = true
				if _, watching := active[s.ID]; watching {
					continue
				}
				if s.Status != session.StatusRunning {
					// Not yet live (starting) or already ended: nothing to watch.
					// A later poll picks it up once it flips to running.
					continue
				}
				wctx, cancel := context.WithCancel(ctx)
				active[s.ID] = cancel
				wg.Add(1)
				go func(s session.Session) {
					defer wg.Done()
					watchOneACPSession(wctx, out, dial, probe, store, s, prompt)
				}(s)
			}
			// Reap watchers whose session directory is gone.
			for id, cancel := range active {
				if !seen[id] {
					cancel()
					delete(active, id)
					emitACP(ctx, out, acpTabEvent{kind: acpTabRemoved, id: id})
				}
			}

			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return out
}

// watchOneACPSession watches a single session for its lifetime: it announces the
// tab, replays any prior context, dials the socket, pumps every acpclient.Event
// back as an acpTabStream, completes the ACP handshake, and — for a brand-new
// session only — sends the opening prompt. It then blocks until ctx is cancelled
// (the session's directory was reaped) so the connection and the event pump stay
// alive while the agent streams.
//
// Reconnect is the crux: when the TUI restarts it rediscovers ongoing sessions
// here. Such a session has already been prompted (PromptCount>0), so we must NOT
// re-send the opening prompt — that would restart the agent and duplicate the
// turn — and we replay its recorded scrollback so the tab comes back with its
// prior prompts/output.
//
// A dead session is distinguished from a live one and routed to cleanup three
// ways, in order:
//
//   - The backing sandbox is authoritatively gone (probe). A discovered session
//     whose sbx sandbox no longer exists is stale regardless of what its socket
//     file looks like — a crashed wrapper can leave an agent.sock that still
//     accepts (or hangs without answering), and there is no per-dial timeout to
//     rescue us from blocking on it. So we check liveness first and, when gone,
//     skip the dial entirely, mark the tab dead, and reap the directory.
//   - Socket missing/refused (dial fails) — a stale socket from a wrapper that
//     crashed while its sandbox may even still be up; either way it can't be
//     bridged, so the directory is reaped.
//   - A sandbox that vanishes mid-handshake so the bridge closes the connection.
//
// The first two emit a synthetic StateDisconnected to mark the tab dead; the
// third needs none — acpclient already emits the terminal event over Events(),
// which the pump forwards.
func watchOneACPSession(ctx context.Context, out chan<- acpTabEvent, dial acpDialer, probe sandboxProbe, store session.Store, s session.Session, prompt string) {
	emitACP(ctx, out, acpTabEvent{kind: acpTabAdded, id: s.ID, title: acpTabTitle(s)})

	// Verify the backing sandbox still exists before trusting the socket. We only
	// reach here for StatusRunning sessions, whose sandbox provably existed when
	// the wrapper flipped them to running, so a "gone" verdict is a genuine
	// after-the-fact teardown (sandbox removed, wrapper crashed) and not the
	// brief starting-window race where the wrapper writes the record before
	// creating the sandbox. An inconclusive probe (error) is ignored so a flaky
	// `sbx ls` never reaps a live session — the connection paths below still
	// catch a truly dead socket.
	if probe != nil && s.SandboxName != "" {
		if alive, err := probe(ctx, s.SandboxName); err == nil && !alive {
			emitACP(ctx, out, acpTabEvent{
				kind: acpTabStream,
				id:   s.ID,
				ev:   acpclient.Event{State: acpclient.StateDisconnected},
			})
			cleanupDeadSession(ctx, out, store, s.ID)
			return
		}
	}

	conn, err := dial(ctx, s.SocketPath)
	if err != nil {
		// Socket missing or connection refused: the backing sandbox is gone. This
		// is a dead session discovered on (re)start — mark it dead and reclaim the
		// orphaned directory.
		emitACP(ctx, out, acpTabEvent{
			kind: acpTabStream,
			id:   s.ID,
			ev:   acpclient.Event{State: acpclient.StateDisconnected, Err: err},
		})
		cleanupDeadSession(ctx, out, store, s.ID)
		return
	}

	// Pump events to the model. The pump owns the Events() channel; it drains to
	// completion when the connection closes. We wait for it before returning so a
	// late emit can never race the parent's close of out.
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		for ev := range conn.Events() {
			emitACP(ctx, out, acpTabEvent{kind: acpTabStream, id: s.ID, ev: ev})
		}
	}()
	defer func() {
		conn.Close()
		<-pumpDone
	}()

	// Rebuild the tab's prior context before the live stream so reconnected
	// sessions come back with their history above any new output.
	replayPriorContext(ctx, out, s, prompt)

	cwd := ""
	if len(s.Workspaces) > 0 {
		cwd = s.Workspaces[0]
	}
	if err := conn.Connect(ctx, cwd); err != nil {
		// Connected to the socket but the handshake never completed — the bridge
		// closed the connection because the sandbox is gone. Dead session: clean up.
		cleanupDeadSession(ctx, out, store, s.ID)
		return // terminal event already forwarded by the pump
	}

	// Drive the opening prompt only for a brand-new session. A reconnected session
	// (PromptCount>0) was prompted in a prior TUI run; we already replayed it and
	// re-sending would restart the agent.
	if prompt != "" && s.PromptCount == 0 {
		emitACP(ctx, out, acpTabEvent{kind: acpTabPrompt, id: s.ID, text: prompt})
		// Prompt blocks until the turn completes; its streamed chunks arrive via
		// the pump. We ignore the stop reason — the StateCompleted event the pump
		// forwards already updates the tab.
		if _, err := conn.Prompt(ctx, prompt); err == nil {
			// Record that the opening prompt was sent so a future restart reconnects
			// (replays) instead of re-prompting.
			markPrompted(store, s.ID)
			// Orch's non-interactive agents (planning/task/commit) are
			// single-turn: the opening prompt finishing IS the agent's exit
			// signal. Return so the deferred conn.Close() fires, the sandboxed
			// claude-acp-client sees EOF on stdio and exits, acp-wrapper
			// returns, and the daemon's session_ended callback dispatches the
			// next stage (task-after-planning, commit-after-task, etc.). The
			// pump forwards the trailing StateDisconnected before pumpDone
			// closes, so the tab still transitions through completed → dead.
			return
		}
	}

	<-ctx.Done()
}

// replayPriorContext rebuilds a reconnected session's transcript from on-disk
// state so its tab returns with the context it had before the TUI restarted. A
// brand-new session (PromptCount==0, empty log) replays nothing. For a session
// that was already prompted, it re-emits the opening prompt block and then
// replays the recorded ACP stream log as streamed output events — "prior context
// (prompts/streamed output as available)". A missing or unreadable log is not an
// error: reconnection still works, it just lacks scrollback.
func replayPriorContext(ctx context.Context, out chan<- acpTabEvent, s session.Session, prompt string) {
	if s.PromptCount > 0 && prompt != "" {
		emitACP(ctx, out, acpTabEvent{kind: acpTabPrompt, id: s.ID, text: prompt})
	}
	if s.LogPath == "" {
		return
	}
	f, err := os.Open(s.LogPath)
	if err != nil {
		return
	}
	defer f.Close()

	br := bufio.NewReader(f)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if ev, ok := acpclient.ParseStreamFrame(line); ok {
				emitACP(ctx, out, acpTabEvent{kind: acpTabStream, id: s.ID, ev: ev})
			}
		}
		if err != nil {
			return
		}
	}
}

// markPrompted records that a session's opening prompt has been sent by bumping
// PromptCount in session.json, so a later TUI restart reconnects-and-replays
// rather than re-prompting. It is best-effort: a read/write failure just means a
// future restart might re-send the opening prompt, which is recoverable, so we
// never fail the live session over it. The acp-wrapper writes session.json only
// up to "running" (before the TUI ever prompts) and then not again until it
// removes the directory on exit, so this read-modify-write does not race a
// concurrent wrapper update.
func markPrompted(store session.Store, id string) {
	rec, err := store.Read(id)
	if err != nil {
		return
	}
	if rec.PromptCount > 0 {
		return
	}
	rec.PromptCount = 1
	rec.UpdatedAt = time.Now()
	_ = store.Write(rec)
}

// cleanupDeadSession reclaims a session whose sandbox is gone: it removes the
// orphaned directory (session.json, socket, log) and emits acpTabRemoved so the
// dead tab disappears. Removal is best-effort; the parent watcher's reaper also
// emits acpTabRemoved once the directory is gone, and the model's remove is
// idempotent, so a duplicate is harmless.
func cleanupDeadSession(ctx context.Context, out chan<- acpTabEvent, store session.Store, id string) {
	_ = store.Remove(id)
	emitACP(ctx, out, acpTabEvent{kind: acpTabRemoved, id: id})
}

// acpTabTitle is the label shown on a session's tab. The session id already
// encodes the kind and task/branch (e.g. "task-first", "planning-feat-x"), which
// is the most useful at-a-glance handle, so it is used directly.
func acpTabTitle(s session.Session) string {
	return s.ID
}

// emitACP sends ev on out unless ctx is done, so a watcher shutting down never
// blocks on (or sends past the close of) the channel.
func emitACP(ctx context.Context, out chan<- acpTabEvent, ev acpTabEvent) {
	select {
	case out <- ev:
	case <-ctx.Done():
	}
}
