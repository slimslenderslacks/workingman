package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/slimslenderslacks/work/internal/daemon"
)

// stateFileOff is the --state-file value that disables snapshot publishing.
const stateFileOff = "off"

// staleAfter is how old a daemon snapshot may be before `orch status` warns
// that the daemon has probably stopped refreshing it (the daemon rewrites at
// least every 15s).
const staleAfter = time.Minute

// absPath returns p made absolute, or p unchanged if that fails.
func absPath(p string) string {
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// resolveStateFile turns the --state-file flag into the path the daemon
// publishes to: the flag value if set, the default beside the sessions root if
// empty, or "" (disabled) for "off".
func resolveStateFile(flagValue, sessionsRoot string) (string, error) {
	switch flagValue {
	case stateFileOff:
		return "", nil
	case "":
		return daemon.DefaultStateFile(sessionsRoot)
	}
	return filepath.Abs(flagValue)
}

// runStatus implements `orch status [--json] [--root dir]...`. With --root it
// reads the roots (and ACP session dirs and audit log) directly and works with
// no daemon running — live in-memory data is then reported as unavailable.
// Without --root it prints the running daemon's published snapshot. Returns the
// process exit code.
func runStatus(args []string, stdout, stderr io.Writer) int {
	fset := flag.NewFlagSet("orch status", flag.ContinueOnError)
	fset.SetOutput(stderr)
	var roots rootsFlag
	fset.Var(&roots, "root", "orch root directory to read directly, without a daemon (repeatable). Omit to print the running daemon's published snapshot")
	asJSON := fset.Bool("json", false, "print the snapshot as JSON (the state-file schema, see docs/state-snapshot.md)")
	auditPath := fset.String("audit-log", "logs/audit.log", "audit log to tail for recent events (with --root)")
	sessionsRoot := fset.String("sessions-root", "", "ACP sessions root (default ~/.workingman/sessions)")
	stateFile := fset.String("state-file", "", "daemon snapshot file to read when no --root is given (default: <sessions-root>/../state/snapshot.json)")
	events := fset.Int("audit-events", 20, "number of recent audit events to show (with --root)")
	if err := fset.Parse(args); err != nil {
		return 2
	}

	var snap *daemon.Snapshot
	if len(roots) > 0 {
		snap = daemon.BuildOfflineSnapshot(daemon.OfflineOptions{
			Roots:        roots,
			AuditLog:     *auditPath,
			SessionsRoot: *sessionsRoot,
			AuditEvents:  *events,
		})
	} else {
		path, err := resolveStateFile(*stateFile, *sessionsRoot)
		if err != nil || path == "" {
			fmt.Fprintln(stderr, "orch status: no --root given and no snapshot file to read; pass --root <dir> or --state-file <path>")
			return 2
		}
		snap, err = daemon.ReadSnapshot(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				fmt.Fprintf(stderr, "orch status: no daemon snapshot at %s (is the daemon running? or pass --root <dir> to read the roots directly)\n", path)
			} else {
				fmt.Fprintf(stderr, "orch status: %v\n", err)
			}
			return 1
		}
		if age := time.Since(snap.GeneratedAt); snap.Daemon != nil && snap.Daemon.State == daemon.DaemonStateRunning && age > staleAfter {
			snap.Notes = append(snap.Notes, fmt.Sprintf("snapshot is %s old — the daemon may have stopped without a clean shutdown", age.Round(time.Second)))
		}
	}

	var err error
	if *asJSON {
		err = snap.WriteJSON(stdout)
	} else {
		err = snap.WriteText(stdout)
	}
	if err != nil {
		fmt.Fprintf(stderr, "orch status: %v\n", err)
		return 1
	}
	return 0
}
