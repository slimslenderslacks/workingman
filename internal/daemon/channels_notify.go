package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/slimslenderslacks/work/internal/channels"
	"github.com/slimslenderslacks/work/internal/project"
)

const (
	// wolfTopic is the channels topic the wolf's own messages are sent on.
	wolfTopic = "wolf"

	// defaultChannelSendTimeout bounds one async channel send.
	defaultChannelSendTimeout = 20 * time.Second

	maxWolfReasonRunes   = 300
	maxWolfTasksListed   = 5
	wolfReplyInstruction = "Reply to this message to talk to the wolf."
)

// ChannelSender delivers text to every route of a topic and reports the
// transport message ID of each delivery. *channels.Registry implements it.
type ChannelSender interface {
	SendTopic(ctx context.Context, topic, text string) ([]channels.SendResult, error)
}

// channelNotify is the daemon's messaging-channel state: where wolf lifecycle
// messages go, where their message IDs are recorded, and the per-work-stream
// rate limit. A nil *channelNotify (no channels configured) disables it all.
type channelNotify struct {
	sender      ChannelSender
	index       channels.ConversationIndex
	minInterval time.Duration
	sendTimeout time.Duration
	now         func() time.Time

	mu        sync.Mutex
	lastStart map[string]time.Time // work-stream (project path) → last wolf-start send
	inflight  sync.WaitGroup
}

// ChannelOption tunes WithChannels.
type ChannelOption func(*channelNotify)

// WithWolfStartInterval sets the minimum gap between two wolf-start messages
// for the same work stream (0 disables the limit; the default is
// channels.DefaultWolfStartInterval).
func WithWolfStartInterval(d time.Duration) ChannelOption {
	return func(c *channelNotify) {
		if d >= 0 {
			c.minInterval = d
		}
	}
}

// WithChannelSendTimeout bounds each async channel send (<=0 keeps the default).
func WithChannelSendTimeout(d time.Duration) ChannelOption {
	return func(c *channelNotify) {
		if d > 0 {
			c.sendTimeout = d
		}
	}
}

// WithChannels enables the daemon's own channel messages: a "wolf is running"
// message on topic `wolf` once a wolf session has actually started, and a
// "wolf finished" message when it ends. sender is typically the
// *channels.Registry. Every message ID is recorded in index (may be nil) so an
// inbound reply-to can later be mapped back to the wolf session. A nil sender
// leaves channels disabled.
//
// This is separate from WithNotifier, which still carries the macOS (and
// wildcard-topic) notifications unchanged.
func WithChannels(sender ChannelSender, index channels.ConversationIndex, opts ...ChannelOption) Option {
	return func(d *Daemon) {
		if sender == nil {
			return
		}
		c := &channelNotify{
			sender:      sender,
			index:       index,
			minInterval: channels.DefaultWolfStartInterval,
			sendTimeout: defaultChannelSendTimeout,
			now:         time.Now,
			lastStart:   map[string]time.Time{},
		}
		for _, o := range opts {
			o(c)
		}
		d.channels = c
	}
}

// wolfAnnouncement links a wolf session's start message to its end message so
// the end message is sent only when the start one went out.
type wolfAnnouncement struct {
	done      chan struct{} // closed when the start attempt has finished
	mu        sync.Mutex
	announced bool
}

func newWolfAnnouncement() *wolfAnnouncement { return &wolfAnnouncement{done: make(chan struct{})} }

func (a *wolfAnnouncement) wasAnnounced() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.announced
}

// workStreamName is the human label of the work stream: the directory holding
// the .project.yaml, matching SessionInfo.Project.
func workStreamName(projectPath string) string {
	return filepath.Base(filepath.Dir(projectPath))
}

// announceWolfStart sends the "wolf is running" message for a wolf session
// that has just started. It never blocks the caller: the send runs on its own
// goroutine under a timeout, failures are audit-logged (channel_send_error),
// and a work stream that was announced within the rate-limit interval is
// skipped (wolf_start_suppressed).
func (d *Daemon) announceWolfStart(ann *wolfAnnouncement, key, projectPath, sessionID, reason string, failedTaskPaths []string) {
	c := d.channels
	if c == nil {
		close(ann.done)
		return
	}
	ws := workStreamName(projectPath)
	if !c.reserveWolfStart(projectPath) {
		d.audit.Log("wolf_start_suppressed", "path", projectPath, "work_stream", ws, "min_interval", c.minInterval.String())
		close(ann.done)
		return
	}
	text := wolfStartText(ws, reason, failedTaskPaths)
	target := channels.ConversationTarget{
		Kind:        channels.ConversationKindWolf,
		WorkStream:  ws,
		ProjectPath: projectPath,
		SessionKey:  key,
		SessionID:   sessionID,
	}
	c.inflight.Add(1)
	go func() {
		defer c.inflight.Done()
		defer close(ann.done)
		results, ok := d.sendChannelMessage(text, "wolf_start", projectPath)
		if !ok {
			c.releaseWolfStart(projectPath)
			return
		}
		ann.mu.Lock()
		ann.announced = true
		ann.mu.Unlock()
		for _, r := range results {
			if r.Err != nil || r.MessageID == "" || c.index == nil {
				continue
			}
			if err := c.index.Register(r.Route.Channel, r.MessageID, target); err != nil {
				d.audit.Log("channel_index_error", "path", projectPath, "channel", r.Route.Channel, "err", err.Error())
			}
		}
		d.audit.Log("wolf_start_notified", "path", projectPath, "work_stream", ws, "session", sessionID)
	}()
}

// announceWolfEnd sends "wolf finished" once the session has ended, if its
// start message went out. It runs on its own goroutine and never blocks.
func (d *Daemon) announceWolfEnd(ann *wolfAnnouncement, projectPath string) {
	c := d.channels
	if c == nil {
		return
	}
	c.inflight.Add(1)
	go func() {
		defer c.inflight.Done()
		select {
		case <-ann.done:
		case <-d.ctx.Done():
			return
		}
		if !ann.wasAnnounced() {
			return
		}
		status := "unknown"
		if p, err := project.Load(projectPath); err == nil && p.Status != "" {
			status = string(p.Status)
		}
		text := fmt.Sprintf("🐺 wolf finished for %s: project now %s", workStreamName(projectPath), status)
		d.sendChannelMessage(text, "wolf_end", projectPath)
	}()
}

// sendChannelMessage delivers text on the wolf topic under the send timeout.
// It reports whether at least one route accepted it (or there was nothing to
// fail); failures are audit-logged as channel_send_error and never returned
// to dispatch.
func (d *Daemon) sendChannelMessage(text, event, projectPath string) ([]channels.SendResult, bool) {
	c := d.channels
	ctx, cancel := context.WithTimeout(d.ctx, c.sendTimeout)
	defer cancel()
	results, err := safeSendTopic(ctx, c.sender, wolfTopic, text)
	delivered := false
	for _, r := range results {
		if r.Err != nil {
			d.audit.Log("channel_send_error", "event", event, "path", projectPath, "channel", r.Route.Channel, "err", r.Err.Error())
		} else {
			delivered = true
		}
	}
	if err != nil && len(results) == 0 {
		d.audit.Log("channel_send_error", "event", event, "path", projectPath, "err", err.Error())
	}
	return results, delivered
}

// safeSendTopic converts a panic in the sender into an error: a misbehaving
// channel must never take the daemon down.
func safeSendTopic(ctx context.Context, s ChannelSender, topic, text string) (res []channels.SendResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			res, err = nil, fmt.Errorf("panic: %v", r)
		}
	}()
	return s.SendTopic(ctx, topic, text)
}

// reserveWolfStart claims the work stream's rate-limit slot, reporting false
// if a start message went out less than minInterval ago.
func (c *channelNotify) reserveWolfStart(projectPath string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if last, ok := c.lastStart[projectPath]; ok && c.minInterval > 0 && now.Sub(last) < c.minInterval {
		return false
	}
	c.lastStart[projectPath] = now
	return true
}

// releaseWolfStart gives the slot back after a send that reached no one, so
// the next wolf start is not suppressed by a message the user never got.
func (c *channelNotify) releaseWolfStart(projectPath string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.lastStart, projectPath)
}

// wolfStartText renders the plain-text start message.
func wolfStartText(workStream, reason string, failedTaskPaths []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🐺 wolf is running for %s", workStream)
	if r := truncateRunes(strings.Join(strings.Fields(reason), " "), maxWolfReasonRunes); r != "" {
		fmt.Fprintf(&b, "\nBlocked: %s", r)
	}
	if len(failedTaskPaths) > 0 {
		names := make([]string, 0, maxWolfTasksListed)
		for i, p := range failedTaskPaths {
			if i == maxWolfTasksListed {
				names = append(names, fmt.Sprintf("+%d more", len(failedTaskPaths)-maxWolfTasksListed))
				break
			}
			names = append(names, strings.TrimSuffix(filepath.Base(p), filepath.Ext(p)))
		}
		fmt.Fprintf(&b, "\nFailed tasks: %s", strings.Join(names, ", "))
	}
	fmt.Fprintf(&b, "\n%s", wolfReplyInstruction)
	return b.String()
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// sessionName returns the launcher's name for the tracked session under key,
// or "" if there is none.
func (d *Daemon) sessionName(key string) string {
	d.sessionsMu.Lock()
	defer d.sessionsMu.Unlock()
	if e, ok := d.sessions[key]; ok {
		return e.sess.Name()
	}
	return ""
}
