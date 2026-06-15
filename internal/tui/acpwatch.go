package tui

import (
	"context"
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
	return watchACPSessions(ctx, root, interval, realDialer, defaultACPPrompt)
}

// watchACPSessions is the testable core of WatchACPSessions, taking the dialer
// and opening prompt as parameters.
//
// It polls the store on interval. A session in StatusRunning that isn't already
// being watched gets a per-session goroutine (watchOneACPSession). A session
// that has disappeared from the listing — its directory cleaned up — has its
// watcher cancelled and an acpTabRemoved emitted so the tab goes away.
func watchACPSessions(ctx context.Context, root string, interval time.Duration, dial acpDialer, prompt string) <-chan acpTabEvent {
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
					watchOneACPSession(wctx, out, dial, s, prompt)
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
// tab, dials the socket, pumps every acpclient.Event back as an acpTabStream,
// completes the ACP handshake, and sends the opening prompt. It then blocks until
// ctx is cancelled (the session's directory was reaped) so the connection — and
// the event pump — stay alive while the agent streams.
//
// A dial failure is surfaced as a synthetic StateDisconnected event so the tab
// shows the session as dead (the hallmark of a socket whose sandbox is gone). A
// handshake failure needs no synthetic event: acpclient already emits the
// terminal event over Events(), which the pump forwards.
func watchOneACPSession(ctx context.Context, out chan<- acpTabEvent, dial acpDialer, s session.Session, prompt string) {
	emitACP(ctx, out, acpTabEvent{kind: acpTabAdded, id: s.ID, title: acpTabTitle(s)})

	conn, err := dial(ctx, s.SocketPath)
	if err != nil {
		emitACP(ctx, out, acpTabEvent{
			kind: acpTabStream,
			id:   s.ID,
			ev:   acpclient.Event{State: acpclient.StateDisconnected, Err: err},
		})
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

	cwd := ""
	if len(s.Workspaces) > 0 {
		cwd = s.Workspaces[0]
	}
	if err := conn.Connect(ctx, cwd); err != nil {
		return // terminal event already forwarded by the pump
	}

	if prompt != "" {
		emitACP(ctx, out, acpTabEvent{kind: acpTabPrompt, id: s.ID, text: prompt})
		// Prompt blocks until the turn completes; its streamed chunks arrive via
		// the pump. We ignore the stop reason — the StateCompleted event the pump
		// forwards already updates the tab.
		_, _ = conn.Prompt(ctx, prompt)
	}

	<-ctx.Done()
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
