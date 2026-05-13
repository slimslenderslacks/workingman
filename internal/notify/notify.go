// Package notify is the user-notification escape hatch for agents that need
// human attention — currently the wolf agent when a project becomes blocked.
//
// Sender is intentionally a one-method interface so production can use the
// Osascript implementation, tests can use a Recorder, and any future
// channels (Slack, ntfy, …) plug in without touching the daemon.
package notify

import (
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

type Sender interface {
	Send(title, message string) error
}

// Osascript displays a macOS notification using the system `osascript`
// command. AppleScript embeds strings via double-quoted literals; we
// pre-escape backslashes and quotes to keep titles/messages that contain
// punctuation from breaking the script.
type Osascript struct {
	// Binary is the osascript executable. Defaults to "osascript".
	Binary string
}

func (o *Osascript) Send(title, message string) error {
	bin := o.Binary
	if bin == "" {
		bin = "osascript"
	}
	script := fmt.Sprintf(
		`display notification "%s" with title "%s"`,
		escape(message), escape(title),
	)
	out, err := exec.Command(bin, "-e", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("notify: osascript: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func escape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// Noop is a Sender that drops every notification. The daemon uses this as
// its default when no notifier has been configured, so background runs in
// CI / tests do not pop GUI alerts.
type Noop struct{}

func (Noop) Send(_, _ string) error { return nil }

// Recorder is a test Sender that captures every Send call for later
// assertion. Safe for concurrent use.
type Recorder struct {
	mu    sync.Mutex
	calls []Call
}

type Call struct {
	Title, Message string
}

func (r *Recorder) Send(title, message string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, Call{Title: title, Message: message})
	return nil
}

func (r *Recorder) Calls() []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Call, len(r.calls))
	copy(out, r.calls)
	return out
}
