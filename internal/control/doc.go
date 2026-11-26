// Package control holds the write actions that steer a work stream through its
// files: stop/start, summon the wolf, review, cleanup, hide/show and archive
// on project.yaml, and the create-only new project and intake, each with its
// precondition and its user-facing refusal text.
//
// Role: shared (TUI today; the GUI API later)
//
// The daemon has no request API. A front end steers a work stream by writing
// its project.yaml, and the daemon's fsnotify handler reacts to the write.
// Each action here is one small read-modify-write of that file through
// project.Update, so the whole load–mutate–save holds the in-process write lock
// for the path (internal/pathlock). Every action except [Hide]/[Show] writes as
// project.WriterAgent, because the daemon ignores its own `updated_by: daemon`
// writes and must see the request. Hide and Show write as project.WriterDaemon
// on purpose, so a display toggle is never routed. [Archive] is not a file
// write: it tears down the work stream's worktrees and wsp workspace and moves
// its directory to the sibling <root>.backup.
//
// The create-only actions add a file the daemon reacts to, as a human dropping
// it would: [NewProject] writes <root>/<name>/project.md, [QueueIntake] writes
// <project>/intake/<name>.md, and [WithdrawIntake] renames a pending request
// to <name>.withdrawn, which nothing reads. A new file is written to a temp
// name no daemon handler matches and hard-linked into place, so the daemon sees
// it once and complete and an existing name is refused, not replaced. Their
// refusals wrap [ErrInvalid], [ErrExists], [ErrNotFound], [ErrProcessed] or
// [ErrState], so a caller can pick a status.
//
// A refused action returns an error whose text is meant for the user (the TUI
// shows it on its status line) and leaves the file untouched. Keeping the
// preconditions and their wording here is what lets every front end refuse the
// same actions with the same words.
//
// # Key types
//
//   - [Stop], [Start], [SummonWolf], [Review], [Cleanup], [Hide], [Show] — the
//     project-file writes behind the TUI's :stop, :start, :wolf, :review,
//     :cleanup, :hide and :show.
//   - [LoadArchivable], [Archive] — the :archive precondition and teardown.
//   - [NewProject], [QueueIntake], [WithdrawIntake] — the create-only writes
//     behind the API's POST /projects and /projects/{id}/intake (§A.4.3);
//     [ValidProjectName], [ValidIntakeName], [ProjectNameWarning],
//     [DefaultIntakeName] — their name rules.
//   - [WorkspaceRemover] — the part of workspace.Manager that [Archive] needs.
//   - [DisplayName], [BackupRoot] — the names used in confirmations.
//
// # Collaborators
//
// Uses internal/project for the file, internal/pathlock for the in-process
// write lock, internal/sbx for the sandbox-name budget and internal/worktree
// for the archive's task-worktree teardown. Used by internal/tui and
// internal/daemon (the API's writes). See docs/proposals/workingman-gui.md
// §A.4.1, §A.4.3 and §A.7.
package control
