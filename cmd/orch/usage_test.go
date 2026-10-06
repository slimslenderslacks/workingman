package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// `orch --help` must list every subcommand and the flags that switch the
// channels feature on. The flag package exits on -h, so the usage text is read
// from a child run of this test binary.
func TestUsageListsSubcommandsAndFlags(t *testing.T) {
	if os.Getenv("ORCH_USAGE_CHILD") == "1" {
		runDaemon([]string{"-h"})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestUsageListsSubcommandsAndFlags$")
	cmd.Env = append(os.Environ(), "ORCH_USAGE_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("orch -h: %v\n%s", err, out)
	}
	usage := string(out)
	for _, want := range []string{
		"orch tui", "orch status", "orch whatsapp <subcommand>", "orch signal <subcommand>", "signal subcommands:",
		"setup ", "test ", "pair ",
		"-channels-config", "-workingman-agent", "-wolf-host", "-wolf-unblock-grace", "-wolf-idle-timeout", "-state-file", "-headless", "-acp-kit",
	} {
		if !strings.Contains(usage, want) {
			t.Errorf("orch -h does not mention %q:\n%s", want, usage)
		}
	}
}
