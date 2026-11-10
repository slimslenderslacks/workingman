// Package service builds the macOS launchd LaunchAgent that keeps the orch
// daemon running in the background.
//
// Role: daemon
//
// Everything here is pure: [Config.Plist] renders the property list and
// [Config.Args] the daemon command line, with no filesystem or launchctl
// access, so they are unit-testable anywhere. Writing the file and calling
// launchctl lives in cmd/orch (`orch service install|uninstall|status`).
//
// The agent runs `orch --headless --require-ac ...` with RunAtLoad and
// KeepAlive. The daemon itself pauses while on battery (see
// internal/daemon/power_gate.go), so launchd never has to be power-aware.
//
// # Key types
//
//   - [Config] — binary, roots and passthrough flags, log dir.
//
// # Collaborators
//
// No internal imports. Consumed by cmd/orch.
package service
