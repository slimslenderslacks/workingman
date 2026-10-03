package router

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/slimslenderslacks/work/internal/acpchat"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/channels"
)

// conv is a cached attachment to one agent session.
type conv struct {
	tgt   target
	ready chan struct{} // closed once the attach attempt has finished
	agent Agent         // valid after ready, when err == nil
	err   error
	sem   chan struct{} // one turn at a time per conversation, across chats
}

func (cv *conv) close() {
	<-cv.ready
	if cv.agent != nil {
		_ = cv.agent.Close()
	}
}

// conversation returns the attachment to tgt, attaching on first use. A cached
// attachment to an older session under the same key (the wolf was relaunched) is
// replaced. Concurrent callers share one attach attempt.
func (r *Router) conversation(ctx context.Context, tgt target) (*conv, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, context.Canceled
	}
	cv := r.convs[tgt.Key]
	var stale *conv
	if cv != nil && cv.tgt.ID != tgt.ID {
		stale, cv = cv, nil
		delete(r.convs, tgt.Key)
	}
	if cv != nil {
		r.mu.Unlock()
		select {
		case <-cv.ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if cv.err != nil {
			return nil, cv.err
		}
		return cv, nil
	}
	cv = &conv{tgt: tgt, ready: make(chan struct{}), sem: make(chan struct{}, 1)}
	r.convs[tgt.Key] = cv
	r.mu.Unlock()
	if stale != nil {
		r.goFn(stale.close)
	}

	actx, cancel := context.WithTimeout(ctx, r.cfg.AttachTimeout)
	defer cancel()
	opts := r.cfg.AttachOptions
	opts.PermissionTimeout = r.cfg.PermissionTimeout
	opts.OnPermission = func(pctx context.Context, req acpchat.PermissionRequest) acpchat.PermissionDecision {
		return r.askPermission(pctx, tgt, req)
	}
	a, err := r.cfg.Attach(actx, tgt.Ref, opts)
	if err != nil {
		cv.err = err
		r.dropConv(cv)
		close(cv.ready)
		return nil, err
	}
	cv.agent = a
	var events <-chan acpchat.Event
	if tgt.wolf {
		// Subscribed before anyone can Ask: events are recorded only from the
		// first Events call on.
		events = a.Events()
	}
	close(cv.ready)
	if events != nil {
		r.goFn(func() { r.observe(cv, events) })
	}
	r.audit("attached", "kind", kindOf(tgt), "work_stream", tgt.WorkStream)
	return cv, nil
}

func (r *Router) audit(event string, kv ...string) {
	if r.cfg.Audit != nil {
		r.cfg.Audit.Log("router_"+event, kv...)
	}
}

func kindOf(t target) string {
	if t.wolf {
		return "wolf"
	}
	return "workingman"
}

// dropConv forgets cv (if it is still the cached one) and detaches it.
func (r *Router) dropConv(cv *conv) {
	r.mu.Lock()
	if r.convs[cv.tgt.Key] == cv {
		delete(r.convs, cv.tgt.Key)
	}
	r.mu.Unlock()
}

// turn runs one Ask for chat c against tgt and sends the outcome.
func (r *Router) turn(c *chat, tgt target, text string) {
	if !tgt.wolf {
		sess, ok := r.cfg.Source.WorkingmanAgent()
		if !ok {
			r.log("agent_down", c)
			r.notify(c, AgentStartingMessage)
			return
		}
		tgt.Session = sess
		tgt.Key = sess.Key
	}

	stop := r.watchTurn(c)
	defer stop()

	cv, err := r.conversation(r.ctx, tgt)
	if err != nil {
		stop()
		r.turnFailed(c, tgt, nil, err, "")
		return
	}
	select {
	case cv.sem <- struct{}{}:
	case <-r.ctx.Done():
		return
	}
	defer func() { <-cv.sem }()

	r.setActive(tgt.Key, c, +1)
	defer r.setActive(tgt.Key, c, -1)

	tctx, cancel := context.WithTimeout(r.ctx, r.cfg.TurnTimeout)
	defer cancel()
	started := time.Now()
	reply, err := cv.agent.Ask(tctx, text, acpchat.FinalSegmentOnly())
	stop()
	if r.ctx.Err() != nil {
		return
	}
	if err != nil {
		r.turnFailed(c, tgt, cv, err, reply)
		return
	}
	r.log("turn_done", c, "kind", kindOf(tgt), "ms", fmt.Sprint(time.Since(started).Milliseconds()), "reply_runes", fmt.Sprint(len([]rune(reply))))
	if strings.TrimSpace(reply) == "" {
		reply = "(the agent finished without a text reply)"
	}
	r.reply(c, tgt, reply)
}

// turnFailed turns an Ask/attach error into a friendly message for the chat.
func (r *Router) turnFailed(c *chat, tgt target, cv *conv, err error, partial string) {
	r.log("turn_error", c, "kind", kindOf(tgt), "err", audit.Redact(err.Error()))
	switch {
	case errors.Is(err, context.DeadlineExceeded) && cv != nil:
		msg := fmt.Sprintf("That took longer than %s, so I cancelled it.", r.cfg.TurnTimeout.Round(time.Second))
		if p := strings.TrimSpace(partial); p != "" {
			msg += " Here is what came back before the cutoff:\n\n" + p
		}
		r.reply(c, tgt, msg)
	case errors.Is(err, context.DeadlineExceeded):
		r.notify(c, "I couldn't reach the agent in time. Try again in a minute.")
	case errors.Is(err, acpchat.ErrBusy):
		r.notify(c, "The agent is mid-turn, try again in a moment.")
	case errors.Is(err, acpchat.ErrRefused):
		r.notify(c, "The agent declined to answer that.")
	case errors.Is(err, acpchat.ErrSessionGone), errors.Is(err, acpchat.ErrNoSession), errors.Is(err, acpchat.ErrClosed):
		if cv != nil {
			r.dropConv(cv)
			r.goFn(cv.close)
		}
		if tgt.wolf {
			r.unbindWolf(tgt.Key, tgt.ID)
			r.notify(c, fmt.Sprintf("The wolf for %s has ended. You're back on the workingman agent.", tgt.WorkStream))
			return
		}
		r.notify(c, AgentStartingMessage)
	default:
		if cv == nil && !tgt.wolf {
			r.notify(c, AgentStartingMessage) // could not attach to the agent: it is not ready yet
			return
		}
		r.notify(c, "Something went wrong talking to the agent: "+shorten(audit.Redact(err.Error()), 200))
	}
}

func shorten(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// unbindWolf returns every chat bound to the wolf session (key, id) to the
// workingman agent and reports whom it unbound.
func (r *Router) unbindWolf(key, id string) []*chat {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*chat
	for _, c := range r.chats {
		if c.wolf != nil && c.wolf.Key == key && (id == "" || c.wolf.ID == id) {
			c.wolf = nil
			out = append(out, c)
		}
	}
	return out
}

func (r *Router) setActive(key string, c *chat, delta int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.active[key]
	if m == nil {
		m = map[*chat]int{}
		r.active[key] = m
	}
	m[c] += delta
	if m[c] <= 0 {
		delete(m, c)
	}
	if len(m) == 0 {
		delete(r.active, key)
	}
}

// watchTurn shows the typing indicator while a turn runs and acks a slow turn
// with "…thinking". The returned stop is idempotent and returns only once the
// watcher has finished, so no ack can follow the reply.
func (r *Router) watchTurn(c *chat) (stop func()) {
	done := make(chan struct{})
	exited := make(chan struct{})
	var typer channels.Typer
	if ch, ok := r.cfg.Out.Get(c.channel); ok {
		typer, _ = ch.(channels.Typer)
	}
	go func() {
		defer close(exited)
		typing := func() {
			if typer == nil {
				return
			}
			ctx, cancel := context.WithTimeout(r.ctx, r.cfg.SendTimeout)
			defer cancel()
			_ = typer.Typing(ctx, c.id)
		}
		typing()
		thinking := time.NewTimer(r.cfg.ThinkingAfter)
		defer thinking.Stop()
		tick := time.NewTicker(r.cfg.TypingEvery)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-r.ctx.Done():
				return
			case <-thinking.C:
				r.notify(c, "…thinking")
			case <-tick.C:
				typing()
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(done) })
		<-exited
	}
}

// --- sending ---

// notify sends a router-authored message (no agent label).
func (r *Router) notify(c *chat, text string) {
	r.send(c, "", text, nil)
}

// reply sends an agent's text to the chat: secrets redacted, over-long text cut,
// split by the channel's own chunker if it has one, and — for a wolf — labelled
// "[wolf <work-stream>]" and recorded in the conversation index so replying to
// it reaches the same wolf.
func (r *Router) reply(c *chat, tgt target, text string) {
	text = limitRunes(audit.Redact(text), r.cfg.MaxReplyRunes)
	var reg *target
	if tgt.wolf {
		reg = &tgt
	}
	r.send(c, tgt.label(), text, reg)
}

func limitRunes(s string, max int) string {
	rs := []rune(s)
	if len(rs) <= max {
		return s
	}
	return strings.TrimSpace(string(rs[:max])) + fmt.Sprintf("\n\n… (reply cut: %d more characters)", len(rs)-max)
}

func (r *Router) send(c *chat, label, text string, register *target) {
	for _, piece := range r.pieces(c.channel, text) {
		ctx, cancel := context.WithTimeout(r.ctx, r.cfg.SendTimeout)
		id, err := r.cfg.Out.Send(ctx, c.channel, channels.OutboundMessage{ChatID: c.id, Text: piece, Topic: label})
		cancel()
		if err != nil {
			r.log("send_error", c, "err", audit.Redact(err.Error()))
			return
		}
		if register != nil && id != "" && r.cfg.Index != nil {
			if err := r.cfg.Index.Register(c.channel, id, register.conversationTarget()); err != nil {
				r.log("index_error", c, "err", err.Error())
			}
		}
	}
}

// pieces splits text with the channel's own chunker when it has one.
func (r *Router) pieces(channel, text string) []string {
	if ch, ok := r.cfg.Out.Get(channel); ok {
		if k, ok := ch.(channels.Chunker); ok {
			if p := k.ChunkMessage(text); len(p) > 0 {
				return p
			}
		}
	}
	return []string{text}
}
