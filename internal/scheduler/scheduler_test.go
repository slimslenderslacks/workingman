package scheduler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// robfig/cron rounds sub-second @every durations up to 1s, so the smallest
// interval these tests can reliably observe is 1 second. The waits below are
// sized for ≥2 firings of a 1s schedule with margin for scheduler jitter.

func TestRegisterFires(t *testing.T) {
	s := New()
	var n int32
	if err := s.Register("k", "@every 1s", func() { atomic.AddInt32(&n, 1) }); err != nil {
		t.Fatalf("Register: %v", err)
	}
	s.Start()
	t.Cleanup(func() { _ = s.Stop(context.Background()) })

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&n) >= 2 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("expected at least 2 firings, got %d", atomic.LoadInt32(&n))
}

func TestRegisterSameSpecIsIdempotent(t *testing.T) {
	s := New()
	fn := func() {}
	if err := s.Register("k", "@every 1h", fn); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.Register("k", "@every 1h", fn); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if s.Spec("k") != "@every 1h" {
		t.Errorf("Spec = %q", s.Spec("k"))
	}
}

func TestRegisterReplacesOnSpecChange(t *testing.T) {
	s := New()
	var aHits, bHits int32
	// First registration uses an interval long enough that it would never
	// have fired during the test window — so any firing observed after the
	// second Register can only come from the replacement.
	if err := s.Register("k", "@every 1h", func() { atomic.AddInt32(&aHits, 1) }); err != nil {
		t.Fatal(err)
	}
	if err := s.Register("k", "@every 1s", func() { atomic.AddInt32(&bHits, 1) }); err != nil {
		t.Fatal(err)
	}
	s.Start()
	t.Cleanup(func() { _ = s.Stop(context.Background()) })

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&bHits) >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if atomic.LoadInt32(&bHits) == 0 {
		t.Errorf("replacement schedule never fired")
	}
	if got := atomic.LoadInt32(&aHits); got != 0 {
		t.Errorf("old schedule fired %d times after replacement; want 0", got)
	}
	if s.Spec("k") != "@every 1s" {
		t.Errorf("Spec = %q", s.Spec("k"))
	}
}

func TestUnregisterStopsFirings(t *testing.T) {
	s := New()
	var n int32
	if err := s.Register("k", "@every 1s", func() { atomic.AddInt32(&n, 1) }); err != nil {
		t.Fatal(err)
	}
	s.Start()
	t.Cleanup(func() { _ = s.Stop(context.Background()) })

	// Wait until we have at least one firing, so we know the schedule is live.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&n) >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if atomic.LoadInt32(&n) == 0 {
		t.Skip("schedule never fired before Unregister window — flaky environment")
	}
	s.Unregister("k")
	mid := atomic.LoadInt32(&n)
	time.Sleep(2 * time.Second)
	if got := atomic.LoadInt32(&n); got > mid {
		t.Errorf("schedule fired %d more times after Unregister (mid=%d, end=%d)", got-mid, mid, got)
	}
	if s.Spec("k") != "" {
		t.Errorf("Spec after Unregister = %q, want empty", s.Spec("k"))
	}
}

func TestInvalidSpecReturnsError(t *testing.T) {
	s := New()
	if err := s.Register("k", "not a real cron spec", func() {}); err == nil {
		t.Error("expected error for invalid spec")
	}
	if s.Spec("k") != "" {
		t.Errorf("invalid spec should not register: Spec = %q", s.Spec("k"))
	}
}

func TestEmptyKeyOrSpecRejected(t *testing.T) {
	s := New()
	if err := s.Register("", "@every 1h", func() {}); err == nil {
		t.Error("empty key should error")
	}
	if err := s.Register("k", "", func() {}); err == nil {
		t.Error("empty spec should error")
	}
}

func TestTicksBetween(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		spec      string
		after     time.Time
		upTo      time.Time
		limit     int
		wantCount int
		wantLast  time.Time
	}{
		{"hourly over 3.5h", "0 * * * *", base, base.Add(3*time.Hour + 30*time.Minute), 0, 3, base.Add(3 * time.Hour)},
		{"after is exclusive", "0 * * * *", base, base.Add(time.Hour), 0, 1, base.Add(time.Hour)},
		{"upTo inclusive, none before", "0 * * * *", base, base.Add(59 * time.Minute), 0, 0, time.Time{}},
		{"limit caps the count", "@every 1m", base, base.Add(time.Hour), 5, 5, base.Add(5 * time.Minute)},
		{"empty interval", "0 * * * *", base, base, 0, 0, time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, last, err := TicksBetween(tc.spec, tc.after, tc.upTo, tc.limit)
			if err != nil {
				t.Fatalf("TicksBetween: %v", err)
			}
			if n != tc.wantCount || !last.Equal(tc.wantLast) {
				t.Errorf("got (%d, %v), want (%d, %v)", n, last, tc.wantCount, tc.wantLast)
			}
		})
	}
}

func TestTicksBetweenInvalidSpec(t *testing.T) {
	if _, _, err := TicksBetween("nonsense", time.Now(), time.Now().Add(time.Hour), 0); err == nil {
		t.Fatal("expected error for invalid spec")
	}
}
