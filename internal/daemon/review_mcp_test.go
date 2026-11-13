package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/mcpgateway"
	"github.com/slimslenderslacks/work/internal/project"
)

// fakeMCPGateway fakes reviewGatewaySource: URL hands out urls[gen] (or err),
// Invalidate bumps gen so the next URL is a fresh gateway.
type fakeMCPGateway struct {
	mu          sync.Mutex
	err         error
	gen         int
	urls        int
	invalidated []string
}

func (g *fakeMCPGateway) URL(context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.urls++
	if g.err != nil {
		return "", g.err
	}
	return fmt.Sprintf("http://localhost:3333/gateways/gw-%d", g.gen), nil
}

func (g *fakeMCPGateway) Invalidate(_ context.Context, url string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.invalidated = append(g.invalidated, url)
	g.gen++
}

// fakeMCPSession answers pull_request_read by method from results; errs
// overrides a method with an error.
type fakeMCPSession struct {
	id      string
	mu      sync.Mutex
	results map[string]string
	errs    map[string]error
	calls   []string
	closed  int
}

func (s *fakeMCPSession) ID() string { return s.id }

func (s *fakeMCPSession) CallTool(_ context.Context, name string, args map[string]any) (mcpgateway.ToolResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	method, _ := args["method"].(string)
	s.calls = append(s.calls, name+":"+method)
	if name != reviewMCPToolPR {
		return mcpgateway.ToolResult{}, &mcpgateway.RPCError{Code: -32602, Message: "unknown tool " + name}
	}
	if err := s.errs[method]; err != nil {
		return mcpgateway.ToolResult{}, err
	}
	text, ok := s.results[method]
	if !ok {
		return mcpgateway.ToolResult{}, fmt.Errorf("no canned result for %s", method)
	}
	return mcpgateway.ToolResult{Content: []mcpgateway.Content{{Type: "text", Text: text}}}, nil
}

func (s *fakeMCPSession) Close(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed++
	return nil
}

// cannedPR is a pull_request_read result set, shaped like the github MCP's
// real output.
func cannedPR() map[string]string {
	return map[string]string{
		reviewMCPMethodGet:      `{"number":7,"state":"open","merged":false,"head":{"ref":"b","sha":"abc123"},"updated_at":"2026-11-13T00:00:00Z"}`,
		reviewMCPMethodReviews:  `[{"id":200,"state":"COMMENTED"},{"id":250,"state":"APPROVED"}]`,
		reviewMCPMethodComments: `[{"id":300,"body":"hi"}]`,
		reviewMCPMethodReviewComments: `{"review_threads":[` +
			`{"id":"PRRT_1","is_resolved":true,"comments":[{"html_url":"https://github.com/o/r/pull/7#discussion_r400"}]},` +
			`{"id":"PRRT_2","is_resolved":false,"comments":[{"html_url":"https://github.com/o/r/pull/7#discussion_r450"}]}` +
			`],"totalCount":2,"pageInfo":{"hasNextPage":false}}`,
		reviewMCPMethodCheckRuns: `{"total_count":3,"check_runs":[` +
			`{"id":1,"name":"test","status":"completed","conclusion":"failure"},` +
			`{"id":2,"name":"test","status":"completed","conclusion":"success"},` +
			`{"id":3,"name":"lint","status":"in_progress","conclusion":null}]}`,
		reviewMCPMethodStatus: `{"state":"pending","sha":"abc123","statuses":[{"context":"ci/legacy","state":"PENDING"}]}`,
	}
}

// mcpTestRig wires a daemon to a fake gateway and a scripted session opener.
type mcpTestRig struct {
	d     *Daemon
	buf   *safeBuf
	gw    *fakeMCPGateway
	m     *reviewMCP
	now   *time.Time
	logs  func() string
	mu    sync.Mutex
	opens []string
	// next returns the session (or error) for the n-th open (0-based).
	next func(n int, url string) (mcpSession, error)
}

func newMCPTestRig(t *testing.T) *mcpTestRig {
	t.Helper()
	r := &mcpTestRig{buf: &safeBuf{}, gw: &fakeMCPGateway{}}
	d, err := New([]string{t.TempDir()}, audit.New(r.buf))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.reviewGatewaySrc = r.gw
	r.d = d
	r.m = d.reviewMCPClient()
	if r.m == nil {
		t.Fatal("reviewMCPClient is nil with a gateway configured")
	}
	var lmu sync.Mutex
	var lines []string
	r.m.logf = func(format string, args ...any) {
		lmu.Lock()
		defer lmu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	r.logs = func() string {
		lmu.Lock()
		defer lmu.Unlock()
		return strings.Join(lines, "\n")
	}
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	r.now = &now
	r.m.clock = func() time.Time { return *r.now }
	r.m.open = func(_ context.Context, url string) (mcpSession, error) {
		r.mu.Lock()
		n := len(r.opens)
		r.opens = append(r.opens, url)
		r.mu.Unlock()
		return r.next(n, url)
	}
	return r
}

func (r *mcpTestRig) openCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.opens)
}

func watchedProject() *project.Project {
	return &project.Project{
		Status:       project.StatusIdle,
		PullRequests: []project.PullRequest{{Repo: "o/r", Number: 7}},
	}
}

func TestReviewMCPFingerprintSuccess(t *testing.T) {
	r := newMCPTestRig(t)
	sess := &fakeMCPSession{id: "s1", results: cannedPR()}
	r.next = func(int, string) (mcpSession, error) { return sess, nil }

	prs, err := r.m.fingerprintPRs(context.Background(), "/p/project.yaml", watchedProject().PullRequests)
	if err != nil {
		t.Fatalf("fingerprintPRs: %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("got %d PRs, want 1", len(prs))
	}
	pr := prs[0]
	if pr.Repo != "o/r" || pr.Number != 7 || pr.HeadSHA != "abc123" || pr.State != "open" {
		t.Errorf("PR = %+v", pr)
	}
	fp := pr.Fingerprint
	if fp.LatestReviewID != 250 || fp.LatestCommentID != 450 || fp.ReviewThreads != 2 || fp.UnresolvedThreads != 1 {
		t.Errorf("fingerprint = %+v", fp)
	}
	want := []project.CheckState{{Name: "ci/legacy", State: "pending"}, {Name: "lint", State: "in_progress"}, {Name: "test", State: "success"}}
	if fmt.Sprint(fp.CheckStates) != fmt.Sprint(want) {
		t.Errorf("check states = %v, want %v", fp.CheckStates, want)
	}
	if fp.ObservedAt == nil || !fp.ObservedAt.Equal(*r.now) {
		t.Errorf("observed_at = %v", fp.ObservedAt)
	}

	// The daemon-level fingerprint is the canonical project hash, stable
	// across calls, and the one session is reused.
	got1, ok := r.d.reviewPRFingerprint("/p/project.yaml", watchedProject())
	if !ok {
		t.Fatalf("reviewPRFingerprint not ok; audit:\n%s", r.buf.String())
	}
	wantFP, _ := project.PRsFingerprint(prs)
	if got1 != wantFP {
		t.Errorf("fingerprint = %s, want %s", got1, wantFP)
	}
	*r.now = r.now.Add(time.Hour) // ObservedAt is not part of the hash
	got2, ok := r.d.reviewPRFingerprint("/p/project.yaml", watchedProject())
	if !ok || got2 != got1 {
		t.Errorf("second fingerprint = (%s,%v), want (%s,true)", got2, ok, got1)
	}
	if n := r.openCount(); n != 1 {
		t.Errorf("opened %d sessions, want 1 (reused)", n)
	}
	if !strings.Contains(r.buf.String(), "review_mcp_session_opened") {
		t.Errorf("missing review_mcp_session_opened:\n%s", r.buf.String())
	}

	// A change on the PR changes the fingerprint.
	sess.mu.Lock()
	sess.results[reviewMCPMethodComments] = `[{"id":300},{"id":999}]`
	sess.mu.Unlock()
	got3, ok := r.d.reviewPRFingerprint("/p/project.yaml", watchedProject())
	if !ok || got3 == got1 {
		t.Errorf("fingerprint after new comment = (%s,%v), want a different value", got3, ok)
	}
}

func TestReviewMCPMergedPR(t *testing.T) {
	r := newMCPTestRig(t)
	res := cannedPR()
	res[reviewMCPMethodGet] = `{"state":"closed","merged":true,"head":{"sha":"abc123"}}`
	r.next = func(int, string) (mcpSession, error) { return &fakeMCPSession{results: res}, nil }
	prs, err := r.m.fingerprintPRs(context.Background(), "p", watchedProject().PullRequests)
	if err != nil {
		t.Fatalf("fingerprintPRs: %v", err)
	}
	if prs[0].State != "merged" {
		t.Errorf("state = %q, want merged", prs[0].State)
	}
}

func TestReviewMCPToolErrorIsLogged(t *testing.T) {
	r := newMCPTestRig(t)
	sess := &fakeMCPSession{id: "s1", results: cannedPR(), errs: map[string]error{
		reviewMCPMethodReviews: &mcpgateway.ToolError{Tool: reviewMCPToolPR, Text: "API rate limit exceeded"},
	}}
	r.next = func(int, string) (mcpSession, error) { return sess, nil }

	if _, ok := r.d.reviewPRFingerprint("/p/project.yaml", watchedProject()); ok {
		t.Fatal("fingerprint ok despite a failed tool call")
	}
	out := r.buf.String()
	for _, want := range []string{
		"review_mcp_tool_error", "tool=" + reviewMCPToolPR, "method=" + reviewMCPMethodReviews,
		"repo=o/r", "pr=7", "API rate limit exceeded",
		"review_fingerprint_error",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("audit missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(r.logs(), "o/r#7 failed") {
		t.Errorf("daemon.log missing tool failure:\n%s", r.logs())
	}
	// A tool error leaves the session usable: no recreate.
	if strings.Contains(out, "review_mcp_session_lost") || r.openCount() != 1 {
		t.Errorf("tool error should not drop the session (opens=%d):\n%s", r.openCount(), out)
	}
}

func TestReviewMCPSessionLostRecreatesWithBackoff(t *testing.T) {
	r := newMCPTestRig(t)
	lostErr := fmt.Errorf("mcp: tools/call: %w: 404 Not Found", mcpgateway.ErrSessionLost)
	first := &fakeMCPSession{id: "s1", results: cannedPR()}
	second := &fakeMCPSession{id: "s2", results: cannedPR()}
	openErr := errors.New("connection timed out")
	r.next = func(n int, _ string) (mcpSession, error) {
		switch n {
		case 0:
			return first, nil
		case 1:
			return second, nil
		case 2, 3:
			return nil, openErr
		}
		return &fakeMCPSession{id: fmt.Sprintf("s%d", n), results: cannedPR()}, nil
	}

	// Baseline on the first session.
	if _, ok := r.d.reviewPRFingerprint("p", watchedProject()); !ok {
		t.Fatalf("first fingerprint failed:\n%s", r.buf.String())
	}

	// The session is lost mid-probe: it is recreated at once and the call
	// retried on the new session.
	first.mu.Lock()
	first.errs = map[string]error{reviewMCPMethodGet: lostErr}
	first.mu.Unlock()
	if _, ok := r.d.reviewPRFingerprint("p", watchedProject()); !ok {
		t.Fatalf("fingerprint after session loss failed:\n%s", r.buf.String())
	}
	out := r.buf.String()
	for _, want := range []string{"review_mcp_session_lost", "review_mcp_session_recreate", "review_mcp_session_recreated", "session=s2"} {
		if !strings.Contains(out, want) {
			t.Errorf("audit missing %q:\n%s", want, out)
		}
	}

	// Lose s2 too; now recreation fails and backs off.
	second.mu.Lock()
	second.errs = map[string]error{reviewMCPMethodGet: lostErr}
	second.mu.Unlock()
	if _, ok := r.d.reviewPRFingerprint("p", watchedProject()); ok {
		t.Fatal("fingerprint ok though the session could not be recreated")
	}
	if !strings.Contains(r.buf.String(), "review_mcp_session_error") || !strings.Contains(r.buf.String(), "retry_in=5s") {
		t.Errorf("missing review_mcp_session_error with retry_in=5s:\n%s", r.buf.String())
	}
	opens := r.openCount() // 3
	// Inside the backoff window: no new open attempt.
	r.d.reviewPRFingerprint("p", watchedProject())
	if r.openCount() != opens {
		t.Errorf("opened during backoff window (%d → %d)", opens, r.openCount())
	}
	// After the window: one more (failing) attempt; the delay doubles.
	*r.now = r.now.Add(6 * time.Second)
	r.d.reviewPRFingerprint("p", watchedProject())
	if r.openCount() != opens+1 {
		t.Errorf("opens = %d, want %d", r.openCount(), opens+1)
	}
	if !strings.Contains(r.buf.String(), "retry_in=10s") {
		t.Errorf("backoff did not double:\n%s", r.buf.String())
	}
	// After that window: recreated, and the probe works again.
	*r.now = r.now.Add(11 * time.Second)
	if _, ok := r.d.reviewPRFingerprint("p", watchedProject()); !ok {
		t.Fatalf("fingerprint after backoff failed:\n%s", r.buf.String())
	}
	if c := strings.Count(r.buf.String(), "review_mcp_session_recreated"); c != 2 {
		t.Errorf("review_mcp_session_recreated count = %d, want 2", c)
	}
	if !strings.Contains(r.buf.String(), "attempts=3") {
		t.Errorf("recreated should report attempts=3:\n%s", r.buf.String())
	}
}

func TestReviewMCPGatewayGoneIsInvalidated(t *testing.T) {
	r := newMCPTestRig(t)
	sess := &fakeMCPSession{id: "s1", results: cannedPR()}
	r.next = func(n int, url string) (mcpSession, error) {
		if strings.HasSuffix(url, "gw-0") {
			return nil, fmt.Errorf("mcp: initialize: %w: 404 Not Found", mcpgateway.ErrSessionLost)
		}
		return sess, nil
	}
	if _, ok := r.d.reviewPRFingerprint("p", watchedProject()); !ok {
		t.Fatalf("fingerprint failed:\n%s", r.buf.String())
	}
	if len(r.gw.invalidated) != 1 || !strings.HasSuffix(r.gw.invalidated[0], "gw-0") {
		t.Errorf("invalidated = %v, want [gw-0]", r.gw.invalidated)
	}
	if !strings.Contains(r.buf.String(), "review_mcp_gateway_gone") {
		t.Errorf("missing review_mcp_gateway_gone:\n%s", r.buf.String())
	}
}

func TestReviewMCPGatewayUnavailable(t *testing.T) {
	r := newMCPTestRig(t)
	r.gw.err = errors.New("review gateway http://localhost:3333 unavailable")
	r.next = func(int, string) (mcpSession, error) { t.Fatal("opened a session without a gateway"); return nil, nil }

	if _, ok := r.d.reviewPRFingerprint("p", watchedProject()); ok {
		t.Fatal("fingerprint ok without a gateway")
	}
	out := r.buf.String()
	for _, want := range []string{"review_mcp_session_error", "gateway: review gateway http://localhost:3333 unavailable", "review_fingerprint_error", "review_mcp_tool_error"} {
		if !strings.Contains(out, want) {
			t.Errorf("audit missing %q:\n%s", want, out)
		}
	}
	// The gateway comes back after the backoff: the session opens.
	r.gw.mu.Lock()
	r.gw.err = nil
	r.gw.mu.Unlock()
	r.next = func(int, string) (mcpSession, error) { return &fakeMCPSession{results: cannedPR()}, nil }
	*r.now = r.now.Add(time.Minute)
	if _, ok := r.d.reviewPRFingerprint("p", watchedProject()); !ok {
		t.Fatalf("fingerprint failed once the gateway is back:\n%s", r.buf.String())
	}
}

func TestReviewMCPSessionOpenFailure(t *testing.T) {
	r := newMCPTestRig(t)
	r.next = func(int, string) (mcpSession, error) {
		return nil, &mcpgateway.RPCError{Code: -32600, Message: "unsupported protocol version"}
	}
	if _, ok := r.d.reviewPRFingerprint("p", watchedProject()); ok {
		t.Fatal("fingerprint ok though the session could not open")
	}
	out := r.buf.String()
	if !strings.Contains(out, "review_mcp_session_error") || !strings.Contains(out, "unsupported protocol version") {
		t.Errorf("missing review_mcp_session_error:\n%s", out)
	}
	if strings.Contains(out, "review_mcp_session_recreate") {
		t.Errorf("a first open is not a recreation:\n%s", out)
	}
	if !strings.Contains(r.logs(), "cannot open session") {
		t.Errorf("daemon.log missing open failure:\n%s", r.logs())
	}
}

func TestReviewFingerprintWithoutGateway(t *testing.T) {
	buf := &safeBuf{}
	d, err := New([]string{t.TempDir()}, audit.New(buf))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := d.reviewPRFingerprint("p", watchedProject()); ok {
		t.Fatal("fingerprint ok without a gateway")
	}
	if !strings.Contains(buf.String(), "review_fingerprint_error") || !strings.Contains(buf.String(), "mcp-gateway-url") {
		t.Errorf("missing review_fingerprint_error:\n%s", buf.String())
	}
	// No PR yet: nothing to fingerprint, nothing logged.
	buf2 := &safeBuf{}
	d2, _ := New([]string{t.TempDir()}, audit.New(buf2))
	if _, ok := d2.reviewPRFingerprint("p", &project.Project{}); ok || buf2.String() != "" {
		t.Errorf("no-PR fingerprint: ok=%v audit=%q", ok, buf2.String())
	}
}

// TestShutdownClosesMCPSessionBeforeGateway checks the shutdown order: the
// session is DELETEd while its gateway still exists, then the gateway goes.
func TestShutdownClosesMCPSessionBeforeGateway(t *testing.T) {
	buf := &safeBuf{}
	d, err := New([]string{t.TempDir()}, audit.New(buf))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	api := &fakeGatewayAPI{}
	g := newReviewGateway("http://localhost:3333", mcpgateway.Store{Dir: t.TempDir()}, d.audit)
	g.clientFor = func(string) gatewayAPI { return api }
	g.logf = func(string, ...any) {}
	d.reviewGW = g

	m := d.reviewMCPClient()
	m.logf = func(string, ...any) {}
	sess := &fakeMCPSession{id: "s1", results: cannedPR()}
	m.open = func(context.Context, string) (mcpSession, error) { return sess, nil }
	if _, ok := d.reviewPRFingerprint("p", watchedProject()); !ok {
		t.Fatalf("fingerprint failed:\n%s", buf.String())
	}

	d.shutdown()
	if sess.closed != 1 {
		t.Errorf("session closed %d times, want 1", sess.closed)
	}
	out := buf.String()
	ci, gi := strings.Index(out, "review_mcp_session_closed"), strings.Index(out, "review_gateway_deleted")
	if ci < 0 || gi < 0 || ci > gi {
		t.Errorf("want review_mcp_session_closed before review_gateway_deleted:\n%s", out)
	}
	// No session reopens after shutdown.
	if _, ok := d.reviewPRFingerprint("p", watchedProject()); ok {
		t.Error("fingerprint ok after shutdown")
	}
}
