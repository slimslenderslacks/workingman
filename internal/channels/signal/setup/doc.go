// Package setup reads and writes the Signal channel's section of
// channels.yaml for `orch signal setup`, `status` and `test`.
//
// Role: daemon (cmd/orch setup commands)
//
// It is the non-interactive core of those commands: prompting and flag
// handling live in cmd/orch, and nothing here reads a terminal. Save merges an
// [Update] into channels.<name> and adds owner routes for the default topics
// (wolf, workingman), preserving everything else in the file, comments
// included, and writes nothing if the result would not be a valid config.
//
// # Key types
//
//   - [Store] — the config path and channel instance name setup operates on;
//     [NewStore] fills in the defaults ("signal" in the default channels.yaml).
//   - [Current] — the channel as the file describes it today, returned by
//     [Store.Load] for `status`.
//   - [Update] / [SaveResult] — what [Store.Save] changes and what it did.
//   - [MaskID] — masks a number, UUID or group id for display.
//
// # Collaborators
//
// Uses [github.com/slimslenderslacks/work/internal/channels] for the config
// model and validation, and
// [github.com/slimslenderslacks/work/internal/channels/signal] to parse and
// check the channel's options. Used by cmd/orch. See docs/signal.md.
package setup
