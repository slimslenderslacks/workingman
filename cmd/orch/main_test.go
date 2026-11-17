package main

import "testing"

func TestSSHAgentUnusable(t *testing.T) {
	cases := map[string]bool{
		"": true, // unset → warn
		"/var/run/com.apple.launchd.DAfUmm856n/Listeners":  true, // macOS system agent → warn
		"/private/var/run/com.apple.launchd.abc/Listeners": true,
		"/tmp/ssh-XXXX/agent.123":                          false, // private agent → fine
	}
	for in, want := range cases {
		if got := sshAgentUnusable(in); got != want {
			t.Errorf("sshAgentUnusable(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestPinHostGitToPrivateAgent(t *testing.T) {
	run := func(env map[string]string) map[string]string {
		pinHostGitToPrivateAgent(func(k string) string { return env[k] },
			func(k, v string) error { env[k] = v; return nil })
		return env
	}

	env := run(map[string]string{})
	if got := env["GIT_SSH_COMMAND"]; got != "ssh -o IdentityAgent=SSH_AUTH_SOCK" {
		t.Errorf("GIT_SSH_COMMAND = %q", got)
	}
	if env["GIT_CONFIG_COUNT"] != "1" || env["GIT_CONFIG_KEY_0"] != "gpg.ssh.program" || env["GIT_CONFIG_VALUE_0"] != "ssh-keygen" {
		t.Errorf("git config env = %v", env)
	}

	// Existing settings are extended, not clobbered.
	env = run(map[string]string{
		"GIT_SSH_COMMAND":  "ssh -v",
		"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "core.pager", "GIT_CONFIG_VALUE_0": "cat",
	})
	if got := env["GIT_SSH_COMMAND"]; got != "ssh -v -o IdentityAgent=SSH_AUTH_SOCK" {
		t.Errorf("GIT_SSH_COMMAND = %q", got)
	}
	if env["GIT_CONFIG_COUNT"] != "2" || env["GIT_CONFIG_KEY_0"] != "core.pager" ||
		env["GIT_CONFIG_KEY_1"] != "gpg.ssh.program" || env["GIT_CONFIG_VALUE_1"] != "ssh-keygen" {
		t.Errorf("git config env = %v", env)
	}

	// Already pinned: nothing changes.
	env = run(map[string]string{
		"GIT_SSH_COMMAND":  "ssh -o IdentityAgent=/tmp/a.sock",
		"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "gpg.ssh.program", "GIT_CONFIG_VALUE_0": "/x/sign",
	})
	if env["GIT_SSH_COMMAND"] != "ssh -o IdentityAgent=/tmp/a.sock" || env["GIT_CONFIG_COUNT"] != "1" || env["GIT_CONFIG_VALUE_0"] != "/x/sign" {
		t.Errorf("pinned env changed: %v", env)
	}
}
