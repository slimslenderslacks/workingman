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

// usageHeader is the top of `orch --help`: the daemon is the default command,
// the subcommands are dispatched by main before flag parsing, so the flag
// package's own listing (which follows) would not mention them.
const usageHeader = `usage: orch --root <dir> [flags]        run the daemon, with the TUI unless --headless
       orch tui --root <dir>            run only the TUI over a directory
       orch status [--json]             print the running daemon's state snapshot
       orch whatsapp <subcommand>       set up and check the WhatsApp channel (see docs/channels.md)

whatsapp subcommands:
  setup    configure the Business Cloud API channel: credentials, owner allowlist, webhook
  status   show the (masked) config, Graph reachability and the webhook listener
  test     send a test message and print its id or the error
  pair     link a personal WhatsApp number to the bridge backend by QR code

Run "orch <subcommand> -h" for a subcommand's flags. Daemon flags:

`

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "tui" {
		runTUI(args[1:])
		return
	}
	if len(args) > 0 && args[0] == "status" {
		os.Exit(runStatus(args[1:], os.Stdout, os.Stderr))
	}
	if len(args) > 0 && args[0] == "help" {
		fmt.Fprint(os.Stdout, usageHeader)
		return
	}
	if len(args) > 0 && args[0] == "whatsapp" {
		os.Exit(runWhatsApp(args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	runDaemon(args)
}

// runDaemon is the default entry point. It builds the daemon and, unless
// --headless is set, runs a TUI in the same process that subscribes to the
// daemon's live session and project state. Headless mode is the CI path and
// keeps the pre-TUI behaviour: pure daemon loop, no terminal UI.
func runDaemon(args []string) {
	fs := flag.NewFlagSet("orch", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), usageHeader)
		fs.PrintDefaults()
	}
	var roots rootsFlag
	fs.Var(&roots, "root", "directory to watch (repeatable)")
	auditPath := fs.String("audit-log", "logs/audit.log", "path to the audit log")
	workspaceMode := fs.String("workspace-manager", "wsp", `workspace manager: "wsp" (real) or "stub" (test/dev)`)
	stubRoot := fs.String("stub-workspace-root", "", `when --workspace-manager=stub, directory where workspaces are created (default: $TMPDIR/orch-workspaces)`)
	tmuxSession := fs.String("tmux-session", agent.DefaultUmbrellaSession, "name of the umbrella tmux session every agent's window lives in")
	acpKit := fs.String("acp-kit", "", "acp-kit reference layered onto non-interactive agents' sandboxes (a local kit dir or published ref). When set, the planning/task/commit agents, the wolf and the workingman agent launch as acp-wrapper-backed ACP sessions instead of tmux+'sbx exec claude -p'")
	acpWrapper := fs.String("acp-wrapper", "", "path to the acp-wrapper binary (default: acp-wrapper on PATH)")
	sessionsRoot := fs.String("sessions-root", "", "root dir holding per-session dirs for ACP agents (default ~/.workingman/sessions)")
	wolfHost := fs.Bool("wolf-host", false, "escape hatch: run the wolf agent in a tmux window directly on the host (full host access, no sandbox, only reachable by attaching to tmux) instead of as a persistent ACP session in its own sandbox. Only matters when --acp-kit is set")
	wolfUnblockGrace := fs.Duration("wolf-unblock-grace", 0, "how long an ACP wolf lingers after its project leaves status:blocked before its conversation is ended (0 = default 2m, negative = never end on unblock)")
	wolfIdleTimeout := fs.Duration("wolf-idle-timeout", 0, "end an ACP wolf after this long with no ACP traffic (0 = default 24h, negative = never)")
	channelsConfig := fs.String("channels-config", "", "messaging-channels config (default ~/.workingman/channels.yaml, or $WORKINGMAN_CHANNELS_CONFIG). A missing file disables channels and the daemon behaves exactly as without them")
	var workingmanMode workingmanFlag
	fs.Var(&workingmanMode, "workingman-agent", `run the workingman agent: a persistent, read-only observer in its own sandbox that answers questions about the orch state. "auto" (default) turns it on when --acp-kit is set and channels.yaml has an inbound-capable channel; --workingman-agent / =on forces it on (requires --acp-kit); =off disables it`)
	headless := fs.Bool("headless", false, "run the daemon without the embedded TUI (for CI/non-interactive use)")
	stateFile := fs.String("state-file", "", `where the daemon publishes its runtime-state snapshot (JSON, atomic writes) for external readers such as the workingman agent. Must be outside every --root. Default: <sessions-root>/../state/snapshot.json; "off" disables`)
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
	// Pre-load the in-memory audit tail from what earlier runs wrote so the
	// state snapshot's recent-events list is useful immediately after a restart.
	if lines, err := audit.TailFile(*auditPath, 500); err == nil {
		a.Seed(lines)
	}

	// Commit signing relies on the private ssh-agent that start-orch.sh starts
	// and exports as SSH_AUTH_SOCK. We never redirect it: if it is unset or is
	// the macOS launchd agent (no signing key), warn loudly instead.
	if sshAgentUnusable(os.Getenv("SSH_AUTH_SOCK")) {
		a.Log("ssh_auth_sock_unusable",
			"path", os.Getenv("SSH_AUTH_SOCK"),
			"detail", "SSH_AUTH_SOCK is unset or the macOS launchd agent; commit signing will fail — start orch via start-orch.sh so it runs with the private ssh-agent")
	}

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
	// The wolf also runs as a (persistent) ACP session unless --wolf-host is set;
	// the archive agent keeps the tmux launcher above.
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
		r.WolfOnHost = *wolfHost
		r.PersistentUnblockGrace = *wolfUnblockGrace
		r.PersistentIdleTimeout = *wolfIdleTimeout
	} else {
		// No ACP kit configured: keep the legacy sandboxed tmux path for
		// non-interactive agents so the daemon still functions standalone.
		r.Sandbox = runner.DefaultSandboxCreator
	}

	stateDir, err := stateDirFor(*sessionsRoot)
	if err != nil {
		log.Fatal(err)
	}
	ch, err := setupChannels(*channelsConfig, filepath.Dir(*auditPath), stateDir, a)
	if err != nil {
		log.Fatalf("channels: %v", err)
	}
	defer ch.stop(a)

	dopts := []daemon.Option{
		daemon.WithRunner(r),
		daemon.WithScheduler(scheduler.New()),
	}
	dopts = append(dopts, ch.options(a)...)
	stateFilePath, err := resolveStateFile(*stateFile, *sessionsRoot)
	if err != nil {
		log.Fatal(err)
	}
	if stateFilePath != "" {
		dopts = append(dopts,
			daemon.WithStateFile(stateFilePath, 0),
			daemon.WithRuntimeInfo(daemon.RuntimeInfo{
				WorkspaceManager: *workspaceMode,
				AcpKit:           *acpKit,
				AcpWrapper:       *acpWrapper,
				SessionsRoot:     *sessionsRoot,
				TmuxSession:      *tmuxSession,
				AuditLog:         absPath(*auditPath),
				Headless:         *headless,
			}),
		)
	}
	if on, why := workingmanAgentEnabled(workingmanMode, ch, *acpKit); on {
		dopts = append(dopts, daemon.WithWorkingmanAgent(daemon.WorkingmanAgentConfig{
			AuditLog: absPath(*auditPath),
		}))
		a.Log("workingman_agent_flag", "enabled", "true", "why", why)
	} else {
		if workingmanMode == workingmanOn {
			log.Fatalf("%s", why) // asked for explicitly: don't silently run without it
		}
		a.Log("workingman_agent_flag", "enabled", "false", "why", why)
	}
	d, err := daemon.New(roots, a, dopts...)
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	ch.start(ctx, a, d)

	a.Log("daemon_start",
		"pid", fmt.Sprintf("%d", os.Getpid()),
		"workspace_manager", *workspaceMode,
		"roots", strings.Join(roots, ","),
		"headless", fmt.Sprintf("%t", *headless),
		"tmux", tmuxBin,
		"tmux_session", *tmuxSession,
		"acp_kit", *acpKit,
		"wolf_host", fmt.Sprintf("%t", *wolfHost),
		"state_file", stateFilePath,
	)

	if *headless {
		if err := d.Run(ctx); err != nil {
			ch.stop(a)
			a.Log("daemon_error", "err", err.Error())
			log.Fatal(err)
		}
		ch.stop(a)
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

	tuiErr := tui.Run(ctx, roots, sessCh, *auditPath, acpSessionsRoot, interactiveLauncher{d})

	// TUI exited — either the user quit or ctx was already cancelled. Cancel
	// to be sure, then wait for the daemon to wind down so its shutdown
	// (watcher close, scheduler stop, sessions closed) completes before we
	// return.
	cancel()
	daemonErr := <-daemonErrCh
	ch.stop(a)

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
						SandboxName: s.SandboxName,
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
	// Standalone tui has no daemon/runner, so :dir and :session are unavailable
	// (nil launcher). Run `orch --root=...` for the integrated experience.
	if err := tui.Run(ctx, roots, nil, *auditPath, "", nil); err != nil {
		log.Fatal(err)
	}
}

// interactiveLauncher adapts the daemon's interactive-session methods to the
// tui.InteractiveLauncher interface, keeping the tui package decoupled from the
// daemon the same way adaptSessionFeed does for the sessions feed.
type interactiveLauncher struct{ d *daemon.Daemon }

func (l interactiveLauncher) OpenShell(ctx context.Context, projectPath string) (string, error) {
	return l.d.OpenInteractiveShell(ctx, projectPath)
}

func (l interactiveLauncher) OpenSession(ctx context.Context, projectPath string) (string, error) {
	return l.d.OpenInteractiveSession(ctx, projectPath)
}

// sshAgentUnusable reports whether current is an SSH_AUTH_SOCK that cannot
// hold the signing key: unset, or the macOS system (launchd) agent a GUI/login
// shell inherits. Any other value is assumed to be the private agent.
func sshAgentUnusable(current string) bool {
	return current == "" || strings.Contains(current, "com.apple.launchd")
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
