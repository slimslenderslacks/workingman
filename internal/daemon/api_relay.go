package daemon

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/slimslenderslacks/work/internal/acpchat"
	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/api"
	"github.com/slimslenderslacks/work/internal/session"
)

// The session relay's daemon side (§A.5.2, phase P.5): api.Relays over the
// sessions the daemon tracks. The relay reads a session's stream.log through
// relayLog (redacted like the transcript endpoint) and joins it upstream with
// acpchat, adopting the existing ACP session and never creating one: the
// opening prompt is the primer's (priming.go) or the TUI's, never the
// relay's.

// relayConnectTimeout bounds one upstream attach; the relay retries on its
// own schedule after a failure.
const relayConnectTimeout = 30 * time.Second

// relayAttachFunc joins a session for the relay; acpchat.Attach in
// production. Tests replace it.
type relayAttachFunc func(ctx context.Context, ref string, opts acpchat.Options) (relayConversation, error)

// relayConversation is what the relay needs of an attached session;
// *acpchat.Conversation implements it.
type relayConversation interface {
	Ask(ctx context.Context, text string, opts ...acpchat.AskOption) (string, error)
	Cancel() error
	Events() <-chan acpchat.Event
	Close() error
}

func acpchatRelayAttach(ctx context.Context, ref string, opts acpchat.Options) (relayConversation, error) {
	return acpchat.Attach(ctx, ref, opts)
}

// typedInto reports whether a human may prompt a persistent session of this
// kind: the TUI's rule (wolf, workingman, sleepy-head). The Step-Up agent is
// persistent too, but it is the escalation path and takes no typed turns.
func typedInto(kind string) bool {
	k, ok := agent.ParseKind(kind)
	return ok && k != agent.StepUpAgent
}

func (s apiSource) RelaySession(_ context.Context, id string) (api.RelaySession, error) {
	d := s.d
	m := d.model()
	for _, ss := range m.sessions {
		if (sessionID(ss) == id || ss.Key == id) && ss.ACP == nil {
			return api.RelaySession{}, fmt.Errorf("%w: session %q runs in a terminal tab and has no ACP stream to attach to", api.ErrInvalidState, id)
		}
	}
	rec, err := d.sessionRecord(m, id)
	if err != nil {
		return api.RelaySession{}, err
	}
	if rec.Status == session.StatusExited || rec.Status == session.StatusFailed {
		return api.RelaySession{}, fmt.Errorf("%w: session %q has %s", api.ErrInvalidState, id, rec.Status)
	}
	store := session.Store{Root: d.sessionsRootOrEmpty()}
	sid, logPath := rec.ID, rec.LogPath
	kind := rec.Kind
	for _, ss := range m.sessions {
		if ss.ACP != nil && ss.ACP.SessionID == sid && kind == "" {
			kind = ss.Kind
		}
	}
	attach := d.relayAttach
	if attach == nil {
		attach = acpchatRelayAttach
	}
	return api.RelaySession{
		ID:          sid,
		Kind:        kind,
		Persistent:  rec.Persistent,
		Interactive: rec.Persistent && typedInto(kind),
		State: func() (api.RelayState, error) {
			r, err := store.Read(sid)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				return api.RelayState{Ended: "gone"}, nil
			case err != nil:
				return api.RelayState{}, err
			case r.Status == session.StatusExited || r.Status == session.StatusFailed:
				return api.RelayState{PromptCount: r.PromptCount, Ended: string(r.Status)}, nil
			}
			return api.RelayState{PromptCount: r.PromptCount}, nil
		},
		OpenLog: func(after int64) (api.LogReader, error) { return openRelayLog(logPath, after) },
		Attach: func(ctx context.Context, wake func()) (api.Upstream, error) {
			actx, cancel := context.WithTimeout(ctx, relayConnectTimeout)
			defer cancel()
			conv, err := attach(actx, store.Dir(sid), acpchat.Options{
				Root:           store.Root,
				ConnectTimeout: relayConnectTimeout,
				OnFrame:        func([]byte) { wake() },
				// Sessions run in bypassPermissions; nothing should ask, and
				// the relay never answers a permission on its own (§A.5.2).
				OnPermission: func(context.Context, acpchat.PermissionRequest) acpchat.PermissionDecision {
					return acpchat.PermissionDecision{}
				},
			})
			if err != nil {
				return nil, err
			}
			return newRelayUpstream(conv), nil
		},
	}, nil
}

// relayUpstream adapts an attached conversation to api.Upstream.
type relayUpstream struct {
	conv relayConversation
	done chan struct{}
}

func newRelayUpstream(conv relayConversation) *relayUpstream {
	u := &relayUpstream{conv: conv, done: make(chan struct{})}
	events := conv.Events()
	go func() {
		defer close(u.done)
		for range events {
			// The relay reads frames from the log; the stream only tells it
			// when the conversation is over (the channel closes).
		}
	}()
	return u
}

// Prompt runs one turn. It is not bounded by a context, so it never sends
// session/cancel on its own: closing the upstream detaches instead.
func (u *relayUpstream) Prompt(text string) error {
	_, err := u.conv.Ask(context.Background(), text)
	if errors.Is(err, acpchat.ErrRefused) {
		return nil // the turn ended; its frames say how
	}
	return err
}

func (u *relayUpstream) Cancel() error         { return u.conv.Cancel() }
func (u *relayUpstream) Done() <-chan struct{} { return u.done }
func (u *relayUpstream) Close() error          { return u.conv.Close() }

// relayLog reads a stream log forward from an offset, redacting each frame.
type relayLog struct {
	f   *os.File
	br  *bufio.Reader
	pos int64
}

func openRelayLog(path string, after int64) (api.LogReader, error) {
	if path == "" {
		return nil, fs.ErrNotExist
	}
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(after, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return &relayLog{f: f, br: bufio.NewReader(f), pos: after}, nil
}

// Next returns up to limit whole lines appended since the last call. A
// partial last line (the wrapper is mid-write) is left in place: the read
// position is moved back to its start, so the next call reads it whole.
func (l *relayLog) Next(limit int) ([]api.LogFrame, error) {
	var out []api.LogFrame
	for len(out) < limit {
		line, err := l.br.ReadBytes('\n')
		if err != nil {
			if len(line) > 0 {
				if _, serr := l.f.Seek(l.pos, io.SeekStart); serr != nil {
					return out, serr
				}
			}
			l.br.Reset(l.f)
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return out, err
		}
		start := l.pos
		l.pos += int64(len(line))
		if data := redactFrame(bytes.TrimRight(line, "\r\n")); len(data) > 0 {
			out = append(out, api.LogFrame{Start: start, End: l.pos, Data: data})
		}
	}
	return out, nil
}

func (l *relayLog) Close() error { return l.f.Close() }
