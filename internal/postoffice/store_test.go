package postoffice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// clock is a settable test clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Now().UTC()}
	s.Now = c.Now
	s.Logf = t.Logf
	return s, c
}

var sender = Sender{Agent: "task", Sandbox: "wm-p-fix-ci", Project: "/o/p/project.yaml", Task: "fix-ci"}

func putRequest(t *testing.T, s *Store, opts ...string) Message {
	t.Helper()
	m, err := NewMessage(Draft{Kind: KindRequest, From: sender, Body: "Which bucket?", Options: opts}, s.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(m); err != nil {
		t.Fatal(err)
	}
	return m
}

func putNotification(t *testing.T, s *Store) Message {
	t.Helper()
	m, err := NewMessage(Draft{Kind: KindNotification, From: sender, Subject: "FYI", Body: "done"}, s.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(m); err != nil {
		t.Fatal(err)
	}
	return m
}

func answer(id, text, via string) Resolution {
	return Resolution{ID: id, Outcome: OutcomeAnswered, Response: text, Responder: &Responder{Kind: ResponderOperator, Via: via}}
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s exists (err=%v)", path, err)
	}
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func TestOpenLayoutAndPermissions(t *testing.T) {
	s, _ := newStore(t)
	for _, d := range []string{"", DirInbox, DirOutbox, DirDone, DirDeliveries, DirInbox + "/.tmp", DirOutbox + "/.tmp"} {
		fi, err := os.Stat(filepath.Join(s.Root, d))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != dirPerm {
			t.Errorf("%s: mode %v", d, fi.Mode().Perm())
		}
	}
	m := putRequest(t, s)
	fi, err := os.Stat(s.InboxPath(m.ID))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != filePerm {
		t.Errorf("inbox file mode %v", fi.Mode().Perm())
	}
	if _, err := Open(""); err == nil {
		t.Error("empty root accepted")
	}
}

func TestDefaultDir(t *testing.T) {
	t.Setenv(EnvDir, "")
	if got := DefaultDir("/home/u/.workingman/sessions/"); got != "/home/u/.workingman/post-office" {
		t.Fatalf("DefaultDir = %q", got)
	}
	t.Setenv(EnvDir, "/tmp/po")
	if got := DefaultDir("/home/u/.workingman/sessions"); got != "/tmp/po" {
		t.Fatalf("DefaultDir with env = %q", got)
	}
}

func TestPutGetListOpen(t *testing.T) {
	s, c := newStore(t)
	a := putRequest(t, s, "staging", "prod")
	c.Add(time.Millisecond)
	b := putNotification(t, s)
	if err := s.Put(a); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("duplicate Put: %v", err)
	}
	got, err := s.Get(a.ID)
	if err != nil || got.Body != a.Body || len(got.Options) != 2 {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if _, err := s.Get(NewID(c.Now())); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get unknown: %v", err)
	}
	if _, err := s.Get("../../etc/passwd"); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("Get bad id: %v", err)
	}
	// Garbage and staging files are skipped.
	os.WriteFile(filepath.Join(s.Dir(DirInbox), "notes.txt"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(s.Dir(DirInbox), NewID(c.Now().Add(time.Hour))+".json"), []byte("{not json"), 0o600)
	list, err := s.ListOpen()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != a.ID || list[1].ID != b.ID {
		t.Fatalf("ListOpen = %v", ids(list))
	}
	tmps, _ := os.ReadDir(filepath.Join(s.Dir(DirInbox), tmpDir))
	if len(tmps) != 0 {
		t.Fatalf("staging files left: %v", tmps)
	}
}

func ids(ms []Message) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

func TestClaimAnswerAndFinalize(t *testing.T) {
	s, c := newStore(t)
	m := putRequest(t, s, "staging", "prod")

	r, err := s.Claim(answer(m.ID, " Staging ", ViaTUI))
	if err != nil {
		t.Fatal(err)
	}
	if r.Option != "staging" || r.Response != " Staging " || r.At.IsZero() || r.ClaimID == "" {
		t.Fatalf("claim = %+v", r)
	}
	// Claimed but not yet finalised: still listed, Lookup shows the claim.
	if list, _ := s.ListOpen(); len(list) != 1 {
		t.Fatalf("open after claim: %d", len(list))
	}
	if _, res, err := s.Lookup(m.ID); err != nil || res == nil || res.ClaimID != r.ClaimID {
		t.Fatalf("Lookup pending = %+v, %v", res, err)
	}

	// Second claimer loses and sees the winner.
	w, err := s.Claim(Resolution{ID: m.ID, Outcome: OutcomeCancelled, Reason: "never mind"})
	if !errors.Is(err, ErrAlreadyClosed) || w.ClaimID != r.ClaimID || w.Via() != ViaTUI {
		t.Fatalf("losing claim = %+v, %v", w, err)
	}

	c.Add(time.Minute)
	d, err := s.Finalize(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != m.ID || d.Subject != m.Subject || d.Resolution.ClaimID != r.ClaimID || d.Orphan {
		t.Fatalf("done = %+v", d)
	}
	mustNotExist(t, s.InboxPath(m.ID))
	mustNotExist(t, s.OutboxPath(m.ID))
	p, _ := s.DonePath(m.ID)
	mustExist(t, p)
	if !strings.Contains(p, m.CreatedAt.Format(dayLayout)) {
		t.Fatalf("done path %s not under the creation day", p)
	}

	// After finalisation: losers still see the winner, Lookup reads done/.
	w, err = s.Claim(answer(m.ID, "prod", ViaSlack))
	if !errors.Is(err, ErrAlreadyClosed) || w.ClaimID != r.ClaimID {
		t.Fatalf("claim after finalise = %+v, %v", w, err)
	}
	mustNotExist(t, s.OutboxPath(m.ID))
	got, res, err := s.Lookup(m.ID)
	if err != nil || res == nil || res.Option != "staging" || got.Body != m.Body {
		t.Fatalf("Lookup done = %+v %+v %v", got, res, err)
	}
	// Finalize is idempotent.
	if d2, err := s.Finalize(m.ID); err != nil || d2.Resolution.ClaimID != r.ClaimID {
		t.Fatalf("re-Finalize = %+v, %v", d2, err)
	}
	// The done record is the inbox record plus "resolution".
	data, _ := os.ReadFile(p)
	var raw map[string]any
	json.Unmarshal(data, &raw)
	if raw["id"] != m.ID || raw["resolution"].(map[string]any)["outcome"] != "answered" {
		t.Fatalf("done json = %s", data)
	}
}

func TestClaimRules(t *testing.T) {
	s, c := newStore(t)
	req := putRequest(t, s)
	note := putNotification(t, s)
	if _, err := s.Claim(Resolution{ID: req.ID, Outcome: OutcomeDismissed}); err == nil || errors.Is(err, ErrAlreadyClosed) {
		t.Fatalf("dismiss request: %v", err)
	}
	if _, err := s.Claim(answer(note.ID, "ok", ViaTUI)); err == nil {
		t.Fatal("answer notification accepted")
	}
	if _, err := s.Claim(Resolution{ID: NewID(c.Now()), Outcome: OutcomeAnswered}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	if _, err := s.Claim(Resolution{ID: "msg_bad", Outcome: OutcomeAnswered}); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("bad id: %v", err)
	}
	if _, err := s.Claim(Resolution{ID: note.ID, Outcome: OutcomeDismissed}); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	// Long answers and reasons are capped; an explicit option is kept.
	r, err := s.Claim(Resolution{ID: req.ID, Outcome: OutcomeAnswered, Response: strings.Repeat("x", 2*MaxResponse), Option: "given"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Response) > MaxResponse || r.Option != "given" {
		t.Fatalf("claim = len %d option %q", len(r.Response), r.Option)
	}
	stored, _, _ := s.Claimed(req.ID)
	if stored.Response != r.Response {
		t.Fatal("stored claim differs from returned")
	}
}

func TestClaimAfterExpiryBecomesExpired(t *testing.T) {
	s, c := newStore(t)
	m := putRequest(t, s)
	c.Add(DefaultRequestExpiry + time.Second)
	r, err := s.Claim(answer(m.ID, "late", ViaSlack))
	if !errors.Is(err, ErrAlreadyClosed) || r.Outcome != OutcomeExpired {
		t.Fatalf("late answer = %+v, %v", r, err)
	}
	got, _, _ := s.Claimed(m.ID)
	if got.Outcome != OutcomeExpired || got.Response != "" {
		t.Fatalf("stored = %+v", got)
	}
}

func TestExpire(t *testing.T) {
	s, c := newStore(t)
	req := putRequest(t, s)
	note := putNotification(t, s)
	claimed := putRequest(t, s)
	if _, err := s.Claim(answer(claimed.ID, "x", ViaTUI)); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Expire(); err != nil || len(got) != 0 {
		t.Fatalf("early Expire = %v, %v", got, err)
	}
	c.Add(DefaultRequestExpiry)
	got, err := s.Expire()
	if err != nil || len(got) != 1 || got[0] != req.ID {
		t.Fatalf("Expire = %v, %v", got, err)
	}
	c.Add(DefaultNotificationExpiry)
	got, _ = s.Expire()
	if len(got) != 1 || got[0] != note.ID {
		t.Fatalf("Expire notifications = %v", got)
	}
	done, err := s.Recover()
	if err != nil || len(done) != 3 {
		t.Fatalf("Recover = %d, %v", len(done), err)
	}
	if list, _ := s.ListOpen(); len(list) != 0 {
		t.Fatalf("open after expiry: %v", ids(list))
	}
	_, res, _ := s.Lookup(claimed.ID)
	if res.Outcome != OutcomeAnswered {
		t.Fatalf("claimed message expired instead: %+v", res)
	}
}

func TestMarkReceived(t *testing.T) {
	s, c := newStore(t)
	m := putRequest(t, s)
	at := c.Now().Add(time.Second)
	got, err := s.MarkReceived(m.ID, at)
	if err != nil || got.ReceivedAt == nil || !got.ReceivedAt.Equal(at) {
		t.Fatalf("MarkReceived = %+v, %v", got, err)
	}
	if again, _ := s.Get(m.ID); again.ReceivedAt == nil {
		t.Fatal("received_at not persisted")
	}
	if _, err := s.Claim(answer(m.ID, "x", ViaStepUp)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkReceived(m.ID, at); !errors.Is(err, ErrAlreadyClosed) {
		t.Fatalf("MarkReceived after claim: %v", err)
	}
	s.Finalize(m.ID)
	if _, err := s.MarkReceived(m.ID, at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("MarkReceived after finalise: %v", err)
	}
	mustNotExist(t, s.InboxPath(m.ID))
	if err := s.Put(m); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("Put of a finalised id: %v", err)
	}
}

// TestConcurrentClaimsGoroutines races many claimers on each message and
// checks exactly one wins and every loser sees the winner.
func TestConcurrentClaimsGoroutines(t *testing.T) {
	s, _ := newStore(t)
	const messages, claimers = 20, 16
	for range messages {
		m := putRequest(t, s)
		var wg sync.WaitGroup
		var mu sync.Mutex
		var winners []Resolution
		var losers []Resolution
		start := make(chan struct{})
		for i := range claimers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				var r Resolution
				if i%4 == 3 {
					r = Resolution{ID: m.ID, Outcome: OutcomeCancelled, Reason: strconv.Itoa(i)}
				} else {
					r = answer(m.ID, strconv.Itoa(i), ViaTUI)
				}
				// Finalisation runs concurrently with the claims.
				if i == claimers/2 {
					go s.Recover()
				}
				got, err := s.Claim(r)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					winners = append(winners, got)
				case errors.Is(err, ErrAlreadyClosed):
					losers = append(losers, got)
				default:
					t.Errorf("claim %d: %v", i, err)
				}
			}()
		}
		close(start)
		wg.Wait()
		if len(winners) != 1 || len(losers) != claimers-1 {
			t.Fatalf("%s: %d winners, %d losers", m.ID, len(winners), len(losers))
		}
		for _, l := range losers {
			if l.ClaimID != winners[0].ClaimID {
				t.Fatalf("loser saw %+v, winner %+v", l, winners[0])
			}
		}
		if _, err := s.Recover(); err != nil {
			t.Fatal(err)
		}
		d, ok, err := s.Done(m.ID)
		if err != nil || !ok || d.Resolution.ClaimID != winners[0].ClaimID {
			t.Fatalf("done = %+v %v %v", d.Resolution, ok, err)
		}
	}
	if entries, _ := os.ReadDir(s.Dir(DirOutbox)); len(entries) != 1 { // only .tmp
		t.Fatalf("outbox not empty: %v", entries)
	}
	tmps, _ := os.ReadDir(filepath.Join(s.Dir(DirOutbox), tmpDir))
	if len(tmps) != 0 {
		t.Fatalf("outbox staging left: %d", len(tmps))
	}
}

// Subprocess claimer: re-executes the test binary (see TestMain).
const (
	envChildRoot = "POSTOFFICE_TEST_CLAIM_ROOT"
	envChildID   = "POSTOFFICE_TEST_CLAIM_ID"
	envChildTag  = "POSTOFFICE_TEST_CLAIM_TAG"
	envChildGo   = "POSTOFFICE_TEST_CLAIM_GO"
)

func TestMain(m *testing.M) {
	if root := os.Getenv(envChildRoot); root != "" {
		os.Exit(childClaim(root))
	}
	os.Exit(m.Run())
}

// childClaim waits for the go file, then claims every id with its tag and
// prints one "won <id> <claim>" or "lost <id> <winner claim>" line per id.
func childClaim(root string) int {
	s := &Store{Root: root}
	for !exists(os.Getenv(envChildGo)) {
		time.Sleep(time.Millisecond)
	}
	for id := range strings.SplitSeq(os.Getenv(envChildID), ",") {
		r, err := s.Claim(answer(id, os.Getenv(envChildTag), ViaSlack))
		switch {
		case err == nil:
			fmt.Printf("won %s %s\n", id, r.ClaimID)
		case errors.Is(err, ErrAlreadyClosed):
			fmt.Printf("lost %s %s\n", id, r.ClaimID)
		default:
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	return 0
}

func TestConcurrentClaimsProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	s, _ := newStore(t)
	s.Now = nil // children use the real clock
	var msgIDs []string
	for range 10 {
		msgIDs = append(msgIDs, putRequest(t, s).ID)
	}
	goFile := filepath.Join(t.TempDir(), "go")
	const procs = 6
	type result struct {
		out []byte
		err error
	}
	results := make([]chan result, procs)
	for i := range procs {
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		cmd.Env = append(os.Environ(),
			envChildRoot+"="+s.Root,
			envChildID+"="+strings.Join(msgIDs, ","),
			envChildTag+"=proc"+strconv.Itoa(i),
			envChildGo+"="+goFile,
		)
		cmd.Stderr = os.Stderr
		ch := make(chan result, 1)
		results[i] = ch
		go func() {
			out, err := cmd.Output()
			ch <- result{out, err}
		}()
	}
	time.Sleep(200 * time.Millisecond) // let the children start and spin
	if err := os.WriteFile(goFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	won := map[string][]string{}  // id -> winning claim ids
	seen := map[string][]string{} // id -> claim ids losers saw
	for i := range procs {
		res := <-results[i]
		if res.err != nil {
			t.Fatalf("child %d: %v", i, res.err)
		}
		for line := range strings.SplitSeq(strings.TrimSpace(string(res.out)), "\n") {
			f := strings.Fields(line)
			if len(f) != 3 {
				t.Fatalf("child %d: bad line %q", i, line)
			}
			if f[0] == "won" {
				won[f[1]] = append(won[f[1]], f[2])
			} else {
				seen[f[1]] = append(seen[f[1]], f[2])
			}
		}
	}
	for _, id := range msgIDs {
		if len(won[id]) != 1 {
			t.Fatalf("%s: %d winners", id, len(won[id]))
		}
		if len(seen[id]) != procs-1 {
			t.Fatalf("%s: %d losers", id, len(seen[id]))
		}
		for _, c := range seen[id] {
			if c != won[id][0] {
				t.Fatalf("%s: loser saw %s, winner %s", id, c, won[id][0])
			}
		}
		r, ok, err := s.Claimed(id)
		if err != nil || !ok || r.ClaimID != won[id][0] {
			t.Fatalf("%s: stored claim %+v %v %v", id, r, ok, err)
		}
	}
}

// finalizeSteps are the points a finalisation can crash between (§3.3):
// each leaves the files the way a crash after that step would.
var finalizeSteps = []struct {
	name   string
	inbox  bool // inbox/<id>.json still present
	done   bool // done record written
	outbox bool // outbox claim still present
}{
	{"claimed, nothing finalised", true, false, true},
	{"crash after done written", true, true, true},
	{"crash after inbox removed", false, true, true},
	{"fully finalised, no outbox", false, true, false},
}

func TestRecoverAfterCrashBetweenSteps(t *testing.T) {
	for _, step := range finalizeSteps {
		t.Run(step.name, func(t *testing.T) {
			s, _ := newStore(t)
			m := putRequest(t, s, "staging")
			claim, err := s.Claim(answer(m.ID, "1", ViaTUI))
			if err != nil {
				t.Fatal(err)
			}
			donePath, _ := s.DonePath(m.ID)
			if step.done {
				os.MkdirAll(filepath.Dir(donePath), dirPerm)
				data, _ := marshal(DoneRecord{Message: m, Resolution: claim})
				os.WriteFile(donePath, data, filePerm)
			}
			if !step.inbox {
				os.Remove(s.InboxPath(m.ID))
			}
			if !step.outbox {
				os.Remove(s.OutboxPath(m.ID))
			}
			// A losing claimer during the crash window still sees the winner.
			if w, err := s.Claim(answer(m.ID, "other", ViaSlack)); !errors.Is(err, ErrAlreadyClosed) || w.ClaimID != claim.ClaimID {
				t.Fatalf("racing claim = %+v, %v", w, err)
			}

			// "Restart": a fresh Store over the same root.
			s2 := &Store{Root: s.Root, Logf: t.Logf}
			if _, err := s2.Recover(); err != nil {
				t.Fatal(err)
			}
			mustNotExist(t, s.InboxPath(m.ID))
			mustNotExist(t, s.OutboxPath(m.ID))
			d, ok, err := s2.Done(m.ID)
			if err != nil || !ok || d.Resolution.ClaimID != claim.ClaimID || d.Resolution.Option != "staging" || d.Subject != m.Subject {
				t.Fatalf("done = %+v %v %v", d, ok, err)
			}
			// And again: recovery is idempotent.
			if got, err := s2.Recover(); err != nil || len(got) != 0 {
				t.Fatalf("second Recover = %d, %v", len(got), err)
			}
			if _, err := s2.Finalize(m.ID); err != nil {
				t.Fatalf("Finalize after recovery: %v", err)
			}
		})
	}
}

func TestRecoverCrashMidWrite(t *testing.T) {
	s, c := newStore(t)
	m := putRequest(t, s)
	if _, err := s.Claim(answer(m.ID, "x", ViaTUI)); err != nil {
		t.Fatal(err)
	}
	// A crash mid-write leaves only staging files, never a partial record.
	old := c.Now().Add(-2 * staleTmp)
	var tmps []string
	for _, d := range []string{DirInbox, DirOutbox, DirDone} {
		p := filepath.Join(s.Root, d, tmpDir, m.ID+".json.123")
		os.WriteFile(p, []byte(`{"id":`), filePerm)
		os.Chtimes(p, old, old)
		tmps = append(tmps, p)
	}
	fresh := filepath.Join(s.Root, DirOutbox, tmpDir, "fresh.json.1")
	os.WriteFile(fresh, nil, filePerm)
	if _, err := s.Recover(); err != nil {
		t.Fatal(err)
	}
	for _, p := range tmps {
		mustNotExist(t, p)
	}
	mustExist(t, fresh) // may belong to a claim in flight
	if _, ok, _ := s.Done(m.ID); !ok {
		t.Fatal("not finalised")
	}
}

func TestRecoverOrphanAndStaleClaims(t *testing.T) {
	s, c := newStore(t)

	// Orphan: a claim with no inbox record and no done record.
	orphanID := NewID(c.Now())
	data, _ := marshal(Resolution{ID: orphanID, Outcome: OutcomeAnswered, Response: "?", ClaimID: "abc"})
	os.WriteFile(s.OutboxPath(orphanID), data, filePerm)

	// Stale: a late claim for a message finalised by another claim.
	m := putRequest(t, s)
	first, _ := s.Claim(answer(m.ID, "first", ViaTUI))
	s.Finalize(m.ID)
	late, _ := marshal(Resolution{ID: m.ID, Outcome: OutcomeCancelled, ClaimID: "late"})
	os.WriteFile(s.OutboxPath(m.ID), late, filePerm)

	// Garbage in outbox/.
	os.WriteFile(filepath.Join(s.Dir(DirOutbox), "junk.json"), []byte("{}"), filePerm)
	unreadable := NewID(c.Now())
	os.WriteFile(s.OutboxPath(unreadable), []byte("{trunc"), filePerm)

	if _, err := s.Recover(); err != nil {
		t.Fatal(err)
	}
	d, ok, _ := s.Done(orphanID)
	if !ok || !d.Orphan || d.Resolution.Response != "?" {
		t.Fatalf("orphan = %+v %v", d, ok)
	}
	d, _, _ = s.Done(m.ID)
	if d.Resolution.ClaimID != first.ClaimID {
		t.Fatalf("stale claim replaced the winner: %+v", d.Resolution)
	}
	for _, p := range []string{s.OutboxPath(orphanID), s.OutboxPath(m.ID), s.OutboxPath(unreadable), filepath.Join(s.Dir(DirOutbox), "junk.json")} {
		mustNotExist(t, p)
	}
	bad, _ := os.ReadDir(filepath.Join(s.Dir(DirOutbox), badDir))
	if len(bad) != 2 {
		t.Fatalf("quarantined %d files, want 2", len(bad))
	}
	// Orphans are not listed as history.
	list, _ := s.ListDone(time.Time{})
	if len(list) != 1 || list[0].ID != m.ID {
		t.Fatalf("ListDone = %+v", list)
	}
}

// TestClaimRacesFinalize checks the window where a claimer read the inbox
// record before finalisation removed it and links after finalisation
// removed the outbox: it must lose to the finalised claim.
func TestClaimRacesFinalize(t *testing.T) {
	s, _ := newStore(t)
	m := putRequest(t, s)
	first, _ := s.Claim(answer(m.ID, "first", ViaTUI))
	if _, err := s.Finalize(m.ID); err != nil {
		t.Fatal(err)
	}
	// Simulate the late link directly: put the inbox record back (as the
	// claimer saw it), claim, then check.
	data, _ := marshal(m)
	os.WriteFile(s.InboxPath(m.ID), data, filePerm)
	w, err := s.Claim(answer(m.ID, "late", ViaSlack))
	if !errors.Is(err, ErrAlreadyClosed) || w.ClaimID != first.ClaimID {
		t.Fatalf("late claim = %+v, %v", w, err)
	}
	mustNotExist(t, s.OutboxPath(m.ID))
	if _, err := s.Recover(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Finalize(m.ID); err != nil {
		t.Fatal(err)
	}
	mustNotExist(t, s.InboxPath(m.ID))
}

func TestListDoneAndPrune(t *testing.T) {
	s, c := newStore(t)
	old := putRequest(t, s)
	s.Claim(answer(old.ID, "a", ViaTUI))
	s.Finalize(old.ID)
	oldPath, _ := s.DonePath(old.ID)
	past := c.Now().Add(-DefaultRetention - time.Hour)
	os.Chtimes(oldPath, past, past)

	c.Add(48 * time.Hour) // a different day directory
	recent := putRequest(t, s)
	s.Claim(answer(recent.ID, "b", ViaTUI))
	s.Finalize(recent.ID)

	list, err := s.ListDone(c.Now().Add(-time.Hour))
	if err != nil || len(list) != 1 || list[0].ID != recent.ID {
		t.Fatalf("ListDone(recent) = %v, %v", list, err)
	}
	if all, _ := s.ListDone(time.Time{}); len(all) != 2 {
		t.Fatalf("ListDone(all) = %d", len(all))
	}

	n, err := s.Prune(DefaultRetention)
	if err != nil || n != 1 {
		t.Fatalf("Prune = %d, %v", n, err)
	}
	mustNotExist(t, oldPath)
	mustNotExist(t, filepath.Dir(oldPath)) // empty day directory removed
	mustExist(t, filepath.Join(s.Dir(DirDone), tmpDir))
	if _, _, err := s.Lookup(old.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Lookup pruned: %v", err)
	}
	if _, _, err := s.Lookup(recent.ID); err != nil {
		t.Fatalf("Lookup recent: %v", err)
	}
}

func TestTryLock(t *testing.T) {
	s, _ := newStore(t)
	l, err := s.TryLock()
	if err != nil {
		t.Fatal(err)
	}
	// flock is per open file description, so a second open in this process
	// conflicts just as another process would.
	if _, err := s.TryLock(); !errors.Is(err, ErrLocked) {
		t.Fatalf("second TryLock: %v", err)
	}
	if err := l.Unlock(); err != nil {
		t.Fatal(err)
	}
	l2, err := s.TryLock()
	if err != nil {
		t.Fatalf("TryLock after Unlock: %v", err)
	}
	l2.Unlock()
}

func TestWatch(t *testing.T) {
	s, _ := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	ch := s.Watch(ctx, 50*time.Millisecond, DirInbox, DirOutbox, DirDone)

	expect := func(want string) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case d := <-ch:
				if d == want {
					return
				}
			case <-deadline:
				t.Fatalf("no %s change", want)
			}
		}
	}
	m := putRequest(t, s)
	expect(DirInbox)
	if _, err := s.Claim(answer(m.ID, "x", ViaTUI)); err != nil {
		t.Fatal(err)
	}
	expect(DirOutbox)
	if _, err := s.Finalize(m.ID); err != nil {
		t.Fatal(err)
	}
	expect(DirDone) // a new day directory, then the file in it
	// A second message on the same day lands in an already-watched day dir.
	m2 := putRequest(t, s)
	s.Claim(answer(m2.ID, "x", ViaTUI))
	drain(ch)
	s.Finalize(m2.ID)
	expect(DirDone)

	cancel()
	for range ch {
	}
}

func drain(ch <-chan string) {
	for {
		select {
		case <-ch:
		case <-time.After(100 * time.Millisecond):
			return
		}
	}
}

func TestWatchPollFallback(t *testing.T) {
	s, _ := newStore(t)
	w := &watcher{store: s, dirs: []string{DirInbox, DirDone}, out: make(chan string, 2), pending: map[string]bool{}, snaps: map[string]string{}}
	for _, d := range w.dirs {
		w.snaps[d] = w.snapshot(d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.run(ctx, 20*time.Millisecond) // fsw is nil: polling only
	putRequest(t, s)
	select {
	case d := <-w.out:
		if d != DirInbox {
			t.Fatalf("got %s", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("poll saw no change")
	}
	// Staging writes are not changes.
	os.WriteFile(filepath.Join(s.Dir(DirInbox), tmpDir, "x"), nil, filePerm)
	select {
	case d := <-w.out:
		t.Fatalf("staging write reported as %s", d)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestWaiters(t *testing.T) {
	var w Waiters
	a := w.Wait("a")
	a2 := w.Wait("a")
	b := w.Wait("b")
	w.Wake("a")
	for _, ch := range []<-chan struct{}{a, a2} {
		select {
		case <-ch:
		default:
			t.Fatal("a not woken")
		}
	}
	select {
	case <-b:
		t.Fatal("b woken")
	default:
	}
	w.Wake("missing")
	w.WakeAll()
	select {
	case <-b:
	default:
		t.Fatal("WakeAll missed b")
	}
}
