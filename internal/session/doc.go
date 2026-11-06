// Package session defines the on-disk record (session.json and agent.sock
// under ~/.workingman/sessions/<id>/) that lets ACP agent sessions outlive the
// TUI and the daemon that started them.
//
// Role: shared (acp-wrapper writes; daemon, runner, TUI and acpchat read)
//
// Each ACP session owns a directory under a sessions root, by default
// ~/.workingman/sessions:
//
//	<root>/<id>/session.json   session metadata (this package)
//	<root>/<id>/agent.sock     the ACP bridge socket (acp-wrapper)
//
// acp-wrapper is the writer. It records the session when it launches the
// sandbox, updates its status as the session runs, and removes the directory
// when the sandbox exits. The readers rely on session.json holding everything
// they need without asking the writer. A restarting TUI lists the root and
// reconnects to each running session's agent.sock. A restarting daemon uses
// ProjectPath, TaskPath and Kind to match running sessions back to its
// dispatch state. The runner reads Error to explain a session that failed to
// start. Writes are atomic (temp file + rename), so a reader never sees a
// half-written file.
//
// # Key types
//
//   - [Store] — a sessions root: Dir, Path, SocketPath, Write, Read, List and
//     Remove. Created with [NewStore], which falls back to [DefaultRoot].
//   - [Session] — the session.json schema: sandbox name, status, socket and
//     log paths, owning project/task/kind, and failure flags (SigningBroken,
//     PushBroken, Error).
//   - [Status] — starting, running, exited or failed.
//
// # Collaborators
//
// No internal imports. Written by internal/acpwrapper. Read by internal/tui
// (ACP watcher reconnects), internal/daemon (startup reconciliation and the
// state snapshot's sessions list), internal/runner (start failures),
// internal/acpchat (attaching to a running agent) and cmd/orch (default
// sessions root). See docs/state-snapshot.md.
package session
