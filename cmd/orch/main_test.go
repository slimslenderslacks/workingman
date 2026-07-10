package main

import (
	"os"
	"path/filepath"
	"testing"
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
