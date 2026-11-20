package slack

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	// SentFile and WatchFile are the channel's state files in its state
	// directory (~/.workingman/slack/).
	SentFile  = "sent.json"
	WatchFile = "watch.json"

	// DefaultSentTTL is how long a posted ts is remembered.
	DefaultSentTTL = 30 * 24 * time.Hour
)

// Watch is one sent post-office request whose thread is polled for answers.
type Watch struct {
	// ID is the message ID Send returned: "<channel_id>:<ts>".
	ID string `json:"id"`
	// Chat is the chat id the request was sent to (the route's chat, e.g. a
	// user id for the DM-to-self), reported as the ChatID of its replies so
	// the Dispatcher can tell which delivery answered.
	Chat     string    `json:"chat,omitempty"`
	OpenedAt time.Time `json:"opened_at"`
	// ThreadCursor is the newest reply ts seen; read_thread's oldest is
	// exclusive, so it is passed as is.
	ThreadCursor string `json:"thread_cursor,omitempty"`
	// ReplyCount is the "Thread: N replies" count when the thread was last
	// read; a higher count in read_channel means new replies.
	ReplyCount int `json:"reply_count"`
	// Closed is set once Resolve has marked the request closed; later human
	// replies get an "already closed" answer instead of being forwarded.
	Closed *Closed `json:"closed,omitempty"`
}

// Closed records how a watched request was resolved.
type Closed struct {
	Outcome string    `json:"outcome"`
	Via     string    `json:"via,omitempty"`
	At      time.Time `json:"at"`
}

// sentState is sent.json: every message ID the channel posted, with when.
type sentState struct {
	Version int                  `json:"version"`
	Sent    map[string]time.Time `json:"sent"`
}

// watchState is watch.json: the watched requests and the top-level cursor.
type watchState struct {
	Version int `json:"version"`
	// ChannelID is the resolved conversation id (a DM-to-self configured as
	// the user id U… resolves to D…).
	ChannelID string `json:"channel_id,omitempty"`
	// TopCursor is the newest top-level ts seen; older top-level messages are
	// never emitted.
	TopCursor string   `json:"top_cursor,omitempty"`
	Watches   []*Watch `json:"watches"`
	// Unconfirmed are sends whose response was lost and which could not be
	// found afterwards; their echo, if it shows up, is ours.
	Unconfirmed []Unconfirmed `json:"unconfirmed,omitempty"`
}

// Unconfirmed is a send of unknown outcome.
type Unconfirmed struct {
	Chat   string    `json:"chat"`
	Thread string    `json:"thread,omitempty"`
	Key    string    `json:"key"` // echoKey of the text
	At     time.Time `json:"at"`
}

// unconfirmedTTL bounds how long an unconfirmed send's echo is expected.
const unconfirmedTTL = time.Hour

// state is the channel's persisted memory. Not safe for concurrent use: only
// the worker touches it.
type state struct {
	dir   string // "" keeps it in memory only
	sent  sentState
	watch watchState
}

// loadState reads the state files in dir. A missing file is empty; a corrupt
// one is reported and replaced by an empty state, so the channel still
// starts (it then only misses replies to requests it sent before).
func loadState(dir string) (*state, error) {
	st := &state{dir: dir}
	st.sent.Sent = map[string]time.Time{}
	if dir == "" {
		return st, nil
	}
	var errs []error
	if err := readJSON(filepath.Join(dir, SentFile), &st.sent); err != nil {
		errs = append(errs, err)
		st.sent = sentState{}
	}
	if err := readJSON(filepath.Join(dir, WatchFile), &st.watch); err != nil {
		errs = append(errs, err)
		st.watch = watchState{}
	}
	if st.sent.Sent == nil {
		st.sent.Sent = map[string]time.Time{}
	}
	var ws []*Watch
	for _, w := range st.watch.Watches {
		if w != nil && w.ID != "" {
			ws = append(ws, w)
		}
	}
	st.watch.Watches = ws
	return st, errors.Join(errs...)
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("slack: %s: %w", path, err)
	}
	return nil
}

// save writes both files (write-then-rename, 0600).
func (s *state) save() error {
	if s.dir == "" {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	s.sent.Version, s.watch.Version = 1, 1
	return errors.Join(
		writeJSON(filepath.Join(s.dir, SentFile), s.sent),
		writeJSON(filepath.Join(s.dir, WatchFile), s.watch),
	)
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op after a successful rename
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func (s *state) markSent(id string, at time.Time) { s.sent.Sent[id] = at }

func (s *state) isSent(id string) bool {
	_, ok := s.sent.Sent[id]
	return ok
}

func (s *state) watchFor(id string) *Watch {
	for _, w := range s.watch.Watches {
		if w.ID == id {
			return w
		}
	}
	return nil
}

func (s *state) addWatch(id, chat string, at time.Time) {
	if s.watchFor(id) == nil {
		s.watch.Watches = append(s.watch.Watches, &Watch{ID: id, Chat: chat, OpenedAt: at})
	}
}

func (s *state) addUnconfirmed(chat, thread, text string, at time.Time) {
	s.watch.Unconfirmed = append(s.watch.Unconfirmed, Unconfirmed{Chat: chat, Thread: thread, Key: echoKey(text), At: at})
}

// takeUnconfirmed removes and reports an unconfirmed send in thread ("" for
// top level) with the given echo key.
func (s *state) takeUnconfirmed(thread, key string) bool {
	for i, u := range s.watch.Unconfirmed {
		if u.Thread == thread && u.Key == key {
			s.watch.Unconfirmed = append(s.watch.Unconfirmed[:i], s.watch.Unconfirmed[i+1:]...)
			return true
		}
	}
	return false
}

// echoKey is a message's text reduced to what survives Slack's rendering of
// it: emoji (which come back as :shortcodes:), markdown punctuation and
// whitespace are dropped, the rest lower-cased and cut to 80 runes.
func echoKey(text string) string {
	text = shortcodeRE.ReplaceAllString(text, "")
	var b strings.Builder
	n := 0
	for _, r := range strings.ToLower(text) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			if n++; n == 80 {
				break
			}
		}
	}
	return b.String()
}

var shortcodeRE = regexp.MustCompile(`:[a-z0-9_+\-]+:`)

// prune drops sent entries older than sentTTL and watches older than
// watchTTL, and reports whether anything changed.
func (s *state) prune(now time.Time, sentTTL, watchTTL time.Duration) bool {
	changed := false
	for id, at := range s.sent.Sent {
		if now.Sub(at) > sentTTL {
			delete(s.sent.Sent, id)
			changed = true
		}
	}
	kept := s.watch.Watches[:0]
	for _, w := range s.watch.Watches {
		if now.Sub(w.OpenedAt) > watchTTL {
			changed = true
			continue
		}
		kept = append(kept, w)
	}
	s.watch.Watches = kept
	unc := s.watch.Unconfirmed[:0]
	for _, u := range s.watch.Unconfirmed {
		if now.Sub(u.At) > unconfirmedTTL {
			changed = true
			continue
		}
		unc = append(unc, u)
	}
	s.watch.Unconfirmed = unc
	return changed
}

// --- message ids and timestamps ---------------------------------------------

// MessageID is the channel's message ID for a post: "<channel_id>:<ts>".
func MessageID(channelID, ts string) string { return channelID + ":" + ts }

// SplitMessageID splits a MessageID into its channel id and ts. A bare ts
// (no channel) comes back with an empty channel id.
func SplitMessageID(id string) (channelID, ts string) {
	if i := strings.LastIndexByte(id, ':'); i >= 0 {
		return id[:i], id[i+1:]
	}
	return "", id
}

// parseTS splits a Slack ts ("1795141398.745179") into seconds and
// microseconds.
func parseTS(ts string) (sec, usec int64, ok bool) {
	s, u, found := strings.Cut(ts, ".")
	sec, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	if found {
		if len(u) > 6 || u == "" {
			return 0, 0, false
		}
		u += strings.Repeat("0", 6-len(u))
		if usec, err = strconv.ParseInt(u, 10, 64); err != nil {
			return 0, 0, false
		}
	}
	return sec, usec, true
}

func formatTS(sec, usec int64) string { return fmt.Sprintf("%d.%06d", sec, usec) }

// tsOf is a time as a Slack ts.
func tsOf(t time.Time) string { return formatTS(t.Unix(), int64(t.Nanosecond()/1000)) }

// tsTime is a Slack ts as a time; the zero time when it does not parse.
func tsTime(ts string) time.Time {
	sec, usec, ok := parseTS(ts)
	if !ok {
		return time.Time{}
	}
	return time.Unix(sec, usec*1000)
}

// tsLess reports whether ts a is older than b. Unparsable values sort first.
func tsLess(a, b string) bool {
	as, au, aok := parseTS(a)
	bs, bu, bok := parseTS(b)
	switch {
	case !aok || !bok:
		return !aok && bok
	case as != bs:
		return as < bs
	}
	return au < bu
}

// tsBefore is the ts one microsecond older than ts, so an exclusive "oldest"
// still includes ts itself.
func tsBefore(ts string) string {
	sec, usec, ok := parseTS(ts)
	if !ok {
		return ts
	}
	if usec == 0 {
		return formatTS(sec-1, 999999)
	}
	return formatTS(sec, usec-1)
}

// State is a read-only view of the channel's state files, for `orch slack
// status`.
type State struct {
	// ChannelID is the resolved conversation id ("" until the first send or
	// read); TopCursor the newest top-level ts the poll has seen.
	ChannelID string
	TopCursor string
	// Watches are the watched requests, oldest first.
	Watches []Watch
	// Sent is how many posted message ids are remembered; Unconfirmed how
	// many sends of unknown outcome still await their echo.
	Sent        int
	Unconfirmed int
}

// ReadState reads the state files in dir (~/.workingman/slack). Missing
// files are an empty state; a corrupt one is an error.
func ReadState(dir string) (State, error) {
	var sent sentState
	var watch watchState
	if err := errors.Join(
		readJSON(filepath.Join(dir, SentFile), &sent),
		readJSON(filepath.Join(dir, WatchFile), &watch),
	); err != nil {
		return State{}, err
	}
	st := State{ChannelID: watch.ChannelID, TopCursor: watch.TopCursor, Sent: len(sent.Sent), Unconfirmed: len(watch.Unconfirmed)}
	for _, w := range watch.Watches {
		if w != nil && w.ID != "" {
			st.Watches = append(st.Watches, *w)
		}
	}
	return st, nil
}
