package main

import (
	"flag"
	"testing"
)

func parseWorkingmanFlag(t *testing.T, args ...string) workingmanFlag {
	t.Helper()
	var f workingmanFlag
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.Var(&f, "workingman-agent", "")
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return f
}

func TestWorkingmanFlagParsing(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want workingmanFlag
	}{
		{nil, workingmanAuto},
		{[]string{"--workingman-agent"}, workingmanOn},
		{[]string{"--workingman-agent=true"}, workingmanOn},
		{[]string{"--workingman-agent=on"}, workingmanOn},
		{[]string{"--workingman-agent=false"}, workingmanOff},
		{[]string{"--workingman-agent=off"}, workingmanOff},
		{[]string{"--workingman-agent=auto"}, workingmanAuto},
	} {
		if got := parseWorkingmanFlag(t, tc.args...); got != tc.want {
			t.Errorf("%v → %q, want %q", tc.args, got, tc.want)
		}
	}
	var f workingmanFlag
	if err := f.Set("maybe"); err == nil {
		t.Error(`Set("maybe") should be rejected`)
	}
}

func TestWorkingmanAgentEnabled(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode workingmanFlag
		kit  string
		want bool
	}{
		{"auto with ACP kit", workingmanAuto, "kit", true},
		{"auto without ACP kit", workingmanAuto, "", false},
		{"off wins over the ACP kit", workingmanOff, "kit", false},
		{"on with ACP kit", workingmanOn, "kit", true},
		{"on without ACP kit", workingmanOn, "", false},
	} {
		got, why := workingmanAgentEnabled(tc.mode, tc.kit)
		if got != tc.want {
			t.Errorf("%s: enabled = %v (%s), want %v", tc.name, got, why, tc.want)
		}
		if why == "" {
			t.Errorf("%s: no reason given", tc.name)
		}
	}
}
