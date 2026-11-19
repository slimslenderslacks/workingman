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
// [Scan] loads every pending request in the inbox at once, reporting
// malformed files as per-file [*FileError]s instead of failing. [Watch]
// follows the inbox with fsnotify and emits an [Event] when a request is
// added, updated or removed, or fails to parse. It debounces each file and
// re-reads a file that fails to parse a few times before reporting it, so a
// request written in several chunks is parsed once, when complete; atomic
// renames into the inbox are picked up as new files. Start [Watch] before
// calling [Scan] so no file is missed in between.
//
// # Key types
//
//   - [Request] / [Choice] — a pending decision as read from the inbox;
//     [ParseRequest] decodes and validates one.
//   - [Answer] — the user's pick, written to the outbox by [WriteAnswer].
//   - [Pending] — a parsed request plus its inbox file name; returned by
//     [Scan] and carried by [Event]s from [Watch].
//   - [Event] / [EventKind] — inbox changes reported by [Watch].
//   - [DefaultInbox] / [DefaultOutbox] / [EnsureDirs] — default locations
//     and directory creation.
//
// # Collaborators
//
// Has no internal dependencies. Used by the decision TUI command.
package decision
