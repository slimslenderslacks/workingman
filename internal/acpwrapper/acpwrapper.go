// Package acpwrapper implements the `acp-wrapper` binary's core: it creates an
// sbx claude sandbox with the acp-kit ACP bridge layered on, execs acp-kit's
// `claude-acp-client` entrypoint inside that sandbox (which speaks the Agent
// Client Protocol over the child process's stdio), and exposes a per-session
// unix-domain socket (`agent.sock`) that the workingman TUI connects to. The
// socket bridges a connecting TUI to the sandboxed ACP client's stdio so the
// TUI can send prompts and watch streamed ACP responses.
//
// This package owns the entrypoint scaffolding, config parsing, sandbox launch
// and acp-kit exec, plus the socket-bridge wiring. It records each session as
// session.json next to the socket (via the session package) so a restarting TUI
// can rediscover and reconnect to it. The richer bridging semantics (framing,
// multiplexing several reconnecting TUIs, resuming a stream) are layered on by
// dependent tasks; this package defines where those artifacts live and the
// seams they hook into.
package acpwrapper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/slimslenderslacks/work/internal/session"
)

// SocketName is the unix-domain socket the wrapper creates inside a session
// directory. The TUI connects to <SessionsRoot>/<SessionID>/agent.sock. It is
// aliased from the session package so the layout has one source of truth.
const SocketName = session.SocketName

// LogName is the append-only file inside a session directory where the bridge
// records the raw ACP stream the agent emits (one newline-delimited frame per
// line). The live socket only carries the stream from "now", so this log is what
// lets a TUI that reconnects after a restart replay the session's prior
// assistant output and rebuild its scrollback. Its absolute path is recorded as
// session.Session.LogPath.
const LogName = "stream.log"

// sandboxNamePrefix is prepended to the session id when deriving a sandbox
// name. sbx rejects names containing underscores, so normalize() also rewrites
// any underscores to hyphens.
const sandboxNamePrefix = "acp-"

// Config is everything the wrapper needs to launch a sandboxed ACP client and
// expose it over a session socket. Zero-value fields are filled with defaults
// by normalize(); SessionID and KitPath have no sensible default and are
// required.
type Config struct {
	// SessionID uniquely identifies this ACP session. It names the session
	// directory (<SessionsRoot>/<SessionID>) and, by default, the sandbox.
	// Required; must be a single path segment.
	SessionID string

	// SessionsRoot is the parent of every session directory. Defaults to
	// ~/.workingman/sessions.
	SessionsRoot string

	// SandboxName is the sbx sandbox to create/exec into. Defaults to
	// "acp-<SessionID>". Underscores are rewritten to hyphens (sbx rejects
	// them).
	SandboxName string

	// KitPath is the acp-kit reference passed to `sbx ... --kit`: a path to a
	// local kit directory or a published OCI/git reference. It layers the ACP
	// bridge + claude-acp-client entrypoint onto the base claude sandbox.
	// Required — without it the sandbox has no claude-acp-client to exec.
	KitPath string

	// Workspaces are host paths bind-mounted into the sandbox. The first is the
	// primary one: the ACP client's working directory (`sbx exec -w`). At least
	// one is required.
	Workspaces []string

	// SbxPath is the sbx executable. Defaults to "sbx" (resolved on PATH).
	SbxPath string

	// ExitWhenEmpty makes the wrapper shut its sandboxed ACP client down
	// (close its stdin) once the last connected TUI disconnects, after at
	// least one client has connected. Set by orch's daemon for the
	// non-interactive autonomous flow: planning/task/commit each run one turn
	// driven by the TUI's watcher, and the wrapper must exit when that turn
	// ends so the daemon's session-end callback dispatches the next stage.
	// Leave false for interactive/long-lived sessions that should survive
	// transient TUI disconnects.
	ExitWhenEmpty bool
}

// SessionDir is the per-session directory holding the socket and session.json.
func (c Config) SessionDir() string {
	return filepath.Join(c.SessionsRoot, c.SessionID)
}

// SocketPath is the unix socket the TUI connects to for this session.
func (c Config) SocketPath() string {
	return filepath.Join(c.SessionDir(), SocketName)
}

// LogPath is the raw ACP stream log for this session — see LogName.
func (c Config) LogPath() string {
	return filepath.Join(c.SessionDir(), LogName)
}

// sessionRecord projects the wrapper's config into the session.json the TUI
// reads to reconnect. createdAt is preserved across status updates so the
// "running" record keeps the "starting" record's birth time; updatedAt stamps
// each write. Must be called after normalize() so the derived sandbox name and
// absolute paths are populated.
func (c Config) sessionRecord(status session.Status, createdAt, updatedAt time.Time) session.Session {
	return session.Session{
		ID:          c.SessionID,
		SandboxName: c.SandboxName,
		Status:      status,
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
		SocketPath:  c.SocketPath(),
		Workspaces:  c.Workspaces,
		Kit:         c.KitPath,
		LogPath:     c.LogPath(),
	}
}

// primaryWorkspace is the first mount — the ACP client's cwd inside the
// sandbox. Empty when no workspaces are configured.
func (c Config) primaryWorkspace() string {
	if len(c.Workspaces) > 0 {
		return c.Workspaces[0]
	}
	return ""
}

// execArgs builds the argv (after the sbx binary) that runs acp-kit's
// claude-acp-client entrypoint inside the sandbox over `sbx exec`, producing an
// ACP client on the child's stdio.
//
// Unlike the daemon's interactive tmux launches, this is NOT `-it`: ACP is
// newline-delimited JSON-RPC on raw stdio, so a pty would corrupt the protocol
// stream. `-w` pins the working directory to the primary workspace so the
// agent's cwd matches the mounted host path.
func (c Config) execArgs() []string {
	args := []string{"exec"}
	if cwd := c.primaryWorkspace(); cwd != "" {
		args = append(args, "-w", cwd)
	}
	args = append(args, c.SandboxName, "--", "claude-acp-client")
	return args
}

// normalize fills defaults and validates required fields. It mutates the
// receiver so the resolved values (abs paths, derived sandbox name) are visible
// to callers via the path helpers.
func (c *Config) normalize() error {
	c.SessionID = strings.TrimSpace(c.SessionID)
	if c.SessionID == "" {
		return errors.New("acpwrapper: session id is required")
	}
	if c.SessionID == "." || c.SessionID == ".." || strings.ContainsAny(c.SessionID, `/\`) {
		return fmt.Errorf("acpwrapper: invalid session id %q: must be a single path segment", c.SessionID)
	}

	if strings.TrimSpace(c.KitPath) == "" {
		return errors.New("acpwrapper: kit path is required (the acp-kit reference to install into the sandbox)")
	}

	if len(c.Workspaces) == 0 {
		return errors.New("acpwrapper: at least one workspace is required")
	}
	for i, w := range c.Workspaces {
		abs, err := filepath.Abs(w)
		if err != nil {
			return fmt.Errorf("acpwrapper: workspace %q: %w", w, err)
		}
		c.Workspaces[i] = abs
	}

	if c.SessionsRoot == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("acpwrapper: resolve home dir for default sessions root: %w", err)
		}
		c.SessionsRoot = filepath.Join(home, ".workingman", "sessions")
	}
	abs, err := filepath.Abs(c.SessionsRoot)
	if err != nil {
		return fmt.Errorf("acpwrapper: sessions root %q: %w", c.SessionsRoot, err)
	}
	c.SessionsRoot = abs

	if c.SandboxName == "" {
		c.SandboxName = sandboxNamePrefix + c.SessionID
	}
	// sbx rejects sandbox names containing underscores.
	c.SandboxName = strings.ReplaceAll(c.SandboxName, "_", "-")

	if c.SbxPath == "" {
		c.SbxPath = "sbx"
	}
	return nil
}

// commandFunc runs an external command and returns its combined output. It is
// the single seam through which sandbox management shells out to sbx; tests
// replace it to assert argv and simulate sbx responses without a sandbox host.
type commandFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

func execCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// ensureSandbox makes the named sandbox exist with exactly c.Workspaces
// mounted and the acp-kit kit layered on. It mirrors the daemon's idempotent
// SandboxCreator so the wrapper is safe to relaunch:
//
//  1. `sbx ls --json` to find an existing sandbox by name.
//  2. Same workspace set → no-op.
//  3. Different set → `sbx rm --force` then recreate (self-heals drift).
//  4. Otherwise `sbx create claude --name <name> --kit <kit> <ws...>`.
func ensureSandbox(ctx context.Context, run commandFunc, c Config) error {
	existing, err := readSandboxWorkspaces(ctx, run, c.SbxPath, c.SandboxName)
	if err != nil {
		return fmt.Errorf("acpwrapper: sbx ls: %w", err)
	}
	if existing != nil {
		if sameWorkspaceSet(existing, c.Workspaces) {
			return nil
		}
		if out, err := run(ctx, c.SbxPath, "rm", "--force", c.SandboxName); err != nil {
			return fmt.Errorf("acpwrapper: sbx rm %s: %w: %s", c.SandboxName, err, strings.TrimSpace(string(out)))
		}
	}
	args := []string{"create", "claude", "--name", c.SandboxName, "--kit", c.KitPath}
	args = append(args, c.Workspaces...)
	if out, err := run(ctx, c.SbxPath, args...); err != nil {
		return fmt.Errorf("acpwrapper: sbx create %s: %w: %s", c.SandboxName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// readSandboxWorkspaces returns the workspace list for the named sandbox, or
// nil if no sandbox by that name exists. `sbx ls --json` is the stable read
// interface sbx exposes.
func readSandboxWorkspaces(ctx context.Context, run commandFunc, sbxPath, name string) ([]string, error) {
	out, err := run(ctx, sbxPath, "ls", "--json")
	if err != nil {
		return nil, err
	}
	var data struct {
		Sandboxes []struct {
			Name       string   `json:"name"`
			Workspaces []string `json:"workspaces"`
		} `json:"sandboxes"`
	}
	if err := json.Unmarshal(out, &data); err != nil {
		return nil, fmt.Errorf("decode sbx ls output: %w", err)
	}
	for _, s := range data.Sandboxes {
		if s.Name == name {
			return s.Workspaces, nil
		}
	}
	return nil, nil
}

// sameWorkspaceSet reports whether a and b contain the same paths, ignoring
// order (sbx exposes no ordering semantics for mounts).
func sameWorkspaceSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]bool, len(a))
	for _, x := range a {
		seen[x] = true
	}
	for _, x := range b {
		if !seen[x] {
			return false
		}
	}
	return true
}

// Run is the wrapper's blocking main loop. It normalizes the config, ensures
// the sandbox, starts the sandboxed ACP client over `sbx exec`, listens on the
// session's agent.sock, and bridges each TUI connection to the ACP client's
// stdio. It returns when ctx is cancelled or the ACP client exits, removing the
// socket on the way out.
func Run(ctx context.Context, c Config) error {
	if err := c.normalize(); err != nil {
		return err
	}

	if err := os.MkdirAll(c.SessionDir(), 0o755); err != nil {
		return fmt.Errorf("acpwrapper: create session dir %s: %w", c.SessionDir(), err)
	}

	// Record the session as soon as the directory exists so a TUI that lists
	// the sessions root mid-startup sees it (as "starting") rather than a bare,
	// metadata-less directory. The store roots at the same SessionsRoot the path
	// helpers use, so it writes session.json next to agent.sock.
	store := session.Store{Root: c.SessionsRoot}
	createdAt := time.Now()
	if err := store.Write(c.sessionRecord(session.StatusStarting, createdAt, createdAt)); err != nil {
		return err
	}

	if err := ensureSandbox(ctx, execCommand, c); err != nil {
		_ = store.Remove(c.SessionID) // never started — don't leave a stale record
		return err
	}

	// Cancelling this child context tears down the ACP client process when the
	// listener stops (and vice versa), so neither outlives the other.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Start the sandboxed ACP client. Its stdin/stdout carry newline-delimited
	// ACP JSON-RPC, which the socket bridges to/from the TUI; its stderr is the
	// bridge's own diagnostics (the entrypoint keeps logging off stdout).
	proc := exec.CommandContext(ctx, c.SbxPath, c.execArgs()...)
	proc.Stderr = os.Stderr
	procStdin, err := proc.StdinPipe()
	if err != nil {
		return fmt.Errorf("acpwrapper: acp client stdin pipe: %w", err)
	}
	procStdout, err := proc.StdoutPipe()
	if err != nil {
		return fmt.Errorf("acpwrapper: acp client stdout pipe: %w", err)
	}
	if err := proc.Start(); err != nil {
		return fmt.Errorf("acpwrapper: start acp client (%s %s): %w", c.SbxPath, strings.Join(c.execArgs(), " "), err)
	}

	// Bind agent.sock. Remove a stale socket from a prior run first — bind
	// fails if the path already exists.
	if err := os.Remove(c.SocketPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("acpwrapper: remove stale socket %s: %w", c.SocketPath(), err)
	}
	ln, err := net.Listen("unix", c.SocketPath())
	if err != nil {
		return fmt.Errorf("acpwrapper: listen on %s: %w", c.SocketPath(), err)
	}

	// Open the stream log the bridge tees the agent's ACP output into, so a TUI
	// that reconnects after a restart can replay this session's prior output.
	// O_APPEND keeps a relaunch's output continuous rather than truncating. A
	// failure here is non-fatal: without the log, reconnect just loses replayable
	// scrollback (the live stream still works), so we log and press on. The nil
	// interface (not a nil *os.File) is deliberate — passing a typed nil would
	// make the hub's `log != nil` guard true and panic on Write.
	var logW io.Writer
	if logFile, err := os.OpenFile(c.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "acp-wrapper:", err)
	} else {
		logW = logFile
		defer logFile.Close()
	}

	// The ACP client is started and the socket is accepting: mark the session
	// running so a reconnecting TUI knows the transport is live. A failed update
	// here is non-fatal — the session is genuinely usable; we just log and press
	// on rather than tear down a working session over a metadata hiccup.
	if err := store.Write(c.sessionRecord(session.StatusRunning, createdAt, time.Now())); err != nil {
		fmt.Fprintln(os.Stderr, "acp-wrapper:", err)
	}

	// When the ACP client exits, the session's transport is gone: cancel the
	// context and close the listener so the accept loop unwinds.
	waitErr := make(chan error, 1)
	go func() {
		err := proc.Wait()
		cancel()
		ln.Close()
		waitErr <- err
	}()
	// When ctx is cancelled (signal, or proc-exit above), stop accepting.
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	serve(ctx, ln, procStdin, procStdout, logW, c.ExitWhenEmpty)

	// The ACP client has exited and the session's transport is gone: remove the
	// whole session directory (session.json and the socket together) so a
	// reconnecting TUI doesn't discover a dead session and dial a dead socket.
	if rmErr := store.Remove(c.SessionID); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
		return fmt.Errorf("acpwrapper: remove session %s: %w", c.SessionID, rmErr)
	}

	procErr := <-waitErr
	// A non-nil proc error is only meaningful if we weren't deliberately
	// shutting down (signal/ctx cancel kills the child and surfaces as an
	// error we don't want to report as a failure).
	if procErr != nil && ctx.Err() == nil {
		return fmt.Errorf("acpwrapper: acp client exited: %w", procErr)
	}
	return nil
}

// serve bridges TUI connections on ln to the one sandboxed ACP client's stdio
// until the listener is closed. A single hub fans the ACP client's stdout out
// to every connected client and serializes each client's framed input into the
// ACP client's stdin, so several TUIs (including a watcher that reconnects
// later) share the same session without corrupting the newline-delimited
// JSON-RPC stream. See bridge.go for the framing/fan-out details.
//
// The stdout reader runs in its own goroutine; the accept loop registers each
// connection with the hub. When ln is closed (ctx cancelled or ACP client
// exited) the hub is torn down and serve returns. logW, when non-nil, receives a
// copy of every agent frame for reconnect replay (see hub.log).
func serve(ctx context.Context, ln net.Listener, procStdin io.WriteCloser, procStdout io.Reader, logW io.Writer, exitWhenEmpty bool) {
	h := newHub(procStdin, logW)
	if exitWhenEmpty {
		h.enableExitWhenEmpty(procStdin)
	}
	// One reader drains the ACP client's stdout and broadcasts whole frames to
	// every connected client. It also tears the hub down on stdout EOF.
	go h.run(procStdout)
	for {
		conn, err := ln.Accept()
		if err != nil {
			h.shutdown() // listener closed: ctx cancelled or ACP client exited
			return
		}
		h.add(conn)
	}
}
