package main

import "testing"

func TestStepUpAgentEnabled(t *testing.T) {
	up := func() bool { return true }
	down := func() bool { return false }
	for _, tc := range []struct {
		name      string
		mode      workingmanFlag
		kit       string
		reachable func() bool
		want      bool
	}{
		{"auto with ACP kit and post-office", workingmanAuto, "kit", up, true},
		{"auto without post-office", workingmanAuto, "kit", down, false},
		{"auto without ACP kit", workingmanAuto, "", up, false},
		{"off wins", workingmanOff, "kit", up, false},
		{"on without post-office", workingmanOn, "kit", down, true},
		{"on without ACP kit", workingmanOn, "", up, false},
	} {
		got, why := stepUpAgentEnabled(tc.mode, tc.kit, tc.reachable)
		if got != tc.want {
			t.Errorf("%s: enabled = %v (%s), want %v", tc.name, got, why, tc.want)
		}
		if why == "" {
			t.Errorf("%s: no reason given", tc.name)
		}
	}
}
