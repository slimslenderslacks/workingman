// Package task is the data model and on-disk format of one tasks/*.yaml file,
// including the task-name rules that keep a task's sandbox name legal.
//
// Role: shared (daemon, runner, TUI and acp-wrapper)
//
// Each task is a single YAML file under a work stream's tasks/ directory,
// written by the planning, task, commit and review agents and by the daemon.
// [Load] backfills Model to [ModelDefault] and records the file path in
// Task.Path; callers keep that path instead of rebuilding it from the name,
// because filenames may carry sort prefixes. Model and Harness are what the
// planner *requested*; ResolvedModel and ResolvedHarness are what the task
// actually ran under, recorded by the acp-wrapper via [Update] (see
// [Task.DisplayModel] and [Task.DisplayHarness]).
//
// A task name is lowercase kebab-case and becomes half of the task's sandbox
// name "<work-stream>-<task-name>", which must fit [sbx.MaxNameLen]. So there
// are two limits: [MaxNameLen] for a name with no context, and
// [MaxNameLenFor] for a name inside a known work stream. [ValidNameFor] and
// [SlugifyFor] use the per-project limit. [Save] refuses (and does not
// truncate) an over-long name; repairing names is taskgraph.Load's job.
// [Save] and [Update] hold the in-process write lock for the file's path
// (internal/pathlock), so in-process writers of one task file are serialized.
// [UpdateIf] is Update with a check on the file's bytes under that lock: the
// GUI API's If-Match on a task.
//
// # Key types
//
//   - [Task] — one task file: name, description, depends_on, status, retry
//     bookkeeping, the commit agent's results (Summary, Commits,
//     CreatedFiles), sandbox settings (StaticMCPs, Policies, SaveSandbox) and
//     the review-agent Source.
//   - [Status] — the task lifecycle: ready → running → success|failed|blocked
//     → committed. The loader rejects any other value.
//   - [Source] — the PR comment, failed check or push batch that a
//     review-agent task was created for. [Task.IsPushTask] identifies push
//     tasks ([SourceKindPush]).
//   - [Commit] — one repo/SHA pair the commit agent produced.
//
// # Collaborators
//
// Uses internal/pathlock for the per-path write lock, internal/policy for the
// per-task sandbox [policy.Rule] list and internal/sbx for the sandbox-name
// budget. Used by internal/taskgraph (builds the DAG from these files),
// internal/daemon and internal/runner (dispatch, status transitions, sandbox
// setup), internal/tui (Tasks pane, adding seeds) and internal/acpwrapper
// (whether to keep a sandbox based on task status).
package task
