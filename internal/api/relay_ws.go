package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// maxRelayMessage bounds what a browser may send in one message.
const maxRelayMessage = 64 << 10

// handleRelay serves GET /sessions/{sid}/acp: the session relay's WebSocket
// (§A.5.2). The checks before the upgrade answer as JSON errors like any
// other endpoint; after it, refusals are error messages on the socket.
func (s *Server) handleRelay(w http.ResponseWriter, r *http.Request) {
	a := authFrom(r.Context())
	client := clientID(a)
	sid := r.PathValue("sid")
	refuse := func(status int, code, msg string) {
		s.log("api_attach_refused", "session", sid, "code", code, "client", client)
		writeError(w, status, code, msg)
	}
	mode, err := oneOf(r, "mode", ModeObserve, ModeObserve, ModeInteractive)
	if err != nil {
		refuse(http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}
	replay, after, err := parseReplay(r.URL.Query().Get("replay"))
	if err != nil {
		refuse(http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}
	scope := ScopeAttach
	if mode == ModeInteractive {
		scope = ScopeInteract
	}
	if !hasScope(a, scope) {
		refuse(http.StatusForbidden, CodeForbidden, "this session lacks the "+scope+" scope")
		return
	}
	if a.session != nil && (r.Header.Get("Origin") == "" || !checkCSRFValue(r.URL.Query().Get("csrf"), a.session)) {
		// Cross-site WebSocket hijacking (§A.6.3): a browser's upgrade
		// carries the cookie, so it must also prove it read the CSRF token.
		refuse(http.StatusForbidden, CodeForbidden, "a browser's session attach needs Origin and ?csrf= with the session's CSRF token")
		return
	}
	if !websocketUpgrade(r) {
		refuse(http.StatusBadRequest, CodeBadRequest, "this endpoint is a WebSocket; send an upgrade request")
		return
	}
	if !offersSubprotocol(r, SubprotocolACP) {
		refuse(http.StatusBadRequest, CodeBadRequest, "offer the "+SubprotocolACP+" WebSocket subprotocol")
		return
	}
	src, ok := s.src.(Relays)
	if !ok {
		refuse(http.StatusServiceUnavailable, CodeUnavailable, "session attach is not available")
		return
	}
	sess, err := src.RelaySession(r.Context(), sid)
	if err != nil {
		status, code := writeErrStatus(err)
		refuse(status, code, err.Error())
		return
	}
	interactive := sess.Interactive && sess.Persistent
	if mode == ModeInteractive && !interactive {
		refuse(http.StatusConflict, CodeInvalidState, "session "+sid+" ("+sess.Kind+") cannot be typed into; only persistent sessions take prompts, so attach with mode=observe")
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{SubprotocolACP}})
	if err != nil {
		// Accept has answered the request itself.
		s.log("api_attach_refused", "session", sid, "code", CodeBadRequest, "client", client, "err", err.Error())
		return
	}
	s.relays.active.Add(1)
	defer s.relays.active.Done()
	conn.SetReadLimit(maxRelayMessage)
	s.log("api_attach", "session", sess.ID, "kind", sess.Kind, "mode", mode, "replay", replay, "client", client)
	defer s.log("api_detach", "session", sess.ID, "mode", mode, "client", client)
	sub := &subscriber{srv: s, conn: conn, sess: sess, mode: mode, client: client}
	sub.serve(replay, after)
}

// parseReplay parses ?replay=all|none|after:<offset> (default all).
func parseReplay(v string) (string, int64, error) {
	switch {
	case v == "" || v == "all":
		return "all", 0, nil
	case v == "none":
		return "none", 0, nil
	case strings.HasPrefix(v, "after:"):
		n, err := strconv.ParseInt(strings.TrimPrefix(v, "after:"), 10, 64)
		if err != nil || n < 0 {
			break
		}
		return "after", n, nil
	}
	return "", 0, badQuery("replay must be all, none or after:<offset>")
}

func checkCSRFValue(got string, bs *browserSession) bool {
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(bs.csrf)) == 1
}

func websocketUpgrade(r *http.Request) bool {
	return headerHasToken(r.Header, "Connection", "upgrade") && headerHasToken(r.Header, "Upgrade", "websocket")
}

func offersSubprotocol(r *http.Request, want string) bool {
	return headerHasToken(r.Header, "Sec-WebSocket-Protocol", want)
}

// headerHasToken reports whether a comma-separated header lists token,
// ignoring case.
func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// bearerFromSubprotocol returns the token a WebSocket upgrade offers as a
// "bearer.<token>" subprotocol, or "".
func bearerFromSubprotocol(r *http.Request) string {
	for _, v := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, t := range strings.Split(v, ",") {
			if tok, ok := strings.CutPrefix(strings.TrimSpace(t), bearerSubprotocol); ok && tok != "" {
				return tok
			}
		}
	}
	return ""
}

// subscriber is one browser on a relay.
type subscriber struct {
	srv    *Server
	conn   *websocket.Conn
	sess   RelaySession
	mode   string
	client string
}

// send writes one message, bounded by relayWriteTimeout.
func (b *subscriber) send(ctx context.Context, v any) error {
	var data []byte
	switch x := v.(type) {
	case []byte:
		data = x
	default:
		var err error
		if data, err = json.Marshal(v); err != nil {
			return err
		}
	}
	wctx, cancel := context.WithTimeout(ctx, relayWriteTimeout)
	defer cancel()
	return b.conn.Write(wctx, websocket.MessageText, data)
}

func (b *subscriber) sendError(ctx context.Context, code, msg string) {
	_ = b.send(ctx, RelayError{T: MsgError, Code: code, Message: msg})
}

// serve runs the connection: join the relay, send hello and the replay, then
// the live frames, while a reader handles what the browser sends.
func (b *subscriber) serve(replay string, after int64) {
	hub := b.srv.relays
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer b.conn.CloseNow()

	rel := hub.join(b.sess, b)
	defer hub.leave(rel, b)

	// The boundary between replay and live: frames before pos come from the
	// log, items from seq on come from the ring.
	rel.mu.Lock()
	pos, seq, promptCount := rel.pos, rel.next(), rel.promptCount
	turn, stop := rel.turn, rel.stopReason
	rel.mu.Unlock()
	if replay == "after" && after > pos {
		replay, after = "reset", 0
	}
	if replay == "all" {
		after = 0
	}
	hello := RelayHello{
		T: MsgHello, Session: b.sess.ID, Kind: b.sess.Kind, Persistent: b.sess.Persistent,
		InteractiveAllowed: rel.interactive(), Mode: b.mode, PromptCount: promptCount, Replay: replay,
	}
	if b.send(ctx, hello) != nil {
		return
	}
	if replay != "none" && b.replay(ctx, after, pos) != nil {
		return
	}
	if b.send(ctx, map[string]string{"t": MsgLive}) != nil {
		return
	}
	if b.send(ctx, RelayTurn{T: MsgTurn, State: turn, StopReason: stop}) != nil {
		return
	}

	go b.read(ctx, cancel, rel)
	b.follow(ctx, rel, seq)
}

// replay sends the log's frames from offset after up to pos.
func (b *subscriber) replay(ctx context.Context, after, pos int64) error {
	if after >= pos || b.sess.OpenLog == nil {
		return nil
	}
	lr, err := b.sess.OpenLog(after)
	if err != nil {
		return nil // no log yet: nothing to replay
	}
	defer lr.Close()
	for {
		frames, err := lr.Next(relayReadBatch)
		for i := range frames {
			f := frames[i]
			if f.Start >= pos {
				return nil
			}
			if !json.Valid(f.Data) {
				f.Data, _ = json.Marshal(string(f.Data))
			}
			if err := b.send(ctx, frameMessage(MsgReplay, &f)); err != nil {
				return err
			}
		}
		if err != nil || len(frames) == 0 {
			return nil
		}
	}
}

// follow sends the ring's items from seq on until the relay ends, the
// browser goes away or the server stops.
func (b *subscriber) follow(ctx context.Context, rel *relay, seq uint64) {
	for {
		rel.mu.Lock()
		if seq < rel.base {
			rel.mu.Unlock()
			b.sendError(ctx, "too_slow", "this connection fell too far behind the session; reconnect with replay=after:<offset>")
			b.conn.Close(websocket.StatusPolicyViolation, "too slow")
			return
		}
		items := rel.ring[seq-rel.base:]
		ended, ch := rel.ended, rel.changed
		rel.mu.Unlock()
		for _, it := range items {
			msg := it.control
			if it.frame != nil {
				msg = frameMessage(MsgFrame, it.frame)
			}
			if b.send(ctx, msg) != nil {
				return
			}
			seq++
		}
		if len(items) > 0 {
			continue
		}
		if ended != "" {
			_ = b.send(ctx, RelayEnded{T: MsgEnded, Reason: ended})
			b.conn.Close(websocket.StatusNormalClosure, "session ended")
			return
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return
		case <-b.srv.stopStreams:
			_ = b.send(context.Background(), RelayEnded{T: MsgEnded, Reason: EndedDaemonStopping})
			b.conn.Close(websocket.StatusGoingAway, EndedDaemonStopping)
			return
		}
	}
}

// read handles the browser's messages until the connection closes; then it
// cancels ctx, which ends follow.
func (b *subscriber) read(ctx context.Context, cancel context.CancelFunc, rel *relay) {
	defer cancel()
	for {
		typ, data, err := b.conn.Read(ctx)
		if err != nil {
			return
		}
		var m RelayClientMessage
		if typ != websocket.MessageText || json.Unmarshal(data, &m) != nil {
			b.sendError(ctx, CodeBadRequest, `messages are JSON objects: {"t":"prompt","text":...}, {"t":"cancel"} or {"t":"ping"}`)
			continue
		}
		switch m.T {
		case MsgPing:
			_ = b.send(ctx, map[string]any{"t": MsgPong, "time": time.Now().UTC()})
		case MsgPrompt, MsgCancel:
			if b.mode != ModeInteractive {
				b.sendError(ctx, CodeForbidden, "this connection observes; attach with mode=interactive to "+m.T)
				continue
			}
			if m.T == MsgCancel {
				if err := rel.cancel(); err != nil {
					b.sendError(ctx, errCode(err), err.Error())
					continue
				}
				b.srv.log("api_cancel", "session", b.sess.ID, "client", b.client)
				continue
			}
			if strings.TrimSpace(m.Text) == "" {
				b.sendError(ctx, CodeBadRequest, "a prompt needs text")
				continue
			}
			err := rel.prompt(m.Text, func(err error) {
				b.sendError(context.Background(), CodeUnavailable, "the prompt failed: "+err.Error())
			})
			if err != nil {
				b.sendError(ctx, errCode(err), err.Error())
				continue
			}
			// The prompt text is not logged (§A.6.3).
			b.srv.log("api_prompt", "session", b.sess.ID, "client", b.client, "chars", strconv.Itoa(len(m.Text)))
		default:
			b.sendError(ctx, CodeBadRequest, "unknown message type "+strconv.Quote(m.T))
		}
	}
}

func errCode(err error) string {
	_, code := writeErrStatus(err)
	return code
}
