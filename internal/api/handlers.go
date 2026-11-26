package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// CodeUnavailable is the error code of a 503.
const CodeUnavailable = "unavailable"

// Headers on a transcript page: the offset to continue from and the log's
// size when it was read.
const (
	HeaderTranscriptNext = "X-Transcript-Next"
	HeaderTranscriptSize = "X-Transcript-Size"
)

// routes registers the resource endpoints of §A.2 on mux.
func (s *Server) routes(mux *http.ServeMux) {
	p := Prefix
	mux.HandleFunc(p+"/projects", s.get(s.handleProjects))
	mux.HandleFunc(p+"/projects/{id}", s.get(s.handleProject))
	mux.HandleFunc(p+"/projects/{id}/tasks", s.get(s.handleTasks))
	mux.HandleFunc(p+"/projects/{id}/tasks/{name}", s.get(s.handleTask))
	mux.HandleFunc(p+"/projects/{id}/graph", s.get(s.handleGraph))
	mux.HandleFunc(p+"/projects/{id}/intake", s.get(s.handleIntake))
	mux.HandleFunc(p+"/projects/{id}/intake/{name}", s.get(s.handleIntakeRequest))
	mux.HandleFunc(p+"/projects/{id}/pulls", s.get(s.handlePulls))
	mux.HandleFunc(p+"/projects/{id}/audit", s.get(s.handleProjectAudit))
	mux.HandleFunc(p+"/sessions", s.get(s.handleSessions))
	mux.HandleFunc(p+"/sessions/{sid}", s.get(s.handleSession))
	mux.HandleFunc(p+"/sessions/{sid}/transcript", s.get(s.handleTranscript))
	mux.HandleFunc(p+"/sessions/{sid}/prompt", s.get(s.handlePrompt))
	mux.HandleFunc(p+"/sandboxes", s.get(s.handleSandboxes))
	mux.HandleFunc(p+"/audit", s.get(s.handleAudit))
	mux.HandleFunc(p+"/messages", s.get(s.handleMessages))
	mux.HandleFunc(p+"/agents", s.get(s.handleAgents))
}

// badQuery is a 400 for a query parameter the endpoint cannot use.
type badQuery string

func (e badQuery) Error() string { return string(e) }

// writeResult writes v, or the error that stopped it, as a response.
func writeResult(w http.ResponseWriter, r *http.Request, v any, err error) {
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, r, http.StatusOK, v)
}

// list writes the items fn returns in the list envelope. The cursor is read
// before fn runs, so the items are at least as new as it: a client that
// follows the event stream from the cursor may see an event the list already
// reflects (events carry whole resources, so that is harmless) but never
// misses one.
func (s *Server) list(w http.ResponseWriter, r *http.Request, fn func(context.Context) ([]any, error)) {
	cursor := s.src.Cursor()
	items, err := fn(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, r, http.StatusOK, NewList(items, s.now(), cursor))
}

func writeErr(w http.ResponseWriter, err error) {
	var bq badQuery
	switch {
	case errors.As(err, &bq):
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, CodeNotFound, err.Error())
	case errors.Is(err, ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, CodeUnavailable, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error())
	}
}

// csv splits a comma-separated query parameter, dropping blanks.
func csv(r *http.Request, name string) []string {
	var out []string
	for _, v := range r.URL.Query()[name] {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// intParam parses an integer query parameter in [lo, hi]; def when absent.
func intParam(r *http.Request, name string, def, lo, hi int) (int, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return 0, badQuery(name + " must be an integer from " + strconv.Itoa(lo) + " to " + strconv.Itoa(hi))
	}
	return n, nil
}

func boolParam(r *http.Request, name string) (*bool, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return nil, badQuery(name + " must be true or false")
	}
	return &b, nil
}

func oneOf(r *http.Request, name, def string, allowed ...string) (string, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	if !slices.Contains(allowed, v) {
		return "", badQuery(name + " must be one of " + strings.Join(allowed, ", "))
	}
	return v, nil
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	q := ProjectQuery{Status: csv(r, "status"), Root: -1, Q: r.URL.Query().Get("q")}
	var err error
	if q.Hidden, err = oneOf(r, "hidden", HiddenFalse, HiddenFalse, HiddenTrue, HiddenAny); err != nil {
		writeErr(w, err)
		return
	}
	if q.Root, err = intParam(r, "root", -1, 0, 1<<20); err != nil {
		writeErr(w, err)
		return
	}
	s.list(w, r, func(ctx context.Context) ([]any, error) { return s.src.Projects(ctx, q) })
}

func (s *Server) handleProject(w http.ResponseWriter, r *http.Request) {
	var opts ProjectOptions
	for _, inc := range csv(r, "include") {
		if inc != "fingerprint" {
			writeErr(w, badQuery("include must be fingerprint"))
			return
		}
		opts.Fingerprint = true
	}
	v, err := s.src.Project(r.Context(), r.PathValue("id"), opts)
	writeResult(w, r, v, err)
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	id, status := r.PathValue("id"), csv(r, "status")
	s.list(w, r, func(ctx context.Context) ([]any, error) { return s.src.Tasks(ctx, id, status) })
}

func (s *Server) handleTask(w http.ResponseWriter, r *http.Request) {
	v, err := s.src.Task(r.Context(), r.PathValue("id"), r.PathValue("name"))
	writeResult(w, r, v, err)
}

func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	v, err := s.src.Graph(r.Context(), r.PathValue("id"))
	writeResult(w, r, v, err)
}

func (s *Server) handleIntake(w http.ResponseWriter, r *http.Request) {
	state, err := oneOf(r, "state", "all", append([]string{"all"}, IntakeStates...)...)
	if err != nil {
		writeErr(w, err)
		return
	}
	id := r.PathValue("id")
	s.list(w, r, func(ctx context.Context) ([]any, error) { return s.src.Intake(ctx, id, state) })
}

func (s *Server) handleIntakeRequest(w http.ResponseWriter, r *http.Request) {
	v, err := s.src.IntakeRequest(r.Context(), r.PathValue("id"), r.PathValue("name"))
	writeResult(w, r, v, err)
}

func (s *Server) handlePulls(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.list(w, r, func(ctx context.Context) ([]any, error) { return s.src.Pulls(ctx, id) })
}

func (s *Server) handleProjectAudit(w http.ResponseWriter, r *http.Request) {
	s.audit(w, r, r.PathValue("id"))
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	s.audit(w, r, r.URL.Query().Get("project"))
}

func (s *Server) audit(w http.ResponseWriter, r *http.Request, project string) {
	q := AuditQuery{Events: csv(r, "event"), Project: project}
	var err error
	if q.Limit, err = intParam(r, "limit", DefaultAuditLimit, 1, MaxAuditLimit); err != nil {
		writeErr(w, err)
		return
	}
	if v := r.URL.Query().Get("since"); v != "" {
		if q.Since, err = strconv.ParseUint(v, 10, 64); err != nil {
			writeErr(w, badQuery("since must be an event seq (a non-negative integer)"))
			return
		}
		q.HasSince = true
	}
	s.list(w, r, func(ctx context.Context) ([]any, error) { return s.src.Audit(ctx, q) })
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	q := SessionQuery{Project: r.URL.Query().Get("project"), Kind: csv(r, "kind")}
	var err error
	if q.Live, err = boolParam(r, "live"); err != nil {
		writeErr(w, err)
		return
	}
	s.list(w, r, func(ctx context.Context) ([]any, error) { return s.src.Sessions(ctx, q) })
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	v, err := s.src.Session(r.Context(), r.PathValue("sid"))
	writeResult(w, r, v, err)
}

// handleTranscript writes a page of a session's stream log as NDJSON, one
// frame per line, with the offset to continue from in X-Transcript-Next.
func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request) {
	limit, err := intParam(r, "limit", DefaultTranscriptLimit, 1, MaxTranscriptLimit)
	if err != nil {
		writeErr(w, err)
		return
	}
	var after int64
	if v := r.URL.Query().Get("after"); v != "" {
		if after, err = strconv.ParseInt(v, 10, 64); err != nil || after < 0 {
			writeErr(w, badQuery("after must be a byte offset (a non-negative integer)"))
			return
		}
	}
	tr, err := s.src.Transcript(r.Context(), r.PathValue("sid"), after, limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	var body bytes.Buffer
	for _, f := range tr.Frames {
		body.Write(f)
		body.WriteByte('\n')
	}
	h := w.Header()
	h.Set(HeaderTranscriptNext, strconv.FormatInt(tr.Next, 10))
	h.Set(HeaderTranscriptSize, strconv.FormatInt(tr.Size, 10))
	etag := ETag(body.Bytes())
	h.Set("ETag", etag)
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", "application/x-ndjson")
	h.Set("Content-Length", strconv.Itoa(body.Len()))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write(body.Bytes())
	}
}

func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	v, err := s.src.Prompt(r.Context(), r.PathValue("sid"))
	writeResult(w, r, v, err)
}

func (s *Server) handleSandboxes(w http.ResponseWriter, r *http.Request) {
	retained, err := boolParam(r, "retained")
	if err != nil {
		writeErr(w, err)
		return
	}
	q := SandboxQuery{Retained: retained != nil && *retained}
	s.list(w, r, func(ctx context.Context) ([]any, error) { return s.src.Sandboxes(ctx, q) })
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	q := MessageQuery{}
	var err error
	if q.State, err = oneOf(r, "state", MessagesOpen, MessagesOpen, MessagesClosed, MessagesAll); err != nil {
		writeErr(w, err)
		return
	}
	if v := r.URL.Query().Get("since"); v != "" {
		if q.Since, err = time.Parse(time.RFC3339, v); err != nil {
			writeErr(w, badQuery("since must be an RFC 3339 time"))
			return
		}
	}
	s.list(w, r, func(ctx context.Context) ([]any, error) { return s.src.Messages(ctx, q) })
}

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	s.list(w, r, s.src.Agents)
}
