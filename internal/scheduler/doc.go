// Package scheduler runs the daemon's timed callbacks (project cron schedules
// and PR review polls) on robfig/cron, keyed by a caller-chosen string.
//
// Role: daemon
//
// The daemon calls [Scheduler.Register] every time it observes a
// project.yaml, so Register is idempotent on (key, spec): the same spec again
// is a no-op, a changed spec replaces the old entry, and an invalid spec
// returns an error without touching the existing one. [Scheduler.Unregister]
// drops a key. All methods are safe for concurrent use. Callbacks run on
// cron's own goroutines.
//
// Specs are standard 5-field cron expressions or robfig's @every shortcut
// (like "@every 5m"). Production project schedules use 5-field specs; the
// review poll and fast-cycle tests use @every.
//
// The library keeps no history and fires at most one late tick after a sleep,
// so [TicksBetween] lets the daemon count the ticks of a spec in a time window.
// It uses them to report schedules missed while the host slept or the daemon
// was down; see the missed-schedule part of the cron section of state-machine.md.
//
// # Key types
//
//   - [Scheduler] — the key → cron entry table. [New] creates one;
//     [Scheduler.Start] begins firing and [Scheduler.Stop] waits for running
//     callbacks, bounded by a context.
//
// # Collaborators
//
// No internal imports. cmd/orch creates the Scheduler and passes it to
// internal/daemon with daemon.WithScheduler. The daemon registers each
// project's cron field under the project path, and its adaptive #review poll
// under "<path>#review". See the cron section of state-machine.md.
package scheduler
