// Package agent defines the process model for orch's claude-code sessions:
// agent kinds, the launch spec, and the Launcher and Session interfaces.
//
// Role: daemon
//
// An agent has a [Kind] (project, planning, task, commit, wolf, archive,
// review, workingman; see agents.md for what each one does). The daemon picks a
// Kind, internal/runner turns it into a [Spec] (session name, tab title,
// working directory, argv), and a [Launcher] starts it and returns a [Session],
// the live handle the daemon holds until the agent exits. Launchers don't look
// at Kind; they run whatever Command the Spec carries, so tests can launch
// `sleep` through the same path.
//
// There are two production launchers. [ProcessLauncher] (this package) runs the
// command as a plain host child process in its own process group. It is used
// for ACP agents, whose acp-wrapper host process needs no terminal.
// terminal.TabLauncher runs the command in a new terminal tab, for sessions a
// human drives at a real TTY.
//
// The package also has PATH helpers ([AugmentSearchPath],
// [DefaultPATHCandidates]). cmd/orch calls them at startup so child processes
// can find claude, sbx and wsp.
//
// # Key types
//
//   - [Kind] — which orchestrator role a session plays. [Kind.String] and
//     [ParseKind] convert to and from the name stored in session records;
//     [Kind.Interactive] is true only for the wolf. New kinds are appended so
//     existing iota values don't shift.
//   - [Spec] — what a Launcher needs to start one session.
//   - [Launcher] — starts a Spec. Implemented by [ProcessLauncher] and
//     terminal.TabLauncher, and stubbed in tests.
//   - [Session] — a running agent: Name, Wait (blocks until exit), and Close
//     (idempotent teardown).
//   - [ProcessLauncher] — the host child-process Launcher. Close sends SIGTERM,
//     then SIGKILL after a grace period.
//
// # Collaborators
//
// No internal imports; this is the bottom of the launch path. Used by
// internal/runner (builds Specs and calls the configured Launchers),
// internal/terminal (TabLauncher implements Launcher and returns a Session),
// internal/prompts (templates are chosen by Kind), internal/daemon (tracks
// Sessions by Kind), and cmd/orch (wires a ProcessLauncher as the runner's
// AcpLauncher and augments PATH). See agents.md.
package agent
