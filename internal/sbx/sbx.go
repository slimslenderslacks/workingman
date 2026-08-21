// Package sbx wraps the parts of the sbx sandbox CLI's stable `sbx ls --json`
// read interface that more than one package needs to query. It exists so that
// "does a sandbox by this name still exist" has exactly one implementation
// shared by the TUI's ACP watcher (internal/tui/acpwatch.go) and the daemon's
// session reconciliation (internal/daemon/reconcile.go), instead of each
// growing its own copy of the same exec+JSON-decode logic.
package sbx

import (
	"context"
	"encoding/json"
	"os/exec"
)

// Alive reports whether the named sandbox currently exists according to
// `sbx ls --json`.
//
// The boolean is only meaningful when err is nil: alive=false with a nil
// error means the sandbox is authoritatively absent. A non-nil error means
// the query itself could not run (sbx missing, transient failure) and is
// inconclusive — callers must not treat that as "gone".
func Alive(ctx context.Context, name string) (bool, error) {
	out, err := exec.CommandContext(ctx, "sbx", "ls", "--json").Output()
	if err != nil {
		return false, err
	}
	var data struct {
		Sandboxes []struct {
			Name string `json:"name"`
		} `json:"sandboxes"`
	}
	if err := json.Unmarshal(out, &data); err != nil {
		return false, err
	}
	for _, s := range data.Sandboxes {
		if s.Name == name {
			return true, nil
		}
	}
	return false, nil
}
