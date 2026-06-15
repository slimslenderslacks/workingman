package main

import (
	"context"
	"flag"
	"fmt"
	"io"
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
	"github.com/slimslenderslacks/work/internal/session"
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

// runDaemon is the default entry point. It builds the daemon and, unless
// --headless is set, runs a TUI in the same process that subscribes to the
// daemon's live session and project state. Headless mode is the CI path and
// keeps the pre-TUI behaviour: pure daemon loop, no terminal UI.
func runDaemon(args []string) {
	fs := flag.NewFlagSet("orch", flag.ExitOnError)
	var roots rootsFlag
	fs.Var(&roots, "root", "directory to watch (repeatable)")
	auditPath := fs.String("audit-log", "logs/audit.log", "path to the audit log")
	workspaceMode := fs.String("workspace-manager", "wsp", `workspace manager: "wsp" (real) or "stub" (test/dev)`)
	stubRoot := fs.String("stub-workspace-root", "", `when --workspace-manager=stub, directory where workspaces are created (default: $TMPDIR/orch-workspaces)`)
	tmuxSession := fs.String("tmux-session", agent.DefaultUmbrellaSession, "name of the umbrella tmux session every agent's window lives in")
	acpKit := fs.String("acp-kit", "", "acp-kit reference layered onto non-interactive agents' sandboxes (a local kit dir or published ref). When set, planning/task/commit agents launch as acp-wrapper-backed ACP sessions instead of tmux+`sbx exec claude -p`")
	acpWrapper := fs.String("acp-wrapper", "", "path to the acp-wrapper binary (default: acp-wrapper on PATH)")
	sessionsRoot := fs.String("sessions-root", "", "root dir holding per-session dirs for ACP agents (default ~/.workingman/sessions)")
	headless := fs.Bool("headless", false, "run the daemon without the embedded TUI (for CI/non-interactive use)")
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

	// A GUI-launched daemon inherits the bare macOS default PATH, missing
	// the Nix profiles, Homebrew, etc. Augment so child shells (the tmux
	// session running claude, git, wsp, ...) can find their binaries.
	augmentedPATH := agent.AugmentSearchPath(agent.DefaultPATHCandidates())
	tmuxBin, err := agent.ResolveTmux()
	if err != nil {
		log.Fatalf("locating tmux: %v\nPATH=%s", err, augmentedPATH)
	}

	r := &runner.Runner{
		Workspaces: wsMgr,
		Launcher: &agent.TmuxLauncher{
			Binary:      tmuxBin,
			SessionName: *tmuxSession,
		},
		Audit: a,
		// Command defaults to claude-code via runner.DefaultCommandBuilder.
	}

	// When --acp-kit is set, non-interactive agents (planning/task/commit) are
	// launched as acp-wrapper-backed ACP sessions rather than tmux windows
	// running `sbx exec claude -p`. The wrapper is a host process: its stderr
	// (diagnostics) goes to a log file so it never corrupts the TUI alt-screen.
	// Interactive agents (project/wolf) keep the tmux launcher above.
	if *acpKit != "" {
		acpLogPath := filepath.Join(filepath.Dir(*auditPath), "acp-wrapper.log")
		acpLog, err := os.OpenFile(acpLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			log.Fatalf("open acp-wrapper log: %v", err)
		}
		defer acpLog.Close()
		r.AcpLauncher = &agent.ProcessLauncher{Stderr: acpLog}
		r.Kit = *acpKit
		r.AcpWrapperPath = *acpWrapper
		r.SessionsRoot = *sessionsRoot
	} else {
		// No ACP kit configured: keep the legacy sandboxed tmux path for
		// non-interactive agents so the daemon still functions standalone.
		r.Sandbox = runner.DefaultSandboxCreator
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
		"headless", fmt.Sprintf("%t", *headless),
		"tmux", tmuxBin,
		"tmux_session", *tmuxSession,
		"acp_kit", *acpKit,
	)

	if *headless {
		if err := d.Run(ctx); err != nil {
			a.Log("daemon_error", "err", err.Error())
			log.Fatal(err)
		}
		a.Log("daemon_stop")
		return
	}

	// Stdlib log writes to stderr by default; with the TUI on the alt-screen
	// any stray log line would tear the rendering. Redirect to a sibling of
	// the audit log so the output is preserved without corrupting the UI.
	// Anything fatal that happens *before* this point still goes to stderr,
	// which is what we want — the TUI isn't up yet.
	logPath := filepath.Join(filepath.Dir(*auditPath), "daemon.log")
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatalf("open daemon log: %v", err)
	}
	defer logFile.Close()
	restoreLog := redirectStdlibLog(logFile)
	defer restoreLog()

	daemonErrCh := make(chan error, 1)
	go func() {
		daemonErrCh <- d.Run(ctx)
	}()

	sessCh := adaptSessionFeed(ctx, d.WatchSessions(ctx, 0))

	// The TUI watches the same ACP sessions root the runner writes to, so the
	// `a`-key tab view shows one tab per live ACP session. Only meaningful when
	// non-interactive agents run as ACP sessions (--acp-kit set); otherwise no
	// ACP sessions exist, so we skip the watcher.
	acpSessionsRoot := ""
	if *acpKit != "" {
		acpSessionsRoot = *sessionsRoot
		if acpSessionsRoot == "" {
			if def, err := session.DefaultRoot(); err == nil {
				acpSessionsRoot = def
			}
		}
	}

	tuiErr := tui.Run(ctx, roots, sessCh, *auditPath, acpSessionsRoot)

	// TUI exited — either the user quit or ctx was already cancelled. Cancel
	// to be sure, then wait for the daemon to wind down so its shutdown
	// (watcher close, scheduler stop, sessions closed) completes before we
	// return.
	cancel()
	daemonErr := <-daemonErrCh

	a.Log("daemon_stop")

	// Restore stderr logging before surfacing errors so the user actually
	// sees them. defer would also do this, but we want the message on the
	// real stderr instead of buried in daemon.log.
	restoreLog()
	if daemonErr != nil {
		a.Log("daemon_error", "err", daemonErr.Error())
		fmt.Fprintf(os.Stderr, "daemon: %v\n", daemonErr)
		os.Exit(1)
	}
	if tuiErr != nil {
		fmt.Fprintf(os.Stderr, "tui: %v\n", tuiErr)
		os.Exit(1)
	}
}

// adaptSessionFeed bridges daemon.SessionInfo snapshots into the tui's
// SessionView shape so the two packages stay decoupled. The output channel
// closes when in closes or ctx is done.
func adaptSessionFeed(ctx context.Context, in <-chan []daemon.SessionInfo) <-chan []tui.SessionView {
	out := make(chan []tui.SessionView)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case infos, ok := <-in:
				if !ok {
					return
				}
				views := make([]tui.SessionView, len(infos))
				for i, s := range infos {
					views[i] = tui.SessionView{
						ID:          s.ID,
						AgentName:   s.AgentName,
						Project:     s.Project,
						TmuxTarget:  s.TmuxTarget,
						Status:      string(s.Status),
						StartedAt:   s.StartedAt,
						TaskName:    s.TaskName,
						Interactive: s.Interactive,
					}
				}
				select {
				case out <- views:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

// redirectStdlibLog points the default logger at w and returns a function that
// restores the prior state. Idempotent: calling the returned function a second
// time is a no-op.
func redirectStdlibLog(w io.Writer) func() {
	prevOut := log.Writer()
	prevFlags := log.Flags()
	prevPrefix := log.Prefix()
	log.SetOutput(w)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetPrefix("orch ")
	var restored bool
	return func() {
		if restored {
			return
		}
		restored = true
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
		log.SetPrefix(prevPrefix)
	}
}

func runTUI(args []string) {
	fs := flag.NewFlagSet("orch tui", flag.ExitOnError)
	var roots rootsFlag
	fs.Var(&roots, "root", "directory to scan for .project.yaml files (repeatable)")
	auditPath := fs.String("audit-log", "", "path to an audit log file to tail in the bottom pane (optional)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// The standalone `orch tui` mode has no daemon to subscribe to, so the
	// sessions pane gets a nil source — the pane renders "(none)" in that
	// case. To see live sessions, run `orch --root=...` (the integrated
	// mode wires the daemon's WatchSessions into the TUI).
	if err := tui.Run(ctx, roots, nil, *auditPath, ""); err != nil {
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
