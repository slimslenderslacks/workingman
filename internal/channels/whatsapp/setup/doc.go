// Package setup holds the non-interactive core of `orch whatsapp setup`: the
// field validators (ported from hermes-agent's hermes_cli/setup_whatsapp_cloud.py),
// the read/merge/write of the whatsapp section of channels.yaml and its 0600
// secrets file, secret masking, and the text that tells the user what to paste
// into Meta's developer console. Prompting and flag handling live in
// cmd/orch; nothing here reads a terminal.
//
// Secrets never go into channels.yaml. Setup writes them to a 0600 secrets
// file (default ~/.workingman/secrets.yaml, a YAML map of string values) and
// the config refers to them with `{file: ..., key: ...}` credential refs, the
// same shape the channel registry resolves at daemon start.
package setup
