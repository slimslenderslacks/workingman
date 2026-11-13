package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/mcpgateway"
	"github.com/slimslenderslacks/work/internal/project"
)

// The PR fingerprint probe: GitHub MCP tool calls over the daemon's
// github-only gateway (review_gateway.go), on one MCP session per daemon
// (mcpgateway.Session). It replaces the host `gh pr view` probe, so the daemon
// needs no local GitHub auth.
//
// Session lifecycle:
//   - Opened lazily on the first fingerprint call and reused for every call
//     after it.
//   - A call failing with mcpgateway.ErrSessionLost drops the session; the
//     next call recreates it (first attempt immediately, then with
//     exponential backoff while attempts keep failing). If the gateway itself
//     is gone — opening a session on its URL is answered ErrSessionLost — the
//     gateway is invalidated so a fresh one is minted, and the open is retried
//     on the new URL.
//   - closed (DELETE) at shutdown, before the gateway is deleted.
//
// Tools (github MCP server). Everything is read through one tool,
// pull_request_read, selecting the data with its `method` argument:
//
//	get                 → head.sha, state, merged
//	get_reviews         → latest review id (page/perPage pagination)
//	get_comments        → latest conversation comment id (page/perPage)
//	get_review_comments → review threads, is_resolved, inline comment ids
//	                      (cursor pagination: perPage/after, pageInfo)
//	get_check_runs      → check runs at the head commit (name/status/conclusion)
//	get_status          → combined commit statuses at the head (context/state)
const (
	// reviewMCPToolPR is the github MCP tool every fingerprint read goes
	// through. Its required arguments are method, owner, repo and pullNumber.
	reviewMCPToolPR = "pull_request_read"

	reviewMCPMethodGet            = "get"
	reviewMCPMethodReviews        = "get_reviews"
	reviewMCPMethodComments       = "get_comments"
	reviewMCPMethodReviewComments = "get_review_comments"
	reviewMCPMethodCheckRuns      = "get_check_runs"
	reviewMCPMethodStatus         = "get_status"

	// reviewMCPPerPage is the page size asked for (the tool's maximum), and
	// reviewMCPMaxPages bounds how many pages one list is read to, so a huge
	// PR can't turn a cheap probe into hundreds of calls.
	reviewMCPPerPage  = 100
	reviewMCPMaxPages = 10

	// reviewMCPCallTimeout bounds one tool call; reviewMCPOpenTimeout bounds
	// getting a session (gateway URL + initialize).
	reviewMCPCallTimeout = 30 * time.Second
	reviewMCPOpenTimeout = 30 * time.Second
	// reviewMCPCloseTimeout bounds the session DELETE at shutdown.
	reviewMCPCloseTimeout = 5 * time.Second

	reviewMCPBackoffBase = 5 * time.Second
	reviewMCPBackoffMax  = 5 * time.Minute
)

// errNoReviewGateway is the fingerprint error when the daemon has no gateway
// configured (cmd/orch's --mcp-gateway-url empty).
var errNoReviewGateway = errors.New("no review MCP gateway configured (--mcp-gateway-url)")

// mcpSession is the slice of *mcpgateway.Session the probe uses — the test
// seam for faking tool calls.
type mcpSession interface {
	CallTool(ctx context.Context, name string, args map[string]any) (mcpgateway.ToolResult, error)
	Close(ctx context.Context) error
}

// reviewMCP owns the daemon's one MCP session. Safe for concurrent use.
type reviewMCP struct {
	gateway reviewGatewaySource
	// open opens a session on a gateway URL (mcpgateway.OpenSession in
	// production).
	open        func(ctx context.Context, url string) (mcpSession, error)
	audit       *audit.Logger
	logf        func(format string, args ...any) // daemon.log
	clock       func() time.Time
	backoffBase time.Duration
	backoffMax  time.Duration

	// openMu serializes session creation so concurrent polls never open two.
	openMu sync.Mutex

	mu       sync.Mutex
	sess     mcpSession
	url      string // gateway URL sess was opened on
	lost     bool   // a session existed and was lost; the next open is a recreation
	failures int    // consecutive failed opens
	lastErr  error
	nextTry  time.Time
	closed   bool
}

func newReviewMCP(gw reviewGatewaySource, a *audit.Logger) *reviewMCP {
	return &reviewMCP{
		gateway: gw,
		open: func(ctx context.Context, url string) (mcpSession, error) {
			return mcpgateway.OpenSession(ctx, url, mcpgateway.SessionOptions{ClientName: "orch-daemon"})
		},
		audit:       a,
		logf:        log.Printf,
		clock:       time.Now,
		backoffBase: reviewMCPBackoffBase,
		backoffMax:  reviewMCPBackoffMax,
	}
}

// reviewMCPClient returns the daemon's MCP probe, creating it on first use
// over reviewGateway(). Nil when no gateway is configured.
func (d *Daemon) reviewMCPClient() *reviewMCP {
	d.reviewMCPMu.Lock()
	defer d.reviewMCPMu.Unlock()
	if d.reviewMCPc == nil {
		gw := d.reviewGateway()
		if gw == nil {
			return nil
		}
		d.reviewMCPc = newReviewMCP(gw, d.audit)
	}
	return d.reviewMCPc
}

// closeReviewMCP ends the MCP session, if one was ever opened. Called from
// shutdown before the gateway is deleted.
func (d *Daemon) closeReviewMCP() {
	d.reviewMCPMu.Lock()
	m := d.reviewMCPc
	d.reviewMCPMu.Unlock()
	if m != nil {
		m.close()
	}
}

// session returns the live session, opening (or recreating) one when there is
// none and the backoff window allows.
func (m *reviewMCP) session(ctx context.Context) (mcpSession, error) {
	m.mu.Lock()
	if s := m.sess; s != nil {
		m.mu.Unlock()
		return s, nil
	}
	m.mu.Unlock()

	m.openMu.Lock()
	defer m.openMu.Unlock()
	m.mu.Lock()
	if s := m.sess; s != nil { // another poll opened it while we waited
		m.mu.Unlock()
		return s, nil
	}
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("review MCP session: daemon is shutting down")
	}
	if wait := m.nextTry.Sub(m.clock()); wait > 0 {
		err := fmt.Errorf("review MCP session unavailable (last error: %v); next retry in %s", m.lastErr, wait.Round(time.Second))
		m.mu.Unlock()
		return nil, err
	}
	recreate, attempt := m.lost, m.failures+1
	m.mu.Unlock()

	if recreate {
		m.audit.Log("review_mcp_session_recreate", "attempt", strconv.Itoa(attempt))
		m.logf("review MCP: recreating lost session (attempt %d)", attempt)
	}
	ctx, cancel := context.WithTimeout(ctx, reviewMCPOpenTimeout)
	defer cancel()
	s, url, err := m.openOnGateway(ctx)
	if err != nil {
		m.fail(ctx, url, err)
		return nil, err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		closeCtx, cancelClose := context.WithTimeout(context.WithoutCancel(ctx), reviewMCPCloseTimeout)
		defer cancelClose()
		_ = s.Close(closeCtx)
		return nil, errors.New("review MCP session: daemon is shutting down")
	}
	m.sess, m.url = s, url
	m.lost, m.failures, m.lastErr, m.nextTry = false, 0, nil, time.Time{}
	m.mu.Unlock()

	id := sessionID(s)
	if recreate {
		m.audit.Log("review_mcp_session_recreated", "url", url, "session", id, "attempts", strconv.Itoa(attempt))
		m.logf("review MCP: session %s recreated on %s after %d attempt(s)", id, url, attempt)
	} else {
		m.audit.Log("review_mcp_session_opened", "url", url, "session", id)
		m.logf("review MCP: opened session %s on %s", id, url)
	}
	return s, nil
}

// openOnGateway gets the gateway URL and opens a session there. A gateway that
// answers the open with ErrSessionLost is gone (deleted, or standalone-codemode
// restarted): it is invalidated and the open retried once on a fresh gateway.
func (m *reviewMCP) openOnGateway(ctx context.Context) (mcpSession, string, error) {
	var url string
	for try := 0; try < 2; try++ {
		var err error
		url, err = m.gateway.URL(ctx)
		if err != nil {
			return nil, url, fmt.Errorf("gateway: %w", err)
		}
		s, err := m.open(ctx, url)
		if err == nil {
			return s, url, nil
		}
		if !errors.Is(err, mcpgateway.ErrSessionLost) || try == 1 {
			return nil, url, fmt.Errorf("initialize: %w", err)
		}
		m.audit.Log("review_mcp_gateway_gone", "url", url, "err", err.Error())
		m.logf("review MCP: gateway %s is gone (%v); invalidating it", url, err)
		m.gateway.Invalidate(ctx, url)
	}
	return nil, url, errors.New("unreachable")
}

// fail records a failed session open and schedules the next attempt.
func (m *reviewMCP) fail(ctx context.Context, url string, err error) {
	if ctx.Err() != nil && errors.Is(context.Cause(ctx), context.Canceled) {
		return // shutdown cut it short
	}
	m.mu.Lock()
	m.failures++
	n := m.failures
	delay := m.backoffBase << min(n-1, 30)
	if delay <= 0 || delay > m.backoffMax {
		delay = m.backoffMax
	}
	m.lastErr = err
	m.nextTry = m.clock().Add(delay)
	m.mu.Unlock()
	m.audit.Log("review_mcp_session_error",
		"url", url,
		"attempt", strconv.Itoa(n),
		"retry_in", delay.String(),
		"err", err.Error())
	m.logf("review MCP: cannot open session on %q (attempt %d, retrying in %s): %v", url, n, delay, err)
}

// dropSession forgets s after it was lost so the next call recreates it.
func (m *reviewMCP) dropSession(s mcpSession, err error) {
	m.mu.Lock()
	if m.sess != s {
		m.mu.Unlock()
		return
	}
	url := m.url
	m.sess, m.url, m.lost = nil, "", true
	m.mu.Unlock()
	m.audit.Log("review_mcp_session_lost", "url", url, "session", sessionID(s), "err", err.Error())
	m.logf("review MCP: session %s on %s lost: %v", sessionID(s), url, err)
}

// callTool calls tool with args on the session. A lost session is dropped and
// — when a new one can be had right away — the call retried once on it. Every
// failure is logged with the tool, method, repo and PR number.
func (m *reviewMCP) callTool(ctx context.Context, projectPath, repo string, number int, tool string, args map[string]any) (mcpgateway.ToolResult, error) {
	method, _ := args["method"].(string)
	var res mcpgateway.ToolResult
	var err error
	for try := 0; try < 2; try++ {
		var s mcpSession
		s, err = m.session(ctx)
		if err != nil {
			err = fmt.Errorf("mcp session: %w", err)
			break
		}
		cctx, cancel := context.WithTimeout(ctx, reviewMCPCallTimeout)
		res, err = s.CallTool(cctx, tool, args)
		cancel()
		if err == nil {
			return res, nil
		}
		m.logToolError(projectPath, repo, number, tool, method, err)
		if !errors.Is(err, mcpgateway.ErrSessionLost) {
			return res, err
		}
		m.dropSession(s, err)
	}
	if err != nil && !errors.Is(err, mcpgateway.ErrSessionLost) {
		// No session to call on (the session error itself was already
		// logged as review_mcp_session_error).
		m.logToolError(projectPath, repo, number, tool, method, err)
	}
	return res, err
}

func (m *reviewMCP) logToolError(projectPath, repo string, number int, tool, method string, err error) {
	m.audit.Log("review_mcp_tool_error",
		"path", projectPath,
		"tool", tool,
		"method", method,
		"repo", repo,
		"pr", strconv.Itoa(number),
		"err", err.Error())
	m.logf("review MCP: %s(%s) on %s#%d failed: %v", tool, method, repo, number, err)
}

// close ends the session (best-effort DELETE) and stops further opens.
func (m *reviewMCP) close() {
	m.openMu.Lock()
	defer m.openMu.Unlock()
	m.mu.Lock()
	m.closed = true
	s, url := m.sess, m.url
	m.sess, m.url = nil, ""
	m.mu.Unlock()
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), reviewMCPCloseTimeout)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		m.audit.Log("review_mcp_session_close_error", "url", url, "session", sessionID(s), "err", err.Error())
		m.logf("review MCP: could not close session %s on %s: %v", sessionID(s), url, err)
		return
	}
	m.audit.Log("review_mcp_session_closed", "url", url, "session", sessionID(s))
	m.logf("review MCP: closed session %s on %s", sessionID(s), url)
}

func sessionID(s mcpSession) string {
	if v, ok := s.(interface{ ID() string }); ok {
		return v.ID()
	}
	return ""
}

// fingerprintPRs reads each watched PR's change-detection signals over the
// MCP and returns them as project.PullRequest values (Repo, Number, HeadSHA,
// State, Fingerprint) — the same shape the review agent records, so
// project.PRsFingerprint compares the two directly. Entries without a repo or
// number are skipped. Any unreadable PR fails the whole probe.
func (m *reviewMCP) fingerprintPRs(ctx context.Context, projectPath string, prs []project.PullRequest) ([]project.PullRequest, error) {
	var out []project.PullRequest
	for _, pr := range prs {
		if pr.Repo == "" || pr.Number == 0 {
			continue
		}
		got, err := m.fingerprintPR(ctx, projectPath, pr.Repo, pr.Number)
		if err != nil {
			return nil, err
		}
		out = append(out, got)
	}
	return out, nil
}

func (m *reviewMCP) fingerprintPR(ctx context.Context, projectPath, repo string, number int) (project.PullRequest, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return project.PullRequest{}, fmt.Errorf("repo %q is not owner/name", repo)
	}
	read := func(method string, extra map[string]any, v any) error {
		args := map[string]any{"method": method, "owner": owner, "repo": name, "pullNumber": number}
		for k, x := range extra {
			args[k] = x
		}
		res, err := m.callTool(ctx, projectPath, repo, number, reviewMCPToolPR, args)
		if err != nil {
			return fmt.Errorf("%s(%s) %s#%d: %w", reviewMCPToolPR, method, repo, number, err)
		}
		if err := json.Unmarshal([]byte(res.Text()), v); err != nil {
			err = fmt.Errorf("%s(%s) %s#%d: decode result: %w", reviewMCPToolPR, method, repo, number, err)
			m.logToolError(projectPath, repo, number, reviewMCPToolPR, method, err)
			return err
		}
		return nil
	}

	var head struct {
		State  string `json:"state"`
		Merged bool   `json:"merged"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := read(reviewMCPMethodGet, nil, &head); err != nil {
		return project.PullRequest{}, err
	}
	fp := &project.PRFingerprint{}

	// Reviews and conversation comments: numbered pages of up to perPage.
	type idOnly struct {
		ID int64 `json:"id"`
	}
	maxID := func(method string) (int64, error) {
		var best int64
		for page := 1; page <= reviewMCPMaxPages; page++ {
			var items []idOnly
			if err := read(method, map[string]any{"page": page, "perPage": reviewMCPPerPage}, &items); err != nil {
				return 0, err
			}
			for _, it := range items {
				best = max(best, it.ID)
			}
			if len(items) < reviewMCPPerPage {
				break
			}
		}
		return best, nil
	}
	var err error
	if fp.LatestReviewID, err = maxID(reviewMCPMethodReviews); err != nil {
		return project.PullRequest{}, err
	}
	if fp.LatestCommentID, err = maxID(reviewMCPMethodComments); err != nil {
		return project.PullRequest{}, err
	}

	// Review threads: cursor pages. Inline comment ids are only exposed in
	// their html_url (…#discussion_r<id>).
	after := ""
	for page := 1; page <= reviewMCPMaxPages; page++ {
		extra := map[string]any{"perPage": reviewMCPPerPage}
		if after != "" {
			extra["after"] = after
		}
		var threads struct {
			ReviewThreads []struct {
				IsResolved bool `json:"is_resolved"`
				Comments   []struct {
					HTMLURL string `json:"html_url"`
				} `json:"comments"`
			} `json:"review_threads"`
			PageInfo struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
		}
		if err := read(reviewMCPMethodReviewComments, extra, &threads); err != nil {
			return project.PullRequest{}, err
		}
		for _, t := range threads.ReviewThreads {
			fp.ReviewThreads++
			if !t.IsResolved {
				fp.UnresolvedThreads++
			}
			for _, c := range t.Comments {
				fp.LatestCommentID = max(fp.LatestCommentID, discussionID(c.HTMLURL))
			}
		}
		if !threads.PageInfo.HasNextPage || threads.PageInfo.EndCursor == "" {
			break
		}
		after = threads.PageInfo.EndCursor
	}

	// Check runs at the head; a re-run repeats a name, so the newest (highest
	// id) run of each name wins.
	type run struct {
		id    int64
		state string
	}
	runs := map[string]run{}
	seen := 0
	for page := 1; page <= reviewMCPMaxPages; page++ {
		var cr struct {
			TotalCount int `json:"total_count"`
			CheckRuns  []struct {
				ID         int64  `json:"id"`
				Name       string `json:"name"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			} `json:"check_runs"`
		}
		if err := read(reviewMCPMethodCheckRuns, map[string]any{"page": page, "perPage": reviewMCPPerPage}, &cr); err != nil {
			return project.PullRequest{}, err
		}
		for _, c := range cr.CheckRuns {
			state := c.Status
			if strings.EqualFold(c.Status, "completed") && c.Conclusion != "" {
				state = c.Conclusion
			}
			if prev, ok := runs[c.Name]; !ok || c.ID >= prev.id {
				runs[c.Name] = run{id: c.ID, state: strings.ToLower(state)}
			}
		}
		seen += len(cr.CheckRuns)
		if len(cr.CheckRuns) == 0 || seen >= cr.TotalCount {
			break
		}
	}
	for n, r := range runs {
		fp.CheckStates = append(fp.CheckStates, project.CheckState{Name: n, State: r.state})
	}

	// Commit statuses (the combined status already holds the latest per
	// context).
	var st struct {
		Statuses []struct {
			Context string `json:"context"`
			State   string `json:"state"`
		} `json:"statuses"`
	}
	if err := read(reviewMCPMethodStatus, map[string]any{"perPage": reviewMCPPerPage}, &st); err != nil {
		return project.PullRequest{}, err
	}
	for _, s := range st.Statuses {
		fp.CheckStates = append(fp.CheckStates, project.CheckState{Name: s.Context, State: strings.ToLower(s.State)})
	}
	sort.Slice(fp.CheckStates, func(i, j int) bool {
		a, b := fp.CheckStates[i], fp.CheckStates[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.State < b.State
	})
	now := m.clock().UTC()
	fp.ObservedAt = &now

	state := strings.ToLower(head.State)
	if head.Merged {
		state = "merged"
	}
	return project.PullRequest{Repo: repo, Number: number, HeadSHA: head.Head.SHA, State: state, Fingerprint: fp}, nil
}

var discussionRE = regexp.MustCompile(`#discussion_r(\d+)$`)

// discussionID extracts a review comment's numeric id from its html_url; 0
// when the url carries none.
func discussionID(u string) int64 {
	mm := discussionRE.FindStringSubmatch(u)
	if mm == nil {
		return 0
	}
	id, _ := strconv.ParseInt(mm[1], 10, 64)
	return id
}
