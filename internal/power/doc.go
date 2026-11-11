// Package power reports whether the host is running on AC power or battery, so
// the daemon can stand down while a laptop is unplugged.
//
// Role: daemon
//
// On macOS the answer comes from `pmset -g batt`; [ParsePmset] is the pure
// parser for its output. Everywhere else [New] returns [ACSource], which always
// reports AC, making the power gate a no-op. [Source] is the seam the daemon
// depends on, so tests inject a fake.
//
// An unreadable or unrecognised pmset reading is an error, and the daemon
// treats it as "keep the last known state" rather than as battery: a desktop
// Mac with no battery reports "AC Power" and must never pause.
//
// # Key types
//
//   - [Source] — OnBattery(ctx) (bool, error).
//   - [Pmset] — the darwin implementation, with an injectable command runner.
//   - [ACSource] — the always-AC implementation.
//
// # Collaborators
//
// No internal imports. cmd/orch builds a Source with [New] and hands it to
// internal/daemon with daemon.WithPowerSource (when --require-ac is on).
package power
