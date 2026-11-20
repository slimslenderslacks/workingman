// Package yamledit edits channels.yaml as a yaml.Node tree, so the `orch
// signal` and `orch slack` setup commands can rewrite their own section while
// comments and everything else in the file survive.
//
// Role: daemon (cmd/orch setup commands)
//
// # Key functions
//
//   - [Read] / [Encode] / [WriteFileAtomic] — parse the file (a missing or
//     empty one is an empty mapping), render it with two-space indents, and
//     write it back through a temp file and a rename.
//   - [MapGet], [MapSet], [MapDelete], [EnsureMap], [EnsureSeq], [SetValue] —
//     mapping-node helpers; MapSet keeps the replaced value's comments.
//   - [AddRoutes] — appends `routes:` entries that are not there yet.
//
// # Collaborators
//
// Uses gopkg.in/yaml.v3 and the route type of
// [github.com/slimslenderslacks/work/internal/channels]. Used by
// [github.com/slimslenderslacks/work/internal/channels/signal/setup] and
// [github.com/slimslenderslacks/work/internal/channels/slack/setup].
package yamledit
