package daemon

import (
	"testing"
	"time"
)

func TestWolfSessionsListsLiveWolves(t *testing.T) {
	h := newChannelHarness(t)
	if got := h.d.WolfSessions(); len(got) != 0 {
		t.Fatalf("no wolf yet, got %+v", got)
	}

	h.launch("blocked on review")
	var got []WolfSession
	eventually(t, "wolf tracked", func() bool { got = h.d.WolfSessions(); return len(got) == 1 })
	w := got[0]
	if w.Key != wolfSessionKey(h.proj) || w.ProjectPath != h.proj || w.WorkStream != "myproj" {
		t.Fatalf("wolf = %+v", w)
	}
	if w.ID == "" || w.ID != h.d.sessionName(w.Key) {
		t.Fatalf("ID %q, want the launcher's session name %q", w.ID, h.d.sessionName(w.Key))
	}
	if w.Attachable || w.Dir != "" {
		t.Fatalf("a wolf without an ACP launcher cannot be attached to: %+v", w)
	}
	if one, ok := h.d.WolfSession(w.Key); !ok || one.ID != w.ID {
		t.Fatalf("WolfSession(key) = %+v, %v", one, ok)
	}
	if _, ok := h.d.WolfSession("nope#wolf"); ok {
		t.Fatal("unknown key found")
	}

	_ = h.launcher.last().Close()
	eventually(t, "wolf gone", func() bool { return len(h.d.WolfSessions()) == 0 })
}

func TestSnapshotSummary(t *testing.T) {
	s := &Snapshot{
		GeneratedAt:        time.Now(),
		LiveStateAvailable: true,
		Projects: []ProjectSnapshot{
			{Status: "working", TaskCounts: map[string]int{"running": 2, "ready": 1, "failed": 0}},
			{Status: "blocked", TaskCounts: map[string]int{"failed": 1}},
			{Status: "working", TaskCounts: map[string]int{"committed": 4}},
			{LoadError: "bad yaml"},
		},
		Sessions: []SessionSnapshot{{Kind: "wolf"}, {Kind: "task"}, {Kind: "task"}},
	}
	want := "4 projects (1 blocked, 1 unreadable, 2 working) · 3 live sessions (2 task, 1 wolf) · tasks: 1 ready, 2 running, 1 failed, 4 committed"
	if got := s.Summary(); got != want {
		t.Fatalf("Summary:\n got %q\nwant %q", got, want)
	}

	empty := &Snapshot{}
	if got, want := empty.Summary(), "0 projects · 0 live sessions · live state unavailable"; got != want {
		t.Fatalf("empty Summary: got %q want %q", got, want)
	}
}
