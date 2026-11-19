// Command decision-tui is a host-side terminal UI for answering decision
// requests. It watches an inbox directory for JSON request files, lists the
// pending ones, renders the selected request's markdown summary and lets the
// user pick one of the request's own choices. The answer is written
// atomically to the outbox as `<id>.json` and the request is moved into
// `<inbox>/answered/`.
//
// Usage:
//
//	decision-tui [--inbox DIR] [--outbox DIR]
//
// The directories default to ~/.workingman/decisions/inbox and
// ~/.workingman/decisions/outbox and are created if missing. The command is
// standalone; it is not started by the orch daemon.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/slimslenderslacks/work/internal/decision"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "decision-tui: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	defInbox, err := decision.DefaultInbox()
	if err != nil {
		return err
	}
	defOutbox, err := decision.DefaultOutbox()
	if err != nil {
		return err
	}
	inbox := flag.String("inbox", defInbox, "directory to watch for decision request JSON files")
	outbox := flag.String("outbox", defOutbox, "directory to write answer JSON files to")
	flag.Parse()
	if flag.NArg() != 0 {
		flag.Usage()
		return fmt.Errorf("unexpected arguments: %v", flag.Args())
	}

	if err := decision.EnsureDirs(*inbox, *outbox); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Start watching before scanning so no file is missed in between.
	events, err := decision.Watch(ctx, *inbox)
	if err != nil {
		return err
	}
	pending, errs := decision.Scan(*inbox)

	// Pick the glamour style up front: auto-detection queries the terminal,
	// which must not happen once bubbletea owns it.
	style := "light"
	if lipgloss.HasDarkBackground() {
		style = "dark"
	}

	m := newModel(config{
		Inbox:         *inbox,
		Outbox:        *outbox,
		Events:        events,
		GlamourStyle:  style,
		InitialErrors: errs,
	}, pending)
	_, err = tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion()).Run()
	return err
}
