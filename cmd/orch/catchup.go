package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/slimslenderslacks/work/internal/daemon"
)

// runCatchup implements `orch catchup <project.yaml>`: it asks the running
// daemon to fire that project's cron schedule once, immediately. This is the
// one-shot the sleepy-head agent uses; the daemon only honours it while the
// project is recorded as having missed ticks, so repeating it is harmless.
func runCatchup(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("orch catchup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: orch catchup <project-path>   (path to the project's project.yaml)")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	path := absPath(fs.Arg(0))
	if err := daemon.RequestCatchup(path); err != nil {
		fmt.Fprintf(stderr, "orch catchup: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "catch-up requested for %s; the daemon fires its schedule once\n", path)
	return 0
}
