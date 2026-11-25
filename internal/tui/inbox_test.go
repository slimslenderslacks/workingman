package tui

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/slimslenderslacks/work/internal/postoffice"
)

// fakeInbox is an in-memory InboxStore. Claim follows the real store's
// first-claim-wins rule; a claimed message stays listed until finalize.
type fakeInbox struct {
	mu         sync.Mutex
	msgs       []postoffice.Message
	deliveries map[string][]string
	claims     map[string]postoffice.Resolution
	claimCalls []postoffice.Resolution
}

func newFakeInbox(msgs ...postoffice.Message) *fakeInbox {
	return &fakeInbox{msgs: msgs, deliveries: map[string][]string{}, claims: map[string]postoffice.Resolution{}}
}

func (f *fakeInbox) ListOpen() ([]postoffice.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]postoffice.Message(nil), f.msgs...), nil
}

func (f *fakeInbox) Deliveries(id string) (postoffice.DeliveryRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	chans, ok := f.deliveries[id]
	if !ok {
		return postoffice.DeliveryRecord{}, false, nil
	}
	r := postoffice.DeliveryRecord{ID: id}
	for _, c := range chans {
		r.Deliveries = append(r.Deliveries, postoffice.Delivery{Channel: c, Status: postoffice.DeliverySent})
	}
	return r, true, nil
}

func (f *fakeInbox) Claimed(id string) (postoffice.Resolution, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.claims[id]
	return r, ok, nil
}

func (f *fakeInbox) Claim(r postoffice.Resolution) (postoffice.Resolution, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claimCalls = append(f.claimCalls, r)
	if w, ok := f.claims[r.ID]; ok {
		return w, fmt.Errorf("claim %s: %w", r.ID, postoffice.ErrAlreadyClosed)
	}
	if r.At.IsZero() {
		r.At = time.Now().UTC()
	}
	f.claims[r.ID] = r
	return r, nil
}

// finalize drops a message from the inbox, as post-office does.
func (f *fakeInbox) finalize(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.claims, id)
	for i, m := range f.msgs {
		if m.ID == id {
			f.msgs = append(f.msgs[:i], f.msgs[i+1:]...)
			return
		}
	}
}

var taskSender = postoffice.Sender{Agent: "task", Sandbox: "wm-p-fix-ci", Project: "/o/wm/project.yaml", Task: "fix-ci"}

func newInboxMsg(t *testing.T, d postoffice.Draft) postoffice.Message {
	t.Helper()
	if d.From.Sandbox == "" {
		d.From = taskSender
	}
	m, err := postoffice.NewMessage(d, time.Now().Add(-3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// inboxModel returns a model wired to store, sized, with one feed snapshot
// applied.
func inboxModel(t *testing.T, store *fakeInbox) model {
	t.Helper()
	m := newModel(nil, nil, nil)
	m.width, m.height = 100, 30
	m.inboxStore = store
	m.inboxCh = make(chan []InboxItem)
	return refreshInbox(t, m, store)
}

// refreshInbox applies a fresh feed snapshot, as WatchInbox would.
func refreshInbox(t *testing.T, m model, store InboxStore) model {
	t.Helper()
	items, err := scanInbox(store, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	step, _ := m.Update(inboxMsg{items: items})
	return step.(model)
}

func pressI(m model) model { return key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("I")}) }

func TestInboxBadge(t *testing.T) {
	store := newFakeInbox()
	m := inboxModel(t, store)
	if b := m.inboxBadge(); b != "" {
		t.Errorf("empty inbox badge = %q, want none", b)
	}

	note := newInboxMsg(t, postoffice.Draft{Kind: postoffice.KindNotification, Subject: "Release tagged", Body: "v0.9.0"})
	toAgent := newInboxMsg(t, postoffice.Draft{Kind: postoffice.KindRequest, Recipient: "step-up", Body: "Need proxy.golang.org"})
	store.msgs = []postoffice.Message{note, toAgent}
	m = refreshInbox(t, m, store)
	if b := ansi.Strip(m.inboxBadge()); b != "✉ 2" {
		t.Errorf("badge with no operator request = %q, want ✉ 2", b)
	}

	req := newInboxMsg(t, postoffice.Draft{Kind: postoffice.KindRequest, Body: "Which bucket?"})
	req2 := newInboxMsg(t, postoffice.Draft{Kind: postoffice.KindRequest, Body: "Which region?"})
	store.msgs = append(store.msgs, req, req2)
	m = refreshInbox(t, m, store)
	if b := ansi.Strip(m.inboxBadge()); b != "✉ 2 ?" {
		t.Errorf("badge with two operator requests = %q, want ✉ 2 ?", b)
	}

	// An answered request no longer counts, while it waits to be finalised.
	if _, err := store.Claim(postoffice.Resolution{ID: req.ID, Outcome: postoffice.OutcomeAnswered}); err != nil {
		t.Fatal(err)
	}
	m = refreshInbox(t, m, store)
	if b := ansi.Strip(m.inboxBadge()); b != "✉ 1 ?" {
		t.Errorf("badge after one answer = %q, want ✉ 1 ?", b)
	}
}

func TestInboxBadgeShownInEveryMode(t *testing.T) {
	store := newFakeInbox(newInboxMsg(t, postoffice.Draft{Kind: postoffice.KindRequest, Body: "Which bucket?"}))
	m := inboxModel(t, store)
	m.projectRoot = t.TempDir()

	if f := ansi.Strip(m.renderFooter()); !strings.HasPrefix(f, "✉ 1 ?") {
		t.Errorf("footer should start with the badge; got %q", f)
	}
	m.statusMsg = "something went wrong"
	if f := ansi.Strip(m.renderFooter()); !strings.HasPrefix(f, "✉ 1 ?") || !strings.Contains(f, "something went wrong") {
		t.Errorf("badge must stay visible next to a status message; got %q", f)
	}
	m.statusMsg = ""

	m.acpCh = make(chan acpTabEvent)
	m.showACP = true
	if header := strings.SplitN(ansi.Strip(m.View()), "\n", 2)[0]; !strings.Contains(header, "ACP Sessions") || !strings.Contains(header, "✉ 1 ?") {
		t.Errorf("ACP header line should carry the badge; got %q", header)
	}
	m.showACP = false

	m.mode = modeCommandPicker
	if !strings.Contains(ansi.Strip(m.View()), "✉ 1 ?") {
		t.Error("command picker modal should show the badge")
	}
	m.mode = modeConfirmArchive
	m.archiveTarget = m.projectRoot + "/p/project.yaml"
	if !strings.Contains(ansi.Strip(m.View()), "✉ 1 ?") {
		t.Error("archive confirm modal should show the badge")
	}
}

func TestInboxHotkeyFromEveryNonTypingMode(t *testing.T) {
	store := newFakeInbox(newInboxMsg(t, postoffice.Draft{Kind: postoffice.KindRequest, Body: "Which bucket?"}))
	for _, tc := range []struct {
		name  string
		setup func(model) model
		back  func(model) bool
	}{
		{"normal", func(m model) model { return m }, func(m model) bool { return m.mode == modeNormal && !m.showACP }},
		{"acp view", func(m model) model { m.acpCh = make(chan acpTabEvent); m.showACP = true; return m },
			func(m model) bool { return m.mode == modeNormal && m.showACP }},
		{"command picker", func(m model) model { m.mode = modeCommandPicker; return m }, func(m model) bool { return m.mode == modeCommandPicker }},
		{"archive confirm", func(m model) model { m.mode = modeConfirmArchive; return m }, func(m model) bool { return m.mode == modeConfirmArchive }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.setup(inboxModel(t, store))
			m = pressI(m)
			if m.mode != modeInbox {
				t.Fatalf("I: mode = %v, want modeInbox", m.mode)
			}
			if !strings.Contains(ansi.Strip(m.View()), "Inbox") {
				t.Error("inbox view not rendered")
			}
			// I toggles it closed, back to where it was opened from.
			if back := pressI(m); !tc.back(back) {
				t.Errorf("I in the inbox did not return to %s (mode %v, showACP %v)", tc.name, back.mode, back.showACP)
			}
			if back := key(m, tea.KeyMsg{Type: tea.KeyEsc}); !tc.back(back) {
				t.Errorf("esc in the inbox did not return to %s (mode %v)", tc.name, back.mode)
			}
		})
	}
}

func TestInboxHotkeyIsTextWhileComposing(t *testing.T) {
	m, _ := wolfModel(t)
	m.inboxCh = make(chan []InboxItem)
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m = pressI(m)
	if m.mode == modeInbox {
		t.Fatal("I opened the inbox while composing an ACP message")
	}
	if got, _ := m.acp.selected(); got.input.draft != "I" {
		t.Errorf("draft = %q, want I", got.input.draft)
	}
}

func TestInboxHotkeyNeedsAStore(t *testing.T) {
	m := newModel(nil, nil, nil)
	if m = pressI(m); m.mode == modeInbox {
		t.Error("I opened an inbox with no store wired in")
	}
}

func TestInboxViewListsRows(t *testing.T) {
	req := newInboxMsg(t, postoffice.Draft{Kind: postoffice.KindRequest, Subject: "Which bucket for the export?", Body: "staging or prod?", Options: []string{"staging", "prod"}})
	note := newInboxMsg(t, postoffice.Draft{Kind: postoffice.KindNotification, Subject: "Release v0.9.0 tagged", Body: "done"})
	toAgent := newInboxMsg(t, postoffice.Draft{Kind: postoffice.KindRequest, Recipient: "step-up", Subject: "Need network access", Body: "proxy.golang.org"})
	recv := time.Now()
	toAgent.ReceivedAt = &recv
	// Agent traffic first in the store; the view still puts it last.
	store := newFakeInbox(toAgent, note, req)
	store.deliveries[req.ID] = []string{"slack", "signal"}
	m := pressI(inboxModel(t, store))
	out := ansi.Strip(m.View())

	for _, want := range []string{
		"? " + req.ShortID() + "  task fix-ci (wm)",
		"slack,signal",
		"Which bucket for the export?",
		"· " + note.ShortID(),
		"Release v0.9.0 tagged",
		"To agents (read-only)",
		"→ " + toAgent.ShortID() + "  task fix-ci (wm) → step-up",
		"received",
		"staging or prod?",
		"Options: 1) staging  2) prod",
		"(enter to reply, or 1-2 to pick an option)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("inbox view missing %q; got:\n%s", want, out)
		}
	}
	if i, j := strings.Index(out, req.ShortID()), strings.Index(out, toAgent.ShortID()); i > j {
		t.Error("operator request should be listed before agent traffic")
	}
	if m.inboxSel != req.ID {
		t.Errorf("selection = %s, want the operator request %s first", m.inboxSel, req.ID)
	}
	if n := len(strings.Split(m.View(), "\n")); n >= m.height {
		t.Errorf("inbox view is %d rows, want fewer than the %d-row terminal", n, m.height)
	}
}

func TestInboxReplyClaims(t *testing.T) {
	req := newInboxMsg(t, postoffice.Draft{Kind: postoffice.KindRequest, Body: "Which bucket?"})
	store := newFakeInbox(req)
	m := pressI(inboxModel(t, store))

	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.inboxTyping {
		t.Fatal("enter on a request should start a reply")
	}
	// While typing, command keys (and I) are text.
	m = typeText(m, "Use dI staging")
	if m.mode != modeInbox {
		t.Fatal("typing I closed the inbox")
	}
	if !strings.Contains(ansi.Strip(m.View()), "› Use dI staging") {
		t.Errorf("reply draft not rendered:\n%s", ansi.Strip(m.View()))
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})

	if len(store.claimCalls) != 1 {
		t.Fatalf("claims = %v, want one", store.claimCalls)
	}
	c := store.claimCalls[0]
	if c.ID != req.ID || c.Outcome != postoffice.OutcomeAnswered || c.Response != "Use dI staging" {
		t.Errorf("claim = %+v", c)
	}
	if c.Responder == nil || c.Responder.Kind != postoffice.ResponderOperator || c.Responder.Via != postoffice.ViaTUI {
		t.Errorf("responder = %+v, want operator via tui", c.Responder)
	}
	if m.inboxTyping || m.inboxReply.draft != "" {
		t.Errorf("after send: typing=%v draft=%q, want stopped and cleared", m.inboxTyping, m.inboxReply.draft)
	}
	// The row stays, greyed, until post-office finalises it.
	if !strings.Contains(ansi.Strip(m.View()), "sending…") {
		t.Errorf("answered row should say sending…:\n%s", ansi.Strip(m.View()))
	}
	if b := m.inboxBadge(); b != "" {
		t.Errorf("badge after answering the only request = %q, want none", ansi.Strip(b))
	}
	// A second enter can't answer it again.
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.inboxTyping || len(store.claimCalls) != 1 {
		t.Error("enter on an answered request should not start another reply")
	}

	store.finalize(req.ID)
	m = refreshInbox(t, m, store)
	if len(m.inbox) != 0 || m.inboxSel != "" {
		t.Errorf("finalised message should leave the inbox; inbox=%v sel=%q", m.inbox, m.inboxSel)
	}
	if !strings.Contains(ansi.Strip(m.View()), "(no messages)") {
		t.Error("empty inbox should say so")
	}
}

func TestInboxReplyEscKeepsDraft(t *testing.T) {
	store := newFakeInbox(newInboxMsg(t, postoffice.Draft{Kind: postoffice.KindRequest, Body: "Which bucket?"}))
	m := pressI(inboxModel(t, store))
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m = typeText(m, "half")
	m = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.inboxTyping || m.mode != modeInbox {
		t.Fatalf("first esc: typing=%v mode=%v, want stopped typing, still in the inbox", m.inboxTyping, m.mode)
	}
	if m.inboxReply.draft != "half" {
		t.Errorf("esc lost the draft: %q", m.inboxReply.draft)
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m = typeText(m, "!")
	if m.inboxReply.draft != "half!" {
		t.Errorf("draft = %q, want half!", m.inboxReply.draft)
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyCtrlU})
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(store.claimCalls) != 0 {
		t.Error("an empty reply must not be sent")
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m = key(m, tea.KeyMsg{Type: tea.KeyEsc}); m.mode != modeNormal {
		t.Errorf("second esc: mode = %v, want the inbox closed", m.mode)
	}
}

func TestInboxOptionKeys(t *testing.T) {
	req := newInboxMsg(t, postoffice.Draft{Kind: postoffice.KindRequest, Body: "Which bucket?", Options: []string{"staging", "prod"}})
	store := newFakeInbox(req)
	m := pressI(inboxModel(t, store))

	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("3")})
	if len(store.claimCalls) != 0 || !strings.Contains(m.inboxErr, "no option 3") {
		t.Errorf("3 with two options: claims=%v err=%q", store.claimCalls, m.inboxErr)
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	if len(store.claimCalls) != 1 {
		t.Fatalf("claims = %v, want one", store.claimCalls)
	}
	if c := store.claimCalls[0]; c.Response != "prod" || c.Option != "prod" || c.Outcome != postoffice.OutcomeAnswered {
		t.Errorf("option claim = %+v, want answered prod", c)
	}
	_ = m
}

func TestInboxDismissNotification(t *testing.T) {
	note := newInboxMsg(t, postoffice.Draft{Kind: postoffice.KindNotification, Subject: "FYI", Body: "done"})
	req := newInboxMsg(t, postoffice.Draft{Kind: postoffice.KindRequest, Body: "Which bucket?"})
	store := newFakeInbox(note, req)
	m := pressI(inboxModel(t, store))

	// The request is listed first; d refuses it.
	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	if len(store.claimCalls) != 0 || !strings.Contains(m.inboxErr, "only notifications") {
		t.Errorf("d on a request: claims=%v err=%q", store.claimCalls, m.inboxErr)
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if m.inboxSel != note.ID {
		t.Fatalf("j: selection = %s, want the notification", m.inboxSel)
	}
	// enter on a notification doesn't start a reply.
	if m = key(m, tea.KeyMsg{Type: tea.KeyEnter}); m.inboxTyping {
		t.Error("enter on a notification started a reply")
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	if len(store.claimCalls) != 1 {
		t.Fatalf("claims = %v, want one", store.claimCalls)
	}
	if c := store.claimCalls[0]; c.ID != note.ID || c.Outcome != postoffice.OutcomeDismissed {
		t.Errorf("dismiss claim = %+v", c)
	}
	_ = m
}

func TestInboxAgentRowsAreReadOnly(t *testing.T) {
	toAgent := newInboxMsg(t, postoffice.Draft{Kind: postoffice.KindRequest, Recipient: "step-up", Body: "Need proxy.golang.org", Options: []string{"yes"}})
	store := newFakeInbox(toAgent)
	m := pressI(inboxModel(t, store))
	for _, k := range []string{"enter", "1", "d"} {
		msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		if k == "enter" {
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		}
		m = key(m, msg)
		if m.inboxTyping || len(store.claimCalls) != 0 || !strings.Contains(m.inboxErr, "read-only") {
			t.Errorf("%s on an agent row: typing=%v claims=%v err=%q", k, m.inboxTyping, store.claimCalls, m.inboxErr)
		}
	}
	if b := ansi.Strip(m.inboxBadge()); b != "✉ 1" {
		t.Errorf("agent-only badge = %q, want ✉ 1", b)
	}
}

func TestInboxAlreadyAnsweredElsewhere(t *testing.T) {
	req := newInboxMsg(t, postoffice.Draft{Kind: postoffice.KindRequest, Body: "Which bucket?"})
	store := newFakeInbox(req)
	m := pressI(inboxModel(t, store))
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m = typeText(m, "staging")

	// Slack answers first; the feed has not caught up yet.
	if _, err := store.Claim(postoffice.Resolution{ID: req.ID, Outcome: postoffice.OutcomeAnswered, Responder: &postoffice.Responder{Kind: postoffice.ResponderOperator, Via: postoffice.ViaSlack}}); err != nil {
		t.Fatal(err)
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.inboxErr != "already answered via slack" {
		t.Errorf("err = %q, want already answered via slack", m.inboxErr)
	}
	out := ansi.Strip(m.View())
	if !strings.Contains(out, "already answered via slack") || !strings.Contains(out, "answered via slack") {
		t.Errorf("view should say it was answered elsewhere:\n%s", out)
	}
	if m.inboxTyping {
		t.Error("typing should stop once the request is closed")
	}
}

func TestInboxClosedWhileTyping(t *testing.T) {
	req := newInboxMsg(t, postoffice.Draft{Kind: postoffice.KindRequest, Body: "Which bucket?"})
	store := newFakeInbox(req)
	m := pressI(inboxModel(t, store))
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m = typeText(m, "stag")

	store.claims[req.ID] = postoffice.Resolution{ID: req.ID, Outcome: postoffice.OutcomeCancelled, Reason: "found it", At: time.Now()}
	m = refreshInbox(t, m, store)
	if m.inboxTyping {
		t.Error("a request cancelled while typing should stop the reply")
	}
	if !strings.Contains(m.inboxErr, "cancelled this request: found it") {
		t.Errorf("err = %q", m.inboxErr)
	}
}

func TestInboxExpiredIsNotAnswerable(t *testing.T) {
	req := newInboxMsg(t, postoffice.Draft{Kind: postoffice.KindRequest, Body: "Which bucket?", ExpiresIn: time.Minute})
	store := newFakeInbox(req)
	m := pressI(inboxModel(t, store))
	if !m.inbox[0].Expired {
		t.Fatal("a request past its expiry should be shown as expired")
	}
	if m = key(m, tea.KeyMsg{Type: tea.KeyEnter}); m.inboxTyping || m.inboxErr != "this message has expired" {
		t.Errorf("enter on an expired request: typing=%v err=%q", m.inboxTyping, m.inboxErr)
	}
	if b := m.inboxBadge(); b != "" {
		t.Errorf("badge = %q, want none for an expired request", ansi.Strip(b))
	}
}

func TestInboxStalledClaimSaysWaitingForPostOffice(t *testing.T) {
	req := newInboxMsg(t, postoffice.Draft{Kind: postoffice.KindRequest, Body: "Which bucket?"})
	store := newFakeInbox(req)
	store.claims[req.ID] = postoffice.Resolution{ID: req.ID, Outcome: postoffice.OutcomeAnswered, At: time.Now().Add(-time.Minute),
		Responder: &postoffice.Responder{Kind: postoffice.ResponderOperator, Via: postoffice.ViaTUI}}
	m := pressI(inboxModel(t, store))
	if out := ansi.Strip(m.View()); !strings.Contains(out, "answered — waiting for post-office") {
		t.Errorf("stalled claim not shown:\n%s", out)
	}
}

func TestInboxSelectionFollowsFeed(t *testing.T) {
	a := newInboxMsg(t, postoffice.Draft{Kind: postoffice.KindRequest, Body: "a"})
	b := newInboxMsg(t, postoffice.Draft{Kind: postoffice.KindRequest, Body: "b"})
	c := newInboxMsg(t, postoffice.Draft{Kind: postoffice.KindRequest, Body: "c"})
	store := newFakeInbox(a, b, c)
	m := pressI(inboxModel(t, store))
	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if m.inboxSel != b.ID {
		t.Fatalf("j: selected %s, want b", m.inboxSel)
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if m.inboxSel != a.ID {
		t.Fatalf("k past the top: selected %s, want a", m.inboxSel)
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	// b closes: the selection moves to the row now in its place.
	store.finalize(b.ID)
	m = refreshInbox(t, m, store)
	if m.inboxSel != c.ID {
		t.Errorf("after b closed: selected %s, want c", m.inboxSel)
	}
}

func TestInboxBodyScroll(t *testing.T) {
	var body strings.Builder
	for i := range 80 {
		fmt.Fprintf(&body, "line %02d\n", i)
	}
	store := newFakeInbox(newInboxMsg(t, postoffice.Draft{Kind: postoffice.KindRequest, Subject: "long", Body: body.String()}))
	m := pressI(inboxModel(t, store))
	if out := ansi.Strip(m.View()); !strings.Contains(out, "line 00") || strings.Contains(out, "line 79") {
		t.Fatalf("unscrolled body should show the top only:\n%s", out)
	}
	for range 10 {
		m = key(m, tea.KeyMsg{Type: tea.KeyCtrlF})
	}
	if out := ansi.Strip(m.View()); !strings.Contains(out, "line 79") || strings.Contains(out, "line 00") {
		t.Errorf("paged body should reach the bottom:\n%s", out)
	}
	for range 20 {
		m = key(m, tea.KeyMsg{Type: tea.KeyCtrlB})
	}
	if m.inboxScroll != 0 {
		t.Errorf("ctrl+b past the top: scroll = %d, want 0", m.inboxScroll)
	}
}

func TestInboxBellRingsOnNewOperatorRequest(t *testing.T) {
	var buf bytes.Buffer
	prev := bellOut
	bellOut = &buf
	t.Cleanup(func() { bellOut = prev })

	old := newInboxMsg(t, postoffice.Draft{Kind: postoffice.KindRequest, Body: "already here"})
	store := newFakeInbox(old)
	m := newModel(nil, nil, nil)
	m.inboxStore, m.inboxCh, m.inboxBell = store, make(chan []InboxItem), true

	ring := func(m model) (model, bool) {
		items, _ := scanInbox(store, time.Now())
		next, cmd := m.applyInbox(items)
		if cmd != nil {
			cmd()
		}
		rang := buf.Len() > 0
		buf.Reset()
		return next, rang
	}
	var rang bool
	if m, rang = ring(m); rang {
		t.Error("the first snapshot should not ring")
	}
	store.msgs = append(store.msgs, newInboxMsg(t, postoffice.Draft{Kind: postoffice.KindNotification, Subject: "n", Body: "n"}))
	if m, rang = ring(m); rang {
		t.Error("a new notification should not ring")
	}
	store.msgs = append(store.msgs, newInboxMsg(t, postoffice.Draft{Kind: postoffice.KindRequest, Body: "new"}))
	if m, rang = ring(m); !rang {
		t.Error("a new operator request should ring")
	}
	if m, rang = ring(m); rang {
		t.Error("an unchanged inbox should not ring again")
	}
	m.inboxBell = false
	store.msgs = append(store.msgs, newInboxMsg(t, postoffice.Draft{Kind: postoffice.KindRequest, Body: "quiet"}))
	if _, rang = ring(m); rang {
		t.Error("the bell should stay quiet when turned off")
	}
}

func TestWatchInboxEmitsOnChange(t *testing.T) {
	store := newFakeInbox()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := WatchInbox(ctx, store, 10*time.Millisecond)
	next := func() []InboxItem {
		t.Helper()
		select {
		case items := <-ch:
			return items
		case <-time.After(2 * time.Second):
			t.Fatal("no snapshot")
		}
		return nil
	}
	if items := next(); len(items) != 0 {
		t.Fatalf("first snapshot = %v, want empty", items)
	}
	req := newInboxMsg(t, postoffice.Draft{Kind: postoffice.KindRequest, Body: "Which bucket?"})
	store.mu.Lock()
	store.msgs = append(store.msgs, req)
	store.mu.Unlock()
	if items := next(); len(items) != 1 || items[0].Message.ID != req.ID {
		t.Fatalf("snapshot = %v, want the request", items)
	}
	store.mu.Lock()
	store.deliveries[req.ID] = []string{"slack"}
	store.mu.Unlock()
	if items := next(); len(items) != 1 || len(items[0].DeliveredTo) != 1 {
		t.Fatalf("a new delivery should emit; got %v", items)
	}
	// No change, no snapshot.
	select {
	case items := <-ch:
		t.Errorf("unchanged inbox emitted %v", items)
	case <-time.After(60 * time.Millisecond):
	}
	cancel()
	for range ch {
	}
}

// TestInboxAgainstRealStore runs a reply through the real post-office store:
// the claim lands in outbox/, and the row leaves once it is finalised.
func TestInboxAgainstRealStore(t *testing.T) {
	store, err := postoffice.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	req, err := postoffice.NewMessage(postoffice.Draft{Kind: postoffice.KindRequest, From: taskSender, Body: "Which bucket?", Options: []string{"staging", "prod"}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(req); err != nil {
		t.Fatal(err)
	}
	m := newModel(nil, nil, nil)
	m.width, m.height = 100, 30
	m.inboxStore, m.inboxCh = store, make(chan []InboxItem)
	m = pressI(refreshInbox(t, m, store))

	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1")})
	if m.inboxErr != "" {
		t.Fatalf("claim failed: %s", m.inboxErr)
	}
	r, ok, err := store.Claimed(req.ID)
	if err != nil || !ok {
		t.Fatalf("no outbox claim: ok=%v err=%v", ok, err)
	}
	if r.Option != "staging" || r.Responder == nil || r.Responder.Via != postoffice.ViaTUI {
		t.Errorf("claim = %+v", r)
	}
	if _, err := store.Finalize(req.ID); err != nil {
		t.Fatal(err)
	}
	if m = refreshInbox(t, m, store); len(m.inbox) != 0 {
		t.Errorf("finalised message still listed: %v", m.inbox)
	}
}
