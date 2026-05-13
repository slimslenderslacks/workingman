package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/daemon"
	"github.com/slimslenderslacks/work/internal/notify"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/scheduler"
	"github.com/slimslenderslacks/work/internal/tui"
	"github.com/slimslenderslacks/work/internal/workspace"
)

type rootsFlag []string

func (r *rootsFlag) String() string { return strings.Join(*r, ",") }
func (r *rootsFlag) Set(s string) error {
	abs, err := filepath.Abs(s)
	if err != nil {
		return err
	}
	*r = append(*r, abs)
	return nil
}

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "tui" {
		runTUI(args[1:])
		return
	}
	runDaemon(args)
}

func runDaemon(args []string) {
	fs := flag.NewFlagSet("orch", flag.ExitOnError)
	var roots rootsFlag
	fs.Var(&roots, "root", "directory to watch (repeatable)")
	auditPath := fs.String("audit-log", "logs/audit.log", "path to the audit log")
	workspaceMode := fs.String("workspace-manager", "wsp", `workspace manager: "wsp" (real) or "stub" (test/dev)`)
	stubRoot := fs.String("stub-workspace-root", "", `when --workspace-manager=stub, directory where workspaces are created (default: $TMPDIR/orch-workspaces)`)
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	if len(roots) == 0 {
		fmt.Fprintln(os.Stderr, "at least one --root is required")
		fs.Usage()
		os.Exit(2)
	}

	if err := os.MkdirAll(filepath.Dir(*auditPath), 0o755); err != nil {
		log.Fatalf("mkdir audit log dir: %v", err)
	}
	f, err := os.OpenFile(*auditPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatalf("open audit log: %v", err)
	}
	defer f.Close()
	a := audit.New(f)

	wsMgr, err := buildWorkspaceManager(*workspaceMode, *stubRoot)
	if err != nil {
		log.Fatal(err)
	}

	r := &runner.Runner{
		Workspaces: wsMgr,
		Launcher:   &agent.TmuxLauncher{},
		Audit:      a,
		// Command defaults to claude-code via runner.DefaultCommandBuilder.
	}

	d, err := daemon.New(roots, a,
		daemon.WithRunner(r),
		daemon.WithNotifier(&notify.Osascript{}),
		daemon.WithScheduler(scheduler.New()),
	)
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	a.Log("daemon_start",
		"pid", fmt.Sprintf("%d", os.Getpid()),
		"workspace_manager", *workspaceMode,
		"roots", strings.Join(roots, ","),
	)
	if err := d.Run(ctx); err != nil {
		a.Log("daemon_error", "err", err.Error())
		log.Fatal(err)
	}
	a.Log("daemon_stop")
}

func runTUI(args []string) {
	fs := flag.NewFlagSet("orch tui", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := tui.Run(ctx); err != nil {
		log.Fatal(err)
	}
}

func buildWorkspaceManager(mode, stubRoot string) (workspace.Manager, error) {
	switch mode {
	case "wsp":
		return workspace.NewWsp(), nil
	case "stub":
		root := stubRoot
		if root == "" {
			root = filepath.Join(os.TempDir(), "orch-workspaces")
		}
		if err := os.MkdirAll(root, 0o755); err != nil {
			return nil, fmt.Errorf("stub workspace root: %w", err)
		}
		return workspace.NewStub(root), nil
	default:
		return nil, fmt.Errorf("unknown --workspace-manager value %q (expected wsp|stub)", mode)
	}
}
