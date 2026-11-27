// Package postoffice is the agent messaging store: the message model and the
// file-backed Store under ~/.workingman/post-office/ that post-office, the
// orch daemon and the TUI share. The directory tree is their only
// integration point; there is no RPC. See docs/messaging-design.md §2-§3,
// and docs/messaging.md for the user guide.
//
// Role: shared (post-office, daemon and TUI)
//
// A message is a durable file, not a tool call:
//
//	inbox/<id>.json        open messages; written by post-office (write to
//	                       inbox/.tmp, fsync, rename), apart from the orch
//	                       daemon's Step-Up executor creating its
//	                       notifications with Put
//	outbox/<id>.json       the terminal claim; created by whoever closes the
//	                       message first (TUI, GUI, Dispatcher, post-office)
//	done/<day>/<id>.json   the inbox record plus its resolution; written only
//	                       by post-office; <day> is the UTC date in the id
//	deliveries/<id>.json   the Dispatcher's per-channel send records
//	                       ([DeliveryRecord]); written only by the Dispatcher
//	post-office.lock       flock held by the running post-office
//
// Every terminal transition (answered, cancelled, expired, dismissed) goes
// through [Store.Claim], which writes the full claim to outbox/.tmp and
// link(2)s it to outbox/<id>.json. The link fails if the name exists, so
// exactly one claimer wins, from any number of goroutines or processes, and
// readers never see a partial file. Losers get [ErrAlreadyClosed] together
// with the winning [Resolution].
//
// post-office then finalises each claim with [Store.Finalize] (done/ written,
// then inbox/ removed, then outbox/ removed). Every step is idempotent and
// [Store.Recover] re-runs it for every leftover claim, so a crash between
// steps converges; a claim with no inbox record is finalised as an orphan,
// and a claim arriving after the message was finalised is discarded.
// [Store.Expire] claims overdue messages and [Store.Prune] drops done/
// records after [DefaultRetention]. "Remove the message from the TUI inbox"
// is simply "the file left inbox/".
//
// Directories are 0700 and files 0600. Root is
// $WORKINGMAN_POST_OFFICE_DIR when set ([DefaultDir]).
//
// # Key types
//
//   - [Message] — the inbox record (§2.9): kind, sender, recipient, subject,
//     body, options, expiry. [NewMessage] builds one from a [Draft] with a
//     fresh "msg_"+ULID id ([NewID]); [ShortID] and [MatchShortID] give the
//     6-character form humans type.
//   - [Sender] — who sent it (§2.3), filled from the caller's gateway, never
//     from tool arguments; [Sender.Display] renders it for humans.
//   - [Resolution] and [Responder] — the claim (§2.7); [MatchOption] maps an
//     answer to a request's canonical option.
//   - [DoneRecord] — a finalised message.
//   - [Store] — Open, Put, MarkReceived, Get, ListOpen, Lookup, ListDone,
//     Claim, Finalize, Recover, Expire, Prune, TryLock, and [Store.Watch], a
//     coalescing change notifier (fsnotify plus a poll fallback).
//   - [Waiters] — wakes in-process goroutines blocked on one message id.
//   - [Dispatcher] — runs in the orch daemon (§5.2): sends each operator
//     message to every channel route for [Topic] ("post-office", else the
//     wildcard routes), registers sent requests in the channels conversation
//     index, retries failed sends with backoff, and, once a request is in
//     done/, sends a [ClosingNote] to every delivery except the one that
//     answered (through channels.Resolver when the channel has it).
//     [RenderMessage] is the outbound text.
//   - [DeliveryRecord] / [Delivery] — deliveries/<id>.json; [Store.Deliveries]
//     reads it.
//
// # Collaborators
//
// The store and model have no internal imports. The Dispatcher uses
// [github.com/slimslenderslacks/work/internal/channels] (the Registry, as
// [Outbound], and the ConversationIndex) and
// [github.com/slimslenderslacks/work/internal/audit] for redaction.
// cmd/post-office writes the inbox, finalises, expires and prunes; the
// daemon's Dispatcher and the TUI read the inbox and call Claim; the daemon's
// API calls Claim for the GUI's answers (Via [ViaGUI]); the router
// (internal/router) calls Claim for answers from a channel. cmd/orch starts
// the Dispatcher with the channels. Who may call what is in each method's
// comment.
package postoffice
