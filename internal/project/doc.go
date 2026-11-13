// Package project is the data model and on-disk format of a work stream's
// project.yaml, plus the two sidecar records the daemon and the wolf agent
// keep next to it.
//
// Role: shared (daemon, TUI and acp-wrapper)
//
// A work stream is a control directory <root>/<work-stream>/ holding
// project.yaml, a tasks/ directory, and optionally blocked-session.yaml and
// last-planned.yaml. Agents, the TUI and the daemon all read and write
// project.yaml, so the file carries an updated_by marker ([Writer]): [Save]
// stamps it as the daemon, and the daemon ignores fsnotify events for its own
// writes. A write that must be seen by the daemon (a TUI request flag such as
// Cleanup or ReviewNow) goes through [SaveAs] with [WriterAgent]. Saves are
// atomic (temp file + rename), because an empty project.yaml means
// "unpopulated" and would dispatch a project agent.
//
// [Project] also holds the predicates the daemon routes on, so their rules
// live in one place: Unpopulated, CronStopReason/CronUnbounded (cron stop
// conditions), WatchingPR (is an idle project still in the PR-resolution
// loop) and AllBranchesMerged (may it move to merged). The status enum is
// fixed; the loader rejects unknown values and maps the legacy "done" and
// "reviewing" statuses to idle.
//
// # Key types
//
//   - [Project] — the parsed project.yaml: description, repos, branch, status,
//     cron settings, request flags (Cleanup, Replan, ReviewNow) and the PRs
//     being watched. [Load] reads one; [Save] and [SaveAs] write one.
//   - [Status] — the project lifecycle (ready, working, blocked, idle,
//     stopped, merged). The state machine is in state-machine.md.
//   - [Writer] — who last wrote the file (agent or daemon); the daemon's
//     guard against reacting to its own writes.
//   - [Repo] — one repo entry; accepts the {org, name} mapping or the
//     "org/name[@base]" shorthand.
//   - [PullRequest] — one PR the review agent is watching, stamped on each
//     poll, with the [PRFingerprint] it observed. [PullRequest.CanonicalFingerprint]
//     and [PRsFingerprint] give the comparable encoding shared with the
//     daemon's own MCP-probed fingerprint.
//   - [BlockedSession] — blocked-session.yaml, the wolf agent's running
//     diagnosis of why the project is blocked, kept across wolf runs.
//   - [PlannedSnapshot] — last-planned.yaml, the daemon-only copy of the
//     intent fields as of the last successful planning run, used to find repo
//     additions and other human edits made since.
//   - [MigrateLegacy] and [CanonicalPath] — rename the old .project.yaml to
//     project.yaml and map recorded paths to the new name.
//
// # Collaborators
//
// No internal imports. internal/daemon watches, routes on and rewrites
// project files and keeps the sidecars. internal/tui loads projects for the
// gallery and writes the request flags (`:cleanup`, `:review`, `:hide`,
// `:stop`). internal/acpwrapper loads project.yaml to end a persistent wolf
// session once the project leaves blocked. See state-machine.md and
// design.md.
package project
