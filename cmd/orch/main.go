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
	"strconv"
	"strings"
	"syscall"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/daemon"
	"github.com/slimslenderslacks/work/internal/mcpgateway"
	"github.com/slimslenderslacks/work/internal/postoffice"
	"github.com/slimslenderslacks/work/internal/power"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/sbx"
	"github.com/slimslenderslacks/work/internal/scheduler"
	"github.com/slimslenderslacks/work/internal/session"
	"github.com/slimslenderslacks/work/internal/terminal"
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
       orch catchup <project-path>      fire a behind project's schedule once, now (used by sleepy-head)
       orch whatsapp <subcommand>       set up and check the WhatsApp channel (see docs/channels.md)
       orch signal <subcommand>         set up and check the Signal channel (see docs/channels.md)
       orch slack <subcommand>          set up and check the Slack channel (see docs/channels.md)
       orch service install|uninstall|status   run the daemon in the background via launchd (macOS)

whatsapp subcommands:
  setup    configure the Business Cloud API channel: credentials, owner allowlist, webhook
  status   show the (masked) config, Graph reachability and the webhook listener
  test     send a test message and print its id or the error
  pair     link a personal WhatsApp number to the bridge backend by QR code

signal subcommands:
  setup    configure signal-cli URL, account, owner allowlist, optional groups and routes
  status   show the (masked) config and whether signal-cli is reachable and registered
  test     send a test message and print its timestamp or the error

slack subcommands:
  setup    configure the gateway URL, conversation (default: your DM-to-self), allow_from and the post-office route
  status   show the config, whether the gateway serves the slack tools, and the watch list and cursors
  test     post a test message and wait for a reply in its thread

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
	if len(args) > 0 && args[0] == "catchup" {
		os.Exit(runCatchup(args[1:], os.Stdout, os.Stderr))
	}
	if len(args) > 0 && args[0] == "help" {
		fmt.Fprint(os.Stdout, usageHeader)
		return
	}
	if len(args) > 0 && args[0] == "whatsapp" {
		os.Exit(runWhatsApp(args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	if len(args) > 0 && args[0] == "service" {
		os.Exit(runService(args[1:], os.Stdout, os.Stderr))
	}
	if len(args) > 0 && args[0] == "signal" {
		os.Exit(runSignal(args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	if len(args) > 0 && args[0] == "slack" {
		os.Exit(runSlack(args[1:], os.Stdin, os.Stdout, os.Stderr))
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
	acpKit := fs.String("acp-kit", "", "acp-kit reference layered onto non-interactive agents' sandboxes (a local kit dir or published ref). When set, the planning/task/commit agents, the wolf and the workingman agent launch as acp-wrapper-backed ACP sessions instead of in terminal tabs running 'sbx exec claude -p'")
	acpImage := fs.String("acp-image", "", "prebuilt container image every ACP sandbox is created from, passed to `sbx create -t` (default: the stock claude template). Point it at acp-kit's prebuilt image (`make image-build`) so the kit's ~540MB npm install is paid once at image-build time instead of on every sandbox create. --acp-kit is still layered on top; only matters when --acp-kit is set")
	extraCACerts := fs.String("extra-ca-certs", os.Getenv(sbx.ExtraCAEnv), "PEM bundle of extra CAs every sandbox must trust, for a laptop whose VPN re-signs HTTPS with a corporate root (default $WORKINGMAN_EXTRA_CA_CERTS). Validated at startup: orch exits if the file is unreadable or holds no PEM certificate, and warns (subject + notAfter) about expired certs. Passed to every sandbox-creating path; empty changes nothing. See docs/sandbox-custom-ca.md")
	acpWrapper := fs.String("acp-wrapper", "", "path to the acp-wrapper binary (default: acp-wrapper on PATH)")
	sessionsRoot := fs.String("sessions-root", "", "root dir holding per-session dirs for ACP agents (default ~/.workingman/sessions)")
	wolfHost := fs.Bool("wolf-host", false, "escape hatch: run the wolf agent in a new terminal tab directly on the host (full host access, no sandbox, only reachable from that tab) instead of as a persistent ACP session in its own sandbox. Only matters when --acp-kit is set")
	wolfUnblockGrace := fs.Duration("wolf-unblock-grace", 0, "how long an ACP wolf lingers after its project leaves status:blocked before its conversation is ended (0 = default 2m, negative = never end on unblock)")
	wolfIdleTimeout := fs.Duration("wolf-idle-timeout", 0, "end an ACP wolf after this long with no ACP traffic (0 = default 24h, negative = never)")
	channelsConfig := fs.String("channels-config", "", "messaging-channels config (default ~/.workingman/channels.yaml, or $WORKINGMAN_CHANNELS_CONFIG). A missing file disables channels and the daemon behaves exactly as without them")
	var workingmanMode workingmanFlag
	fs.Var(&workingmanMode, "workingman-agent", `run the workingman agent: a persistent assistant in its own sandbox that answers questions about the orch state and, on request, edits the orch roots (projects, tasks, intake). "auto" (default) turns it on whenever --acp-kit is set, with or without channels; --workingman-agent / =on forces it on (requires --acp-kit); =off disables it`)
	var stepUpMode workingmanFlag
	fs.Var(&stepUpMode, "step-up-agent", `run the Step-Up agent: a persistent agent in its own sandbox that takes other agents' requests for more sandbox capability (such as a network route) through post-office, asks the operator and replies. "auto" (default) turns it on when --acp-kit is set and post-office answers on `+postoffice.DefaultAddr+`; --step-up-agent / =on forces it on (requires --acp-kit); =off disables it`)
	headless := fs.Bool("headless", false, "run the daemon without the embedded TUI (for CI/non-interactive use)")
	mcpGatewayURL := fs.String("mcp-gateway-url", os.Getenv("WORKINGMAN_MCP_GATEWAY_URL"), "base URL of the host's standalone-codemode gateway, e.g. http://localhost:3333 (default $WORKINGMAN_MCP_GATEWAY_URL). When set, every sandbox gets its own sub-gateway (POST /gateways) over the task's static_mcps — or --mcp-gateway-servers when a task names none — exposed inside it as MCP_URL (localhost rewritten to host.docker.internal), with the gateway's port allowed in its network policy; the gateway is DELETEd when the sandbox is removed. Empty disables the gateway (and MCP_URL)")
	mcpGatewayStatic := fs.Bool("mcp-gateway-static", os.Getenv("WORKINGMAN_MCP_GATEWAY_STATIC") == "1", "give no sandbox a dynamic gateway (default false, or true when $WORKINGMAN_MCP_GATEWAY_STATIC=1). By default the wolf and task agents' gateways are dynamic: they also serve mcp-find and mcp-add over the whole catalog")
	workingmanMCPDynamic := fs.Bool("workingman-agent-mcp-dynamic", os.Getenv("WORKINGMAN_AGENT_MCP_DYNAMIC") != "0", "give the workingman agent a dynamic gateway: --mcp-gateway-servers plus mcp-find and mcp-add over the whole catalog, so it can attach servers such as Slack mid-session (default true; false when $WORKINGMAN_AGENT_MCP_DYNAMIC=0). --mcp-gateway-static overrides it")
	mcpGatewayServers := fs.String("mcp-gateway-servers", os.Getenv("WORKINGMAN_MCP_GATEWAY_SERVERS"), "comma-separated catalog server names requested for a sandbox's gateway when its task has no static_mcps (default $WORKINGMAN_MCP_GATEWAY_SERVERS). Empty means such sandboxes get no gateway")
	stateFile := fs.String("state-file", "", `where the daemon publishes its runtime-state snapshot (JSON, atomic writes) for external readers such as the workingman agent. Must be outside every --root. Default: <sessions-root>/../state/snapshot.json; "off" disables`)
	maxParallelTasks := fs.Int("max-parallel-tasks", 0, "cap on task agents running at once across all projects (each is a sandbox plus a model session); a project still runs at most its own max_parallel_tasks. Commit, planning, review and wolf agents do not count. 0 means no cap")
	requireAC := fs.Bool("require-ac", false, "pause dispatching new work and cron firings while the laptop is on battery (macOS, via pmset), resuming on AC; skipped cron ticks are recorded as missed. No-op off macOS. `orch service install` turns it on")
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

	// Commit signing relies on the private ssh-agent that start.sh starts
	// and exports as SSH_AUTH_SOCK. We never redirect it: if it is unset or is
	// the macOS launchd agent (no signing key), warn loudly instead.
	if sshAgentUnusable(os.Getenv("SSH_AUTH_SOCK")) {
		a.Log("ssh_auth_sock_unusable",
			"path", os.Getenv("SSH_AUTH_SOCK"),
			"detail", "SSH_AUTH_SOCK is unset or the macOS launchd agent; commit signing will fail — start orch via start.sh so it runs with the private ssh-agent")
	} else {
		// Host-side git (wsp clones/fetches, applyBaseBranches' `git fetch`)
		// must use the private agent too, never 1Password's: see
		// pinHostGitToPrivateAgent.
		for _, kv := range pinHostGitToPrivateAgent(os.Getenv, os.Setenv) {
			a.Log("host_git_env", "set", kv)
		}
	}

	// Resolve a local --acp-kit to an absolute path up front: every
	// acp-wrapper (and session.json) gets this value, and a relative one would
	// otherwise depend on the cwd orch happened to start in. A stale or
	// mismatched kit is the usual reason a sandbox ends up with the stock
	// mcp-gateway MCP server instead of okta-gateway, so say so loudly.
	resolvedKit, kitWarnings := resolveAcpKit(*acpKit, *acpImage)
	*acpKit = resolvedKit
	for _, w := range kitWarnings {
		fmt.Fprintln(os.Stderr, "orch: warning:", w)
		a.Log("acp_kit_warning", "kit", *acpKit, "image", *acpImage, "detail", w)
	}

	// The per-laptop extra CA bundle: an invalid one fails startup here rather
	// than every sandbox launch later. Expired certs only warn, because the
	// file may still hold a valid root beside them.
	extraCA, err := loadExtraCACerts(*extraCACerts, os.Stderr, a)
	if err != nil {
		log.Fatalf("orch: --extra-ca-certs: %v", err)
	}

	wsMgr, err := buildWorkspaceManager(*workspaceMode, *stubRoot)
	if err != nil {
		log.Fatal(err)
	}

	// A GUI-launched daemon inherits the bare macOS default PATH, missing
	// the Nix profiles, Homebrew, etc. Augment so child processes (claude in
	// its terminal tab, git, wsp, ...) can find their binaries; the tab
	// launcher forwards this PATH into the tabs it opens.
	agent.AugmentSearchPath(agent.DefaultPATHCandidates())

	// Sessions that need a human at a real TTY — the --wolf-host wolf, the
	// TUI's :session/:orch/:workspace — open in a new tab of the user's terminal. The
	// terminal is resolved when a tab is first needed (detected from the
	// environment, else ~/.config/workingman/settings.yaml), so an unknown
	// terminal only fails the launch that needs one.
	tabs := &terminal.TabLauncher{}

	r := &runner.Runner{
		Workspaces: wsMgr,
		Launcher:   tabs,
		Audit:      a,
		// Command defaults to claude-code via runner.DefaultCommandBuilder.
		SessionsRoot:      *sessionsRoot,
		MCPGatewayURL:     strings.TrimSpace(*mcpGatewayURL),
		MCPGatewayServers: splitList(*mcpGatewayServers),
		MCPGatewayStatic:  *mcpGatewayStatic,
		ExtraCACerts:      extraCA,

		WorkingmanAgentMCPStatic: !*workingmanMCPDynamic,
	}
	a.Log("mcp_gateway_config", "url", r.MCPGatewayURL, "default_servers", strings.Join(r.MCPGatewayServers, ","), "static", strconv.FormatBool(r.MCPGatewayStatic),
		"workingman_agent_dynamic", strconv.FormatBool(r.MCPGatewayDynamic(agent.WorkingmanAgent)))

	// When --acp-kit is set, non-interactive agents (planning/task/commit/
	// archive) are launched as acp-wrapper-backed ACP sessions rather than
	// terminal tabs running `sbx exec claude -p`. The wrapper is a host process: its
	// stderr (diagnostics) goes to a log file so it never corrupts the TUI
	// alt-screen. The wolf also runs as a (persistent) ACP session unless
	// --wolf-host is set, in which case it keeps the tab launcher above.
	if *acpKit != "" {
		acpLogPath := filepath.Join(filepath.Dir(*auditPath), "acp-wrapper.log")
		acpLog, err := os.OpenFile(acpLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			log.Fatalf("open acp-wrapper log: %v", err)
		}
		defer acpLog.Close()
		r.AcpLauncher = &agent.ProcessLauncher{Stderr: acpLog}
		r.Kit = *acpKit
		r.Image = *acpImage
		r.AcpWrapperPath = *acpWrapper
		r.SessionsRoot = *sessionsRoot
		r.WolfOnHost = *wolfHost
		r.PersistentUnblockGrace = *wolfUnblockGrace
		r.PersistentIdleTimeout = *wolfIdleTimeout
	} else {
		// No ACP kit configured: keep the legacy sandboxed terminal-tab path for
		// non-interactive agents so the daemon still functions standalone.
		r.Sandbox = runner.DefaultSandboxCreator
	}

	stateDir, err := stateDirFor(*sessionsRoot)
	if err != nil {
		log.Fatal(err)
	}
	postOfficeDir, err := postOfficeDirFor(*sessionsRoot)
	if err != nil {
		log.Fatal(err)
	}
	ch, err := setupChannels(*channelsConfig, filepath.Dir(*auditPath), stateDir, postOfficeDir, a)
	if err != nil {
		log.Fatalf("channels: %v", err)
	}
	defer ch.stop(a)

	dopts := []daemon.Option{
		daemon.WithRunner(r),
		daemon.WithScheduler(scheduler.New()),
		daemon.WithCronState(filepath.Join(stateDir, "cron_state.json")),
		daemon.WithMaxParallelTasks(*maxParallelTasks),
	}
	if *requireAC {
		dopts = append(dopts, daemon.WithPowerSource(power.New(), 0))
	}
	if r.MCPGatewayURL != "" {
		// The daemon's own github-only gateway, for the PR fingerprint check.
		// Its record lives beside the per-sandbox ones, in a subdirectory the
		// wrappers' sweep never looks at.
		gwRoot, err := r.ResolveSessionsRoot()
		if err != nil {
			log.Fatal(err)
		}
		dopts = append(dopts, daemon.WithReviewGateway(daemon.ReviewGatewayConfig{
			BaseURL:  r.MCPGatewayURL,
			StateDir: daemon.ReviewGatewayStateDir(mcpgateway.StateDir(gwRoot)),
		}))
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
				AcpImage:         *acpImage,
				AcpWrapper:       *acpWrapper,
				ExtraCACerts:     extraCA,
				SessionsRoot:     *sessionsRoot,
				AuditLog:         absPath(*auditPath),
				Headless:         *headless,
			}),
		)
	}
	if on, why := workingmanAgentEnabled(workingmanMode, *acpKit); on {
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
	if on, why := stepUpAgentEnabled(stepUpMode, *acpKit, postOfficeReachable); on {
		dopts = append(dopts, daemon.WithStepUpAgent(daemon.StepUpAgentConfig{
			AuditLog: absPath(*auditPath),
		}))
		a.Log("step_up_agent_flag", "enabled", "true", "why", why)
	} else {
		if stepUpMode == workingmanOn {
			log.Fatalf("%s", why)
		}
		a.Log("step_up_agent_flag", "enabled", "false", "why", why)
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
		"terminal", terminal.Detect(os.Getenv),
		"acp_kit", *acpKit,
		"acp_image", *acpImage,
		"wolf_host", fmt.Sprintf("%t", *wolfHost),
		"require_ac", fmt.Sprintf("%t", *requireAC),
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

	tuiErr := tui.Run(ctx, roots, sessCh, acpSessionsRoot, postOfficeDir, interactiveLauncher{d}, tabs)

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
						TabName:     s.TabName,
						Status:      string(s.Status),
						StartedAt:   s.StartedAt,
						TaskName:    s.TaskName,
						Interactive: s.Interactive,
						SandboxName: s.SandboxName,
						Harness:     s.Harness,
						Model:       s.Model,
						Mounts:      s.Mounts,
						MCPs:        s.MCPs,
						MCPDynamic:  s.MCPDynamic,
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
	fs.Var(&roots, "root", "directory to scan for project.yaml files (repeatable)")
	// The TUI no longer shows an audit pane (tail -f the daemon's --audit-log
	// instead); the flag is still accepted so existing invocations don't break.
	_ = fs.String("audit-log", "", "deprecated and ignored: the TUI no longer shows the audit log; tail -f it instead")
	sessionsRoot := fs.String("sessions-root", "", "sessions root the post-office store sits beside, for the inbox (default ~/.workingman/sessions; $WORKINGMAN_POST_OFFICE_DIR overrides the store)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	// The inbox reads the post-office store directly, so it works here too,
	// with no daemon.
	postOfficeDir, err := postOfficeDirFor(*sessionsRoot)
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// The standalone `orch tui` mode has no daemon to subscribe to, so the
	// sessions pane gets a nil source — the pane renders "(none)" in that
	// case. To see live sessions, run `orch --root=...` (the integrated
	// mode wires the daemon's WatchSessions into the TUI).
	// Standalone tui has no daemon/runner, so :session, :orch and :workspace are unavailable
	// (nil launcher). Run `orch --root=...` for the integrated experience.
	if err := tui.Run(ctx, roots, nil, "", postOfficeDir, nil, nil); err != nil {
		log.Fatal(err)
	}
}

// interactiveLauncher adapts the daemon's interactive-session methods to the
// tui.InteractiveLauncher interface, keeping the tui package decoupled from the
// daemon the same way adaptSessionFeed does for the sessions feed.
type interactiveLauncher struct{ d *daemon.Daemon }

func (l interactiveLauncher) OpenSession(ctx context.Context, projectPath string) (string, error) {
	return l.d.OpenInteractiveSession(ctx, projectPath)
}

func (l interactiveLauncher) OpenOrchDir(ctx context.Context, projectPath string) (string, error) {
	return l.d.OpenOrchDirTab(ctx, projectPath)
}

func (l interactiveLauncher) OpenWorkspaceDir(ctx context.Context, projectPath string) (string, error) {
	return l.d.OpenWorkspaceDirTab(ctx, projectPath)
}

func (l interactiveLauncher) OpenTaskWorktreeDir(ctx context.Context, projectPath, taskPath string) (string, error) {
	return l.d.OpenTaskWorktreeDirTab(ctx, projectPath, taskPath)
}

func (l interactiveLauncher) OpenTaskSession(ctx context.Context, projectPath, taskPath string) (string, error) {
	return l.d.OpenTaskSession(ctx, projectPath, taskPath)
}

// sshAgentUnusable reports whether current is an SSH_AUTH_SOCK that cannot
// hold the signing key: unset, or the macOS system (launchd) agent a GUI/login
// shell inherits. Any other value is assumed to be the private agent.
func sshAgentUnusable(current string) bool {
	return current == "" || strings.Contains(current, "com.apple.launchd")
}

// pinHostGitToPrivateAgent makes every host-side git/ssh the daemon spawns
// (wsp, git, and their children inherit orch's environment) use the private
// ssh-agent start.sh loaded once from 1Password, so nothing after startup ever
// reaches 1Password and prompts for a fingerprint:
//
//   - GIT_SSH_COMMAND gets `-o IdentityAgent=SSH_AUTH_SOCK`. A typical
//     1Password setup puts `IdentityAgent ~/Library/Group Containers/...1password.../agent.sock`
//     in ~/.ssh/config, which overrides SSH_AUTH_SOCK, so every ssh fetch/clone
//     would otherwise go through 1Password's agent. Command-line -o options win
//     over ssh_config. An existing GIT_SSH_COMMAND is extended, not replaced.
//   - gpg.ssh.program is forced to ssh-keygen (appended via GIT_CONFIG_COUNT/
//     KEY_n/VALUE_n) so a host-side signed commit or tag never runs 1Password's
//     op-ssh-sign; ssh-keygen signs through SSH_AUTH_SOCK, i.e. the private agent.
//     Only gpg.ssh.program is set — not user.signingkey/gpg.format/commit.gpgsign,
//     which gitsign.Resolve reads to decide whether sandboxes sign.
//
// Callers only invoke it when SSH_AUTH_SOCK is the private agent. It returns
// the KEY=VALUE pairs it set, for the audit log.
func pinHostGitToPrivateAgent(getenv func(string) string, setenv func(string, string) error) []string {
	var set []string
	put := func(k, v string) {
		if setenv(k, v) == nil {
			set = append(set, k+"="+v)
		}
	}
	const agentOpt = "-o IdentityAgent=SSH_AUTH_SOCK"
	switch cur := strings.TrimSpace(getenv("GIT_SSH_COMMAND")); {
	case cur == "":
		put("GIT_SSH_COMMAND", "ssh "+agentOpt)
	case !strings.Contains(cur, "IdentityAgent"):
		put("GIT_SSH_COMMAND", cur+" "+agentOpt)
	}
	n := 0
	if c := strings.TrimSpace(getenv("GIT_CONFIG_COUNT")); c != "" {
		v, err := strconv.Atoi(c)
		if err != nil || v < 0 {
			return set // malformed: leave git's own error alone
		}
		n = v
	}
	for i := 0; i < n; i++ {
		if strings.EqualFold(getenv("GIT_CONFIG_KEY_"+strconv.Itoa(i)), "gpg.ssh.program") {
			return set
		}
	}
	put("GIT_CONFIG_KEY_"+strconv.Itoa(n), "gpg.ssh.program")
	put("GIT_CONFIG_VALUE_"+strconv.Itoa(n), "ssh-keygen")
	put("GIT_CONFIG_COUNT", strconv.Itoa(n+1))
	return set
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

// splitList splits a comma-separated flag value, dropping blank entries.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
