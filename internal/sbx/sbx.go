package sbx

import (
	"context"
	"encoding/json"
	"fmt"
)

// Sandbox is one entry of `sbx ls --json`.
type Sandbox struct {
	Name       string   `json:"name"`
	Workspaces []string `json:"workspaces"`
}

// DecodeList parses `sbx ls --json` output. Older sbx releases wrap the list
// as {"sandboxes": [...]}; newer ones (v0.50+) print a bare array (`[]` when
// empty). Both are accepted. As DecodeSecretList does, it skips lines sbx
// prints around the document — `WARN: ...` lines and JSON log objects (which
// have no "sandboxes" key) — and decodes only the first array or
// {"sandboxes": ...} object starting its own line, ignoring trailing data.
func DecodeList(out []byte) ([]Sandbox, error) {
	raw, ok := firstJSON(out, func(raw json.RawMessage) bool {
		return raw[0] == '[' || hasKey(raw, "sandboxes")
	})
	if !ok {
		return nil, fmt.Errorf("no sandbox list in output %q", truncate(out))
	}
	if raw[0] == '[' {
		var list []Sandbox
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, err
		}
		return list, nil
	}
	var data struct {
		Sandboxes []Sandbox `json:"sandboxes"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	return data.Sandboxes, nil
}

// List runs `sbx ls --json` (see Call) and decodes it with DecodeList.
func (q Client) List(ctx context.Context) ([]Sandbox, error) {
	var list []Sandbox
	err := q.Call(ctx, func(out []byte) (err error) {
		list, err = DecodeList(out)
		return err
	}, "ls", "--json")
	return list, err
}

// Alive reports whether the named sandbox currently exists according to
// `sbx ls --json`, run through a Client with the default retries under
// DefaultLockPath (its callers, the TUI's ACP watcher and the daemon's
// session reconciliation, have no sessions root of their own at hand).
//
// The boolean is only meaningful when err is nil: alive=false with a nil
// error means the sandbox is authoritatively absent. A non-nil error means
// the query itself could not run (sbx missing, transient failure) and is
// inconclusive — callers must not treat that as "gone".
func Alive(ctx context.Context, name string) (bool, error) {
	list, err := Client{LockPath: DefaultLockPath()}.List(ctx)
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
