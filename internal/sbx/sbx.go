// Package sbx wraps the parts of the sbx sandbox CLI's stable `sbx ls --json`
// read interface that more than one package needs to query. It exists so that
// "does a sandbox by this name still exist" has exactly one implementation
// shared by the TUI's ACP watcher (internal/tui/acpwatch.go) and the daemon's
// session reconciliation (internal/daemon/reconcile.go), instead of each
// growing its own copy of the same exec+JSON-decode logic.
package sbx

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
)

// Sandbox is one entry of `sbx ls --json`.
type Sandbox struct {
	Name       string   `json:"name"`
	Workspaces []string `json:"workspaces"`
}

// DecodeList parses `sbx ls --json` output. Older sbx releases wrap the list
// as {"sandboxes": [...]}; newer ones (v0.50+) print a bare array (`[]` when
// empty). Both are accepted.
func DecodeList(out []byte) ([]Sandbox, error) {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var list []Sandbox
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return nil, err
		}
		return list, nil
	}
	var data struct {
		Sandboxes []Sandbox `json:"sandboxes"`
	}
	if err := json.Unmarshal(trimmed, &data); err != nil {
		return nil, err
	}
	return data.Sandboxes, nil
}

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
	list, err := DecodeList(out)
	if err != nil {
		return false, err
	}
	for _, s := range list {
		if s.Name == name {
			return true, nil
		}
	}
	return false, nil
}
