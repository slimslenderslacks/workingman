// Package pathlock serializes in-process writers of the same file with one
// mutex per path.
//
// Role: shared (daemon and TUI, through internal/project and internal/task)
//
// The project and task files are replaced with a temp file plus a rename, so a
// reader never sees a torn file. That does not stop two writers that each
// load, mutate and save the same file from losing one of the changes. [Lock]
// closes that window for writers inside one process — the daemon, the TUI it
// hosts and, later, the API — when every one of them takes the lock for the
// path around its read-modify-write. internal/project and internal/task take
// it inside their Save and Update helpers, so callers get it for free.
//
// The lock is advisory and process-local. Agents in sandboxes, a standalone
// TUI process and humans in editors write the files without it; it does
// nothing about them. Paths are made absolute and cleaned before they are
// used as keys, so "a/../p.yaml" and "p.yaml" share a lock; symlinks are not
// resolved. The mutex is not reentrant: code holding the lock for a path must
// not call [Lock] for the same path again.
//
// # Key types
//
//   - [Lock] — takes the lock for a path and returns its unlock function.
//
// # Collaborators
//
// No internal imports. Used by internal/project (project.Save, SaveAs,
// Update) and internal/task (task.Save, Update). See
// docs/proposals/workingman-gui.md §A.4.1.
package pathlock
