// Package workspace provisions and tears down the multi-repo directory each
// agent runs in, with the wsp CLI behind a small Manager interface.
//
// Role: shared (daemon/runner and TUI)
//
// A workspace is named after the project's branch and holds a clone of each of
// the project's repos, all on that branch: `wsp new` creates it under
// ~/dev/workspaces/<branch>/ by default. [WspManager] is the production
// driver. It runs `wsp ... --json`, registers repos in wsp's registry if
// needed, creates a project's new_repos on GitHub with `gh repo create`,
// resets each repo to its base_branch on first creation, and handles wsp's
// odd exit codes so that Create and Remove are idempotent. [StubManager] only
// creates and removes directories and never runs wsp or git. It is used by
// tests and `orch --workspace-manager=stub`.
//
// # Key types
//
//   - [Manager] — Create, Path, AddRepos and Remove for a workspace keyed by
//     branch. Implemented by [WspManager] and [StubManager].
//   - [Repo] — one repo to clone: wsp identity or shortname, base branch, and
//     whether to create it on the remote first. [Repo.DirName] is its
//     directory inside the workspace.
//
// # Collaborators
//
// No internal imports. cmd/orch picks the implementation and hands it to the
// daemon. internal/runner provisions a workspace for each agent it launches,
// and internal/daemon calls AddRepos when a project's repo list grows (see
// project.PlannedSnapshot). internal/tui uses [NewWsp] to remove workspaces on
// `:archive`. task.Commit.Repo matches [Repo.DirName].
package workspace
