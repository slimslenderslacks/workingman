package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Write actions (docs/proposals/workingman-gui.md §A.4.1). Every write is one
// file write the daemon reacts to, made through internal/control, the same
// code the TUI uses. A write answers 202 with the resource as the file now
// is (and its ETag), or 200 with "noop": true when nothing needed writing;
// the daemon's reaction arrives later on the event stream. Each write is
// recorded in the audit log as api_write (or api_write_refused) with the
// client that made it.

// Error codes of the write endpoints (§A.4.1).
const (
	CodePreconditionFailed   = "precondition_failed"
	CodePreconditionRequired = "precondition_required"
	CodeConflict             = "conflict"
	CodeInvalidState         = "invalid_state"
)

// Errors an [Actions] method returns to pick the response status, beside
// [ErrNotFound] and [ErrUnavailable]. Wrap them to add the message the client
// sees, and use [WithCurrent] to attach the resource as it is now.
var (
	// ErrInvalid is a 400: the request names or carries something the action
	// cannot use.
	ErrInvalid = errors.New("invalid request")
	// ErrConflict is a 409 conflict: the name is taken, or the resource has
	// moved on (an intake request the planner already took).
	ErrConflict = errors.New("conflict")
	// ErrInvalidState is a 409 invalid_state: the resource's state refuses
	// the action; the message is the TUI's own refusal text.
	ErrInvalidState = errors.New("invalid state")
	// ErrPreconditionFailed is a 412: If-Match names an etag the resource
	// no longer has.
	ErrPreconditionFailed = errors.New("precondition failed")
	// ErrPreconditionRequired is a 428: the action reads a field the daemon
	// or an agent may also write, so it needs If-Match.
	ErrPreconditionRequired = errors.New("precondition required")
)

// currentError carries the resource as it is now with a write's error, for
// the error body's "current" field.
type currentError struct {
	err     error
	current any
}

func (e *currentError) Error() string { return e.err.Error() }
func (e *currentError) Unwrap() error { return e.err }

// WithCurrent attaches current, the resource as it is now, to err, so the
// client can show what changed (a 412's or a 409's "current").
func WithCurrent(err error, current any) error {
	if err == nil {
		return nil
	}
	return &currentError{err: err, current: current}
}

// WriteResult is what a successful [Actions] method returns.
type WriteResult struct {
	// Noop reports that nothing needed writing; the response is then 200
	// instead of 202.
	Noop bool
	// Body is the response body.
	Body any
	// ETag is the written resource's etag, sent as the ETag header ("" for
	// none, for example a project that is only seeded).
	ETag string
	// Path is the file written, and ETagBefore / ETagAfter the resource's
	// etag before and after the write, for the api_write audit line.
	Path       string
	ETagBefore string
	ETagAfter  string
}

// NewProjectRequest is the POST /projects body (§A.4.3).
type NewProjectRequest struct {
	// Root is the index of the root (GET /status's daemon.roots) to create
	// the project under.
	Root int `json:"root"`
	// Name is the work-stream name: one lowercase kebab-case segment.
	Name string `json:"name"`
	// Description is written to project.md, for the project agent.
	Description string `json:"description"`
}

// QueueIntakeRequest is the POST /projects/{id}/intake body (§A.4.3).
type QueueIntakeRequest struct {
	// Name is the request's name without .md; the daemon picks one when it
	// is empty.
	Name string `json:"name,omitempty"`
	// Body is the request's markdown; it must not be blank.
	Body string `json:"body"`
}

// Project actions (§A.4.2): POST /projects/{id}/actions/{action}. archive is
// left out on purpose (R.4 Q11): it is the one destructive action, and stays
// in the TUI.
const (
	ActionStop    = "stop"
	ActionStart   = "start"
	ActionWolf    = "wolf"
	ActionReview  = "review"
	ActionCleanup = "cleanup"
	ActionHide    = "hide"
	ActionShow    = "show"
	ActionCatchup = "catchup"
)

// ProjectActions are the project actions the API serves, in the order of
// §A.4.2's table.
var ProjectActions = []string{ActionStop, ActionStart, ActionWolf, ActionReview, ActionCleanup, ActionHide, ActionShow, ActionCatchup}

// IfMatchRequired reports whether a project action needs If-Match (§A.4.1):
// those that read and then write a field the daemon or an agent also writes.
// The others take it when sent.
func IfMatchRequired(action string) bool {
	switch action {
	case ActionStop, ActionStart, ActionWolf, ActionReview:
		return true
	}
	return false
}

// MaxWolfReason bounds a wolf action's reason.
const MaxWolfReason = 500

// ProjectActionRequest is a project action's body: {} for every action but
// wolf, which may carry the blocked_reason to write when the project has
// none.
type ProjectActionRequest struct {
	Reason string `json:"reason,omitempty"`
}

// AnswerRequest is the POST /messages/{id}/answer body (§A.4.5): one of the
// request's options, or a free-text response.
type AnswerRequest struct {
	Option   string `json:"option,omitempty"`
	Response string `json:"response,omitempty"`
}

// WriteRequest is what every write action gets beside its own arguments.
type WriteRequest struct {
	// IfMatch is the If-Match header ("" when absent): the etag the client
	// last saw, which the write must still find or fail with
	// ErrPreconditionFailed.
	IfMatch string
	// Client names who is writing, for the audit log: a browser session id
	// or "bearer".
	Client string
}

// Actions are the write actions behind the endpoints of §A.4. Every value a
// method returns is marshalled to JSON as is, so it must already be redacted.
type Actions interface {
	// NewProject seeds a work stream: <root>/<name>/project.md.
	NewProject(ctx context.Context, w WriteRequest, req NewProjectRequest) (WriteResult, error)
	// QueueIntake writes <project>/intake/<name>.md.
	QueueIntake(ctx context.Context, w WriteRequest, project string, req QueueIntakeRequest) (WriteResult, error)
	// WithdrawIntake withdraws a pending intake request.
	WithdrawIntake(ctx context.Context, w WriteRequest, project, name string) (WriteResult, error)
	// ProjectAction runs one of [ProjectActions] on a project's
	// project.yaml (or, for catchup, beside it).
	ProjectAction(ctx context.Context, w WriteRequest, project, action string, req ProjectActionRequest) (WriteResult, error)
	// AnswerMessage answers an operator request in post-office.
	AnswerMessage(ctx context.Context, w WriteRequest, id string, req AnswerRequest) (WriteResult, error)
	// DismissMessage dismisses an operator notification in post-office.
	DismissMessage(ctx context.Context, w WriteRequest, id string) (WriteResult, error)
}

// MatchETag reports whether an If-Match header admits a resource whose etag
// is etag: "*" admits any existing resource, else one of the listed tags
// must be etag exactly (If-Match uses the strong comparison, so a W/ tag
// never matches). An empty header admits anything.
func MatchETag(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return true
	}
	if header == "*" {
		return etag != ""
	}
	for _, t := range strings.Split(header, ",") {
		if strings.TrimSpace(t) == etag && etag != "" {
			return true
		}
	}
	return false
}

// maxWriteBody bounds a write request's body.
const maxWriteBody = 1 << 20

// clientID names a request's credentials in the audit log.
func clientID(a auth) string {
	if a.session != nil {
		return a.session.id
	}
	return "bearer"
}

// hasScope reports whether a request's credentials carry scope. The bearer
// token carries every scope there is; a browser session carries those its
// login granted.
func hasScope(a auth, scope string) bool {
	if a.session == nil {
		return true
	}
	for _, s := range a.session.scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// decodeBody reads a write's JSON body into v, refusing unknown fields. An
// empty body decodes as {}.
func decodeBody(r *http.Request, v any) error {
	data, err := io.ReadAll(io.LimitReader(r.Body, maxWriteBody+1))
	if err != nil {
		return fmt.Errorf("%w: read body: %v", ErrInvalid, err)
	}
	if len(data) > maxWriteBody {
		return fmt.Errorf("%w: the body is larger than %d bytes", ErrInvalid, maxWriteBody)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		data = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: the body is not the JSON this endpoint takes: %v", ErrInvalid, err)
	}
	if dec.More() {
		return fmt.Errorf("%w: the body has more than one JSON value", ErrInvalid)
	}
	return nil
}

// write runs one write action: it checks the scope, runs fn, writes its
// result or error, and logs api_write or api_write_refused. Idempotency-Key
// replays are handled around it (see idempotent).
func (s *Server) write(action, scope string, fn func(r *http.Request, w WriteRequest) (WriteResult, error)) http.HandlerFunc {
	return s.idempotent(func(w http.ResponseWriter, r *http.Request) {
		a := authFrom(r.Context())
		client := clientID(a)
		if !hasScope(a, scope) {
			s.log("api_write_refused", "action", action, "code", CodeForbidden, "client", client)
			writeError(w, http.StatusForbidden, CodeForbidden, "this session lacks the "+scope+" scope")
			return
		}
		res, err := fn(r, WriteRequest{IfMatch: r.Header.Get("If-Match"), Client: client})
		if err != nil {
			status, code := writeErrStatus(err)
			s.log("api_write_refused", "action", action, "code", code, "client", client, "err", err.Error())
			var ce *currentError
			if errors.As(err, &ce) {
				writeErrorCurrent(w, status, code, err.Error(), ce.current)
				return
			}
			writeError(w, status, code, err.Error())
			return
		}
		kv := []string{"action", action, "path", res.Path}
		// Etags are logged bare (pj-9f1c2e7a): the audit line quotes values
		// itself.
		for _, e := range [][2]string{{"etag_before", res.ETagBefore}, {"etag_after", res.ETagAfter}} {
			if e[1] != "" {
				kv = append(kv, e[0], strings.Trim(e[1], `"`))
			}
		}
		kv = append(kv, "client", client)
		if res.Noop {
			kv = append(kv, "noop", "true")
		}
		s.log("api_write", kv...)
		status := http.StatusAccepted
		if res.Noop {
			status = http.StatusOK
		}
		writeWritten(w, r, status, res.ETag, res.Body)
	})
}

// writeErrStatus maps a write's error to its status and code.
func writeErrStatus(err error) (int, string) {
	var bq badQuery
	switch {
	case errors.As(err, &bq), errors.Is(err, ErrInvalid):
		return http.StatusBadRequest, CodeBadRequest
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound, CodeNotFound
	case errors.Is(err, ErrPreconditionFailed):
		return http.StatusPreconditionFailed, CodePreconditionFailed
	case errors.Is(err, ErrPreconditionRequired):
		return http.StatusPreconditionRequired, CodePreconditionRequired
	case errors.Is(err, ErrConflict):
		return http.StatusConflict, CodeConflict
	case errors.Is(err, ErrInvalidState):
		return http.StatusConflict, CodeInvalidState
	case errors.Is(err, ErrUnavailable):
		return http.StatusServiceUnavailable, CodeUnavailable
	}
	return http.StatusInternalServerError, CodeInternal
}

func writeErrorCurrent(w http.ResponseWriter, status int, code, msg string, current any) {
	body, err := json.Marshal(ErrorBody{Error: ErrorDetail{Code: code, Message: msg, Current: current}})
	if err != nil {
		writeError(w, status, code, msg)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(append(body, '\n'))
}

// writeWritten writes a write's response: the body, with the resource's
// etag (not the body's) as ETag.
func writeWritten(w http.ResponseWriter, r *http.Request, status int, etag string, v any) {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, "encode response: "+err.Error())
		return
	}
	body = append(body, '\n')
	h := w.Header()
	if etag != "" {
		h.Set("ETag", etag)
	}
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	w.Write(body)
}

// ---------------------------------------------------------------------------
// Idempotency-Key
// ---------------------------------------------------------------------------

const (
	// HeaderIdempotencyKey names a write so a retry of it is answered with
	// the first response instead of being done twice (§A.4.1).
	HeaderIdempotencyKey = "Idempotency-Key"
	// HeaderIdempotentReplay is set on a response replayed for a key.
	HeaderIdempotentReplay = "Idempotent-Replayed"
	// IdempotencyTTL is how long a key's response is kept.
	IdempotencyTTL = 24 * time.Hour

	// maxIdempotencyKeys bounds the store; past it the oldest key is
	// dropped.
	maxIdempotencyKeys = 4096
	maxIdempotencyKey  = 255
)

// idemEntry is one key's request fingerprint and, once it is done, its
// response.
type idemEntry struct {
	fingerprint string
	created     time.Time
	done        bool
	status      int
	header      http.Header
	body        []byte
}

// idempotency remembers write responses by (client, key) in memory, so a
// daemon restart forgets them.
type idempotency struct {
	mu sync.Mutex
	m  map[string]*idemEntry
}

func newIdempotency() *idempotency { return &idempotency{m: map[string]*idemEntry{}} }

// validKey accepts 1..255 visible ASCII characters.
func validKey(k string) bool {
	if k == "" || len(k) > maxIdempotencyKey {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x21 || k[i] > 0x7e {
			return false
		}
	}
	return true
}

// begin claims key for a request with fingerprint. It returns the stored
// entry to replay when the key is done, or an error message when the key is
// in flight or was used for another request; otherwise the caller owns the
// key and must call finish or abandon.
func (id *idempotency) begin(key, fingerprint string, now time.Time) (replay *idemEntry, refusal string) {
	id.mu.Lock()
	defer id.mu.Unlock()
	for k, e := range id.m {
		if now.Sub(e.created) >= IdempotencyTTL {
			delete(id.m, k)
		}
	}
	if e, ok := id.m[key]; ok {
		switch {
		case e.fingerprint != fingerprint:
			return nil, "this " + HeaderIdempotencyKey + " was used for a different request"
		case !e.done:
			return nil, "a request with this " + HeaderIdempotencyKey + " is still in progress"
		}
		return e, ""
	}
	if len(id.m) >= maxIdempotencyKeys {
		id.evictOldest()
	}
	id.m[key] = &idemEntry{fingerprint: fingerprint, created: now}
	return nil, ""
}

func (id *idempotency) evictOldest() {
	keys := make([]string, 0, len(id.m))
	for k := range id.m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return id.m[keys[i]].created.Before(id.m[keys[j]].created) })
	delete(id.m, keys[0])
}

func (id *idempotency) finish(key string, status int, header http.Header, body []byte) {
	id.mu.Lock()
	defer id.mu.Unlock()
	if e, ok := id.m[key]; ok {
		e.done, e.status, e.header, e.body = true, status, header, body
	}
}

// abandon forgets a key whose request did not complete (a server error), so
// a retry runs again.
func (id *idempotency) abandon(key string) {
	id.mu.Lock()
	defer id.mu.Unlock()
	delete(id.m, key)
}

// captureWriter buffers a response so it can be stored for its key.
type captureWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (c *captureWriter) Header() http.Header         { return c.header }
func (c *captureWriter) Write(b []byte) (int, error) { return c.body.Write(b) }
func (c *captureWriter) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}

// idempotent wraps a write handler with Idempotency-Key handling: a request
// without the header runs as is; one with it runs once per (client, key),
// and a retry with the same method, path and body gets the first response
// again, marked Idempotent-Replayed. Reusing a key for a different request
// is a 422. A response with a 5xx status is not kept, so a retry runs again.
func (s *Server) idempotent(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get(HeaderIdempotencyKey)
		if key == "" {
			h(w, r)
			return
		}
		if !validKey(key) {
			writeError(w, http.StatusBadRequest, CodeBadRequest, HeaderIdempotencyKey+" must be 1 to 255 visible ASCII characters")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxWriteBody+1))
		if err != nil {
			writeError(w, http.StatusBadRequest, CodeBadRequest, "read body: "+err.Error())
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		sum := sha256.Sum256(body)
		fp := r.Method + " " + r.URL.Path + " " + r.Header.Get("If-Match") + " " + hex.EncodeToString(sum[:])
		k := clientID(authFrom(r.Context())) + "\x00" + key
		replay, refusal := s.idem.begin(k, fp, s.now())
		if refusal != "" {
			writeError(w, http.StatusUnprocessableEntity, CodeConflict, refusal)
			return
		}
		if replay != nil {
			for name, vs := range replay.header {
				w.Header()[name] = vs
			}
			w.Header().Set(HeaderIdempotentReplay, "true")
			w.WriteHeader(replay.status)
			w.Write(replay.body)
			return
		}
		c := &captureWriter{header: http.Header{}}
		h(c, r)
		if c.status == 0 {
			c.status = http.StatusOK
		}
		if c.status >= 500 {
			s.idem.abandon(k)
		} else {
			s.idem.finish(k, c.status, c.header.Clone(), c.body.Bytes())
		}
		for name, vs := range c.header {
			w.Header()[name] = vs
		}
		w.WriteHeader(c.status)
		w.Write(c.body.Bytes())
	}
}

// ---------------------------------------------------------------------------
// Endpoints
// ---------------------------------------------------------------------------

// methods dispatches a path's handlers by method; GET also serves HEAD. Any
// other method is a 405 with Allow.
func (s *Server) methods(hs map[string]http.HandlerFunc) http.HandlerFunc {
	allow := make([]string, 0, len(hs)+1)
	for m := range hs {
		allow = append(allow, m)
		if m == http.MethodGet {
			allow = append(allow, http.MethodHead)
		}
	}
	sort.Strings(allow)
	return func(w http.ResponseWriter, r *http.Request) {
		m := r.Method
		if m == http.MethodHead {
			m = http.MethodGet
		}
		if h, ok := hs[m]; ok {
			h(w, r)
			return
		}
		w.Header().Set("Allow", strings.Join(allow, ", "))
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, r.Method+" is not allowed on "+r.URL.Path)
	}
}

// actions is the source's write side; nil when it has none (every write is
// then a 503).
func (s *Server) actions() Actions {
	a, _ := s.src.(Actions)
	return a
}

func (s *Server) handleNewProject(r *http.Request, w WriteRequest) (WriteResult, error) {
	act := s.actions()
	if act == nil {
		return WriteResult{}, fmt.Errorf("%w: this API serves no writes", ErrUnavailable)
	}
	var req NewProjectRequest
	if err := decodeBody(r, &req); err != nil {
		return WriteResult{}, err
	}
	return act.NewProject(r.Context(), w, req)
}

func (s *Server) handleQueueIntake(r *http.Request, w WriteRequest) (WriteResult, error) {
	act := s.actions()
	if act == nil {
		return WriteResult{}, fmt.Errorf("%w: this API serves no writes", ErrUnavailable)
	}
	var req QueueIntakeRequest
	if err := decodeBody(r, &req); err != nil {
		return WriteResult{}, err
	}
	return act.QueueIntake(r.Context(), w, r.PathValue("id"), req)
}

func (s *Server) handleWithdrawIntake(r *http.Request, w WriteRequest) (WriteResult, error) {
	act := s.actions()
	if act == nil {
		return WriteResult{}, fmt.Errorf("%w: this API serves no writes", ErrUnavailable)
	}
	var req struct{}
	if err := decodeBody(r, &req); err != nil {
		return WriteResult{}, err
	}
	return act.WithdrawIntake(r.Context(), w, r.PathValue("id"), r.PathValue("name"))
}

// projectActionHandler serves POST /projects/{id}/actions/{action}: one
// write handler per action, so each is audited and replayed under its own
// name. An action the API does not serve (archive among them) is a 404.
func (s *Server) projectActionHandler() http.HandlerFunc {
	hs := map[string]http.HandlerFunc{}
	for _, a := range ProjectActions {
		hs[a] = s.write(a, ScopeWrite, func(r *http.Request, w WriteRequest) (WriteResult, error) {
			return s.handleProjectAction(r, w, a)
		})
	}
	return func(w http.ResponseWriter, r *http.Request) {
		a := r.PathValue("action")
		h, ok := hs[a]
		if !ok {
			msg := "no project action " + strconv.Quote(a) + "; the API serves " + strings.Join(ProjectActions, ", ")
			if a == "archive" {
				msg = "archive is not served by the API: it is destructive and multi-step; run :archive in the TUI"
			}
			writeError(w, http.StatusNotFound, CodeNotFound, msg)
			return
		}
		h(w, r)
	}
}

func (s *Server) handleProjectAction(r *http.Request, w WriteRequest, action string) (WriteResult, error) {
	act := s.actions()
	if act == nil {
		return WriteResult{}, fmt.Errorf("%w: this API serves no writes", ErrUnavailable)
	}
	var req ProjectActionRequest
	if err := decodeBody(r, &req); err != nil {
		return WriteResult{}, err
	}
	switch {
	case req.Reason != "" && action != ActionWolf:
		return WriteResult{}, fmt.Errorf("%w: only wolf takes a reason", ErrInvalid)
	case len(req.Reason) > MaxWolfReason:
		return WriteResult{}, fmt.Errorf("%w: the reason is longer than %d bytes", ErrInvalid, MaxWolfReason)
	case IfMatchRequired(action) && strings.TrimSpace(w.IfMatch) == "":
		return WriteResult{}, fmt.Errorf("%w: %s reads and writes fields the daemon also writes; send If-Match with the project's etag", ErrPreconditionRequired, action)
	}
	return act.ProjectAction(r.Context(), w, r.PathValue("id"), action, req)
}

func (s *Server) handleAnswerMessage(r *http.Request, w WriteRequest) (WriteResult, error) {
	act := s.actions()
	if act == nil {
		return WriteResult{}, fmt.Errorf("%w: this API serves no writes", ErrUnavailable)
	}
	var req AnswerRequest
	if err := decodeBody(r, &req); err != nil {
		return WriteResult{}, err
	}
	if (strings.TrimSpace(req.Option) == "") == (strings.TrimSpace(req.Response) == "") {
		return WriteResult{}, fmt.Errorf("%w: send one of option or response", ErrInvalid)
	}
	return act.AnswerMessage(r.Context(), w, r.PathValue("id"), req)
}

func (s *Server) handleDismissMessage(r *http.Request, w WriteRequest) (WriteResult, error) {
	act := s.actions()
	if act == nil {
		return WriteResult{}, fmt.Errorf("%w: this API serves no writes", ErrUnavailable)
	}
	var req struct{}
	if err := decodeBody(r, &req); err != nil {
		return WriteResult{}, err
	}
	return act.DismissMessage(r.Context(), w, r.PathValue("id"))
}
