package main

import (
	"net"
	"time"

	"github.com/slimslenderslacks/work/internal/postoffice"
)

// stepUpAgentEnabled decides whether the daemon runs the Step-Up agent, and
// says why (for the audit log). It takes the same auto/on/off flag type as
// --workingman-agent. auto turns it on when --acp-kit is set and post-office
// answers (reachable): the agent's only tools are post-office's, so without it
// the agent could do nothing. on needs only --acp-kit; a post-office that comes
// up later is picked up by the gateway on the agent's next launch.
func stepUpAgentEnabled(mode workingmanFlag, acpKit string, reachable func() bool) (bool, string) {
	switch mode {
	case workingmanOff:
		return false, "--step-up-agent=off"
	case workingmanOn:
		if acpKit == "" {
			return false, "--step-up-agent needs --acp-kit"
		}
		return true, "--step-up-agent"
	}
	if acpKit == "" {
		return false, "auto: no --acp-kit"
	}
	if !reachable() {
		return false, "auto: post-office is not reachable at " + postoffice.DefaultAddr
	}
	return true, "auto: --acp-kit set and post-office reachable"
}

// postOfficeReachable reports whether something accepts connections on
// post-office's address.
func postOfficeReachable() bool {
	c, err := net.DialTimeout("tcp", postoffice.DefaultAddr, time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}
