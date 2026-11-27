package postoffice

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Version is the inbox record format written by this package.
const Version = 1

// Kind is what a message expects of its recipient.
type Kind string

const (
	// KindNotification expects no reply.
	KindNotification Kind = "notification"
	// KindRequest expects a reply; the sender may wait, poll or give up.
	KindRequest Kind = "request"
)

// Outcome is the terminal state a claim moves a message to.
type Outcome string

const (
	OutcomeAnswered  Outcome = "answered"  // requests: a recipient replied
	OutcomeCancelled Outcome = "cancelled" // requests: the sender gave up
	OutcomeExpired   Outcome = "expired"   // both: expires_at passed
	OutcomeDismissed Outcome = "dismissed" // notifications: the operator dismissed it
)

// Allows reports whether outcome is a valid terminal state for kind (§2.1).
func (k Kind) Allows(o Outcome) bool {
	switch k {
	case KindRequest:
		return o == OutcomeAnswered || o == OutcomeCancelled || o == OutcomeExpired
	case KindNotification:
		return o == OutcomeDismissed || o == OutcomeExpired
	}
	return false
}

// RecipientOperator is the normalised recipient for the human operator.
const RecipientOperator = "operator"

// Responder kinds and channels ("via").
const (
	ResponderOperator = "operator"
	ResponderAgent    = "agent"

	ViaTUI      = "tui"
	ViaGUI      = "gui" // the browser GUI, through the daemon's API
	ViaSlack    = "slack"
	ViaSignal   = "signal"
	ViaWhatsApp = "whatsapp"
	ViaStepUp   = "step-up"
)

// Limits and defaults from §2.5 and §4.4.
const (
	MaxSubject    = 200
	MaxBody       = 16000
	MaxOptions    = 10
	MaxOption     = 100
	MaxReason     = 500
	MaxResponse   = 16 << 10 // answers longer than this are truncated
	truncatedMark = "\n…[truncated]"

	DefaultRequestExpiry      = 24 * time.Hour
	DefaultNotificationExpiry = 7 * 24 * time.Hour
	MinExpiry                 = time.Minute
	MaxExpiry                 = 7 * 24 * time.Hour
)

// Sender identifies who sent a message (§2.3). post-office fills it from the
// caller's gateway; it is never taken from tool arguments.
type Sender struct {
	Agent     string `json:"agent"`             // agent.Kind string: task, wolf, workingman, step-up, …
	Sandbox   string `json:"sandbox"`           // sbx sandbox name
	Project   string `json:"project,omitempty"` // project.yaml path
	Task      string `json:"task,omitempty"`    // empty for agents without a task
	SessionID string `json:"session_id,omitempty"`
}

// Display renders the sender for humans: "task fix-ci (workingman-messaging)",
// "step-up", or "workingman agent".
func (s Sender) Display() string {
	project := ""
	if s.Project != "" {
		project = filepath.Base(filepath.Dir(s.Project))
	}
	agent := s.Agent
	if agent == "" {
		agent = s.Sandbox
	}
	switch {
	case s.Task != "" && project != "":
		return fmt.Sprintf("%s %s (%s)", agent, s.Task, project)
	case s.Task != "":
		return agent + " " + s.Task
	case agent == "workingman":
		return "workingman agent"
	case project != "":
		return fmt.Sprintf("%s (%s)", agent, project)
	}
	return agent
}

// Same reports whether two senders are the same caller for ownership checks:
// the same sandbox, and the same task when either names one (§2.6).
func (s Sender) Same(o Sender) bool {
	return s.Sandbox != "" && s.Sandbox == o.Sandbox && s.Task == o.Task
}

// Message is the inbox record, inbox/<id>.json (§2.9). Only post-office
// writes it.
type Message struct {
	Version    int        `json:"version"`
	ID         string     `json:"id"`
	Kind       Kind       `json:"kind"`
	From       Sender     `json:"from"`
	Recipient  string     `json:"recipient"`
	Subject    string     `json:"subject"`
	Body       string     `json:"body"`
	Options    []string   `json:"options,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	ReceivedAt *time.Time `json:"received_at"`
	InReplyTo  string     `json:"in_reply_to,omitempty"`
}

// Expired reports whether the message's expiry has passed at now. Readers
// treat such a message as expired even before post-office claims it (§2.5).
func (m Message) Expired(now time.Time) bool {
	return !m.ExpiresAt.IsZero() && !now.Before(m.ExpiresAt)
}

// ToOperator reports whether the message is addressed to the human operator.
func (m Message) ToOperator() bool { return m.Recipient == RecipientOperator }

// ShortID is the human-facing short form of the message id (§2.2).
func (m Message) ShortID() string { return ShortID(m.ID) }

// Validate checks a record before it is written.
func (m Message) Validate() error {
	switch {
	case m.Version != Version:
		return fmt.Errorf("postoffice: unsupported version %d", m.Version)
	case !ValidID(m.ID):
		return fmt.Errorf("%w: %q", ErrInvalidID, m.ID)
	case m.Kind != KindRequest && m.Kind != KindNotification:
		return fmt.Errorf("postoffice: unknown kind %q", m.Kind)
	case m.Recipient == "":
		return errors.New("postoffice: recipient is required")
	case m.From.Sandbox == "":
		return errors.New("postoffice: sender sandbox is required")
	case strings.TrimSpace(m.Body) == "":
		return errors.New("postoffice: body is required")
	case len(m.Body) > MaxBody:
		return fmt.Errorf("postoffice: body is longer than %d bytes", MaxBody)
	case strings.TrimSpace(m.Subject) == "":
		return errors.New("postoffice: subject is required")
	case utf8.RuneCountInString(m.Subject) > MaxSubject:
		return fmt.Errorf("postoffice: subject is longer than %d characters", MaxSubject)
	case len(m.Options) > MaxOptions:
		return fmt.Errorf("postoffice: more than %d options", MaxOptions)
	case m.Kind == KindNotification && len(m.Options) > 0:
		return errors.New("postoffice: notifications take no options")
	case m.CreatedAt.IsZero() || m.ExpiresAt.IsZero():
		return errors.New("postoffice: created_at and expires_at are required")
	case !m.ExpiresAt.After(m.CreatedAt):
		return errors.New("postoffice: expires_at must be after created_at")
	}
	for i, o := range m.Options {
		if strings.TrimSpace(o) == "" {
			return fmt.Errorf("postoffice: option %d is empty", i+1)
		}
		if utf8.RuneCountInString(o) > MaxOption {
			return fmt.Errorf("postoffice: option %d is longer than %d characters", i+1, MaxOption)
		}
	}
	return nil
}

// Draft is what a sender supplies for a new message; NewMessage turns it
// into a complete, validated record.
type Draft struct {
	Kind      Kind
	From      Sender
	Recipient string // empty or "operator" means the operator
	Subject   string // defaults to the first line of Body
	Body      string
	Options   []string
	ExpiresIn time.Duration // zero means the kind's default; clamped to [MinExpiry, MaxExpiry]
	InReplyTo string
}

// NewMessage builds an inbox record from d with a fresh id, created at now.
// It normalises the recipient, defaults the subject and expiry, and
// validates the result. Whether a non-operator recipient exists is the
// caller's (post-office's recipient registry) concern.
func NewMessage(d Draft, now time.Time) (Message, error) {
	now = now.UTC()
	expiry := d.ExpiresIn
	if expiry == 0 {
		expiry = DefaultRequestExpiry
		if d.Kind == KindNotification {
			expiry = DefaultNotificationExpiry
		}
	}
	expiry = min(max(expiry, MinExpiry), MaxExpiry)
	subject := strings.TrimSpace(d.Subject)
	if subject == "" {
		subject = firstLine(d.Body, MaxSubject)
	}
	var options []string
	for _, o := range d.Options {
		options = append(options, strings.TrimSpace(o))
	}
	m := Message{
		Version:   Version,
		ID:        NewID(now),
		Kind:      d.Kind,
		From:      d.From,
		Recipient: NormalizeRecipient(d.Recipient),
		Subject:   subject,
		Body:      d.Body,
		Options:   options,
		CreatedAt: now,
		ExpiresAt: now.Add(expiry),
		InReplyTo: d.InReplyTo,
	}
	return m, m.Validate()
}

// NormalizeRecipient maps "", "operator" (any case, trimmed) to
// RecipientOperator and lower-cases and trims anything else.
func NormalizeRecipient(r string) string {
	r = strings.ToLower(strings.TrimSpace(r))
	if r == "" {
		return RecipientOperator
	}
	return r
}

// firstLine returns the first non-empty line of s, cut to n runes.
func firstLine(s string, n int) string {
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if utf8.RuneCountInString(line) > n {
			r := []rune(line)
			line = string(r[:n-1]) + "…"
		}
		return line
	}
	return ""
}

// Responder says who closed a request with an answer (§2.7).
type Responder struct {
	Kind string `json:"kind"`           // operator | agent
	Via  string `json:"via"`            // tui | gui | slack | signal | whatsapp | step-up
	Name string `json:"name,omitempty"` // display name, when the channel knows it
	// Chat is the channel chat the answer came from, so the Dispatcher can
	// skip that delivery when it sends closing notes (§5.2).
	Chat string `json:"chat,omitempty"`
}

// Resolution is the terminal claim, outbox/<id>.json (§2.7), and the
// "resolution" of a done record.
type Resolution struct {
	ID        string     `json:"id"`
	Outcome   Outcome    `json:"outcome"`
	Response  string     `json:"response,omitempty"`
	Option    string     `json:"option,omitempty"`
	Responder *Responder `json:"responder,omitempty"`
	Reason    string     `json:"reason,omitempty"`
	At        time.Time  `json:"at"`
	// ClaimID is a random token naming this claim, so a claimer can tell
	// its own claim from another one with the same content.
	ClaimID string `json:"claim_id"`
}

// Via returns the channel that resolved the message, or "" when none did
// (cancelled, expired).
func (r Resolution) Via() string {
	if r.Responder == nil {
		return ""
	}
	return r.Responder.Via
}

// DoneRecord is a finalised message, done/<day>/<id>.json: the inbox record
// plus its resolution (§3.3). Orphan is set when a claim had no inbox record,
// in which case only Message.ID is meaningful.
type DoneRecord struct {
	Message
	Resolution Resolution `json:"resolution"`
	Orphan     bool       `json:"orphan,omitempty"`
}

// MatchOption returns the canonical option an answer selects, or "" when it
// selects none (§2.7). An answer matches an option when they are equal after
// trimming, ignoring case, or when it is the option's 1-based number.
// Options are suggestions: a non-matching answer is still a valid answer.
func MatchOption(options []string, answer string) string {
	a := strings.TrimSpace(answer)
	if a == "" {
		return ""
	}
	for _, o := range options {
		if strings.EqualFold(strings.TrimSpace(o), a) {
			return o
		}
	}
	if n, err := strconv.Atoi(a); err == nil && n >= 1 && n <= len(options) {
		return options[n-1]
	}
	return ""
}

// TruncateResponse caps an answer at MaxResponse bytes, cutting on a rune
// boundary and appending a marker.
func TruncateResponse(s string) string {
	if len(s) <= MaxResponse {
		return s
	}
	cut := MaxResponse - len(truncatedMark)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncatedMark
}

// IDPrefix starts every message id.
const IDPrefix = "msg_"

// crockford is the ULID alphabet.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var idState struct {
	sync.Mutex
	lastMS  uint64
	lastRnd [10]byte
}

// NewID returns "msg_" + a ULID for now: 48 bits of milliseconds and 80
// random bits, Crockford base32. Ids made in the same millisecond by this
// process increase monotonically, so they sort by creation order. The time
// part is always now's, so IDTime (and the done/ day) stays exact even if
// the clock steps back.
func NewID(now time.Time) string {
	ms := uint64(max(now.UnixMilli(), 0)) & (1<<48 - 1)
	idState.Lock()
	var rnd [10]byte
	if ms == idState.lastMS {
		rnd = idState.lastRnd
		incr(&rnd)
	} else {
		if _, err := rand.Read(rnd[:]); err != nil {
			panic("postoffice: crypto/rand: " + err.Error())
		}
	}
	idState.lastMS, idState.lastRnd = ms, rnd
	idState.Unlock()

	var b [16]byte
	binary.BigEndian.PutUint16(b[0:2], uint16(ms>>32))
	binary.BigEndian.PutUint32(b[2:6], uint32(ms))
	copy(b[6:], rnd[:])
	return IDPrefix + encodeULID(b)
}

// incr adds one to a big-endian 80-bit number, wrapping on overflow.
func incr(b *[10]byte) {
	for i := len(b) - 1; i >= 0; i-- {
		b[i]++
		if b[i] != 0 {
			return
		}
	}
}

func encodeULID(b [16]byte) string {
	// 128 bits → 26 characters of 5 bits, the first holding the top 3 bits.
	hi := binary.BigEndian.Uint64(b[0:8])
	lo := binary.BigEndian.Uint64(b[8:16])
	var out [26]byte
	for i := 25; i >= 0; i-- {
		out[i] = crockford[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:])
}

// ValidID reports whether id is "msg_" followed by a well-formed ULID.
func ValidID(id string) bool {
	u, ok := strings.CutPrefix(id, IDPrefix)
	if !ok || len(u) != 26 || u[0] > '7' {
		return false
	}
	for i := 0; i < len(u); i++ {
		if strings.IndexByte(crockford, u[i]) < 0 {
			return false
		}
	}
	return true
}

// IDTime returns the creation time encoded in a valid id.
func IDTime(id string) (time.Time, bool) {
	if !ValidID(id) {
		return time.Time{}, false
	}
	var ms uint64
	for _, c := range id[len(IDPrefix) : len(IDPrefix)+10] {
		ms = ms<<5 | uint64(strings.IndexByte(crockford, byte(c)))
	}
	return time.UnixMilli(int64(ms)).UTC(), true
}

// ShortIDLen is the length of a short id.
const ShortIDLen = 6

// ShortID is the last ShortIDLen characters of id, lower-cased: the form
// humans type in channel replies (§2.2).
func ShortID(id string) string {
	if len(id) <= ShortIDLen {
		return strings.ToLower(id)
	}
	return strings.ToLower(id[len(id)-ShortIDLen:])
}

// MatchShortID returns the one message in msgs whose short id is short
// (case-insensitive, with or without a leading '#'). ok is false when none
// matches; ErrAmbiguousShortID is returned when several do.
func MatchShortID(msgs []Message, short string) (Message, bool, error) {
	short = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(short), "#"))
	var found []Message
	for _, m := range msgs {
		if m.ShortID() == short {
			found = append(found, m)
		}
	}
	switch len(found) {
	case 0:
		return Message{}, false, nil
	case 1:
		return found[0], true, nil
	}
	return Message{}, false, fmt.Errorf("%w: %q matches %d messages", ErrAmbiguousShortID, short, len(found))
}
