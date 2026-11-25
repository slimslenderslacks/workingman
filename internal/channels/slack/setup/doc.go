// Package setup reads and writes the Slack channel's section of
// channels.yaml for `orch slack setup`, `status` and `test`.
//
// Role: daemon (cmd/orch setup commands)
//
// It is the non-interactive core of those commands: prompting, flag handling
// and the Slack MCP calls live in cmd/orch, and nothing here reads a
// terminal or talks to a network. Save merges an [Update] into
// channels.<name> and adds a route for the post-office topic, preserving
// everything else in the file, comments included, and writes nothing if the
// result would not be a valid config.
//
// # Key types
//
//   - [Store] — the config path and channel instance name setup operates on;
//     [NewStore] fills in the defaults ("slack" in the default channels.yaml).
//   - [Current] — the channel as the file describes it today, returned by
//     [Store.Load] for `status` and `test`.
//   - [Update] / [SaveResult] — what [Store.Save] changes and what it did.
//
// # Collaborators
//
// Uses [github.com/slimslenderslacks/work/internal/channels] for the config
// model and validation,
// [github.com/slimslenderslacks/work/internal/channels/slack] to parse the
// channel's options, and
// [github.com/slimslenderslacks/work/internal/channels/yamledit] to edit the
// file. Used by cmd/orch. See docs/channels.md.
package setup
