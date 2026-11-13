package main

import "fmt"

// workingmanFlag is the tri-state --workingman-agent flag: "auto" (the default,
// zero value), "on" or "off". It is a boolean flag, so a bare --workingman-agent
// means on and --workingman-agent=false means off; --workingman-agent=auto
// restores the default explicitly.
type workingmanFlag string

const (
	workingmanAuto workingmanFlag = ""
	workingmanOn   workingmanFlag = "on"
	workingmanOff  workingmanFlag = "off"
)

func (f *workingmanFlag) String() string {
	if f == nil || *f == workingmanAuto {
		return "auto"
	}
	return string(*f)
}

func (f *workingmanFlag) IsBoolFlag() bool { return true }

func (f *workingmanFlag) Set(s string) error {
	switch s {
	case "true", "on", "1", "yes":
		*f = workingmanOn
	case "false", "off", "0", "no":
		*f = workingmanOff
	case "auto", "":
		*f = workingmanAuto
	default:
		return fmt.Errorf(`want auto, on or off (got %q)`, s)
	}
	return nil
}

// workingmanAgentEnabled decides whether the daemon runs the workingman agent,
// and says why (for the audit log). auto turns it on whenever --acp-kit is set,
// with or without channels: a human can talk to it from the local TUI as well
// as from an inbound channel. It always needs the ACP launcher (--acp-kit): the
// agent is a persistent sandboxed session with read-only mounts, which the
// legacy terminal-tab path cannot provide.
func workingmanAgentEnabled(mode workingmanFlag, acpKit string) (bool, string) {
	switch mode {
	case workingmanOff:
		return false, "--workingman-agent=off"
	case workingmanOn:
		if acpKit == "" {
			return false, "--workingman-agent needs --acp-kit"
		}
		return true, "--workingman-agent"
	}
	if acpKit == "" {
		return false, "auto: no --acp-kit"
	}
	return true, "auto: --acp-kit set"
}
