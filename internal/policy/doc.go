// Package policy defines the per-task sandbox policy rules (allow or deny one
// network or filesystem resource pattern) and turns them into `sbx policy`
// commands.
//
// Role: shared (daemon/runner, TUI and acp-wrapper)
//
// Tasks list rules under `policies:` in their YAML (task.Task.Policies). The
// daemon side passes each rule to acp-wrapper as a repeatable
// `--policy <action>:<kind>:<resource>` flag ([Rule.Encode] / [Decode]), and
// whichever process creates the sandbox applies the rules with
// [Rule.CLIArgs] right after `sbx create` and before any `sbx exec`. Decode
// and [Rule.Validate] reject unknown actions and kinds, so a typo can't turn
// into a different sbx command than intended.
//
// # Key types
//
//   - [Rule] — one policy entry. The YAML keys are action, type and resource.
//   - [Action] — allow or deny.
//   - [Kind] — network or filesystem.
//
// # Collaborators
//
// No internal imports. Used by internal/task (the Policies field),
// internal/runner and internal/daemon (encode rules into the acp-wrapper
// command line), cmd/acp-wrapper and internal/acpwrapper (decode and apply
// them), and internal/tui (shows a task's rules). See
// docs/sandbox-provider-design.md for the plan to move this behind a sandbox
// provider interface.
package policy
