// Package setup holds the non-interactive core of `orch whatsapp setup`,
// `status` and `test`.
//
// Role: daemon (cmd/orch setup commands)
//
// It provides the field validators (ported from hermes-agent's
// hermes_cli/setup_whatsapp_cloud.py), the read/merge/write of the whatsapp
// section of channels.yaml and its 0600 secrets file, secret masking, and the
// text that tells the user what to paste into Meta's developer console.
// Prompting and flag handling live in
// cmd/orch; nothing here reads a terminal.
//
// Secrets never go into channels.yaml. Setup writes them to a 0600 secrets
// file (default ~/.workingman/secrets.yaml, a YAML map of string values) and
// the config refers to them with `{file: ..., key: ...}` credential refs, the
// same shape the channel registry resolves at daemon start.
//
// # Key types
//
//   - [Store] — the config file, secrets file and channel instance setup
//     operates on; Load returns a [Current] (with each [Cred]) and Save applies
//     an [Update], reporting a [SaveResult].
//   - [Guide] / [WebhookSettings] / [WriteInstructions] — the post-setup Meta
//     console instructions.
//   - ValidateAccessToken, ValidateAppSecret, ValidatePhoneNumberID,
//     ValidateVerifyToken, ValidateWABAID and [NormalizeAllowFrom] — field
//     checks run on every value before it is saved.
//   - [Mask] / [MaskID] — redact secrets and ids for display.
//
// # Collaborators
//
// Uses [github.com/slimslenderslacks/work/internal/channels] for the config
// model and credential refs, and
// [github.com/slimslenderslacks/work/internal/channels/whatsapp] for option
// names, id normalisation and the Graph phone-number check. Used by cmd/orch.
// See docs/channels.md.
package setup
