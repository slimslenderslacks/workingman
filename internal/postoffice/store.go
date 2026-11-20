package postoffice

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// EnvDir overrides the store root (tests, and running a second copy).
const EnvDir = "WORKINGMAN_POST_OFFICE_DIR"

// Subdirectories and files of the store root (§3.1).
const (
	DirInbox      = "inbox"
	DirOutbox     = "outbox"
	DirDone       = "done"
	DirDeliveries = "deliveries"
	tmpDir        = ".tmp"
	badDir        = ".bad"
	LockFile      = "post-office.lock"

	dayLayout = "2006-01-02"
	dirPerm   = 0o700
	filePerm  = 0o600
)

// DefaultRetention is how long done/ records are kept (§3.4).
const DefaultRetention = 30 * 24 * time.Hour

// staleTmp is how old a staging file must be before Recover removes it.
const staleTmp = time.Hour

var (
	// ErrAlreadyClosed is returned by Claim when another claim won. The
	// winning Resolution is returned alongside it.
	ErrAlreadyClosed = errors.New("postoffice: already closed")
	// ErrNotFound means no open, pending or finalised message has the id.
	ErrNotFound = errors.New("postoffice: message not found")
	// ErrInvalidID means an id is not "msg_" + a ULID.
	ErrInvalidID = errors.New("postoffice: invalid message id")
	// ErrAmbiguousShortID means a short id matches several open messages.
	ErrAmbiguousShortID = errors.New("postoffice: ambiguous short id")
	// ErrLocked means another process holds post-office.lock.
	ErrLocked = errors.New("postoffice: store is locked by another post-office")
	// ErrAlreadyReceived is returned by MarkReceived when an agent recipient
	// has already taken the message.
	ErrAlreadyReceived = errors.New("postoffice: already received")
)

// DefaultAddr is where post-office serves /mcp (§4.1). The orch daemon dials
// it to tell whether post-office is running (orch --step-up-agent=auto).
const DefaultAddr = "127.0.0.1:7077"

// DefaultDir is the store root for a sessions root: $WORKINGMAN_POST_OFFICE_DIR
// when set, else a "post-office" directory beside the sessions root
// (<sessions-root>/../post-office), the way mcpgateway.StateDir places
// gateways/.
func DefaultDir(sessionsRoot string) string {
	if d := os.Getenv(EnvDir); d != "" {
		return d
	}
	return filepath.Join(filepath.Dir(filepath.Clean(sessionsRoot)), "post-office")
}

// Store is the post-office message store under one root directory. It is
// shared by post-office, the orch daemon and the TUI; see the package
// comment for who may call what. A Store is safe for concurrent use.
type Store struct {
	// Root is the store directory, e.g. ~/.workingman/post-office.
	Root string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Logf, when set, receives one line per anomaly the store tolerates
	// (unreadable records, orphan claims, stale claims).
	Logf func(format string, args ...any)

	// mu serialises this process's inbox writes with its finalisations, so
	// an inbox update can never re-create a record finalisation removed.
	// Only post-office writes inbox/ and finalises, and post-office.lock
	// keeps that to one process.
	mu sync.Mutex
}

// Open returns a Store at root, creating its directories (0700).
func Open(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("postoffice: store root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("postoffice: store root %q: %w", root, err)
	}
	s := &Store{Root: abs}
	for _, d := range []string{
		abs,
		filepath.Join(abs, DirInbox, tmpDir),
		filepath.Join(abs, DirOutbox, tmpDir),
		filepath.Join(abs, DirDone, tmpDir),
		filepath.Join(abs, DirDeliveries),
	} {
		if err := os.MkdirAll(d, dirPerm); err != nil {
			return nil, fmt.Errorf("postoffice: %w", err)
		}
	}
	// MkdirAll leaves an existing root's mode alone; messages may hold
	// anything an agent wrote, so keep them private.
	if err := os.Chmod(abs, dirPerm); err != nil {
		return nil, fmt.Errorf("postoffice: %w", err)
	}
	return s, nil
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Store) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// Dir returns the path of one of the store's subdirectories (DirInbox, …).
func (s *Store) Dir(name string) string { return filepath.Join(s.Root, name) }

// InboxPath is inbox/<id>.json.
func (s *Store) InboxPath(id string) string { return filepath.Join(s.Root, DirInbox, id+".json") }

// OutboxPath is outbox/<id>.json.
func (s *Store) OutboxPath(id string) string { return filepath.Join(s.Root, DirOutbox, id+".json") }

// DeliveryPath is deliveries/<id>.json, owned by the Dispatcher.
func (s *Store) DeliveryPath(id string) string {
	return filepath.Join(s.Root, DirDeliveries, id+".json")
}

// DonePath is done/<day>/<id>.json, where day is the UTC date the id was
// created. Deriving the day from the id (not the finalisation time) makes
// done records findable without a scan, and finalisation idempotent across
// midnight.
func (s *Store) DonePath(id string) (string, error) {
	t, ok := IDTime(id)
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrInvalidID, id)
	}
	return filepath.Join(s.Root, DirDone, t.Format(dayLayout), id+".json"), nil
}

func checkID(id string) error {
	if !ValidID(id) {
		return fmt.Errorf("%w: %q", ErrInvalidID, id)
	}
	return nil
}

// --- inbox (post-office only) -----------------------------------------------

// Put writes a new inbox record. It fails with fs.ErrExist if a record,
// claim or done record with that id already exists. post-office only.
func (s *Store) Put(m Message) error {
	if err := m.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closedLocked(m.ID) {
		return fmt.Errorf("postoffice: put %s: %w", m.ID, fs.ErrExist)
	}
	data, err := marshal(m)
	if err != nil {
		return err
	}
	return s.publish(DirInbox, m.ID, data, true)
}

// MarkReceived sets received_at on an open record (an agent recipient took
// it via next_message) and returns the updated record. It is a
// compare-and-set, so each message is received once: it returns
// ErrAlreadyReceived, with the stored record, when received_at is already
// set, and ErrAlreadyClosed when the message has a claim or is finalised.
// post-office only.
func (s *Store) MarkReceived(id string, at time.Time) (Message, error) {
	return s.update(id, func(m *Message) error {
		if m.ReceivedAt != nil {
			return fmt.Errorf("postoffice: receive %s: %w", id, ErrAlreadyReceived)
		}
		at := at.UTC()
		m.ReceivedAt = &at
		return nil
	})
}

// update rewrites an open record with fn under the store lock. When fn
// fails, nothing is written and the unchanged record is returned with fn's
// error.
func (s *Store) update(id string, fn func(*Message) error) (Message, error) {
	if err := checkID(id); err != nil {
		return Message{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.Get(id)
	if err != nil {
		return Message{}, err
	}
	if s.closedLocked(id) {
		return Message{}, fmt.Errorf("postoffice: update %s: %w", id, ErrAlreadyClosed)
	}
	if err := fn(&m); err != nil {
		return m, err
	}
	data, err := marshal(m)
	if err != nil {
		return Message{}, err
	}
	return m, s.publish(DirInbox, id, data, false)
}

// closedLocked reports whether id has a claim or a done record.
func (s *Store) closedLocked(id string) bool {
	if exists(s.OutboxPath(id)) {
		return true
	}
	p, err := s.DonePath(id)
	return err == nil && exists(p)
}

// --- reading ------------------------------------------------------------------

// Get returns the inbox record for id, or ErrNotFound.
func (s *Store) Get(id string) (Message, error) {
	if err := checkID(id); err != nil {
		return Message{}, err
	}
	var m Message
	if err := readJSON(s.InboxPath(id), &m); err != nil {
		return Message{}, err
	}
	return m, nil
}

// ListOpen returns every inbox record, oldest first (ids sort by creation).
// Messages that already have a claim are included: they leave the list when
// post-office finalises them. Unreadable files are skipped and logged.
func (s *Store) ListOpen() ([]Message, error) {
	ids, err := s.listIDs(DirInbox)
	if err != nil {
		return nil, err
	}
	out := make([]Message, 0, len(ids))
	for _, id := range ids {
		m, err := s.Get(id)
		if errors.Is(err, ErrNotFound) {
			continue // finalised since the listing
		}
		if err != nil {
			s.logf("postoffice: skip inbox %s: %v", id, err)
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// Claimed returns the pending claim for id (outbox/<id>.json), if any.
func (s *Store) Claimed(id string) (Resolution, bool, error) {
	if err := checkID(id); err != nil {
		return Resolution{}, false, err
	}
	var r Resolution
	err := readJSON(s.OutboxPath(id), &r)
	if errors.Is(err, ErrNotFound) {
		return Resolution{}, false, nil
	}
	if err != nil {
		return Resolution{}, false, err
	}
	return r, true, nil
}

// Done returns the finalised record for id, if any.
func (s *Store) Done(id string) (DoneRecord, bool, error) {
	p, err := s.DonePath(id)
	if err != nil {
		return DoneRecord{}, false, err
	}
	var d DoneRecord
	err = readJSON(p, &d)
	if errors.Is(err, ErrNotFound) {
		return DoneRecord{}, false, nil
	}
	if err != nil {
		return DoneRecord{}, false, err
	}
	return d, true, nil
}

// Lookup returns a message and, when it is closed, its resolution: from the
// claim while finalisation is pending, else from done/. It returns
// ErrNotFound when the id is unknown (or pruned).
func (s *Store) Lookup(id string) (Message, *Resolution, error) {
	if err := checkID(id); err != nil {
		return Message{}, nil, err
	}
	// Read in finalisation order (inbox, then done) so a concurrent
	// finalisation is seen in one place or the other.
	m, err := s.Get(id)
	if err == nil {
		if _, _, err := s.Claimed(id); err != nil {
			return Message{}, nil, err
		}
		if r, ok := s.closedBy(id); ok {
			return m, &r, nil
		}
		return m, nil, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Message{}, nil, err
	}
	d, ok, err := s.Done(id)
	if err != nil {
		return Message{}, nil, err
	}
	if !ok {
		return Message{}, nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return d.Message, &d.Resolution, nil
}

// ListDone returns the done records finalised at or after since, oldest
// first by id. Orphans are skipped.
func (s *Store) ListDone(since time.Time) ([]DoneRecord, error) {
	days, err := s.doneDays()
	if err != nil {
		return nil, err
	}
	var out []DoneRecord
	for _, day := range days {
		dir := filepath.Join(s.Root, DirDone, day)
		ids, err := listIDsIn(dir)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			var d DoneRecord
			if err := readJSON(filepath.Join(dir, id+".json"), &d); err != nil {
				if !errors.Is(err, ErrNotFound) {
					s.logf("postoffice: skip done %s: %v", id, err)
				}
				continue
			}
			if d.Orphan || d.Resolution.At.Before(since) {
				continue
			}
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// --- claim (any process) ---------------------------------------------------

// Claim closes a message with r, if nobody else has (§3.2 rule 2). It is the
// only way to close a message, and is safe to call from any process.
//
// The claim is written in full to outbox/.tmp and then link(2)ed to
// outbox/<id>.json, which fails if that name exists, so readers never see a
// partial claim and exactly one claimer wins. On success Claim returns the
// stored resolution (with At, ClaimID, a matched Option and a truncated
// Response filled in). When another claim won, it returns that claim and an
// error wrapping ErrAlreadyClosed.
//
// An answer or dismissal of a message whose expiry has passed is turned
// into an expiry claim, so a reader acting on a stale view converges on
// "expired" rather than answering it.
func (s *Store) Claim(r Resolution) (Resolution, error) {
	if err := checkID(r.ID); err != nil {
		return Resolution{}, err
	}
	m, err := s.Get(r.ID)
	if errors.Is(err, ErrNotFound) {
		// Already finalised, or unknown.
		if w, ok := s.winner(r.ID); ok {
			return w, fmt.Errorf("postoffice: claim %s: %w", r.ID, ErrAlreadyClosed)
		}
		return Resolution{}, fmt.Errorf("%w: %s", ErrNotFound, r.ID)
	}
	if err != nil {
		return Resolution{}, err
	}
	if !m.Kind.Allows(r.Outcome) {
		return Resolution{}, fmt.Errorf("postoffice: a %s cannot be %s", m.Kind, r.Outcome)
	}

	now := s.now()
	expiredInstead := r.Outcome != OutcomeExpired && m.Expired(now)
	if expiredInstead {
		r = Resolution{ID: r.ID, Outcome: OutcomeExpired}
	}
	if r.At.IsZero() {
		r.At = now
	}
	r.At = r.At.UTC()
	r.ClaimID = randomHex(8)
	r.Response = TruncateResponse(r.Response)
	if len(r.Reason) > MaxReason {
		r.Reason = r.Reason[:MaxReason]
	}
	if r.Outcome == OutcomeAnswered && r.Option == "" {
		r.Option = MatchOption(m.Options, r.Response)
	}
	data, err := marshal(r)
	if err != nil {
		return Resolution{}, err
	}

	won, err := s.link(DirOutbox, r.ID, data)
	if err != nil {
		return Resolution{}, err
	}
	if !won {
		w, _ := s.winner(r.ID)
		return w, fmt.Errorf("postoffice: claim %s: %w", r.ID, ErrAlreadyClosed)
	}
	// A claim that lands after an earlier one was finalised (we read the
	// inbox before finalisation removed it, and linked after it removed the
	// outbox) lost. Finalisation writes done/ before removing the outbox,
	// so the done record is visible here. post-office discards such a
	// stale claim too; removing ours early just keeps outbox/ tidy.
	if d, ok, _ := s.Done(r.ID); ok && d.Resolution.ClaimID != r.ClaimID {
		_ = os.Remove(s.OutboxPath(r.ID))
		return d.Resolution, fmt.Errorf("postoffice: claim %s: %w", r.ID, ErrAlreadyClosed)
	}
	if expiredInstead {
		return r, fmt.Errorf("postoffice: claim %s: %w", r.ID, ErrAlreadyClosed)
	}
	return r, nil
}

// winner reads the claim that closed id: the done record's resolution once
// finalised, else the pending outbox claim. A finalisation can move it from
// one to the other between reads, so it retries briefly.
func (s *Store) winner(id string) (Resolution, bool) {
	for range 5 {
		if r, ok := s.closedBy(id); ok {
			return r, true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return Resolution{}, false
}

// closedBy returns the winning claim for id, if it is closed. It reads the
// outbox before done/: a done record, once written, is authoritative, and a
// claim present before done/ is written is the one finalisation will keep.
// (A claim linked after finalisation removed the outbox is stale; done/
// already exists by then, so this never returns one.)
func (s *Store) closedBy(id string) (Resolution, bool) {
	r, haveClaim, err := s.Claimed(id)
	haveClaim = haveClaim && err == nil
	if d, ok, err := s.Done(id); err == nil && ok {
		return d.Resolution, true
	}
	return r, haveClaim
}

// --- finalisation (post-office only) -------------------------------------

// Finalize moves a claimed message to done/ (§3.3):
//
//  1. read the claim and the inbox record;
//  2. write done/<day>/<id>.json, the record plus its resolution;
//  3. remove inbox/<id>.json, then outbox/<id>.json.
//
// Every step is idempotent, so re-running after a crash between any two
// steps converges. A claim with no inbox record is a stale claim (when the
// message is already done) and is removed, or an orphan (when it is not),
// which is written to done/ with Orphan set. It returns the done record and
// ErrNotFound when id has neither a claim nor a done record. post-office
// only; the caller wakes waiters.
func (s *Store) Finalize(id string) (DoneRecord, error) {
	if err := checkID(id); err != nil {
		return DoneRecord{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finalizeLocked(id)
}

func (s *Store) finalizeLocked(id string) (DoneRecord, error) {
	donePath, _ := s.DonePath(id)
	done, haveDone, err := s.Done(id)
	if err != nil {
		return DoneRecord{}, err
	}

	claim, haveClaim, err := s.Claimed(id)
	if err != nil {
		// A claim is published whole, so an unreadable one did not come from
		// Claim. Move it aside so it is not retried forever.
		s.quarantine(s.OutboxPath(id))
		s.logf("postoffice: quarantined unreadable claim %s: %v", id, err)
		if haveDone {
			return done, nil
		}
		return DoneRecord{}, fmt.Errorf("%w: %s (claim unreadable)", ErrNotFound, id)
	}
	if !haveClaim {
		if haveDone {
			// Steps 2 and 3 are complete; make sure of the inbox (a stray
			// rewrite) and return the record.
			if err := removeIfExists(s.InboxPath(id)); err != nil {
				return DoneRecord{}, err
			}
			return done, nil
		}
		return DoneRecord{}, fmt.Errorf("%w: %s (no claim)", ErrNotFound, id)
	}

	// Step 1.
	msg, err := s.Get(id)
	haveInbox := err == nil
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.logf("postoffice: inbox %s unreadable, finalising as orphan: %v", id, err)
	}

	switch {
	case haveDone && done.Resolution.ClaimID != claim.ClaimID:
		// A late claim for a message already finalised by another claim.
		s.logf("postoffice: discard stale claim %s (%s; closed as %s)", id, claim.Outcome, done.Resolution.Outcome)
		if haveInbox {
			// Cannot happen through this package (done is written only
			// after the inbox record exists, and the inbox is not
			// rewritten once closed); converge anyway.
			if err := removeIfExists(s.InboxPath(id)); err != nil {
				return DoneRecord{}, err
			}
		}
		return done, removeIfExists(s.OutboxPath(id))
	case haveDone:
		// Crashed after step 2: the record is already written.
	case haveInbox:
		done = DoneRecord{Message: msg, Resolution: claim}
	default:
		s.logf("postoffice: orphan claim %s (%s), no inbox record", id, claim.Outcome)
		done = DoneRecord{Message: Message{Version: Version, ID: id}, Resolution: claim, Orphan: true}
	}

	// Step 2.
	if !haveDone {
		if err := os.MkdirAll(filepath.Dir(donePath), dirPerm); err != nil {
			return DoneRecord{}, fmt.Errorf("postoffice: %w", err)
		}
		data, err := marshal(done)
		if err != nil {
			return DoneRecord{}, err
		}
		if err := s.writeAtomic(filepath.Join(s.Root, DirDone, tmpDir), donePath, data); err != nil {
			return DoneRecord{}, err
		}
	}
	// Step 3.
	if err := removeIfExists(s.InboxPath(id)); err != nil {
		return DoneRecord{}, err
	}
	if err := removeIfExists(s.OutboxPath(id)); err != nil {
		return DoneRecord{}, err
	}
	return done, nil
}

// Recover finalises every claim left in outbox/ and removes staging files
// older than an hour. post-office runs it on startup and on every outbox
// change (or poll). It returns the records it finalised; a failure on one
// claim does not stop the others, and the first error is returned.
func (s *Store) Recover() ([]DoneRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	entries, err := os.ReadDir(s.Dir(DirOutbox))
	if err != nil {
		return nil, fmt.Errorf("postoffice: %w", err)
	}
	var out []DoneRecord
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		if !ValidID(id) {
			s.quarantine(filepath.Join(s.Dir(DirOutbox), name))
			s.logf("postoffice: quarantined outbox file %s: not a message id", name)
			continue
		}
		d, err := s.finalizeLocked(id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			note(fmt.Errorf("postoffice: finalise %s: %w", id, err))
			continue
		}
		out = append(out, d)
	}
	cutoff := s.now().Add(-staleTmp)
	for _, d := range []string{DirInbox, DirOutbox, DirDone} {
		note(removeOlder(filepath.Join(s.Root, d, tmpDir), cutoff))
	}
	return out, firstErr
}

// Expire claims every open message whose expiry has passed with outcome
// expired, and returns the ids it closed. post-office runs it on a ticker
// and at startup; the claims are then finalised like any other.
func (s *Store) Expire() ([]string, error) {
	msgs, err := s.ListOpen()
	if err != nil {
		return nil, err
	}
	now := s.now()
	var closed []string
	var firstErr error
	for _, m := range msgs {
		if !m.Expired(now) {
			continue
		}
		_, err := s.Claim(Resolution{ID: m.ID, Outcome: OutcomeExpired, At: now})
		switch {
		case err == nil:
			closed = append(closed, m.ID)
		case errors.Is(err, ErrAlreadyClosed), errors.Is(err, ErrNotFound):
		default:
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return closed, firstErr
}

// Prune removes done/ records finalised more than maxAge ago (by file
// modification time, which is the finalisation time) and day directories
// left empty, and returns how many records it removed (§3.4). post-office
// only.
func (s *Store) Prune(maxAge time.Duration) (int, error) {
	days, err := s.doneDays()
	if err != nil {
		return 0, err
	}
	cutoff := s.now().Add(-maxAge)
	n := 0
	for _, day := range days {
		dir := filepath.Join(s.Root, DirDone, day)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		left := 0
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				continue
			}
			if e.IsDir() || !info.ModTime().Before(cutoff) {
				left++
				continue
			}
			if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
				n++
			} else if !errors.Is(err, fs.ErrNotExist) {
				left++
			}
		}
		if left == 0 {
			_ = os.Remove(dir) // fails harmlessly if something was just added
		}
	}
	return n, nil
}

// --- lock ------------------------------------------------------------------

// Lock is the flock on post-office.lock held by the running post-office.
type Lock struct{ f *os.File }

// TryLock takes post-office.lock without blocking, or returns ErrLocked when
// another process holds it. The lock is released by Unlock or when the
// process exits.
func (s *Store) TryLock() (*Lock, error) {
	f, err := os.OpenFile(filepath.Join(s.Root, LockFile), os.O_RDWR|os.O_CREATE, filePerm)
	if err != nil {
		return nil, fmt.Errorf("postoffice: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("postoffice: lock: %w", err)
	}
	_ = f.Truncate(0)
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	return &Lock{f: f}, nil
}

// Unlock releases the lock.
func (l *Lock) Unlock() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := l.f.Close() // closing the descriptor releases the flock
	l.f = nil
	return err
}

// --- file helpers ------------------------------------------------------------

// publish writes data as <dir>/<id>.json via <dir>/.tmp. With exclusive it
// links (failing with fs.ErrExist if the name exists), else it renames.
func (s *Store) publish(dir, id string, data []byte, exclusive bool) error {
	dst := filepath.Join(s.Root, dir, id+".json")
	if exclusive {
		won, err := s.link(dir, id, data)
		if err == nil && !won {
			err = fmt.Errorf("postoffice: %s: %w", dst, fs.ErrExist)
		}
		return err
	}
	return s.writeAtomic(filepath.Join(s.Root, dir, tmpDir), dst, data)
}

// link writes data to <dir>/.tmp and link(2)s it to <dir>/<id>.json. It
// reports false, without error, when the name already exists.
func (s *Store) link(dir, id string, data []byte) (bool, error) {
	tmp, err := writeTemp(filepath.Join(s.Root, dir, tmpDir), id, data)
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp)
	dst := filepath.Join(s.Root, dir, id+".json")
	if err := os.Link(tmp, dst); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return false, fmt.Errorf("postoffice: %w", err)
	}
	syncDir(filepath.Dir(dst))
	return true, nil
}

// writeAtomic writes data to a temp file in tmpDir, then renames it to dst.
func (s *Store) writeAtomic(tmpDir, dst string, data []byte) error {
	tmp, err := writeTemp(tmpDir, filepath.Base(dst), data)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("postoffice: %w", err)
	}
	syncDir(filepath.Dir(dst))
	return nil
}

// writeTemp writes data to a new 0600 file in dir, fsyncs it, and returns its
// path.
func writeTemp(dir, name string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return "", fmt.Errorf("postoffice: %w", err)
	}
	f, err := os.CreateTemp(dir, name+".*")
	if err != nil {
		return "", fmt.Errorf("postoffice: %w", err)
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("postoffice: write %s: %w", f.Name(), err)
	}
	return f.Name(), nil
}

// syncDir fsyncs a directory so a rename or link in it is durable. Best
// effort: not every filesystem supports it.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

// quarantine moves a bad file into <its dir>/.bad so it is not retried.
func (s *Store) quarantine(path string) {
	dir := filepath.Join(filepath.Dir(path), badDir)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return
	}
	_ = os.Rename(path, filepath.Join(dir, filepath.Base(path)+"."+randomHex(4)))
}

func marshal(v any) ([]byte, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("postoffice: %w", err)
	}
	return append(data, '\n'), nil
}

// readJSON decodes path into v; a missing file is ErrNotFound.
func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s", ErrNotFound, filepath.Base(path))
	}
	if err != nil {
		return fmt.Errorf("postoffice: %w", err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("postoffice: decode %s: %w", path, err)
	}
	return nil
}

func (s *Store) listIDs(dir string) ([]string, error) {
	return listIDsIn(filepath.Join(s.Root, dir))
}

// listIDsIn returns the message ids of the <id>.json files in dir, sorted.
func listIDsIn(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("postoffice: %w", err)
	}
	var ids []string
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if ok && !e.IsDir() && ValidID(id) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// doneDays returns the day directories under done/, oldest first.
func (s *Store) doneDays() ([]string, error) {
	entries, err := os.ReadDir(s.Dir(DirDone))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("postoffice: %w", err)
	}
	var days []string
	for _, e := range entries {
		if _, err := time.Parse(dayLayout, e.Name()); e.IsDir() && err == nil {
			days = append(days, e.Name())
		}
	}
	sort.Strings(days)
	return days, nil
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("postoffice: %w", err)
	}
	return nil
}

// removeOlder removes the regular files in dir modified before cutoff.
func removeOlder(dir string, cutoff time.Time) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("postoffice: %w", err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || e.IsDir() || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := removeIfExists(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("postoffice: crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}
