package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/audit"
)

func TestShouldDefaultSSHAgent(t *testing.T) {
	cases := map[string]bool{
		"": true, // unset → default
		"/var/run/com.apple.launchd.DAfUmm856n/Listeners":                           true, // macOS system agent → replace
		"/private/var/run/com.apple.launchd.abc/Listeners":                          true,
		"/Users/jim/Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock": false, // already 1Password → leave
		"/tmp/my-own-agent.sock":                                                    false, // deliberate custom agent → respect
	}
	for in, want := range cases {
		if got := shouldDefaultSSHAgent(in); got != want {
			t.Errorf("shouldDefaultSSHAgent(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestOnePasswordAgentSockDetectsSocket(t *testing.T) {
	// Point HOME at a temp dir and materialize the 1Password socket layout so
	// the detector's path + socket-mode check is exercised without depending on
	// the real host having 1Password installed.
	home := t.TempDir()
	t.Setenv("HOME", home)

	// No socket yet → empty.
	if got := onePasswordAgentSock(); got != "" {
		t.Errorf("expected empty result before socket exists, got %q", got)
	}

	dir := filepath.Join(home, "Library", "Group Containers", "2BUA8C4S2C.com.1password", "t")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(dir, "agent.sock")

	// A regular file at the socket path must NOT be treated as an agent — the
	// os.ModeSocket gate is what keeps a stray file from being mistaken for the
	// live 1Password agent.
	if err := os.WriteFile(sockPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := onePasswordAgentSock(); got != "" {
		t.Errorf("a plain file must not count as the agent socket, got %q", got)
	}
}

// TestLogOnePasswordAgentMissingDistinguishesCauses asserts the diagnostic
// added for item 3 (onePasswordAgentSock hardcodes one container id) tells
// apart "1Password looks installed here but has no live agent socket"
// (likely SSH agent integration disabled, or locked) from "no 1Password app
// data found under this id at all" (not installed, or a build using a
// different container id we don't check) — so a missing/wrong path doesn't
// fail silently with nothing to go on.
func TestLogOnePasswordAgentMissingDistinguishesCauses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	var buf bytes.Buffer
	logOnePasswordAgentMissing(audit.New(&buf))
	if got := buf.String(); !strings.Contains(got, "no 1Password app data found") {
		t.Errorf("with no container dir, got %q, want mention of missing app data", got)
	}

	if err := os.MkdirAll(onePasswordContainerDir(home), 0o755); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	logOnePasswordAgentMissing(audit.New(&buf))
	if got := buf.String(); !strings.Contains(got, "no live SSH agent socket") {
		t.Errorf("with container dir present but no socket, got %q, want mention of missing live socket", got)
	}
}
