package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/slimslenderslacks/work/internal/postoffice"
)

// The inbox (docs/messaging-design.md §7) shows the post-office messages
// agents sent: requests and notifications for the operator, which the
// operator answers or dismisses here, and read-only rows for messages
// addressed to other agents (Step-Up). It reads the post-office store
// directly, so it works with or without a daemon and with or without
// channels; an answer is a Store.Claim, which post-office then delivers.

// InboxStore is the part of the post-office store the inbox uses.
// *postoffice.Store implements it; tests use a fake.
type InboxStore interface {
	// ListOpen returns every inbox record, oldest first.
	ListOpen() ([]postoffice.Message, error)
	// Deliveries returns the Dispatcher's send record for a message.
	Deliveries(id string) (postoffice.DeliveryRecord, bool, error)
	// Claimed returns a message's pending claim (answered, but not yet
	// finalised by post-office).
	Claimed(id string) (postoffice.Resolution, bool, error)
	// Claim closes a message; it returns ErrAlreadyClosed and the winning
	// resolution when another claim got there first.
	Claim(r postoffice.Resolution) (postoffice.Resolution, error)
}

// InboxItem is one open message as the inbox shows it.
type InboxItem struct {
	Message postoffice.Message
	// DeliveredTo lists the channels the Dispatcher sent it to.
	DeliveredTo []string
	// Claim is set once the message has been answered or dismissed (here or
	// on a channel) and post-office has not yet finalised it; the row stays,
	// greyed, until the inbox file goes.
	Claim *postoffice.Resolution
	// Stalled is set when Claim has waited longer than inboxStallAfter,
	// which means post-office is probably not running.
	Stalled bool
	// Expired is set once the message's expiry has passed, even before
	// post-office claims it, so nothing looks answerable when it is not.
	Expired bool
}

// open reports whether the item can still be acted on.
func (it InboxItem) open() bool { return it.Claim == nil && !it.Expired }

// operatorRequest reports whether the item is an operator request that is
// still waiting for an answer: what the badge's "?" and the bell are for.
func (it InboxItem) operatorRequest() bool {
	return it.open() && it.Message.ToOperator() && it.Message.Kind == postoffice.KindRequest
}

// inboxStallAfter is how long a claim may wait for post-office to finalise
// it before the row says the answer is saved but not delivered (§7.5).
const inboxStallAfter = 10 * time.Second

// inboxMsg carries a feed snapshot into Update.
type inboxMsg struct{ items []InboxItem }

// WatchInbox polls store on interval and emits the open messages whenever
// they differ from the previous snapshot. The first snapshot is emitted
// immediately. The channel closes when ctx is cancelled. interval <= 0 falls
// back to one second. Like WatchProjects the channel is unbuffered.
func WatchInbox(ctx context.Context, store InboxStore, interval time.Duration) <-chan []InboxItem {
	return watchInbox(ctx, store, interval, time.Now)
}

func watchInbox(ctx context.Context, store InboxStore, interval time.Duration, now func() time.Time) <-chan []InboxItem {
	if interval <= 0 {
		interval = time.Second
	}
	out := make(chan []InboxItem)
	go func() {
		defer close(out)
		snap, _ := scanInbox(store, now())
		select {
		case out <- snap:
		case <-ctx.Done():
			return
		}
		prev := snap
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				snap, err := scanInbox(store, now())
				if err != nil || reflect.DeepEqual(prev, snap) {
					continue
				}
				prev = snap
				select {
				case out <- snap:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

// scanInbox reads the open messages with their deliveries and pending
// claims. A message whose side files can't be read is still listed.
func scanInbox(store InboxStore, now time.Time) ([]InboxItem, error) {
	msgs, err := store.ListOpen()
	if err != nil {
		return nil, err
	}
	items := make([]InboxItem, 0, len(msgs))
	for _, m := range msgs {
		it := InboxItem{Message: m, Expired: m.Expired(now)}
		if d, ok, err := store.Deliveries(m.ID); err == nil && ok {
			it.DeliveredTo = d.DeliveredTo()
		}
		if c, ok, err := store.Claimed(m.ID); err == nil && ok {
			it.Claim = &c
			it.Stalled = now.Sub(c.At) > inboxStallAfter
		}
		items = append(items, it)
	}
	return items, nil
}

func waitForInbox(ch <-chan []InboxItem) tea.Cmd {
	return func() tea.Msg {
		items, ok := <-ch
		if !ok {
			return nil
		}
		return inboxMsg{items: items}
	}
}

// bellOut is where ringBell writes BEL; stderr for the same reason as
// clipboardOut. A var so tests can capture it.
var bellOut io.Writer = os.Stderr

// EnvInboxBell turns the new-request bell off when set to "0".
const EnvInboxBell = "WORKINGMAN_TUI_BELL"

func ringBell() tea.Msg {
	_, _ = io.WriteString(bellOut, "\a")
	return nil
}

// applyInbox takes a feed snapshot: it orders the rows, keeps the selection
// on the same message (or the row that took its place), and rings the bell
// once when an operator request arrives that wasn't in the previous
// snapshot. The first snapshot never rings: those requests aren't new.
func (m model) applyInbox(items []InboxItem) (model, tea.Cmd) {
	items = orderInbox(items)
	var cmd tea.Cmd
	if m.inboxLoaded && m.inboxBell {
		seen := map[string]bool{}
		for _, it := range m.inbox {
			seen[it.Message.ID] = true
		}
		for _, it := range items {
			if it.operatorRequest() && !seen[it.Message.ID] {
				cmd = ringBell
				break
			}
		}
	}
	prevIdx := inboxIndex(m.inbox, m.inboxSel)
	m.inbox = items
	m.inboxLoaded = true
	switch i := inboxIndex(items, m.inboxSel); {
	case len(items) == 0:
		m.inboxSel = ""
	case i >= 0:
		// Still listed, but it may have closed elsewhere (answered on a
		// channel, cancelled, expired) while a reply was being typed.
		if m.inboxTyping && !items[i].open() {
			m.inboxTyping = false
			m.inboxReply.reset()
			m.inboxErr = inboxClosedText(items[i])
		}
	default:
		// The selected message closed: select the one now in its place.
		m.inboxSel = items[min(max(prevIdx, 0), len(items)-1)].Message.ID
		m.inboxScroll = 0
		m.inboxTyping = false
		m.inboxReply.reset()
	}
	return m, cmd
}

// orderInbox puts the rows in display order: operator requests, then
// operator notifications, then messages to agents; oldest first in each
// group (ids sort by creation time, and ListOpen returns them that way).
func orderInbox(items []InboxItem) []InboxItem {
	group := func(it InboxItem) int {
		switch {
		case !it.Message.ToOperator():
			return 2
		case it.Message.Kind == postoffice.KindRequest:
			return 0
		}
		return 1
	}
	out := make([]InboxItem, 0, len(items))
	for g := range 3 {
		for _, it := range items {
			if group(it) == g {
				out = append(out, it)
			}
		}
	}
	return out
}

func inboxIndex(items []InboxItem, id string) int {
	for i, it := range items {
		if it.Message.ID == id {
			return i
		}
	}
	return -1
}

func (m model) selectedInboxItem() (InboxItem, bool) {
	if i := inboxIndex(m.inbox, m.inboxSel); i >= 0 {
		return m.inbox[i], true
	}
	return InboxItem{}, false
}

// inboxBadge is the inbox indicator shown in every mode (§7.2): nothing when
// the inbox has nothing open, "✉ N" (faint) when only notifications or
// agent-addressed messages are, and "✉ N ?" (bold yellow, the
// interactive-session colour) when N operator requests wait for an answer.
func (m model) inboxBadge() string {
	open, requests := 0, 0
	for _, it := range m.inbox {
		if !it.open() {
			continue
		}
		open++
		if it.operatorRequest() {
			requests++
		}
	}
	switch {
	case requests > 0:
		return sessionRowInteractiveStyle.Render(fmt.Sprintf("✉ %d ?", requests))
	case open > 0:
		return hintStyle.Render(fmt.Sprintf("✉ %d", open))
	}
	return ""
}

// withInboxBadge prefixes a line with the badge, when there is one.
func (m model) withInboxBadge(s string) string {
	if b := m.inboxBadge(); b != "" {
		return b + "  " + s
	}
	return s
}

// canOpenInbox reports whether `I` opens the inbox: a store is wired in and
// no text is being typed (in ACP compose `I` is just a character).
func (m model) canOpenInbox() bool {
	return m.inboxCh != nil && m.mode != modeInbox && !(m.showACP && m.acpComposing)
}

// openInbox switches to the inbox view, remembering the mode to return to.
func (m model) openInbox() model {
	m.inboxPrevMode = m.mode
	m.mode = modeInbox
	m.pendingKey = ""
	m.inboxErr = ""
	if _, ok := m.selectedInboxItem(); !ok && len(m.inbox) > 0 {
		m.inboxSel = m.inbox[0].Message.ID
		m.inboxScroll = 0
	}
	return m
}

// handleInboxKey processes a keystroke in the inbox view (§7.4).
func (m model) handleInboxKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.inboxTyping {
		return m.handleInboxReplyKey(msg)
	}
	m.inboxErr = ""
	k := msg.String()
	switch k {
	case "I", "esc":
		m.mode = m.inboxPrevMode
		return m, nil
	case "j":
		m = m.moveInboxSelection(1)
	case "k":
		m = m.moveInboxSelection(-1)
	case "ctrl+f":
		m.inboxScroll += acpPageRows(m.inboxLayout().bodyH)
	case "ctrl+b":
		m.inboxScroll = max(m.inboxScroll-acpPageRows(m.inboxLayout().bodyH), 0)
	case "y":
		if it, ok := m.selectedInboxItem(); ok {
			if err := copyToClipboard(it.Message.Body); err != nil {
				m.inboxErr = "copy: " + err.Error()
			}
		}
	case "enter":
		it, ok := m.selectedInboxItem()
		if !ok {
			break
		}
		if why := inboxNotAnswerable(it); why != "" {
			m.inboxErr = why
			break
		}
		m.inboxTyping = true
	case "d":
		it, ok := m.selectedInboxItem()
		if !ok {
			break
		}
		switch {
		case !it.Message.ToOperator():
			m.inboxErr = "read-only: addressed to " + it.Message.Recipient
		case it.Message.Kind != postoffice.KindNotification:
			m.inboxErr = "only notifications can be dismissed; answer the request, or let it expire"
		case !it.open():
			m.inboxErr = inboxClosedText(it)
		default:
			m = m.claimInbox(it, postoffice.Resolution{Outcome: postoffice.OutcomeDismissed})
		}
	default:
		if n, err := strconv.Atoi(k); err == nil && n >= 1 && n <= 9 {
			it, ok := m.selectedInboxItem()
			if !ok {
				break
			}
			if why := inboxNotAnswerable(it); why != "" {
				m.inboxErr = why
				break
			}
			if n > len(it.Message.Options) {
				m.inboxErr = fmt.Sprintf("no option %d (enter to type an answer)", n)
				break
			}
			opt := it.Message.Options[n-1]
			m = m.claimInbox(it, postoffice.Resolution{Outcome: postoffice.OutcomeAnswered, Response: opt, Option: opt})
		}
	}
	return m, nil
}

// handleInboxReplyKey handles keys while typing a reply: the shared
// lineInput takes the text, enter submits, esc stops typing (keeping the
// draft) and ctrl+f/ctrl+b still scroll the body.
func (m model) handleInboxReplyKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.inboxErr = ""
	switch msg.String() {
	case "esc":
		m.inboxTyping = false
		return m, nil
	case "ctrl+f":
		m.inboxScroll += acpPageRows(m.inboxLayout().bodyH)
		return m, nil
	case "ctrl+b":
		m.inboxScroll = max(m.inboxScroll-acpPageRows(m.inboxLayout().bodyH), 0)
		return m, nil
	}
	if !m.inboxReply.update(msg) {
		return m, nil
	}
	text := m.inboxReply.value()
	if text == "" {
		return m, nil
	}
	it, ok := m.selectedInboxItem()
	if !ok {
		m.inboxTyping = false
		return m, nil
	}
	m = m.claimInbox(it, postoffice.Resolution{Outcome: postoffice.OutcomeAnswered, Response: text})
	if m.inboxErr == "" {
		m.inboxReply.reset()
	}
	return m, nil
}

// claimInbox closes it with r through the store (§7.5). On success the row
// shows "sending…" at once, without waiting for the next feed snapshot;
// when another claim won, the reason goes on the reply line.
func (m model) claimInbox(it InboxItem, r postoffice.Resolution) model {
	if m.inboxStore == nil {
		m.inboxErr = "no post-office store"
		return m
	}
	r.ID = it.Message.ID
	r.Responder = &postoffice.Responder{Kind: postoffice.ResponderOperator, Via: postoffice.ViaTUI}
	won, err := m.inboxStore.Claim(r)
	switch {
	case errors.Is(err, postoffice.ErrAlreadyClosed):
		m.inboxErr = resolutionText(won)
		m.inboxTyping = false
	case errors.Is(err, postoffice.ErrNotFound):
		m.inboxErr = "this message is already closed"
		m.inboxTyping = false
	case err != nil:
		m.inboxErr = err.Error()
		return m
	default:
		m.inboxTyping = false
	}
	if i := inboxIndex(m.inbox, it.Message.ID); i >= 0 && won.ID != "" {
		// Grey the row now, with whichever claim won. Copy-on-write: m.inbox
		// may share its backing array with an earlier model value.
		items := append([]InboxItem(nil), m.inbox...)
		items[i].Claim = &won
		m.inbox = items
	}
	return m
}

// inboxNotAnswerable says why the item can't be answered, or "" if it can.
func inboxNotAnswerable(it InboxItem) string {
	switch {
	case !it.Message.ToOperator():
		return "read-only: addressed to " + it.Message.Recipient
	case it.Message.Kind != postoffice.KindRequest:
		return "notifications need no reply (d: dismiss)"
	case !it.open():
		return inboxClosedText(it)
	}
	return ""
}

// inboxClosedText says why a closing or expired item can't be acted on.
func inboxClosedText(it InboxItem) string {
	if it.Claim != nil {
		return resolutionText(*it.Claim)
	}
	return "this message has expired"
}

// claimSummary is a short form of a claim for a row: "answered via slack",
// "cancelled", "expired".
func claimSummary(r postoffice.Resolution) string {
	if via := r.Via(); via != "" {
		return string(r.Outcome) + " via " + via
	}
	return string(r.Outcome)
}

// resolutionText describes a claim that closed a message.
func resolutionText(r postoffice.Resolution) string {
	switch r.Outcome {
	case postoffice.OutcomeAnswered:
		if via := r.Via(); via != "" {
			return "already answered via " + via
		}
		return "already answered"
	case postoffice.OutcomeCancelled:
		if r.Reason != "" {
			return "the sender cancelled this request: " + r.Reason
		}
		return "the sender cancelled this request"
	case postoffice.OutcomeExpired:
		return "this message has expired"
	case postoffice.OutcomeDismissed:
		return "already dismissed"
	}
	return "already closed"
}

func (m model) moveInboxSelection(delta int) model {
	if len(m.inbox) == 0 {
		return m
	}
	i := inboxIndex(m.inbox, m.inboxSel) + delta
	i = min(max(i, 0), len(m.inbox)-1)
	if id := m.inbox[i].Message.ID; id != m.inboxSel {
		m.inboxSel = id
		m.inboxScroll = 0
		m.inboxReply.reset()
	}
	return m
}

// --- rendering ----------------------------------------------------------------

// inboxLayout is the inbox view's row budget. The view is the ACP view's
// layout: a title row, a rule, the message list, a rule, the selected
// message's body, a rule, the reply line and the key hint. Like the ACP view
// it stays one row short of the terminal (see acpBodySize).
type inboxLayout struct {
	listH, bodyH int
}

// inboxChromeRows is the title, three rules, the reply line and the hint.
const inboxChromeRows = 6

func (m model) inboxLayout() inboxLayout {
	avail := max(m.height-inboxChromeRows-1, 2)
	list, _ := m.inboxListLines(m.width)
	want := len(list)
	listH := min(want, max(avail/2, 1))
	if want == 0 {
		listH = 1 // "(no messages)"
	}
	return inboxLayout{listH: listH, bodyH: max(avail-listH, 1)}
}

// inboxListLines renders the message list, two rows per message, with a
// heading above the agent-addressed rows. It also reports, for the selected
// message, the index of its first row (-1 when nothing is selected).
func (m model) inboxListLines(width int) (lines []string, selLine int) {
	now := time.Now()
	selLine = -1
	headed := false
	for _, it := range m.inbox {
		if !it.Message.ToOperator() && !headed {
			headed = true
			lines = append(lines, dimStyle.Render(inboxRule(" To agents (read-only) ", width)))
		}
		selected := it.Message.ID == m.inboxSel
		if selected {
			selLine = len(lines)
		}
		meta, subject := inboxRow(it, width, now)
		switch {
		case selected:
			meta = sessionRowSelectedStyle.Render(padToWidth(meta, width))
			subject = sessionRowSelectedStyle.Render(padToWidth(subject, width))
		case !it.open():
			meta, subject = dimStyle.Render(meta), dimStyle.Render(subject)
		case it.operatorRequest():
			meta = sessionRowInteractiveStyle.Render(meta)
		}
		lines = append(lines, meta, subject)
	}
	return lines, selLine
}

// inboxRow is one message's two plain rows: a meta row (glyph, short id,
// sender, and right-aligned age and state) and the subject.
func inboxRow(it InboxItem, width int, now time.Time) (meta, subject string) {
	msg := it.Message
	glyph := "·"
	switch {
	case !msg.ToOperator():
		glyph = "→"
	case msg.Kind == postoffice.KindRequest:
		glyph = "?"
	}
	who := msg.From.Display()
	if !msg.ToOperator() {
		who += " → " + msg.Recipient
	}
	left := " " + glyph + " " + msg.ShortID() + "  " + who
	right := formatAge(now.Sub(msg.CreatedAt))
	if state := inboxRowState(it); state != "" {
		right += "  " + state
	}
	right += " "
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		left = truncate(left, max(width-lipgloss.Width(right)-1, 1))
		gap = max(width-lipgloss.Width(left)-lipgloss.Width(right), 1)
	}
	meta = truncate(left+strings.Repeat(" ", gap)+right, width)
	subject = truncate("   "+msg.Subject, width)
	return meta, subject
}

// inboxRowState is the right-hand state of a row: what is happening to a
// closing message, else where it was delivered. A claim made here is
// "sending…" until post-office finalises it; one made elsewhere says how it
// closed.
func inboxRowState(it InboxItem) string {
	switch {
	case it.Claim != nil && it.Stalled:
		return string(it.Claim.Outcome) + " — waiting for post-office"
	case it.Claim != nil && it.Claim.Via() == postoffice.ViaTUI:
		return "sending…"
	case it.Claim != nil:
		return claimSummary(*it.Claim)
	case it.Expired:
		return "expired"
	case !it.Message.ToOperator():
		if it.Message.ReceivedAt != nil {
			return "received"
		}
		return ""
	}
	return strings.Join(it.DeliveredTo, ",")
}

// inboxBodyLines is the selected message's body pane: a from/expiry line,
// the body wrapped to width, and the options.
func inboxBodyLines(it InboxItem, width int, now time.Time) []string {
	msg := it.Message
	head := "from " + msg.From.Display()
	if !msg.ToOperator() {
		head += " to " + msg.Recipient
	}
	if it.open() {
		head += "  •  expires in " + formatAge(msg.ExpiresAt.Sub(now))
	}
	lines := []string{dimStyle.Render(truncate(head, width)), ""}
	for _, l := range strings.Split(strings.TrimRight(msg.Body, "\n"), "\n") {
		lines = append(lines, acpWrapLines(l, width)...)
	}
	if len(msg.Options) > 0 {
		opts := make([]string, len(msg.Options))
		for i, o := range msg.Options {
			opts[i] = fmt.Sprintf("%d) %s", i+1, o)
		}
		lines = append(lines, "")
		lines = append(lines, acpWrapLines("Options: "+strings.Join(opts, "  "), width)...)
	}
	return lines
}

// inboxRule is a full-width horizontal rule, with an optional label.
func inboxRule(label string, width int) string {
	if width <= 0 {
		return ""
	}
	if label == "" {
		return strings.Repeat("─", width)
	}
	s := "──" + label
	return truncate(s+strings.Repeat("─", max(width-lipgloss.Width(s), 0)), width)
}

// renderInboxView paints the full-screen inbox (§7.4). It is what View
// returns while m.mode is modeInbox.
func (m model) renderInboxView() string {
	width, height := m.width, m.height
	now := time.Now()
	l := m.inboxLayout()

	title := titleStyle.Render("Inbox")
	if b := m.inboxBadge(); b != "" {
		title += "  " + b
	}
	closeHint := hintStyle.Render("I/esc close")
	gap := max(width-lipgloss.Width(title)-lipgloss.Width(closeHint)-1, 1)
	header := lipgloss.NewStyle().MaxWidth(width).Render(" " + title + strings.Repeat(" ", gap) + closeHint)
	rule := dimStyle.Render(inboxRule("", width))

	// The list, windowed so the selected message's two rows are on screen.
	list, selLine := m.inboxListLines(width)
	if len(list) == 0 {
		list = []string{dimStyle.Render(" (no messages)")}
	}
	start, end := centerWindow(len(list), selLine+1, l.listH)
	list = list[start:end]

	// The body, scrolled; inboxScroll is clamped here so a page past the end
	// stops at the last page.
	var body []string
	it, ok := m.selectedInboxItem()
	if ok {
		body = inboxBodyLines(it, max(width-1, 1), now)
		maxTop := max(len(body)-l.bodyH, 0)
		top := min(m.inboxScroll, maxTop)
		body = body[top:min(top+l.bodyH, len(body))]
	}
	for len(body) < l.bodyH {
		body = append(body, "")
	}

	parts := []string{header, rule}
	parts = append(parts, list...)
	parts = append(parts, rule)
	parts = append(parts, body...)
	parts = append(parts, rule, m.renderInboxReplyLine(it, ok, width))

	hint := "j/k select  •  enter reply  •  1-9 pick option  •  d dismiss  •  ^f/^b scroll  •  y copy  •  I/esc close"
	if m.inboxTyping {
		hint = "enter: send  •  esc: stop typing  •  ctrl+u: clear  •  ^f/^b: scroll"
	}
	parts = append(parts, hintStyle.Render(truncate(hint, width)))

	view := lipgloss.JoinVertical(lipgloss.Left, parts...)
	if lines := strings.Split(view, "\n"); len(lines) > height {
		view = strings.Join(lines[:height], "\n")
	}
	return view
}

// renderInboxReplyLine is the row under the body: the reply being typed, an
// error from the last action, or a prompt for what the selected row allows.
func (m model) renderInboxReplyLine(it InboxItem, ok bool, width int) string {
	switch {
	case m.inboxErr != "":
		return statusErrStyle.Render(truncate(m.inboxErr, width))
	case m.inboxTyping:
		return m.inboxReply.view(width)
	case !ok:
		return ""
	}
	prompt := ""
	switch {
	case !it.Message.ToOperator():
		prompt = "(read-only: addressed to " + it.Message.Recipient + ")"
	case !it.open():
		prompt = "(" + inboxRowState(it) + ")"
	case it.Message.Kind == postoffice.KindNotification:
		prompt = "(d to dismiss)"
	case len(it.Message.Options) > 0:
		prompt = "(enter to reply, or 1-" + strconv.Itoa(len(it.Message.Options)) + " to pick an option)"
	default:
		prompt = "(enter to reply)"
	}
	if m.inboxReply.draft != "" && it.operatorRequest() {
		prompt = "(enter to keep typing: " + m.inboxReply.draft + ")"
	}
	return dimStyle.Render(truncate(prompt, width))
}
