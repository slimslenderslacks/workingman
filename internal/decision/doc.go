// Package decision holds the data model and file I/O for the standalone
// host-side decision TUI.
//
// Role: tui (host-side decision TUI, not wired into the daemon)
//
// A decision request is a JSON file dropped into an inbox directory
// (default ~/.workingman/decisions/inbox). It carries an id, a title, a
// markdown summary and the list of choices the user may pick from; the
// choices come from the request, never from the UI. When the user answers,
// [WriteAnswer] writes `<id>.json` into the outbox directory (default
// ~/.workingman/decisions/outbox) atomically: a temp file in the same
// directory is written, fsynced, closed and renamed into place, so a reader
// never sees a partial answer. [ArchiveRequest] then moves the request file
// into `<inbox>/answered/` so it does not show up again.
//
// Request ids become file names, so [Request.Validate] rejects ids that are
// not safe as a single path element (separators, "..", leading dots, control
// characters). [IsCandidateFile] tells a directory scanner which inbox
// entries are worth parsing: `*.json` files that are not hidden and not
// temp or partial writes.
//
// # Key types
//
//   - [Request] / [Choice] — a pending decision as read from the inbox;
//     [ParseRequest] decodes and validates one.
//   - [Answer] — the user's pick, written to the outbox by [WriteAnswer].
//   - [DefaultInbox] / [DefaultOutbox] / [EnsureDirs] — default locations
//     and directory creation.
//
// # Collaborators
//
// Has no internal dependencies. Used by the decision TUI command and its
// inbox watcher.
package decision
