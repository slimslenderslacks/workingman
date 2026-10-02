package notify

import (
	"errors"
	"strings"
	"testing"
	"time"
)

type failSender struct{ err error }

func (f failSender) Send(_, _ string) error { return f.err }

type panicSender struct{}

func (panicSender) Send(_, _ string) error { panic("boom") }

type slowSender struct {
	d   time.Duration
	rec *Recorder
}

func (s slowSender) Send(t, m string) error {
	time.Sleep(s.d)
	return s.rec.Send(t, m)
}

func TestMultiFansOutToAll(t *testing.T) {
	a, b := &Recorder{}, &Recorder{}
	m := NewMulti(a, nil, b)
	if m.Len() != 2 {
		t.Fatalf("Len = %d, want 2 (nil skipped)", m.Len())
	}
	if err := m.Send("T", "M"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	for name, r := range map[string]*Recorder{"a": a, "b": b} {
		got := r.Calls()
		if len(got) != 1 || got[0] != (Call{Title: "T", Message: "M"}) {
			t.Errorf("%s calls = %+v", name, got)
		}
	}
}

func TestMultiSendTopic(t *testing.T) {
	topical := &Recorder{}
	plain := &plainRecorder{}
	m := NewMulti(topical, plain)
	if err := m.SendTopic("wolf", "Wolf started", "proj x"); err != nil {
		t.Fatalf("SendTopic: %v", err)
	}
	if got := topical.Calls(); len(got) != 1 || got[0].Topic != "wolf" {
		t.Errorf("topic-aware sender calls = %+v", got)
	}
	if got := plain.rec.Calls(); len(got) != 1 || got[0].Topic != "" || got[0].Title != "Wolf started" {
		t.Errorf("plain sender should get Send fallback, calls = %+v", got)
	}
}

// plainRecorder implements only Sender (no SendTopic).
type plainRecorder struct{ rec Recorder }

func (p *plainRecorder) Send(t, m string) error { return p.rec.Send(t, m) }

func TestMultiErrorIsolation(t *testing.T) {
	boom := errors.New("boom failure")
	// build returns the senders under test, with the healthy recorder placed
	// both before and after the failing ones so ordering cannot matter.
	tests := []struct {
		name     string
		build    func(ok Sender) []Sender
		wantErrs []string
	}{
		{"error does not block next", func(ok Sender) []Sender { return []Sender{failSender{boom}, ok} }, []string{"boom failure"}},
		{"error does not block previous", func(ok Sender) []Sender { return []Sender{ok, failSender{boom}} }, []string{"boom failure"}},
		{"panic is isolated", func(ok Sender) []Sender { return []Sender{panicSender{}, ok} }, []string{"panic: boom"}},
		{"multiple failures joined", func(ok Sender) []Sender { return []Sender{failSender{boom}, panicSender{}, ok} }, []string{"boom failure", "panic: boom"}},
		{"all ok", func(ok Sender) []Sender { return []Sender{ok} }, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok := &Recorder{}
			err := NewMulti(tt.build(ok)...).Send("t", "m")
			if len(tt.wantErrs) == 0 {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("expected error")
				}
				for _, w := range tt.wantErrs {
					if !strings.Contains(err.Error(), w) {
						t.Errorf("error %q missing %q", err, w)
					}
				}
			}
			if got := len(ok.Calls()); got != 1 {
				t.Errorf("healthy sender calls = %d, want 1", got)
			}
		})
	}
}

func TestMultiSlowSenderDoesNotSerialize(t *testing.T) {
	rec := &Recorder{}
	m := NewMulti(slowSender{100 * time.Millisecond, rec}, slowSender{100 * time.Millisecond, rec})
	start := time.Now()
	if err := m.Send("t", "m"); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 180*time.Millisecond {
		t.Errorf("fan-out took %v; senders appear to run serially", d)
	}
	if len(rec.Calls()) != 2 {
		t.Errorf("calls = %d, want 2", len(rec.Calls()))
	}
}

func TestSendTopicHelperFallsBack(t *testing.T) {
	p := &plainRecorder{}
	if err := SendTopic(p, "wolf", "T", "M"); err != nil {
		t.Fatal(err)
	}
	if got := p.rec.Calls(); len(got) != 1 || got[0].Title != "T" {
		t.Errorf("calls = %+v", got)
	}
	r := &Recorder{}
	_ = SendTopic(r, "wolf", "T", "M")
	if got := r.Calls(); got[0].Topic != "wolf" {
		t.Errorf("topic not forwarded: %+v", got)
	}
}
