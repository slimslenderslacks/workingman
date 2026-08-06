package daemon

import (
	"regexp"
	"testing"
)

var uuidV5Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestSessionUUIDStableAndWellFormed(t *testing.T) {
	// Deterministic: the same work stream always yields the same id, so
	// `:session` resumes the same claude conversation on every launch.
	a := sessionUUID("widget")
	b := sessionUUID("widget")
	if a != b {
		t.Errorf("sessionUUID not stable: %q != %q", a, b)
	}
	if !uuidV5Re.MatchString(a) {
		t.Errorf("sessionUUID(%q) = %q, not a well-formed v5 UUID", "widget", a)
	}
	// Distinct work streams get distinct ids.
	if c := sessionUUID("gadget"); c == a {
		t.Errorf("distinct work streams collided on %q", c)
	}
}

func TestSanitizeSandboxName(t *testing.T) {
	// sbx rejects underscores; they must become hyphens.
	if got := sanitizeSandboxName("session-my_work_stream"); got != "session-my-work-stream" {
		t.Errorf("sanitizeSandboxName = %q, want session-my-work-stream", got)
	}
	if got := sanitizeSandboxName("shell-widget"); got != "shell-widget" {
		t.Errorf("sanitizeSandboxName should leave clean names alone; got %q", got)
	}
}
