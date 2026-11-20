package slack

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/slimslenderslacks/work/internal/channels"
)

// poll is one inbound pass (docs/messaging-design.md §6.4): read_channel
// from the oldest watched request (or the top-level cursor) for fresh
// top-level messages and reply counts, then read_thread on each watched
// request whose count rose. When read_channel cannot be parsed, every
// watched thread is read instead (the per-thread fallback). It runs on the
// worker.
func (c *Channel) poll(ctx context.Context) error {
	now := c.now()
	if c.st.prune(now, DefaultSentTTL, c.cfg.WatchTTL) || c.pruneClosed() {
		c.saveState()
	}

	due, err := c.readChannel(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, w := range c.activeWatches() {
		count, ok := due[w]
		if !ok && !c.fallback && c.inPolledChannel(w) {
			continue
		}
		c.runQueued(ctx)
		if err := c.readThread(ctx, w); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			errs = append(errs, err)
			continue
		}
		if ok {
			w.ReplyCount = count
		}
	}
	c.saveState()
	return errors.Join(errs...)
}

// pruneClosed stops watching requests resolved more than closedGrace ago.
func (c *Channel) pruneClosed() bool {
	now := c.now()
	kept := c.st.watch.Watches[:0]
	changed := false
	for _, w := range c.st.watch.Watches {
		if w.Closed != nil && now.Sub(w.Closed.At) > closedGrace {
			changed = true
			continue
		}
		kept = append(kept, w)
	}
	c.st.watch.Watches = kept
	return changed
}

func (c *Channel) activeWatches() []*Watch {
	return append([]*Watch(nil), c.st.watch.Watches...)
}

// inPolledChannel reports whether w was posted in the conversation
// read_channel reads, so its reply count is seen there. Requests sent
// elsewhere (a route to another chat) have their thread read every poll.
func (c *Channel) inPolledChannel(w *Watch) bool {
	ch, _ := SplitMessageID(w.ID)
	return ch == c.channelID() || ch == c.cfg.Channel
}

// readChannel reads the configured conversation's top-level messages since
// the oldest point of interest, emits the human ones newer than the
// top-level cursor, and returns the watched requests with new replies, with
// their reply counts.
func (c *Channel) readChannel(ctx context.Context) (map[*Watch]int, error) {
	oldest := c.st.watch.TopCursor
	for _, w := range c.st.watch.Watches {
		if _, ts := SplitMessageID(w.ID); tsLess(tsBefore(ts), oldest) {
			oldest = tsBefore(ts)
		}
	}
	var msgs []Message
	cursor := ""
	for page := 0; page < maxPages; page++ {
		c.runQueued(ctx)
		args := map[string]any{
			"channel_id":      c.cfg.Channel,
			"oldest":          oldest,
			"limit":           readChannelLimit,
			"response_format": "detailed",
		}
		if cursor != "" {
			args["cursor"] = cursor
		}
		res, err := c.call(ctx, toolChannel, args)
		if err != nil {
			return nil, err
		}
		text, pagination := readResult(res)
		p := ParseChannel(text, pagination)
		if p.Malformed {
			if !c.fallback {
				c.log.Error("slack: read_channel returned text with no \"Message TS:\" lines; the Slack MCP format may have changed. Falling back to reading each watched thread", "sample", sample(text))
			}
			c.fallback = true
			return nil, nil
		}
		if c.fallback {
			c.log.Info("slack: read_channel parses again; leaving per-thread fallback")
			c.fallback = false
		}
		if p.ChannelID != "" && c.st.watch.ChannelID == "" {
			c.st.watch.ChannelID = p.ChannelID
		}
		msgs = append(msgs, p.Messages...)
		if p.Cursor == "" || p.Cursor == cursor {
			break
		}
		cursor = p.Cursor
	}
	sort.SliceStable(msgs, func(i, j int) bool { return tsLess(msgs[i].TS, msgs[j].TS) })

	chID := c.channelID()
	due := map[*Watch]int{}
	top := c.st.watch.TopCursor
	for _, m := range msgs {
		id := MessageID(chID, m.TS)
		if w := c.st.watchFor(id); w != nil {
			if m.Replies > w.ReplyCount {
				due[w] = m.Replies
			}
			continue
		}
		if !tsLess(top, m.TS) {
			continue // seen before
		}
		top = m.TS
		if c.st.isSent(id) || c.isEcho(m, "", id) {
			continue // ours
		}
		if m.ViaMCP && strings.HasPrefix(m.Text, TestPrefix) {
			continue // `orch slack test`'s, not the operator's
		}
		c.emit(m, c.cfg.Channel, id, "")
	}
	c.st.watch.TopCursor = top
	return due, nil
}

// channelID is the resolved conversation id messages are keyed by: learned
// from the first send or read, else the configured id.
func (c *Channel) channelID() string {
	if id := c.st.watch.ChannelID; id != "" {
		return id
	}
	return c.cfg.Channel
}

// readThread reads the replies to watched request w newer than its cursor.
// Human replies to an open request are emitted with ReplyToID = w.ID, so
// the conversation index routes them to post-office; a reply to a closed
// one is answered in the thread and goes nowhere.
func (c *Channel) readThread(ctx context.Context, w *Watch) error {
	ch, ts := SplitMessageID(w.ID)
	if ch == "" {
		ch = c.channelID()
	}
	var replies []Message
	cursor := ""
	for page := 0; page < maxPages; page++ {
		args := map[string]any{
			"channel_id":      ch,
			"message_ts":      ts,
			"limit":           readThreadLimit,
			"response_format": "detailed",
		}
		if w.ThreadCursor != "" {
			args["oldest"] = w.ThreadCursor
		}
		if cursor != "" {
			args["cursor"] = cursor
		}
		res, err := c.call(ctx, toolThread, args)
		if err != nil {
			return err
		}
		text, pagination := readResult(res)
		p := ParseThread(text, pagination)
		if p.Malformed {
			c.log.Error("slack: read_thread returned text that could not be parsed; the Slack MCP format may have changed", "request", w.ID, "sample", sample(text))
			return fmt.Errorf("slack: read_thread %s: unparsable response", w.ID)
		}
		replies = append(replies, p.Replies...)
		if p.Cursor == "" || p.Cursor == cursor {
			break
		}
		cursor = p.Cursor
	}
	sort.SliceStable(replies, func(i, j int) bool { return tsLess(replies[i].TS, replies[j].TS) })

	for _, r := range replies {
		if r.TS == ts || (w.ThreadCursor != "" && !tsLess(w.ThreadCursor, r.TS)) {
			continue
		}
		w.ThreadCursor = r.TS
		id := MessageID(ch, r.TS)
		if c.st.isSent(id) || c.isEcho(r, ts, id) {
			continue // ours
		}
		if w.Closed != nil {
			if !c.cfg.Allowed(r.UserID) {
				c.drop(r)
				continue
			}
			c.log.Info("slack: reply to a closed request", "request", w.ID, "outcome", w.Closed.Outcome)
			if _, err := c.post(ctx, ch, ts, closedNote(w.Closed)); err != nil {
				c.log.Warn("slack: could not answer a reply to a closed request", "err", err.Error())
			}
			continue
		}
		chat := w.Chat
		if chat == "" {
			chat = c.cfg.Channel
		}
		c.emit(r, chat, id, w.ID)
	}
	return nil
}

func closedNote(cl *Closed) string {
	switch cl.Outcome {
	case "answered":
		if cl.Via != "" {
			return fmt.Sprintf("This request is already closed (answered via %s).", cl.Via)
		}
		return "This request is already closed (answered)."
	case "":
		return "This request is already closed."
	}
	return fmt.Sprintf("This request is already closed (%s).", cl.Outcome)
}

// isEcho reports whether m (in thread, "" for top level) is the echo of a
// send whose outcome was unknown, and if so records it as ours.
func (c *Channel) isEcho(m Message, thread, id string) bool {
	if !m.ViaMCP || !c.st.takeUnconfirmed(thread, echoKey(m.Text)) {
		return false
	}
	c.st.markSent(id, c.now())
	c.log.Info("slack: ignoring the echo of a send whose response was lost", "ts", m.TS)
	return true
}

// emit hands a human message to the handler, if its author is allowed.
func (c *Channel) emit(m Message, chat, id, replyTo string) {
	if !c.cfg.Allowed(m.UserID) {
		c.drop(m)
		return
	}
	c.inbox.push(channels.InboundMessage{
		Channel:    c.name,
		ChatID:     chat,
		SenderID:   m.UserID,
		SenderName: m.UserName,
		MessageID:  id,
		ReplyToID:  replyTo,
		Text:       m.Text,
		Timestamp:  tsTime(m.TS),
	})
}

// drop logs a message from a sender not in allow_from (never its text).
func (c *Channel) drop(m Message) {
	c.log.Warn("slack: dropped message from a sender not in allow_from", "sender", m.UserID, "ts", m.TS)
}

// sample is the start of a response, for a format-change log line.
func sample(s string) string {
	r := []rune(s)
	if len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return s
}
