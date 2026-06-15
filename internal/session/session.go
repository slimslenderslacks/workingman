// Package session defines the on-disk layout that lets non-interactive ACP
// claude sessions survive a TUI restart. Each session owns a directory under a
// sessions root (default ~/.workingman/sessions):
//
//	~/.workingman/sessions/<id>/session.json   session metadata (this package)
//	~/.workingman/sessions/<id>/agent.sock     the ACP bridge socket (acp-wrapper)
//
// acp-wrapper is the writer: it records a session when it launches the sandbox,
// updates the status as the session progresses, and removes the directory when
// the sandbox exits. The TUI is the reader: on startup it Lists the sessions
// root to discover ongoing sessions and reconnects to each agent.sock to resume
// watching the stream. The metadata in session.json is exactly what the reader
// needs to reconnect without the writer being alive to ask.
//
// Writes are atomic (write-temp-then-rename) so a reader that Lists the
// directory while the writer is mid-update never observes a half-written or
// truncated session.json.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// FileName is the metadata file written inside each session directory.
const FileName = "session.json"

// SocketName is the unix-domain socket the acp-wrapper exposes inside each
// session directory and that the TUI dials to reconnect over ACP. It lives here
// so the layout has a single source of truth shared by writer and reader.
const SocketName = "agent.sock"

// Status is the lifecycle state of an ACP session as recorded in session.json.
// It is what a restarting TUI uses to decide whether a discovered session is
// still worth reconnecting to.
type Status string

const (
	// StatusStarting is written before the sandbox/ACP client is confirmed up.
	StatusStarting Status = "starting"
	// StatusRunning means the ACP client is live and the socket is accepting
	// connections — the TUI should reconnect and resume watching.
	StatusRunning Status = "running"
	// StatusExited means the ACP client finished cleanly; the socket is gone.
	StatusExited Status = "exited"
	// StatusFailed means the session ended in error.
	StatusFailed Status = "failed"
)

// Session is the typed representation of session.json: everything a restarting
// TUI needs to rediscover an ACP session and reconnect to it without the writer
// being alive to consult. Unknown JSON fields are ignored on read so the schema
// can grow without breaking older readers.
type Session struct {
	// ID uniquely identifies the session and names its directory. It is a
	// single path segment (no separators, not "." or "..").
	ID string `json:"id"`

	// SandboxName is the sbx sandbox backing this session (e.g. acp-<id>). The
	// TUI checks whether this sandbox still exists before trusting the socket;
	// if the sandbox is gone the session is stale and gets cleaned up.
	SandboxName string `json:"sandbox_name"`

	// SandboxID is sbx's own identifier for the sandbox, when known. Optional —
	// SandboxName is the primary handle.
	SandboxID string `json:"sandbox_id,omitempty"`

	// Status is the session's lifecycle state. See the Status constants.
	Status Status `json:"status"`

	// CreatedAt is when the session directory was first written.
	CreatedAt time.Time `json:"created_at"`

	// UpdatedAt is when session.json was last written. Lets a reader prefer the
	// freshest record and reason about staleness.
	UpdatedAt time.Time `json:"updated_at,omitempty"`

	// SocketPath is the absolute path of agent.sock — the ACP transport the TUI
	// reconnects to. Recorded explicitly so the reader doesn't have to re-derive
	// the layout.
	SocketPath string `json:"socket_path"`

	// Workspaces are the host paths mounted into the sandbox; the first is the
	// ACP client's working directory. Surfaced so the TUI can label the session.
	Workspaces []string `json:"workspaces,omitempty"`

	// Kit is the acp-kit reference layered onto the sandbox. Optional context
	// for diagnostics and for relaunching an equivalent wrapper.
	Kit string `json:"kit,omitempty"`

	// LogPath, when set, is the file the wrapper appends the raw ACP stream to.
	// A reconnecting TUI can replay it to rebuild scrollback the live socket no
	// longer carries (the socket only resumes the stream from "now").
	LogPath string `json:"log_path,omitempty"`

	// PromptCount is how many prompts have been sent to the agent so far — a
	// cheap pointer into the prompt history for the TUI to show without parsing
	// the whole log.
	PromptCount int `json:"prompt_count,omitempty"`
}

// validID reports whether id is usable as a single-segment directory name.
// Mirrors acp-wrapper's session-id validation so writer and reader agree on
// what a legal id is.
func validID(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("session: id is required")
	}
	if id == "." || id == ".." || strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("session: invalid id %q: must be a single path segment", id)
	}
	return nil
}

// Store is a sessions root on disk. All path derivation and the read/write/list
// helpers hang off it so the layout is defined in exactly one place.
type Store struct {
	// Root is the parent of every session directory.
	Root string
}

// DefaultRoot is the sessions root used when none is configured:
// ~/.workingman/sessions.
func DefaultRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("session: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".workingman", "sessions"), nil
}

// NewStore returns a Store rooted at root, falling back to DefaultRoot when root
// is empty. The returned Root is absolute.
func NewStore(root string) (Store, error) {
	if strings.TrimSpace(root) == "" {
		def, err := DefaultRoot()
		if err != nil {
			return Store{}, err
		}
		root = def
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return Store{}, fmt.Errorf("session: sessions root %q: %w", root, err)
	}
	return Store{Root: abs}, nil
}

// Dir is the directory holding a session's metadata and socket.
func (s Store) Dir(id string) string { return filepath.Join(s.Root, id) }

// Path is the session.json file for the given session id.
func (s Store) Path(id string) string { return filepath.Join(s.Dir(id), FileName) }

// SocketPath is the agent.sock for the given session id.
func (s Store) SocketPath(id string) string { return filepath.Join(s.Dir(id), SocketName) }

// Write atomically persists sess to <Root>/<sess.ID>/session.json, creating the
// session directory if needed. It writes a temp file in the same directory and
// renames it into place, so a concurrent reader never sees a partial file. If
// sess.SocketPath is empty it is defaulted to the store's socket path; if
// CreatedAt is zero the caller is responsible for stamping it (Write does not
// invent timestamps).
func (s Store) Write(sess Session) error {
	if err := validID(sess.ID); err != nil {
		return err
	}
	if sess.SocketPath == "" {
		sess.SocketPath = s.SocketPath(sess.ID)
	}

	dir := s.Dir(sess.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("session: create dir %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return fmt.Errorf("session: marshal %s: %w", sess.ID, err)
	}
	data = append(data, '\n')

	return atomicWrite(s.Path(sess.ID), data)
}

// atomicWrite writes data to a temp file in the destination directory, fsyncs
// it, then renames it over path. The rename is atomic on POSIX filesystems, so
// a reader observes either the old file or the fully written new one — never a
// truncated mix. The temp file is removed on any error before the rename.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+FileName+".*")
	if err != nil {
		return fmt.Errorf("session: create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before the rename; harmless after it
	// (the temp name no longer exists).
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("session: write temp %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("session: sync temp %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("session: close temp %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("session: rename %s -> %s: %w", tmpName, path, err)
	}
	return nil
}

// Read loads and decodes the session.json for the given session id. A missing
// file is reported with an error satisfying errors.Is(err, fs.ErrNotExist) so
// callers can distinguish "no such session" from a corrupt one.
func (s Store) Read(id string) (Session, error) {
	if err := validID(id); err != nil {
		return Session{}, err
	}
	return readFile(s.Path(id))
}

func readFile(path string) (Session, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		// Pass fs.ErrNotExist through unwrapped enough for errors.Is.
		return Session{}, fmt.Errorf("session: read %s: %w", path, err)
	}
	var sess Session
	if err := json.Unmarshal(data, &sess); err != nil {
		return Session{}, fmt.Errorf("session: decode %s: %w", path, err)
	}
	return sess, nil
}

// List discovers every session under the store's root and returns the ones with
// a readable session.json, sorted by CreatedAt (oldest first, ties broken by
// ID) for stable display. A missing root is not an error — it means no sessions
// yet, so List returns an empty slice. Directories without a session.json, or
// whose session.json is unreadable/corrupt, are skipped rather than failing the
// whole listing, so one bad session can't hide the others from the TUI.
func (s Store) List() ([]Session, error) {
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("session: read sessions root %s: %w", s.Root, err)
	}

	out := make([]Session, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sess, err := readFile(filepath.Join(s.Root, e.Name(), FileName))
		if err != nil {
			// No metadata (or unreadable): not a session we can reconnect to.
			continue
		}
		out = append(out, sess)
	}

	sortByCreatedAt(out)
	return out, nil
}

// Remove deletes a session's entire directory (session.json, socket, and any
// logs). Used by the writer when the sandbox exits and by the TUI to clean up a
// session whose sandbox is gone. A missing directory is not an error.
func (s Store) Remove(id string) error {
	if err := validID(id); err != nil {
		return err
	}
	if err := os.RemoveAll(s.Dir(id)); err != nil {
		return fmt.Errorf("session: remove %s: %w", s.Dir(id), err)
	}
	return nil
}

// sortByCreatedAt orders sessions oldest-first, breaking ties by ID so the
// result is fully deterministic — matching the daemon's live-session ordering so
// the two panes line up.
func sortByCreatedAt(ss []Session) {
	sort.Slice(ss, func(i, j int) bool {
		if ss[i].CreatedAt.Equal(ss[j].CreatedAt) {
			return ss[i].ID < ss[j].ID
		}
		return ss[i].CreatedAt.Before(ss[j].CreatedAt)
	})
}
