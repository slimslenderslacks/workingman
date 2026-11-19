// Package worktree creates, reuses and removes the per-task git worktrees a
// project's task agents work in, each on its own task branch cut from the
// project's shared branch.
//
// Role: shared (daemon, and later the TUI's :archive)
//
// A task worktree is a directory, <shared>/.worktrees/<task>/, holding one
// `git worktree` per project repo ([Root], [Path]). Every repo's worktree is
// checked out on the task branch wm/<project-branch>/<task> ([BranchName]),
// which lives in its own namespace because a ref refs/heads/<b> and a
// directory refs/heads/<b>/... cannot both exist. The task worktree root has
// the same shape as a wsp workspace, so task agents see one directory per
// repo exactly as before.
//
// [Manager.Ensure] is the one idempotent step every dispatch of a task goes
// through (docs/parallel-task-worktrees.md W4): reuse a registered worktree
// as it is (dirty or not), re-attach an existing task branch whose directory
// is gone, cut a new worktree from the shared tip when neither exists, and
// move anything unrecognised aside to .worktrees/.stale/ rather than delete
// it. [Manager.Remove] is the cleanup after a task is integrated (W6), and
// [Manager.GitCommonDir] resolves the git dir a task sandbox has to mount so
// commits made in the worktree land in the shared repo (W5).
//
// Every git invocation goes through [Manager.Run] ([RunFunc]), so tests can
// substitute a fake; the zero Manager runs the real `git` binary.
//
// # Key types
//
//   - [Manager] — Ensure, Remove, GitCommonDir, IsAncestor, MergeBase,
//     BranchExists, Dirty.
//   - [Spec] — which task worktree: shared workspace, task name, task branch,
//     shared branch, repo dir names.
//   - [Result] / [RepoResult] — what Ensure did per repo (created, reused,
//     re-attached), the base it cut from, and whether a reused tree is dirty.
//   - [RepoError] — a git failure attributed to one repo.
//
// # Collaborators
//
// No internal imports. internal/daemon calls Ensure when it dispatches a task
// agent, records the result in the task file (task.Task.Worktree, TaskBranch,
// Base), and calls IsAncestor and Remove once the commit agent has
// integrated the task. Repo names are workspace.Repo.DirName values.
package worktree
