package channels

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ConversationKindWolf marks a ConversationTarget that points at a wolf
// agent session.
const ConversationKindWolf = "wolf"

// ConversationTarget is what an outbound notification is a conversation
// with: the agent session a reply to that notification should be routed to.
type ConversationTarget struct {
	// Kind is the sort of agent ("wolf").
	Kind string `json:"kind"`
	// WorkStream is the human label of the project / work stream, the same
	// "<work-stream>" shown in the notification text.
	WorkStream string `json:"work_stream,omitempty"`
	// ProjectPath is the absolute path of the .project.yaml the session serves.
	ProjectPath string `json:"project_path,omitempty"`
	// SessionKey is the daemon's session-map key for the session (for a wolf,
	// "<project path>#wolf").
	SessionKey string `json:"session_key"`
	// SessionID is the launcher's name for the live session (the tmux
	// session / ACP session name), which changes on every relaunch and so
	// tells a stale entry from the current session.
	SessionID string `json:"session_id,omitempty"`
}

// ConversationIndex maps the ID a channel assigned to an outbound message
// back to the conversation it started, so an inbound reply-to can be routed
// to the right agent session. Implementations are safe for concurrent use.
type ConversationIndex interface {
	// Register records that messageID, sent on channel, belongs to target.
	Register(channel, messageID string, target ConversationTarget) error
	// Lookup returns the target registered for the message, if any.
	Lookup(channel, messageID string) (ConversationTarget, bool)
}

// Default limits of a FileConversationIndex.
const (
	DefaultConversationMaxEntries = 1000
	DefaultConversationTTL        = 30 * 24 * time.Hour
)

// FileConversationIndex is a ConversationIndex held in memory and mirrored to
// a JSON file after every Register (written to a temp file and renamed, so a
// crash never leaves a torn file). Entries older than the TTL are dropped, and
// the oldest are evicted beyond the size cap, so the file stays small.
type FileConversationIndex struct {
	path       string
	maxEntries int
	ttl        time.Duration
	now        func() time.Time

	mu      sync.Mutex
	entries map[string]conversationEntry
}

type conversationEntry struct {
	Channel    string             `json:"channel"`
	MessageID  string             `json:"message_id"`
	Target     ConversationTarget `json:"target"`
	RecordedAt time.Time          `json:"recorded_at"`
}

type conversationFile struct {
	Version int                 `json:"version"`
	Entries []conversationEntry `json:"entries"`
}

var _ ConversationIndex = (*FileConversationIndex)(nil)

// NewFileConversationIndex opens (creating on first Register) the index at
// path. An empty path keeps the index in memory only. A missing file starts
// empty; an unreadable or corrupt one is an error so the caller can decide
// whether to continue without persistence.
func NewFileConversationIndex(path string) (*FileConversationIndex, error) {
	x := &FileConversationIndex{
		path:       path,
		maxEntries: DefaultConversationMaxEntries,
		ttl:        DefaultConversationTTL,
		now:        time.Now,
		entries:    map[string]conversationEntry{},
	}
	if path == "" {
		return x, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return x, nil
	}
	if err != nil {
		return nil, fmt.Errorf("channels: read conversation index: %w", err)
	}
	var f conversationFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("channels: parse conversation index %s: %w", path, err)
	}
	for _, e := range f.Entries {
		if e.Channel != "" && e.MessageID != "" {
			x.entries[conversationKey(e.Channel, e.MessageID)] = e
		}
	}
	x.pruneLocked()
	return x, nil
}

func conversationKey(channel, messageID string) string { return channel + "\x00" + messageID }

// Register implements ConversationIndex. The in-memory entry is recorded even
// when persisting fails; the error reports only the persistence failure.
func (x *FileConversationIndex) Register(channel, messageID string, target ConversationTarget) error {
	if channel == "" || messageID == "" {
		return errors.New("channels: conversation index: channel and message id are required")
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.entries[conversationKey(channel, messageID)] = conversationEntry{
		Channel: channel, MessageID: messageID, Target: target, RecordedAt: x.now(),
	}
	x.pruneLocked()
	return x.persistLocked()
}

// Lookup implements ConversationIndex.
func (x *FileConversationIndex) Lookup(channel, messageID string) (ConversationTarget, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	e, ok := x.entries[conversationKey(channel, messageID)]
	if !ok || x.expired(e) {
		return ConversationTarget{}, false
	}
	return e.Target, true
}

// Len reports how many entries are held.
func (x *FileConversationIndex) Len() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return len(x.entries)
}

func (x *FileConversationIndex) expired(e conversationEntry) bool {
	return x.ttl > 0 && x.now().Sub(e.RecordedAt) > x.ttl
}

// pruneLocked drops expired entries and evicts the oldest beyond the cap.
func (x *FileConversationIndex) pruneLocked() {
	for k, e := range x.entries {
		if x.expired(e) {
			delete(x.entries, k)
		}
	}
	if x.maxEntries <= 0 || len(x.entries) <= x.maxEntries {
		return
	}
	all := x.sortedLocked()
	for _, e := range all[:len(all)-x.maxEntries] {
		delete(x.entries, conversationKey(e.Channel, e.MessageID))
	}
}

// sortedLocked returns the entries oldest first (ties by key, for determinism).
func (x *FileConversationIndex) sortedLocked() []conversationEntry {
	all := make([]conversationEntry, 0, len(x.entries))
	for _, e := range x.entries {
		all = append(all, e)
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].RecordedAt.Equal(all[j].RecordedAt) {
			return all[i].RecordedAt.Before(all[j].RecordedAt)
		}
		return conversationKey(all[i].Channel, all[i].MessageID) < conversationKey(all[j].Channel, all[j].MessageID)
	})
	return all
}

func (x *FileConversationIndex) persistLocked() error {
	if x.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(conversationFile{Version: 1, Entries: x.sortedLocked()}, "", "  ")
	if err != nil {
		return fmt.Errorf("channels: encode conversation index: %w", err)
	}
	dir := filepath.Dir(x.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("channels: conversation index dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".conversations-*.tmp")
	if err != nil {
		return fmt.Errorf("channels: conversation index temp file: %w", err)
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("channels: write conversation index: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("channels: write conversation index: %w", err)
	}
	if err := os.Rename(tmpName, x.path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("channels: write conversation index: %w", err)
	}
	return nil
}
