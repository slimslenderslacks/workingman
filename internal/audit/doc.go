// Package audit writes and reads the daemon's append-only audit log of
// timestamped key=value events, and redacts secrets from text before it is
// published.
//
// Role: daemon
//
// A [Logger] writes one line per event to an io.Writer the caller owns (the
// --audit-log file in cmd/orch):
//
//	2026-05-12T18:04:00Z event key=val key="val with spaces"
//
// It also keeps the last 500 lines in memory ([Logger.Recent]), preloaded
// after a restart with [Logger.Seed] from [TailFile]. The daemon's state
// snapshot reads that buffer instead of the file, and [Logger.OnLog] tells
// the snapshot when to refresh. Logger is safe for concurrent use.
// [ParseLine] turns a line back into an [Event]. [Redact] masks credential
// patterns (tokens, API keys, bearer headers, URL userinfo, PEM keys) without
// touching git SHAs. The snapshot and the channel router use it before text
// leaves the process.
//
// # Key types
//
//   - [Logger] — the writer, created with [New] in cmd/orch and passed to the
//     daemon, runner and router.
//   - [Event] — a parsed line (time, event name, fields), produced by
//     [ParseLine] for the state snapshot's audit_events.
//   - [Redact] and [TailFile] — secret masking and cheap tail reads of a
//     large log.
//
// # Collaborators
//
// No internal imports. Used by cmd/orch (opens the log and seeds the Logger),
// internal/daemon (logs every transition and publishes recent events in the
// state snapshot), internal/runner (launch events) and internal/router
// (inbound-channel turns, with errors passed through Redact). See
// docs/state-snapshot.md.
package audit
