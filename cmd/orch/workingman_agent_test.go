package main

import (
	"flag"
	"testing"

	"github.com/slimslenderslacks/work/internal/channels"
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

func chWith(t *testing.T, yaml string) *daemonChannels {
	t.Helper()
	cfg, err := channels.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse channels config: %v", err)
	}
	return &daemonChannels{reg: channels.NewRegistry(), cfg: cfg}
}

func TestWorkingmanAgentEnabled(t *testing.T) {
	inbound := chWith(t, "channels:\n  whatsapp:\n    type: whatsapp\n")
	disabled := chWith(t, "channels:\n  whatsapp:\n    type: whatsapp\n    enabled: false\n")

	for _, tc := range []struct {
		name string
		mode workingmanFlag
		ch   *daemonChannels
		kit  string
		want bool
	}{
		{"auto, no channels", workingmanAuto, &daemonChannels{}, "kit", false},
		{"auto, nil channels", workingmanAuto, nil, "kit", false},
		{"auto, only a disabled channel", workingmanAuto, disabled, "kit", false},
		{"auto, inbound channel", workingmanAuto, inbound, "kit", true},
		{"auto, inbound channel but no ACP kit", workingmanAuto, inbound, "", false},
		{"off wins over an inbound channel", workingmanOff, inbound, "kit", false},
		{"on without channels", workingmanOn, &daemonChannels{}, "kit", true},
		{"on without ACP kit", workingmanOn, &daemonChannels{}, "", false},
	} {
		got, why := workingmanAgentEnabled(tc.mode, tc.ch, tc.kit)
		if got != tc.want {
			t.Errorf("%s: enabled = %v (%s), want %v", tc.name, got, why, tc.want)
		}
		if why == "" {
			t.Errorf("%s: no reason given", tc.name)
		}
	}
}
