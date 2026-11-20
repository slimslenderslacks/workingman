package postoffice

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/channels"
)

// Topic is the channels topic operator messages are sent on. With no route
// for it, the wildcard routes receive them (§5.1).
const Topic = "post-office"

// Dispatcher defaults (§5.2).
const (
	DefaultDispatchPoll        = DefaultInboxPoll
	DefaultDispatchSendTimeout = 20 * time.Second
	closingNoteRunes           = 200
)

// DefaultBackoff is the wait before each retry of a failed send; after the
// last one the delivery is recorded as failed.
var DefaultBackoff = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute}

// Outbound is the channel registry as the Dispatcher uses it.
// *channels.Registry implements it.
type Outbound interface {
	RoutesFor(topic string) []channels.Route
	Send(ctx context.Context, channel string, msg channels.OutboundMessage) (string, error)
	Get(name string) (channels.Channel, bool)
}

// Auditor records audit events; *audit.Logger implements it.
type Auditor interface {
	Log(event string, kv ...string)
}

// DispatcherConfig configures a Dispatcher. Store and Out are required.
type DispatcherConfig struct {
	Store *Store
	Out   Outbound
	// Index records each sent request so a reply to it reaches the router's
	// post-office branch. May be nil (replies then go to the workingman agent).
	Index channels.ConversationIndex
	Audit Auditor
	// Poll is the fallback rescan interval; zero means DefaultDispatchPoll.
	Poll time.Duration
	// Backoff overrides DefaultBackoff.
	Backoff []time.Duration
	// SendTimeout bounds one send; zero means DefaultDispatchSendTimeout.
	SendTimeout time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Dispatcher fans operator-addressed messages out to every active channel
// and, once a request is closed, tells the other channels (§5.2). It is the
// only writer of deliveries/. It runs inside the orch daemon.
type Dispatcher struct {
	cfg DispatcherConfig
	mu  sync.Mutex // one pass at a time
}

// NewDispatcher returns a Dispatcher; call Run to start it.
func NewDispatcher(cfg DispatcherConfig) (*Dispatcher, error) {
	if cfg.Store == nil || cfg.Out == nil {
		return nil, errors.New("postoffice: dispatcher needs a Store and an Outbound")
	}
	if cfg.Poll <= 0 {
		cfg.Poll = DefaultDispatchPoll
	}
	if cfg.Backoff == nil {
		cfg.Backoff = DefaultBackoff
	}
	if cfg.SendTimeout <= 0 {
		cfg.SendTimeout = DefaultDispatchSendTimeout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Dispatcher{cfg: cfg}, nil
}

// Run passes over the store at start (so nothing sent or closed while the
// daemon was down is missed), then on every inbox or done/ change and every
// Poll (for retries), until ctx is done.
func (d *Dispatcher) Run(ctx context.Context) {
	changes := d.cfg.Store.Watch(ctx, d.cfg.Poll, DirInbox, DirDone)
	tick := time.NewTicker(d.cfg.Poll)
	defer tick.Stop()
	d.Sync(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-changes:
			if !ok {
				return
			}
		case <-tick.C:
		}
		d.Sync(ctx)
	}
}

// Sync makes one delivery pass (send new operator messages, retry failed
// sends) and one closing pass (closing notes for finalised requests).
func (d *Dispatcher) Sync(ctx context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deliverPass(ctx)
	d.closePass(ctx)
}

func (d *Dispatcher) now() time.Time { return d.cfg.Now().UTC() }

func (d *Dispatcher) log(event string, kv ...string) {
	if d.cfg.Audit != nil {
		d.cfg.Audit.Log("postoffice_"+event, kv...)
	}
}

// --- delivery ----------------------------------------------------------------

func (d *Dispatcher) deliverPass(ctx context.Context) {
	s := d.cfg.Store
	msgs, err := s.ListOpen()
	if err != nil {
		d.log("dispatch_error", "err", err.Error())
		return
	}
	now := d.now()
	for _, m := range msgs {
		if ctx.Err() != nil {
			return
		}
		if !m.ToOperator() {
			continue // agent recipients use next_message, never channels
		}
		rec, ok, err := s.Deliveries(m.ID)
		if err != nil {
			d.log("dispatch_error", "id", m.ID, "err", err.Error())
			continue
		}
		if !ok {
			if m.Expired(now) {
				continue
			}
			if _, claimed, _ := s.Claimed(m.ID); claimed {
				continue // closed (say in the TUI) before we saw it
			}
			routes := d.routesFor(m.Kind)
			if len(routes) == 0 {
				continue // a later daemon with routes will pick it up
			}
			rec = DeliveryRecord{Version: DeliveriesVersion, ID: m.ID, Kind: m.Kind}
			for _, rt := range routes {
				rec.Deliveries = append(rec.Deliveries, Delivery{Channel: rt.Channel, Chat: rt.Chat, Status: DeliveryPending})
			}
		}
		if rec.ClosedNotified != nil || !hasDue(rec, now, DeliveryPending) {
			continue
		}
		if _, claimed, _ := s.Claimed(m.ID); claimed || m.Expired(now) {
			continue // the closing pass marks the rest skipped
		}
		d.sendPending(ctx, m, &rec, now)
		if err := s.putDeliveries(rec); err != nil {
			d.log("dispatch_error", "id", m.ID, "err", err.Error())
		}
	}
}

// routesFor returns the post-office routes a message of kind goes to:
// notifications skip routes with notify: false.
func (d *Dispatcher) routesFor(kind Kind) []channels.Route {
	var out []channels.Route
	seen := map[channels.Route]bool{}
	for _, rt := range d.cfg.Out.RoutesFor(Topic) {
		if kind == KindNotification && !rt.Notifies() {
			continue
		}
		if k := rt.Key(); !seen[k] {
			seen[k] = true
			out = append(out, rt)
		}
	}
	return out
}

func hasDue(rec DeliveryRecord, now time.Time, status string) bool {
	for _, dl := range rec.Deliveries {
		if dl.Status == status && !now.Before(dl.NextAttempt) {
			return true
		}
	}
	return false
}

// sendPending sends m to every due pending delivery, concurrently, so a slow
// channel cannot hold up the others, and records each outcome in rec.
func (d *Dispatcher) sendPending(ctx context.Context, m Message, rec *DeliveryRecord, now time.Time) {
	text := RenderMessage(m, now)
	var wg sync.WaitGroup
	for i := range rec.Deliveries {
		dl := &rec.Deliveries[i]
		if dl.Status != DeliveryPending || now.Before(dl.NextAttempt) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids, err := d.sendText(ctx, dl.Channel, channels.OutboundMessage{ChatID: dl.Chat, Text: text, Topic: Topic})
			dl.Attempts++
			if err != nil {
				dl.LastError = audit.Redact(err.Error())
				if next, ok := d.retryAt(dl.Attempts, now); ok {
					dl.NextAttempt = next
				} else {
					dl.Status = DeliveryFailed
					dl.NextAttempt = time.Time{}
				}
				d.log("delivery_error", "id", m.ID, "channel", dl.Channel, "attempt", fmt.Sprint(dl.Attempts), "final", fmt.Sprint(dl.Status == DeliveryFailed), "err", dl.LastError)
				return
			}
			sent := now
			dl.Status, dl.SentAt, dl.NextAttempt, dl.LastError = DeliverySent, &sent, time.Time{}, ""
			if len(ids) > 0 {
				dl.MessageID = ids[0]
			}
			if m.Kind == KindRequest && d.cfg.Index != nil {
				target := channels.ConversationTarget{
					Kind:        channels.ConversationKindPostOffice,
					WorkStream:  m.From.Display(),
					ProjectPath: m.From.Project,
					SessionKey:  m.ID,
				}
				for _, id := range ids {
					if err := d.cfg.Index.Register(dl.Channel, id, target); err != nil {
						d.log("index_error", "id", m.ID, "channel", dl.Channel, "err", err.Error())
					}
				}
			}
			d.log("delivered", "id", m.ID, "kind", string(m.Kind), "channel", dl.Channel)
		}()
	}
	wg.Wait()
}

// retryAt is when attempt+1 is due, or false when attempt was the last.
func (d *Dispatcher) retryAt(attempt int, now time.Time) (time.Time, bool) {
	if attempt > len(d.cfg.Backoff) {
		return time.Time{}, false
	}
	return now.Add(d.cfg.Backoff[attempt-1]), true
}

// sendText sends text as one message per piece of the channel's Chunker (if
// it has one) and returns the transport ids of the pieces.
func (d *Dispatcher) sendText(ctx context.Context, channel string, msg channels.OutboundMessage) ([]string, error) {
	pieces := []string{msg.Text}
	if ch, ok := d.cfg.Out.Get(channel); ok {
		if k, ok := ch.(channels.Chunker); ok {
			if p := k.ChunkMessage(msg.Text); len(p) > 0 {
				pieces = p
			}
		}
	}
	var ids []string
	for _, p := range pieces {
		sctx, cancel := context.WithTimeout(ctx, d.cfg.SendTimeout)
		piece := msg
		piece.Text = p
		id, err := d.cfg.Out.Send(sctx, channel, piece)
		cancel()
		if err != nil {
			return ids, err
		}
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// --- closing notes ---------------------------------------------------------

func (d *Dispatcher) closePass(ctx context.Context) {
	s := d.cfg.Store
	ids, err := s.listIDs(DirDeliveries)
	if err != nil {
		d.log("dispatch_error", "err", err.Error())
		return
	}
	now := d.now()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		rec, ok, err := s.Deliveries(id)
		if err != nil || !ok {
			if err != nil {
				d.log("dispatch_error", "id", id, "err", err.Error())
			}
			continue
		}
		if rec.ClosedNotified != nil {
			if now.Sub(*rec.ClosedNotified) > DefaultRetention {
				_ = removeIfExists(s.DeliveryPath(id)) // §3.4
			}
			continue
		}
		done, ok, err := s.Done(id)
		if err != nil {
			d.log("dispatch_error", "id", id, "err", err.Error())
			continue
		}
		if !ok {
			if _, _, err := s.Lookup(id); errors.Is(err, ErrNotFound) {
				// Pruned, or finalised away without a record: nothing to say.
				rec.ClosedNotified = &now
				d.put(rec)
			}
			continue // still open (or finalisation pending)
		}
		if d.closeOut(ctx, &rec, done, now) {
			d.put(rec)
		}
	}
}

func (d *Dispatcher) put(rec DeliveryRecord) {
	if err := d.cfg.Store.putDeliveries(rec); err != nil {
		d.log("dispatch_error", "id", rec.ID, "err", err.Error())
	}
}

// closeOut sends the due closing notes for a finalised message and reports
// whether rec changed.
func (d *Dispatcher) closeOut(ctx context.Context, rec *DeliveryRecord, done DoneRecord, now time.Time) bool {
	changed := false
	for i := range rec.Deliveries {
		if dl := &rec.Deliveries[i]; dl.Status == DeliveryPending {
			dl.Status, dl.NextAttempt = DeliverySkipped, time.Time{}
			changed = true
		}
	}
	res := done.Resolution
	kind := rec.Kind
	if done.Kind != "" {
		kind = done.Kind
	}
	if kind != KindRequest || done.Orphan {
		// Notifications get no closing note: it would be noise.
		rec.ClosedNotified = &now
		return true
	}

	note := ClosingNote(done.Message, res)
	var wg sync.WaitGroup
	for i := range rec.Deliveries {
		dl := &rec.Deliveries[i]
		if dl.Status != DeliverySent || dl.Closed != "" || now.Before(dl.NextAttempt) {
			continue
		}
		if answeredBy(res, *dl) {
			dl.Closed = CloseAnswerer
			changed = true
			continue
		}
		changed = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := d.sendClose(ctx, *dl, res, note)
			dl.CloseAttempts++
			if err == nil {
				dl.Closed, dl.NextAttempt, dl.LastError = CloseSent, time.Time{}, ""
				d.log("closed_notified", "id", rec.ID, "channel", dl.Channel, "outcome", string(res.Outcome))
				return
			}
			dl.LastError = audit.Redact(err.Error())
			if next, ok := d.retryAt(dl.CloseAttempts, now); ok {
				dl.NextAttempt = next
			} else {
				dl.Closed, dl.NextAttempt = CloseFailed, time.Time{}
			}
			d.log("close_error", "id", rec.ID, "channel", dl.Channel, "attempt", fmt.Sprint(dl.CloseAttempts), "final", fmt.Sprint(dl.Closed == CloseFailed), "err", dl.LastError)
		}()
	}
	wg.Wait()

	for _, dl := range rec.Deliveries {
		if dl.Status == DeliverySent && dl.Closed == "" {
			return changed // retries outstanding
		}
	}
	rec.ClosedNotified = &now
	return true
}

// answeredBy reports whether dl's chat gave the winning answer: the claim
// names its channel and, when the channel reported it, the chat.
func answeredBy(res Resolution, dl Delivery) bool {
	r := res.Responder
	return res.Outcome == OutcomeAnswered && r != nil && r.Via == dl.Channel && (r.Chat == "" || r.Chat == dl.Chat)
}

// sendClose tells one delivery its message closed: through the channel's
// Resolver when it has one, else as a reply to the original message.
func (d *Dispatcher) sendClose(ctx context.Context, dl Delivery, res Resolution, note string) error {
	if ch, ok := d.cfg.Out.Get(dl.Channel); ok {
		if rv, ok := ch.(channels.Resolver); ok {
			sctx, cancel := context.WithTimeout(ctx, d.cfg.SendTimeout)
			defer cancel()
			return rv.Resolve(sctx, dl.Chat, dl.MessageID, channels.Resolution{
				Outcome: string(res.Outcome), Via: res.Via(), Note: note,
			})
		}
	}
	_, err := d.sendText(ctx, dl.Channel, channels.OutboundMessage{ChatID: dl.Chat, Text: note, ReplyToID: dl.MessageID, Topic: Topic})
	return err
}

// --- rendering ---------------------------------------------------------------

// RenderMessage is the channel text for an operator message (§5.5):
//
//	❓ [req r1t8w] task fix-ci (workingman-messaging)
//	Which bucket for the export?
//
//	…body…
//
//	Options: 1) staging  2) prod
//	Reply to this message, or send #r1t8w <answer>. Expires in 24h.
//
// Notifications use 📣 and omit the last two lines. Secrets are redacted.
func RenderMessage(m Message, now time.Time) string {
	var b strings.Builder
	if m.Kind == KindRequest {
		fmt.Fprintf(&b, "❓ [req %s] %s\n", m.ShortID(), m.From.Display())
	} else {
		fmt.Fprintf(&b, "📣 [note %s] %s\n", m.ShortID(), m.From.Display())
	}
	b.WriteString(audit.Redact(m.Subject))
	if body := strings.TrimSpace(m.Body); body != "" && body != strings.TrimSpace(m.Subject) {
		b.WriteString("\n\n")
		b.WriteString(audit.Redact(body))
	}
	if m.Kind != KindRequest {
		return b.String()
	}
	b.WriteString("\n\n")
	if len(m.Options) > 0 {
		b.WriteString("Options:")
		for i, o := range m.Options {
			sep := "  "
			if i == 0 {
				sep = " "
			}
			fmt.Fprintf(&b, "%s%d) %s", sep, i+1, o)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Reply to this message, or send #%s <answer>. Expires in %s.", m.ShortID(), HumanDuration(m.ExpiresAt.Sub(now)))
	return b.String()
}

// ClosingNote is what the other channels are told when a request closes
// (§5.3).
func ClosingNote(m Message, r Resolution) string {
	tag := "[req " + ShortID(r.ID) + "]"
	switch r.Outcome {
	case OutcomeAnswered:
		via := r.Via()
		if via == "" {
			via = "another channel"
		}
		text := r.Response
		if r.Option != "" {
			text = r.Option
		}
		return fmt.Sprintf("✅ %s Answered via %s: %s", tag, via, cutRunes(audit.Redact(strings.TrimSpace(text)), closingNoteRunes))
	case OutcomeCancelled:
		if reason := strings.TrimSpace(r.Reason); reason != "" {
			return fmt.Sprintf("⌛ %s No longer needed — %s", tag, cutRunes(audit.Redact(reason), closingNoteRunes))
		}
		return fmt.Sprintf("⌛ %s No longer needed — the agent cancelled the request", tag)
	case OutcomeExpired:
		return fmt.Sprintf("⌛ %s Expired — the agent stopped waiting", tag)
	}
	return fmt.Sprintf("%s Closed (%s)", tag, r.Outcome)
}

func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// HumanDuration renders d coarsely for humans: "45m", "3h", "2d". Anything
// under a minute is "1m".
func HumanDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int((d+12*time.Hour)/(24*time.Hour)))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int((d+30*time.Minute)/time.Hour))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return "1m"
}
