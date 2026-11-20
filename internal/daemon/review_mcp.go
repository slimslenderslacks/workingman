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
	"time"

	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/mcpgateway"
	"github.com/slimslenderslacks/work/internal/project"
)

// The PR fingerprint probe: GitHub MCP tool calls over the daemon's
// github-only gateway (review_gateway.go), on one MCP session per daemon (an
// mcpgateway.DaemonSession over mcpgateway.Session). It replaces the old host
// gh CLI probe, so the daemon needs no local GitHub auth.
//
// Session lifecycle (owned by mcpgateway.DaemonSession):
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
// Audit events are review_mcp_*; daemon.log lines start "review MCP:".
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
)

// errNoReviewGateway is the fingerprint error when the daemon has no gateway
// configured (cmd/orch's --mcp-gateway-url empty).
var errNoReviewGateway = errors.New("no review MCP gateway configured (--mcp-gateway-url)")

// mcpSession is the slice of *mcpgateway.Session the probe uses — the test
// seam for faking tool calls.
type mcpSession = mcpgateway.SessionConn

// reviewMCP is the PR fingerprint probe over the daemon's one MCP session.
// Safe for concurrent use.
type reviewMCP struct {
	sess *mcpgateway.DaemonSession
	// open opens a session on a gateway URL (mcpgateway.OpenSession in
	// production); logf and clock are daemon.log and the clock. All three are
	// test seams the session reads through.
	open  func(ctx context.Context, url string) (mcpSession, error)
	audit *audit.Logger
	logf  func(format string, args ...any)
	clock func() time.Time
}

func newReviewMCP(gw mcpgateway.GatewaySource, a *audit.Logger) *reviewMCP {
	m := &reviewMCP{
		open: func(ctx context.Context, url string) (mcpSession, error) {
			return mcpgateway.OpenSession(ctx, url, mcpgateway.SessionOptions{ClientName: "orch-daemon"})
		},
		audit: a,
		logf:  log.Printf,
		clock: time.Now,
	}
	m.sess = &mcpgateway.DaemonSession{
		Gateway: gw,
		Name:    reviewName,
		Open:    func(ctx context.Context, url string) (mcpgateway.SessionConn, error) { return m.open(ctx, url) },
		Audit:   a.Log,
		Logf:    func(format string, args ...any) { m.logf(format, args...) },
		Clock:   func() time.Time { return m.clock() },
	}
	return m
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
		m.sess.Close()
	}
}

// callTool calls tool with args on the session (retrying once on a fresh
// session when it was lost). Every failure is logged with the tool, method,
// repo and PR number.
func (m *reviewMCP) callTool(ctx context.Context, projectPath, repo string, number int, tool string, args map[string]any) (mcpgateway.ToolResult, error) {
	method, _ := args["method"].(string)
	return m.sess.CallToolReporting(ctx, tool, args, func(err error) {
		m.logToolError(projectPath, repo, number, tool, method, err)
	})
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
