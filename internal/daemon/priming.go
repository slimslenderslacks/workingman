package daemon

import (
	"context"
	"errors"
	"io/fs"
	"strconv"
	"time"

	"github.com/slimslenderslacks/work/internal/acpchat"
	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/session"
)

// Session priming (docs/proposals/workingman-gui.md §A.5.4, phase P.1).
//
// An ACP session launches with its handoff files written but no prompt: some
// client has to send the opening turn. That used to be the integrated TUI's
// watcher alone, so a --headless daemon launched sessions that then sat idle.
// The daemon now primes every ACP session it tracks, whether it launched it or
// adopted it from a previous run, next to driveStepUpSession, which primes the
// Step-Up agent the same way:
//
//  1. wait until the wrapper marks session.json running;
//  2. attach with acpchat, creating the ACP session when no other client has;
//  3. claim the opening prompt with session.Store.ClaimOpeningPrompt, a
//     compare-and-set of prompt_count from 0 to 1. A TUI watcher attached to
//     the same session claims the same way, so exactly one of them sends it;
//  4. send it, and disconnect when the turn ends. For a one-shot kind, the
//     last client leaving is what ends the session (the wrapper runs
//     --exit-when-empty), which the session-end handling then routes. A
//     persistent kind (the wolf) stays up for the TUI and channels to join.
//
// The workingman, Step-Up and sleepy-head agents are not primed here: their
// own drivers (the router, driveStepUp) give them their turns.

// Priming timing.
const (
	// defaultPrimePoll is how often the primer re-reads session.json while the
	// wrapper is still starting the sandbox.
	defaultPrimePoll = 250 * time.Millisecond
	// primeDiscoverTimeout is how long the attach waits for another client
	// (the TUI) to create the ACP session before creating it itself.
	primeDiscoverTimeout = 5 * time.Second
	// primeConnectTimeout bounds the attach's own connect retries. The primer
	// only attaches once the session is running, so this covers a socket that
	// refuses for a moment, not a cold sandbox boot.
	primeConnectTimeout = 2 * time.Minute
)

// PrimeConversation is what the primer needs of an attached session;
// *acpchat.Conversation implements it.
type PrimeConversation interface {
	Ask(ctx context.Context, text string, opts ...acpchat.AskOption) (string, error)
	Created() bool
	Close() error
}

// primeAttachFunc joins a session; acpchat.Attach in production.
type primeAttachFunc func(ctx context.Context, ref string, opts acpchat.Options) (PrimeConversation, error)

func acpchatPrimeAttach(ctx context.Context, ref string, opts acpchat.Options) (PrimeConversation, error) {
	return acpchat.Attach(ctx, ref, opts)
}

// primes reports whether the daemon gives sessions of this kind their opening
// prompt: the ACP-backed project-lifecycle kinds (detachable lists exactly
// those), not the daemon's own agents.
func (d *Daemon) primes(kind agent.Kind) bool {
	return d.detachable(kind)
}

// primeSession starts priming the session just tracked under key, when its
// kind is primed and the daemon is running. It returns at once; the work runs
// on its own goroutine until the turn ends, the claim is lost, or the session
// ends. The goroutine counts as a handler, so shutdown waits for it; it
// returns promptly once d.ctx is done.
func (d *Daemon) primeSession(key string, sess agent.Session, kind agent.Kind) {
	if d.ctx == nil || !d.primes(kind) {
		return
	}
	root, err := d.runner.ResolveSessionsRoot()
	if err != nil {
		d.audit.Log("session_prime_error", "key", key, "session", sess.Name(), "err", err.Error())
		return
	}
	if !d.beginHandler() {
		return
	}
	go func() {
		defer d.handlers.Done()
		d.prime(key, sess, kind, session.Store{Root: root})
	}()
}

// prime is primeSession's goroutine.
func (d *Daemon) prime(key string, sess agent.Session, kind agent.Kind, store session.Store) {
	id := sess.Name()
	fields := func(kv ...string) []string {
		return append([]string{"key", key, "session", id, "kind", kind.String()}, kv...)
	}
	rec, ok := d.awaitRunning(key, sess, kind, store)
	if !ok {
		return
	}
	if rec.PromptCount > 0 {
		// Prompted already: by a TUI, or before a daemon restart.
		return
	}

	attach := d.primeAttach
	if attach == nil {
		attach = acpchatPrimeAttach
	}
	conv, err := attach(d.ctx, store.Dir(id), acpchat.Options{
		Root:            store.Root,
		CreateIfMissing: true,
		DiscoverTimeout: orDefaultDuration(d.primeDiscover, primeDiscoverTimeout),
		ConnectTimeout:  primeConnectTimeout,
		// bypassPermissions is set at session/new; nothing should ask.
		OnPermission: func(context.Context, acpchat.PermissionRequest) acpchat.PermissionDecision {
			return acpchat.PermissionDecision{}
		},
	})
	if err != nil {
		if d.ctx.Err() == nil && d.tracks(key, sess) {
			d.audit.Log("session_prime_error", fields("err", err.Error())...)
		}
		return
	}
	defer conv.Close()

	won, err := store.ClaimOpeningPrompt(id)
	switch {
	case err != nil:
		if !errors.Is(err, fs.ErrNotExist) {
			d.audit.Log("session_prime_error", fields("err", err.Error())...)
		}
		return
	case !won:
		// Another client (the TUI) claimed it first and is sending it; it also
		// owns the disconnect at the end of the turn.
		d.audit.Log("session_prime_skipped", fields("reason", "claimed")...)
		return
	}
	d.audit.Log("session_primed", fields("created", strconv.FormatBool(conv.Created()))...)

	// The turn runs under its own context, not d.ctx: a daemon shutting down
	// closes the conversation instead, which detaches without the
	// session/cancel an expired context would send. A persistent session's
	// turn then carries on; a one-shot session ends when its last client
	// leaves, as it always did when the TUI holding it exited.
	stop := context.AfterFunc(d.ctx, func() { _ = conv.Close() })
	defer stop()
	_, err = conv.Ask(context.Background(), runner.OpeningPrompt)
	switch {
	case err == nil, errors.Is(err, acpchat.ErrRefused):
		d.audit.Log("session_prime_turn_ended", fields()...)
	case d.ctx.Err() != nil:
	case errors.Is(err, acpchat.ErrSessionGone), errors.Is(err, acpchat.ErrClosed):
		d.audit.Log("session_prime_turn_ended", fields("reason", "session gone")...)
	default:
		d.audit.Log("session_prime_turn_error", fields("err", err.Error())...)
	}
	// The deferred Close is the end-of-turn disconnect.
}

// awaitRunning polls session.json until the wrapper marks the session
// running, and returns the record. It gives up (false) when the session ends
// first, fails to start, or is no longer the one tracked under key, or when
// the daemon stops.
func (d *Daemon) awaitRunning(key string, sess agent.Session, kind agent.Kind, store session.Store) (session.Session, bool) {
	poll := orDefaultDuration(d.primePoll, defaultPrimePoll)
	id := sess.Name()
	for {
		rec, err := store.Read(id)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return session.Session{}, false
		case err != nil:
			// Unreadable for a moment; try again.
		case rec.Kind != "" && rec.Kind != kind.String():
			// Not the session this daemon launched under that name.
			return session.Session{}, false
		case rec.Status == session.StatusRunning:
			return rec, true
		case rec.Status != session.StatusStarting:
			return session.Session{}, false
		}
		if !d.tracks(key, sess) || !d.sleepCtx(d.ctx, poll) {
			return session.Session{}, false
		}
	}
}

// tracks reports whether a session named like sess is still tracked under
// key. (Names, not the values: a session type need not be comparable.)
func (d *Daemon) tracks(key string, sess agent.Session) bool {
	d.sessionsMu.Lock()
	defer d.sessionsMu.Unlock()
	e, ok := d.sessions[key]
	return ok && e.sess.Name() == sess.Name()
}

func orDefaultDuration(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}
