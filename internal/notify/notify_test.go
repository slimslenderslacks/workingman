package notify

import (
	"strings"
	"sync"
	"testing"
)

func TestRecorderCapturesCalls(t *testing.T) {
	var r Recorder
	if err := r.Send("hello", "world"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := r.Send("again", "and again"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	calls := r.Calls()
	if len(calls) != 2 {
		t.Fatalf("Calls = %v, want 2", calls)
	}
	if calls[0] != (Call{Title: "hello", Message: "world"}) {
		t.Errorf("call[0] = %+v", calls[0])
	}
}

func TestRecorderIsConcurrencySafe(t *testing.T) {
	var r Recorder
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = r.Send("t", "m") }()
	}
	wg.Wait()
	if got := len(r.Calls()); got != 50 {
		t.Errorf("Calls = %d, want 50", got)
	}
}

func TestEscapeProtectsQuotes(t *testing.T) {
	got := escape(`he said "hi"`)
	if !strings.Contains(got, `\"`) {
		t.Errorf("quotes not escaped: %q", got)
	}
}

func TestNoopSucceeds(t *testing.T) {
	var n Noop
	if err := n.Send("anything", "at all"); err != nil {
		t.Errorf("Noop.Send: %v", err)
	}
}
