package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/slimslenderslacks/work/internal/decision"
)

var fixedNow = time.Date(2026, 11, 19, 12, 0, 0, 0, time.FixedZone("PST", -8*3600))

func writeRequest(t *testing.T, inbox string, r decision.Request) decision.Pending {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	name := r.ID + ".json"
	path := filepath.Join(inbox, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return decision.Pending{Request: r, Filename: name, Path: path}
}

func sampleRequest(id, title string) decision.Request {
	return decision.Request{
		ID:      id,
		Title:   title,
		Summary: "# " + title + "\n\nSome **markdown** context.",
		Choices: []decision.Choice{{ID: "yes", Label: "Yes, do it"}, {ID: "no", Label: "No"}},
		Source:  "test",
	}
}

func newTestModel(t *testing.T, pending ...decision.Pending) (model, string, string) {
	t.Helper()
	inbox, outbox := t.TempDir(), t.TempDir()
	return newTestModelIn(t, inbox, outbox, pending...), inbox, outbox
}

func newTestModelIn(t *testing.T, inbox, outbox string, pending ...decision.Pending) model {
	t.Helper()
	m := newModel(config{
		Inbox:        inbox,
		Outbox:       outbox,
		GlamourStyle: "notty",
		Now:          func() time.Time { return fixedNow },
	}, pending)
	return update(t, m, tea.WindowSizeMsg{Width: 100, Height: 30})
}

func update(t *testing.T, m model, msgs ...tea.Msg) model {
	t.Helper()
	for _, msg := range msgs {
		next, _ := m.Update(msg)
		m = next.(model)
	}
	return m
}

func key(s string) tea.Msg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestAnswerWritesOutboxAndArchives(t *testing.T) {
	inbox, outbox := t.TempDir(), t.TempDir()
	a := writeRequest(t, inbox, sampleRequest("a", "First"))
	b := writeRequest(t, inbox, sampleRequest("b", "Second"))
	m := newTestModelIn(t, inbox, outbox, a, b)

	// Move to "Second", open it, pick "No".
	m = update(t, m, key("j"), key("enter"))
	if m.screen != screenDetail || m.current != "b.json" {
		t.Fatalf("screen=%v current=%q, want detail of b.json", m.screen, m.current)
	}
	if v := m.View(); !strings.Contains(v, "Yes, do it") || !strings.Contains(v, "markdown") {
		t.Fatalf("detail view missing choices or summary:\n%s", v)
	}
	m = update(t, m, key("down"), key("down"), key("up"), key("j"))
	if m.choiceCursor != 1 {
		t.Fatalf("choiceCursor=%d, want 1 (clamped)", m.choiceCursor)
	}
	m = update(t, m, key("enter"))

	if m.screen != screenList {
		t.Fatalf("screen=%v after answering, want list", m.screen)
	}
	if len(m.pending) != 1 || m.pending[0].Request.ID != "a" {
		t.Fatalf("pending=%+v, want only a", m.pending)
	}
	if m.statusErr || !strings.Contains(m.status, "Second") {
		t.Fatalf("status=%q err=%v", m.status, m.statusErr)
	}

	data, err := os.ReadFile(filepath.Join(outbox, "b.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got decision.Answer
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	want := decision.Answer{ID: "b", ChoiceID: "no", ChoiceLabel: "No", AnsweredAt: fixedNow.UTC()}
	if got != want || got.AnsweredAt.Location() != time.UTC {
		t.Fatalf("answer=%+v, want %+v", got, want)
	}
	if _, err := os.Stat(filepath.Join(inbox, "b.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("request still in inbox: %v", err)
	}
	if _, err := os.Stat(filepath.Join(inbox, decision.AnsweredDirName, "b.json")); err != nil {
		t.Fatalf("request not archived: %v", err)
	}
}

func TestAnswerWriteErrorKeepsRequest(t *testing.T) {
	inbox := t.TempDir()
	p := writeRequest(t, inbox, sampleRequest("a", "First"))
	m := newTestModelIn(t, inbox, filepath.Join(t.TempDir(), "missing"), p)

	m = update(t, m, key("enter"), key("enter"))
	if !m.statusErr || !strings.Contains(m.status, "answer not saved") {
		t.Fatalf("status=%q err=%v, want write error", m.status, m.statusErr)
	}
	if m.screen != screenDetail || len(m.pending) != 1 {
		t.Fatalf("screen=%v pending=%d, want to stay on detail with request kept", m.screen, len(m.pending))
	}
	if _, err := os.Stat(p.Path); err != nil {
		t.Fatalf("request should stay in inbox: %v", err)
	}
}

func TestWatcherEvents(t *testing.T) {
	m, inbox, _ := newTestModel(t)
	if !strings.Contains(m.View(), "No pending decisions") {
		t.Fatalf("empty view:\n%s", m.View())
	}

	later := fixedNow.Add(time.Hour)
	rb := sampleRequest("b", "Later")
	rb.CreatedAt = &later
	ra := sampleRequest("a", "Earlier")
	ra.CreatedAt = &fixedNow
	pb := decision.Pending{Request: rb, Filename: "b.json", Path: filepath.Join(inbox, "b.json")}
	pa := decision.Pending{Request: ra, Filename: "a.json", Path: filepath.Join(inbox, "a.json")}

	m = update(t, m,
		eventMsg{Kind: decision.EventAdded, Filename: "b.json", Pending: &pb},
		eventMsg{Kind: decision.EventAdded, Filename: "a.json", Pending: &pa},
	)
	if len(m.pending) != 2 || m.pending[0].Filename != "a.json" {
		t.Fatalf("pending=%+v, want sorted [a b]", m.pending)
	}
	// The cursor stayed on b, which was selected before a arrived.
	if m.cursor != 1 {
		t.Fatalf("cursor=%d, want 1 (still on b)", m.cursor)
	}

	// Open b, then it is removed underneath us.
	m = update(t, m, key("enter"))
	if m.current != "b.json" {
		t.Fatalf("current=%q", m.current)
	}
	m = update(t, m, eventMsg{Kind: decision.EventRemoved, Filename: "b.json"})
	if m.screen != screenList || len(m.pending) != 1 || m.cursor != 0 {
		t.Fatalf("screen=%v pending=%d cursor=%d after removal", m.screen, len(m.pending), m.cursor)
	}

	// A malformed file is listed as an error, then cleared once fixed.
	m = update(t, m, eventMsg{Kind: decision.EventError, Filename: "bad.json", Err: errors.New("malformed JSON")})
	if len(m.errors) != 1 || !m.statusErr || !strings.Contains(m.View(), "bad.json: malformed JSON") {
		t.Fatalf("errors=%+v view:\n%s", m.errors, m.View())
	}
	fixed := decision.Pending{Request: sampleRequest("bad", "Fixed"), Filename: "bad.json"}
	m = update(t, m, eventMsg{Kind: decision.EventAdded, Filename: "bad.json", Pending: &fixed})
	if len(m.errors) != 0 || len(m.pending) != 2 {
		t.Fatalf("errors=%+v pending=%d after fix", m.errors, len(m.pending))
	}
}

func TestInitialErrorsAndEscAndQuit(t *testing.T) {
	inbox := t.TempDir()
	p := writeRequest(t, inbox, sampleRequest("a", "First"))
	m := newModel(config{
		Inbox:         inbox,
		Outbox:        t.TempDir(),
		GlamourStyle:  "notty",
		InitialErrors: []error{&decision.FileError{Filename: "x.json", Err: errors.New("boom")}},
	}, []decision.Pending{p})
	if !strings.Contains(m.View(), "x.json: boom") {
		t.Fatalf("initial error not shown:\n%s", m.View())
	}
	m = update(t, m, key("enter"), key("esc"))
	if m.screen != screenList {
		t.Fatalf("esc did not return to list")
	}
	for _, k := range []string{"q", "ctrl+c"} {
		_, cmd := m.Update(key(k))
		if cmd == nil {
			t.Fatalf("%s: no command", k)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatalf("%s did not quit", k)
		}
	}
}

func TestWaitForEvent(t *testing.T) {
	ch := make(chan decision.Event, 1)
	ch <- decision.Event{Kind: decision.EventRemoved, Filename: "a.json"}
	if msg, ok := waitForEvent(ch)().(eventMsg); !ok || msg.Filename != "a.json" {
		t.Fatalf("got %#v", msg)
	}
	close(ch)
	if _, ok := waitForEvent(ch)().(watchClosedMsg); !ok {
		t.Fatal("closed channel did not yield watchClosedMsg")
	}
}

// TestLiveWatch drives the real watcher end to end: a request written to the
// inbox after start shows up through the Init/Update command loop.
func TestLiveWatch(t *testing.T) {
	inbox := t.TempDir()
	ctx := t.Context()
	events, err := decision.Watch(ctx, inbox)
	if err != nil {
		t.Fatal(err)
	}
	m := newModel(config{Inbox: inbox, Outbox: t.TempDir(), Events: events, GlamourStyle: "notty"}, nil)
	cmd := m.Init()
	writeRequest(t, inbox, sampleRequest("live", "Live one"))

	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		m = update(t, m, msg)
	case <-time.After(5 * time.Second):
		t.Fatal("no watcher event")
	}
	if len(m.pending) != 1 || m.pending[0].Request.ID != "live" {
		t.Fatalf("pending=%+v", m.pending)
	}
}
