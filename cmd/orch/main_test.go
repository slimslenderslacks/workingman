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
