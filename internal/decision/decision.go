package decision

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// Choice is one option the user may pick for a [Request].
type Choice struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// Request is a pending decision read from the inbox directory.
type Request struct {
	ID string `json:"id"`
	// Title is a one-line description shown in the pending list.
	Title string `json:"title"`
	// Summary is markdown describing the decision to be made.
	Summary string   `json:"summary"`
	Choices []Choice `json:"choices"`
	// CreatedAt and Source are optional metadata supplied by the requester.
	CreatedAt *time.Time `json:"created_at,omitempty"`
	Source    string     `json:"source,omitempty"`
}

// Answer is the user's response to a [Request], written to the outbox as
// `<ID>.json`.
type Answer struct {
	ID          string    `json:"id"`
	ChoiceID    string    `json:"choice_id"`
	ChoiceLabel string    `json:"choice_label"`
	AnsweredAt  time.Time `json:"answered_at"`
}

// ParseRequest decodes a JSON request and validates it. Malformed JSON and
// invalid requests both return a descriptive error.
func ParseRequest(data []byte) (Request, error) {
	var r Request
	if err := json.Unmarshal(data, &r); err != nil {
		var syn *json.SyntaxError
		var typ *json.UnmarshalTypeError
		switch {
		case errors.As(err, &syn):
			return Request{}, fmt.Errorf("malformed decision request JSON at offset %d: %w", syn.Offset, err)
		case errors.As(err, &typ):
			return Request{}, fmt.Errorf("decision request field %q has wrong type: %w", typ.Field, err)
		default:
			return Request{}, fmt.Errorf("malformed decision request JSON: %w", err)
		}
	}
	if err := r.Validate(); err != nil {
		return Request{}, err
	}
	return r, nil
}

// Validate reports whether r is a usable request: a filename-safe id, a
// title, and at least one choice, each with an id and label, with no
// duplicate choice ids.
func (r Request) Validate() error {
	if r.ID == "" {
		return errors.New("decision request: missing id")
	}
	if err := checkSafeID(r.ID); err != nil {
		return fmt.Errorf("decision request: %w", err)
	}
	if strings.TrimSpace(r.Title) == "" {
		return fmt.Errorf("decision request %q: missing title", r.ID)
	}
	if len(r.Choices) == 0 {
		return fmt.Errorf("decision request %q: no choices", r.ID)
	}
	seen := make(map[string]bool, len(r.Choices))
	for i, c := range r.Choices {
		if strings.TrimSpace(c.ID) == "" {
			return fmt.Errorf("decision request %q: choice %d: missing id", r.ID, i)
		}
		if strings.TrimSpace(c.Label) == "" {
			return fmt.Errorf("decision request %q: choice %q: missing label", r.ID, c.ID)
		}
		if seen[c.ID] {
			return fmt.Errorf("decision request %q: duplicate choice id %q", r.ID, c.ID)
		}
		seen[c.ID] = true
	}
	return nil
}

// checkSafeID rejects ids that are not safe to use as a single file name.
func checkSafeID(id string) error {
	if id == "." || id == ".." || strings.Contains(id, "..") {
		return fmt.Errorf("id %q contains \"..\"", id)
	}
	if strings.HasPrefix(id, ".") {
		return fmt.Errorf("id %q starts with a dot", id)
	}
	if strings.ContainsAny(id, `/\:`) {
		return fmt.Errorf("id %q contains a path separator", id)
	}
	if strings.TrimSpace(id) != id {
		return fmt.Errorf("id %q has leading or trailing whitespace", id)
	}
	for _, c := range id {
		if unicode.IsControl(c) {
			return fmt.Errorf("id %q contains a control character", id)
		}
	}
	if len(id) > 200 {
		return fmt.Errorf("id is too long (%d bytes, max 200)", len(id))
	}
	return nil
}

// IsCandidateFile reports whether an inbox entry name looks like a complete
// request file: it must end in ".json" and must not be hidden or a temp or
// partial write.
func IsCandidateFile(name string) bool {
	base := filepath.Base(name)
	if base == "" || strings.HasPrefix(base, ".") || strings.HasSuffix(base, "~") {
		return false
	}
	if !strings.HasSuffix(base, ".json") || base == ".json" {
		return false
	}
	lower := strings.ToLower(base)
	for _, marker := range []string{".tmp", ".part", ".partial", ".swp", ".crdownload"} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	return true
}

// WriteAnswer atomically writes a as `<a.ID>.json` in outboxDir, replacing
// any existing answer with the same id. The answer is written to a temp file
// in outboxDir, fsynced and renamed into place; the temp file is removed on
// failure.
func WriteAnswer(outboxDir string, a Answer) (err error) {
	if a.ID == "" {
		return errors.New("write answer: missing id")
	}
	if err := checkSafeID(a.ID); err != nil {
		return fmt.Errorf("write answer: %w", err)
	}
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return fmt.Errorf("write answer %q: %w", a.ID, err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(outboxDir, "."+a.ID+".*.tmp")
	if err != nil {
		return fmt.Errorf("write answer %q: %w", a.ID, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return fmt.Errorf("write answer %q: %w", a.ID, err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("write answer %q: sync: %w", a.ID, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("write answer %q: close: %w", a.ID, err)
	}
	if err = os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("write answer %q: chmod: %w", a.ID, err)
	}
	if err = os.Rename(tmpName, filepath.Join(outboxDir, a.ID+".json")); err != nil {
		return fmt.Errorf("write answer %q: rename: %w", a.ID, err)
	}
	return nil
}

// AnsweredDirName is the inbox subdirectory answered requests are moved to.
const AnsweredDirName = "answered"

// ArchiveRequest moves filename (a bare name inside inboxDir) into
// `<inboxDir>/answered/`, creating that directory if needed. An existing
// archived file with the same name is replaced.
func ArchiveRequest(inboxDir, filename string) error {
	if filename == "" || filepath.Base(filename) != filename || filename == "." || filename == ".." {
		return fmt.Errorf("archive request: %q is not a plain file name", filename)
	}
	dst := filepath.Join(inboxDir, AnsweredDirName)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("archive request %q: %w", filename, err)
	}
	if err := os.Rename(filepath.Join(inboxDir, filename), filepath.Join(dst, filename)); err != nil {
		return fmt.Errorf("archive request %q: %w", filename, err)
	}
	return nil
}

// EnsureDirs creates each directory, and any missing parents, if it does
// not already exist.
func EnsureDirs(dirs ...string) error {
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
	}
	return nil
}

// DefaultInbox returns ~/.workingman/decisions/inbox.
func DefaultInbox() (string, error) { return defaultDir("inbox") }

// DefaultOutbox returns ~/.workingman/decisions/outbox.
func DefaultOutbox() (string, error) { return defaultDir("outbox") }

func defaultDir(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".workingman", "decisions", name), nil
}
