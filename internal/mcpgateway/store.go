package mcpgateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Record is the per-sandbox gateway state persisted on the host: which
// gateway a sandbox was created with, so a later launch (after a restart, a
// retry, or a recreate) can tell whether the sandbox's MCP_URL is still valid
// and DELETE the gateway once the sandbox is gone.
type Record struct {
	// Sandbox is the sbx sandbox name the gateway was minted for.
	Sandbox string `json:"sandbox"`
	// BaseURL is the standalone-codemode root the gateway was created on.
	BaseURL string `json:"base_url"`
	// ID and URL are POST /gateways' answer (URL as the host reaches it).
	ID  string `json:"id"`
	URL string `json:"url"`
	// SandboxURL is URL rewritten for the sandbox (SandboxURL): the value of
	// the sandbox's MCP_URL.
	SandboxURL string `json:"sandbox_url"`
	// Requested are the server names asked for; Servers the ones the gateway
	// was actually created over (Requested minus names the catalog lacks).
	Requested []string  `json:"requested"`
	Servers   []string  `json:"servers"`
	CreatedAt time.Time `json:"created_at"`
}

// Store keeps one Record per sandbox as <Dir>/<sandbox>.json.
type Store struct {
	Dir string
}

// StateDir is the default Store directory for a sessions root: a "gateways"
// directory beside it (<sessions-root>/../gateways), next to the daemon's
// state/ directory. It is deliberately not inside the sessions root: session
// directories are removed when their session ends, but a kept sandbox (and so
// its gateway) outlives its session.
func StateDir(sessionsRoot string) string {
	return filepath.Join(filepath.Dir(filepath.Clean(sessionsRoot)), "gateways")
}

func (s Store) path(sandbox string) (string, error) {
	if sandbox == "" || sandbox == "." || sandbox == ".." || strings.ContainsAny(sandbox, `/\`) {
		return "", fmt.Errorf("mcpgateway: invalid sandbox name %q", sandbox)
	}
	return filepath.Join(s.Dir, sandbox+".json"), nil
}

// Load returns the record for sandbox; ok is false when there is none.
func (s Store) Load(sandbox string) (rec Record, ok bool, err error) {
	p, err := s.path(sandbox)
	if err != nil {
		return Record{}, false, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, fmt.Errorf("mcpgateway: read %s: %w", p, err)
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, false, fmt.Errorf("mcpgateway: decode %s: %w", p, err)
	}
	return rec, true, nil
}

// Save writes rec atomically (temp file + rename).
func (s Store) Save(rec Record) error {
	p, err := s.path(rec.Sandbox)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return fmt.Errorf("mcpgateway: create %s: %w", s.Dir, err)
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.Dir, "."+rec.Sandbox+".*.tmp")
	if err != nil {
		return fmt.Errorf("mcpgateway: %w", err)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("mcpgateway: write %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("mcpgateway: %w", err)
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("mcpgateway: %w", err)
	}
	return nil
}

// Remove deletes the record for sandbox; a missing record is not an error.
func (s Store) Remove(sandbox string) error {
	p, err := s.path(sandbox)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("mcpgateway: %w", err)
	}
	return nil
}

// List returns every record in the store, skipping unreadable files.
func (s Store) List() ([]Record, error) {
	if s.Dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("mcpgateway: read %s: %w", s.Dir, err)
	}
	var out []Record
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		rec, ok, err := s.Load(name)
		if err != nil || !ok {
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}
