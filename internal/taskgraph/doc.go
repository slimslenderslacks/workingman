// Package taskgraph loads a project's tasks/*.yaml files into an immutable,
// validated dependency graph that the daemon queries to decide what to
// dispatch next.
//
// Role: daemon
//
// [Load] is a pure snapshot of disk state. The daemon calls it again whenever
// it wants a fresh view (for example after an fsnotify event under tasks/)
// and then asks the [Graph] for [Graph.Ready] tasks (ready, with every
// dependency committed) or whether it is [Graph.AllCommitted]. A missing
// tasks/ directory loads as an empty graph.
//
// Load repairs per-file mistakes and rejects whole-graph ones. A blank or
// malformed task name, or one too long for the project's sandbox-name budget,
// is rewritten to a slug, depends_on edges are rewritten to match, and the
// repair is reported through [Graph.Warnings]. An unknown dependency, a cycle
// or a duplicate valid name makes Load return an error. A file with a blank
// name and a non-blank description is a pending seed waiting for the planning
// agent: it is left out of the graph and counted by [Graph.HasPendingSeed].
//
// # Key types
//
//   - [Graph] — the loaded DAG: Tasks, Task, Ready, AllCommitted, Empty,
//     HasPendingSeed and Warnings.
//   - [Load] — reads and validates a tasks/ directory.
//
// # Collaborators
//
// Built on internal/task (Load, ValidNameFor, SlugifyFor). Used only by
// internal/daemon. internal/tui reads task files directly with a lenient
// loader instead, so one bad file can't hide the whole Tasks pane.
package taskgraph
