package daemon

import (
	"regexp"
	"strings"
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

func TestSessionCommandResumesOrCreates(t *testing.T) {
	uuid := sessionUUID("widget")
	cmd := sessionCommand(uuid)

	// Runs as a bash wrapper so the resume-vs-create decision happens inside
	// the sandbox at launch time.
	if len(cmd) != 3 || cmd[0] != "bash" || cmd[1] != "-lc" {
		t.Fatalf("sessionCommand = %v, want a `bash -lc <script>` invocation", cmd)
	}
	script := cmd[2]

	// The stable id must reach both branches: --resume when the transcript
	// already exists, --session-id (create) when it doesn't. Passing
	// --session-id unconditionally is the bug that crashed the window on
	// reopen, so both flags must be present.
	for _, want := range []string{
		"~/.claude/projects/",
		"--resume",
		"--session-id",
		uuid,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("session script missing %q\nscript: %s", want, script)
		}
	}
	// It must not launch claude with --session-id when the transcript exists —
	// resume has to come first in the conditional.
	if strings.Index(script, "--resume") > strings.Index(script, "else") {
		t.Errorf("resume branch must precede the create (else) branch\nscript: %s", script)
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
