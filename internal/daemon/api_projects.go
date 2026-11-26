package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/api"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/task"
	"github.com/slimslenderslacks/work/internal/taskgraph"
)

// The API's resources (docs/proposals/workingman-gui.md §A.1) are built from
// the snapshot builder's model, so the API and snapshot.json never disagree,
// plus the task- and project-file fields a snapshot leaves out, which are read
// through the daemon's fileCaches. Every resource is redacted as a whole
// (redactStrings) before it is returned; it never shares memory with a cache,
// so that pass cannot race another request.

// ProjectFlags groups a project's boolean request and display flags.
type ProjectFlags struct {
	Review     bool `json:"review"`
	ReviewNow  bool `json:"review_now"`
	Cleanup    bool `json:"cleanup"`
	Archive    bool `json:"archive"`
	Replan     bool `json:"replan"`
	Hidden     bool `json:"hidden"`
	WatchingPR bool `json:"watching_pr"`
}

// APIPullRequest is a pull_requests[] entry of a project. Fingerprint is set
// only in a project's detail form with ?include=fingerprint.
type APIPullRequest struct {
	Repo        string            `json:"repo"`
	Number      int               `json:"number"`
	URL         string            `json:"url,omitempty"`
	HeadSHA     string            `json:"head_sha,omitempty"`
	State       string            `json:"state,omitempty"`
	Fingerprint *APIPRFingerprint `json:"fingerprint,omitempty"`
}

// APIPRFingerprint is the review agent's record of a PR's activity.
type APIPRFingerprint struct {
	LatestCommentID   int64           `json:"latest_comment_id,omitempty"`
	LatestReviewID    int64           `json:"latest_review_id,omitempty"`
	ReviewThreads     int             `json:"review_threads,omitempty"`
	UnresolvedThreads int             `json:"unresolved_threads,omitempty"`
	CheckStates       []APICheckState `json:"check_states,omitempty"`
	ObservedAt        *time.Time      `json:"observed_at,omitempty"`
}

// APICheckState is one check at a PR's head.
type APICheckState struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

// APIRepo is a repos[] or new_repos[] entry.
type APIRepo struct {
	Org        string `json:"org"`
	Name       string `json:"name"`
	BaseBranch string `json:"base_branch,omitempty"`
	Visibility string `json:"visibility,omitempty"`
}

// APIBlockedSession is blocked-session.yaml, the wolf's durable diagnosis.
type APIBlockedSession struct {
	BlockedReason string   `json:"blocked_reason"`
	Summary       string   `json:"summary"`
	Attempted     []string `json:"attempted"`
}

// ProjectSummary is a project in GET /projects (§A.1.1).
type ProjectSummary struct {
	ID          string            `json:"id"`
	WorkStream  string            `json:"work_stream"`
	Root        string            `json:"root"`
	Path        string            `json:"path"`
	ETag        string            `json:"etag,omitempty"`
	Status      string            `json:"status"`
	StoppedFrom string            `json:"stopped_from,omitempty"`
	Blocked     string            `json:"blocked_reason,omitempty"`
	Branch      string            `json:"branch,omitempty"`
	Description string            `json:"description,omitempty"`
	LoadError   string            `json:"load_error,omitempty"`
	Flags       ProjectFlags      `json:"flags"`
	Cron        string            `json:"cron,omitempty"`
	CronActive  bool              `json:"cron_active"`
	CronUntil   *time.Time        `json:"cron_until,omitempty"`
	CronMaxRuns int               `json:"cron_max_runs,omitempty"`
	CronRuns    int               `json:"cron_runs,omitempty"`
	MaxParallel int               `json:"max_parallel_tasks"`
	Verify      string            `json:"verify,omitempty"`
	PullReqs    []APIPullRequest  `json:"pull_requests"`
	TaskCounts  map[string]int    `json:"task_counts"`
	TaskTotal   int               `json:"task_total"`
	CommitQueue []string          `json:"commit_queue"`
	Verified    *VerifyChecks     `json:"verify_checks,omitempty"`
	CreatedAt   *time.Time        `json:"created_at,omitempty"`
	UpdatedAt   time.Time         `json:"updated_at,omitempty"`
	UpdatedBy   string            `json:"updated_by,omitempty"`
	Live        *ProjectLive      `json:"live"`
	Links       map[string]string `json:"links"`
}

// ProjectDetail is GET /projects/{id}: the summary plus the full
// description, the repos, the wolf's blocked-session record and the pending
// intake files.
type ProjectDetail struct {
	ProjectSummary
	Description    string             `json:"description,omitempty"`
	Repos          []APIRepo          `json:"repos"`
	NewRepos       []APIRepo          `json:"new_repos"`
	BlockedSession *APIBlockedSession `json:"blocked_session"`
	PendingIntake  []string           `json:"pending_intake"`
}

// projectID is a project's API id, "p_<root index>.<work-stream>" (§A.1).
func projectID(rootIndex int, workStream string) string {
	return "p_" + strconv.Itoa(rootIndex) + "." + workStream
}

// rootIndexOf is the index of the first root that holds path, or -1.
func (d *Daemon) rootIndexOf(path string) int {
	for i, r := range d.roots {
		if abs, err := filepath.Abs(r); err == nil && within(path, abs) {
			return i
		}
		if within(path, r) {
			return i
		}
	}
	return -1
}

// apiModel is the snapshot model one request works from.
type apiModel struct {
	d        *Daemon
	projects []ProjectSnapshot
	sessions []SessionSnapshot
	ids      map[string]int // project id -> index in projects
}

// model builds the live snapshot model (projects with their live blocks, the
// tracked sessions), as GET /snapshot does, and indexes it by project id.
// When two projects of one root share a work-stream name (nested dirs), the
// first one by path keeps the id.
func (d *Daemon) model() *apiModel {
	snap := d.buildSnapshotN(true, 1)
	m := &apiModel{d: d, projects: snap.Projects, sessions: snap.Sessions, ids: map[string]int{}}
	for i, p := range m.projects {
		id := m.id(p)
		if _, dup := m.ids[id]; !dup {
			m.ids[id] = i
		}
	}
	return m
}

func (m *apiModel) id(p ProjectSnapshot) string {
	return projectID(m.d.rootIndexOf(p.Path), p.WorkStream)
}

// project looks a project up by id.
func (m *apiModel) project(id string) (*ProjectSnapshot, error) {
	i, ok := m.ids[id]
	if !ok {
		return nil, fmt.Errorf("%w: no project %q", api.ErrNotFound, id)
	}
	return &m.projects[i], nil
}

// projectIDOf is the id of the project at path, or "".
func (m *apiModel) projectIDOf(path string) string {
	if path == "" {
		return ""
	}
	for _, p := range m.projects {
		if p.Path == path {
			return m.id(p)
		}
	}
	return ""
}

func (m *apiModel) summary(p *ProjectSnapshot) ProjectSummary {
	id := m.id(*p)
	root := ""
	if i := m.d.rootIndexOf(p.Path); i >= 0 {
		root = m.d.roots[i]
	}
	s := ProjectSummary{
		ID:          id,
		WorkStream:  p.WorkStream,
		Root:        root,
		Path:        p.Path,
		Status:      p.Status,
		Blocked:     p.BlockedReason,
		Branch:      p.Branch,
		Description: p.Description,
		LoadError:   p.LoadError,
		Flags: ProjectFlags{
			Review: p.Review, ReviewNow: p.ReviewNow, Cleanup: p.Cleanup, Archive: p.Archive,
			Replan: p.Replan, Hidden: p.Hidden, WatchingPR: p.WatchingPR,
		},
		Cron:        p.Cron,
		CronActive:  p.CronActive,
		MaxParallel: p.MaxParallelTasks,
		PullReqs:    []APIPullRequest{},
		TaskCounts:  p.TaskCounts,
		TaskTotal:   p.TaskTotal,
		CommitQueue: p.CommitQueue,
		Verified:    p.VerifyChecks,
		UpdatedAt:   p.UpdatedAt,
		Live:        p.Live,
		Links: map[string]string{
			"self":     api.Prefix + "/projects/" + id,
			"tasks":    api.Prefix + "/projects/" + id + "/tasks",
			"graph":    api.Prefix + "/projects/" + id + "/graph",
			"intake":   api.Prefix + "/projects/" + id + "/intake",
			"pulls":    api.Prefix + "/projects/" + id + "/pulls",
			"audit":    api.Prefix + "/projects/" + id + "/audit",
			"sessions": api.Prefix + "/sessions?project=" + id,
		},
	}
	for _, pr := range p.PullRequests {
		s.PullReqs = append(s.PullReqs, APIPullRequest{Repo: pr.Repo, Number: pr.Number, URL: pr.URL, State: pr.State})
	}
	if pf, tag, err := m.d.apiFiles.projects.get(p.Path); err == nil {
		s.ETag = resourceETag("pj", tag)
		s.StoppedFrom = string(pf.StoppedFrom)
		s.CronUntil = utcTime(pf.CronUntil)
		s.CronMaxRuns = pf.CronMaxRuns
		s.CronRuns = pf.CronRuns
		s.Verify = pf.Verify
		s.CreatedAt = utcTime(pf.CreatedAt)
		s.UpdatedBy = string(pf.UpdatedBy)
		// head_sha is not in the snapshot; match the file's entries.
		for i := range s.PullReqs {
			for _, fpr := range pf.PullRequests {
				if fpr.Repo == s.PullReqs[i].Repo && fpr.Number == s.PullReqs[i].Number {
					s.PullReqs[i].HeadSHA = fpr.HeadSHA
				}
			}
		}
	} else if tag != "" {
		s.ETag = resourceETag("pj", tag)
	}
	return s
}

func (m *apiModel) detail(p *ProjectSnapshot, opts api.ProjectOptions) ProjectDetail {
	det := ProjectDetail{ProjectSummary: m.summary(p), Repos: []APIRepo{}, NewRepos: []APIRepo{}, PendingIntake: []string{}}
	det.Description = p.Description
	if pf, _, err := m.d.apiFiles.projects.get(p.Path); err == nil {
		det.Description = strings.TrimSpace(pf.Description)
		det.Repos = apiRepos(pf.Repos)
		det.NewRepos = apiRepos(pf.NewRepos)
		if opts.Fingerprint {
			for i := range det.PullReqs {
				for _, fpr := range pf.PullRequests {
					if fpr.Repo == det.PullReqs[i].Repo && fpr.Number == det.PullReqs[i].Number {
						det.PullReqs[i].Fingerprint = apiFingerprint(fpr.Fingerprint)
					}
				}
			}
		}
	}
	if bs, err := project.LoadBlockedSession(project.BlockedSessionPath(p.Path)); err == nil {
		det.BlockedSession = &APIBlockedSession{BlockedReason: bs.BlockedReason, Summary: bs.Summary, Attempted: append([]string{}, bs.Attempted...)}
	}
	for _, f := range pendingIntakeFiles(filepath.Dir(p.Path)) {
		det.PendingIntake = append(det.PendingIntake, filepath.Base(f))
	}
	return det
}

func apiRepos(rs []project.Repo) []APIRepo {
	out := make([]APIRepo, 0, len(rs))
	for _, r := range rs {
		out = append(out, APIRepo{Org: r.Org, Name: r.Name, BaseBranch: r.BaseBranch, Visibility: r.Visibility})
	}
	return out
}

func apiFingerprint(f *project.PRFingerprint) *APIPRFingerprint {
	if f == nil {
		return nil
	}
	out := &APIPRFingerprint{
		LatestCommentID: f.LatestCommentID, LatestReviewID: f.LatestReviewID,
		ReviewThreads: f.ReviewThreads, UnresolvedThreads: f.UnresolvedThreads,
		ObservedAt: utcTime(f.ObservedAt),
	}
	for _, c := range f.CheckStates {
		out.CheckStates = append(out.CheckStates, APICheckState{Name: c.Name, State: c.State})
	}
	return out
}

func (s apiSource) Projects(_ context.Context, q api.ProjectQuery) ([]any, error) {
	m := s.d.model()
	needle := strings.ToLower(q.Q)
	var out []any
	for i := range m.projects {
		p := &m.projects[i]
		if len(q.Status) > 0 && !slices.Contains(q.Status, p.Status) {
			continue
		}
		if (q.Hidden == api.HiddenFalse && p.Hidden) || (q.Hidden == api.HiddenTrue && !p.Hidden) {
			continue
		}
		if q.Root >= 0 && s.d.rootIndexOf(p.Path) != q.Root {
			continue
		}
		sum := m.summary(p)
		if needle != "" && !strings.Contains(strings.ToLower(p.WorkStream), needle) && !strings.Contains(strings.ToLower(m.fullDescription(p)), needle) {
			continue
		}
		redactStrings(&sum)
		out = append(out, sum)
	}
	return out, nil
}

func (m *apiModel) fullDescription(p *ProjectSnapshot) string {
	if pf, _, err := m.d.apiFiles.projects.get(p.Path); err == nil {
		return pf.Description
	}
	return p.Description
}

func (s apiSource) Project(_ context.Context, id string, opts api.ProjectOptions) (any, error) {
	m := s.d.model()
	p, err := m.project(id)
	if err != nil {
		return nil, err
	}
	det := m.detail(p, opts)
	redactStrings(&det)
	return det, nil
}

// ---------------------------------------------------------------------------
// Tasks
// ---------------------------------------------------------------------------

// TaskCapabilities groups the fields that make up a task's sandbox workload.
type TaskCapabilities struct {
	Model           string          `json:"model"`
	ResolvedModel   string          `json:"resolved_model,omitempty"`
	Harness         string          `json:"harness"`
	ResolvedHarness string          `json:"resolved_harness,omitempty"`
	StaticMCPs      []string        `json:"static_mcps"`
	Policies        []APIPolicyRule `json:"policies"`
	SaveSandbox     bool            `json:"save_sandbox"`
}

// APIPolicyRule is a task's sandbox policy rule, with the YAML keys.
type APIPolicyRule struct {
	Action   string `json:"action"`
	Type     string `json:"type"`
	Resource string `json:"resource"`
}

// APITaskSource is the PR signal a review-created task addresses.
type APITaskSource struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
	Repo string `json:"repo,omitempty"`
	PR   int    `json:"pr"`
}

// APIIntegration is a task's full integration: block.
type APIIntegration struct {
	Result    string       `json:"result"`
	Repo      string       `json:"repo,omitempty"`
	Files     []string     `json:"files,omitempty"`
	Onto      []RepoCommit `json:"onto,omitempty"`
	Output    string       `json:"output,omitempty"`
	Published []string     `json:"published,omitempty"`
	Landed    []string     `json:"landed,omitempty"`
	Attempts  int          `json:"attempts,omitempty"`
}

// TaskOutput groups what the commit agent and the daemon recorded as the
// task's result.
type TaskOutput struct {
	Summary       string       `json:"summary"`
	Commits       []RepoCommit `json:"commits"`
	CreatedFiles  []string     `json:"created_files"`
	TouchesActual []string     `json:"touches_actual"`
}

// TaskResource is a task (§A.1.2): the task file merged with the snapshot's
// derived fields.
type TaskResource struct {
	Project       string           `json:"project"`
	Name          string           `json:"name"`
	Seed          bool             `json:"seed,omitempty"`
	Path          string           `json:"path"`
	ETag          string           `json:"etag,omitempty"`
	Status        string           `json:"status"`
	Attempts      int              `json:"attempts"`
	FailureReason string           `json:"failure_reason"`
	BlockedReason string           `json:"blocked_reason"`
	Description   string           `json:"description"`
	DependsOn     []string         `json:"depends_on"`
	Touches       *[]string        `json:"touches"`
	Areas         []string         `json:"areas"`
	Exclusive     bool             `json:"exclusive"`
	Capabilities  TaskCapabilities `json:"capabilities"`
	Source        *APITaskSource   `json:"source"`
	Worktree      string           `json:"worktree,omitempty"`
	TaskBranch    string           `json:"task_branch,omitempty"`
	Base          []RepoCommit     `json:"base"`
	SucceededAt   *time.Time       `json:"succeeded_at"`
	CompletedAt   *time.Time       `json:"completed_at"`
	QueuePosition int              `json:"queue_position"`
	WaitingOn     []string         `json:"waiting_on"`
	Integration   *APIIntegration  `json:"integration"`
	Output        TaskOutput       `json:"output"`
	// Session is the id of a live session working on the task, and Sandbox
	// its sandbox, or the name a task sandbox would get (so a retained one
	// can be found).
	Session string `json:"session,omitempty"`
	Sandbox string `json:"sandbox,omitempty"`
}

// task builds one task resource from its snapshot entry and its file.
func (m *apiModel) task(p *ProjectSnapshot, ts TaskSnapshot) TaskResource {
	r := TaskResource{
		Project:       m.id(*p),
		Name:          ts.Name,
		Path:          ts.path,
		Status:        ts.Status,
		Attempts:      ts.Attempts,
		FailureReason: ts.FailureReason,
		BlockedReason: ts.BlockedReason,
		DependsOn:     cloneOrEmpty(ts.DependsOn),
		Areas:         cloneOrEmpty(ts.Areas),
		Exclusive:     ts.Exclusive,
		Worktree:      ts.Worktree,
		TaskBranch:    ts.TaskBranch,
		Base:          append([]RepoCommit{}, ts.Base...),
		SucceededAt:   utcTime(ts.SucceededAt),
		CompletedAt:   utcTime(ts.CompletedAt),
		QueuePosition: ts.QueuePosition,
		WaitingOn:     cloneOrEmpty(ts.WaitingOn),
		Capabilities:  TaskCapabilities{Model: task.ModelDefault, Harness: task.HarnessDefault, StaticMCPs: []string{}, Policies: []APIPolicyRule{}},
		Output:        TaskOutput{Commits: []RepoCommit{}, CreatedFiles: []string{}, TouchesActual: []string{}},
	}
	if ts.Touches != nil {
		t := append([]string{}, (*ts.Touches)...)
		r.Touches = &t
	}
	if in := ts.Integration; in != nil {
		r.Integration = &APIIntegration{Result: in.Result, Repo: in.Repo, Files: cloneStrings(in.Files), Landed: cloneStrings(in.Landed), Attempts: in.Attempts}
	}
	if ts.path != "" {
		if tf, tag, err := m.d.apiFiles.tasks.get(ts.path); err == nil {
			r.ETag = resourceETag("tk", tag)
			r.Seed = tf.Name == ""
			r.Description = tf.Description
			r.Capabilities = TaskCapabilities{
				Model:           tf.Model,
				ResolvedModel:   tf.ResolvedModel,
				Harness:         tf.Harness,
				ResolvedHarness: tf.ResolvedHarness,
				StaticMCPs:      cloneOrEmpty(tf.StaticMCPs),
				Policies:        []APIPolicyRule{},
				SaveSandbox:     tf.SaveSandbox,
			}
			if r.Capabilities.Harness == "" {
				r.Capabilities.Harness = task.HarnessDefault
			}
			for _, pr := range tf.Policies {
				r.Capabilities.Policies = append(r.Capabilities.Policies, APIPolicyRule{Action: string(pr.Action), Type: string(pr.Kind), Resource: pr.Resource})
			}
			if src := tf.Source; src != nil {
				r.Source = &APITaskSource{Kind: src.Kind, Ref: src.Ref, Repo: src.Repo, PR: src.PR}
			}
			if in := tf.Integration; in != nil && r.Integration != nil {
				r.Integration.Onto = repoCommits(in.Onto)
				r.Integration.Output = in.Output
				r.Integration.Published = cloneStrings(in.Published)
			}
			r.Output = TaskOutput{
				Summary:       tf.Summary,
				Commits:       append([]RepoCommit{}, repoCommits(tf.Commits)...),
				CreatedFiles:  cloneOrEmpty(tf.CreatedFiles),
				TouchesActual: cloneOrEmpty(tf.TouchesActual),
			}
		} else if tag != "" {
			r.ETag = resourceETag("tk", tag)
		}
	}
	// Back-links: a live task (or, while it integrates, commit) session on
	// this task, else the sandbox a task session would get.
	for _, s := range m.sessions {
		if s.ProjectPath != p.Path || s.Task != ts.Name {
			continue
		}
		if s.Kind != agent.TaskAgent.String() && s.Kind != agent.CommitAgent.String() {
			continue
		}
		if r.Session == "" || s.Kind == agent.TaskAgent.String() {
			r.Session, r.Sandbox = sessionID(s), s.SandboxName
		}
	}
	if r.Sandbox == "" && !r.Seed {
		r.Sandbox = runner.SandboxNameFor(agent.TaskAgent, p.Path, ts.Name)
	}
	return r
}

func cloneOrEmpty(s []string) []string { return append([]string{}, s...) }

func (s apiSource) Tasks(_ context.Context, id string, status []string) ([]any, error) {
	m := s.d.model()
	p, err := m.project(id)
	if err != nil {
		return nil, err
	}
	var out []any
	for _, ts := range p.Tasks {
		if len(status) > 0 && !slices.Contains(status, ts.Status) {
			continue
		}
		r := m.task(p, ts)
		redactStrings(&r)
		out = append(out, r)
	}
	return out, nil
}

func (s apiSource) Task(_ context.Context, id, name string) (any, error) {
	m := s.d.model()
	p, err := m.project(id)
	if err != nil {
		return nil, err
	}
	for _, ts := range p.Tasks {
		if ts.Name == name {
			r := m.task(p, ts)
			redactStrings(&r)
			return r, nil
		}
	}
	return nil, fmt.Errorf("%w: project %s has no task %q", api.ErrNotFound, id, name)
}

// ---------------------------------------------------------------------------
// Graph
// ---------------------------------------------------------------------------

// GraphNode is one task in a project graph.
type GraphNode struct {
	Name          string `json:"name"`
	Status        string `json:"status"`
	Exclusive     bool   `json:"exclusive"`
	QueuePosition int    `json:"queue_position,omitempty"`
}

// GraphEdge is a dependency: To depends on From.
type GraphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Graph is GET /projects/{id}/graph: the task DAG as the scheduler loads it
// (taskgraph.Load). Ready is taskgraph's Ready; Runnable is what a
// scheduling pass would admit by the graph alone (the project's slot limit
// and the overlap rules, without the global task cap or the power gate,
// whose effect is in Deferred). Errors holds the load error when the graph
// does not load (the nodes then come from the lenient per-file read the
// snapshot uses) and taskgraph's repair warnings.
type Graph struct {
	Project     string        `json:"project"`
	Nodes       []GraphNode   `json:"nodes"`
	Edges       []GraphEdge   `json:"edges"`
	Ready       []string      `json:"ready"`
	Runnable    []string      `json:"runnable"`
	Deferred    *DeferredInfo `json:"deferred"`
	CommitQueue []string      `json:"commit_queue"`
	Errors      []string      `json:"errors"`
}

func (s apiSource) Graph(_ context.Context, id string) (any, error) {
	m := s.d.model()
	p, err := m.project(id)
	if err != nil {
		return nil, err
	}
	g := Graph{
		Project: id, Nodes: []GraphNode{}, Edges: []GraphEdge{}, Ready: []string{}, Runnable: []string{},
		CommitQueue: cloneOrEmpty(p.CommitQueue), Errors: []string{},
	}
	if p.Live != nil {
		g.Deferred = p.Live.Deferred
	}
	for _, t := range p.Tasks {
		g.Nodes = append(g.Nodes, GraphNode{Name: t.Name, Status: t.Status, Exclusive: t.Exclusive, QueuePosition: t.QueuePosition})
		for _, dep := range t.DependsOn {
			g.Edges = append(g.Edges, GraphEdge{From: dep, To: t.Name})
		}
	}
	tg, err := s.d.apiFiles.graph(p.Path)
	if err != nil {
		g.Errors = append(g.Errors, err.Error())
	} else {
		g.Errors = append(g.Errors, tg.Warnings()...)
		for _, t := range tg.Ready() {
			g.Ready = append(g.Ready, t.Name)
		}
		limit := p.MaxParallelTasks
		if limit > 1 && !s.d.worktreesEnabled() {
			limit = 1
		}
		inflight := tg.InFlight()
		for _, t := range tg.Runnable(inflight, limit-len(inflight)).Admit {
			g.Runnable = append(g.Runnable, t.Name)
		}
	}
	redactStrings(&g)
	return g, nil
}

// ---------------------------------------------------------------------------
// Intake
// ---------------------------------------------------------------------------

// IntakeRequest is an intake/<name>.md file (§A.1.4). Body is set only on
// GET /projects/{id}/intake/{name}.
type IntakeRequest struct {
	Project   string    `json:"project"`
	Name      string    `json:"name"`
	File      string    `json:"file"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	ETag      string    `json:"etag,omitempty"`
	Body      *string   `json:"body,omitempty"`
}

// intakeExts maps an intake file extension to its state (a blank .md is
// "empty").
var intakeExts = map[string]string{".md": api.IntakePending, ".processed": api.IntakeProcessed}

// intakeRequests reads a project's intake/ directory, sorted by name.
func (d *Daemon) intakeRequests(projectID, projectPath string, withBody bool, only string) []IntakeRequest {
	dir := filepath.Join(filepath.Dir(projectPath), "intake")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []IntakeRequest
	for _, e := range entries {
		state, ok := intakeExts[filepath.Ext(e.Name())]
		if e.IsDir() || !ok {
			continue
		}
		name := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		if only != "" && name != only {
			continue
		}
		path := filepath.Join(dir, e.Name())
		body, tag, err := d.apiFiles.intake.get(path)
		if err != nil {
			continue
		}
		if state == api.IntakePending && strings.TrimSpace(body) == "" {
			state = api.IntakeEmpty
		}
		r := IntakeRequest{Project: projectID, Name: name, File: e.Name(), State: state, ETag: resourceETag("in", tag)}
		if info, err := e.Info(); err == nil {
			r.CreatedAt = info.ModTime().UTC()
		}
		if withBody {
			b := body
			r.Body = &b
		}
		out = append(out, r)
	}
	return out
}

func (s apiSource) Intake(_ context.Context, id, state string) ([]any, error) {
	m := s.d.model()
	p, err := m.project(id)
	if err != nil {
		return nil, err
	}
	var out []any
	for _, r := range s.d.intakeRequests(id, p.Path, false, "") {
		if state != "" && state != "all" && r.State != state {
			continue
		}
		redactStrings(&r)
		out = append(out, r)
	}
	return out, nil
}

func (s apiSource) IntakeRequest(_ context.Context, id, name string) (any, error) {
	m := s.d.model()
	p, err := m.project(id)
	if err != nil {
		return nil, err
	}
	if name == "" || name != filepath.Base(name) || name == "." || name == ".." {
		return nil, fmt.Errorf("%w: no intake request %q", api.ErrNotFound, name)
	}
	reqs := s.d.intakeRequests(id, p.Path, true, name)
	if len(reqs) == 0 {
		return nil, fmt.Errorf("%w: project %s has no intake request %q", api.ErrNotFound, id, name)
	}
	// A request both pending and processed (re-filed under a used name):
	// the pending one is the live one.
	r := reqs[0]
	for _, q := range reqs {
		if q.State != api.IntakeProcessed {
			r = q
		}
	}
	redactStrings(&r)
	return r, nil
}

// ---------------------------------------------------------------------------
// Pull requests
// ---------------------------------------------------------------------------

// PullRequest is a project's PR with its review-loop state (§A.1.6).
type PullRequest struct {
	Project    string          `json:"project"`
	Repo       string          `json:"repo"`
	Number     int             `json:"number"`
	URL        string          `json:"url,omitempty"`
	HeadSHA    string          `json:"head_sha,omitempty"`
	State      string          `json:"state,omitempty"`
	Watching   bool            `json:"watching"`
	ReviewPoll *ReviewPollInfo `json:"review_poll"`
	// FixTasks are the project's tasks whose source is this PR.
	FixTasks []string `json:"fix_tasks"`
}

func (s apiSource) Pulls(_ context.Context, id string) ([]any, error) {
	m := s.d.model()
	p, err := m.project(id)
	if err != nil {
		return nil, err
	}
	var out []any
	for _, r := range m.pulls(p) {
		out = append(out, r)
	}
	return out, nil
}

// pulls builds a project's pull-request resources, redacted.
func (m *apiModel) pulls(p *ProjectSnapshot) []PullRequest {
	id := m.id(*p)
	sum := m.summary(p)
	var out []PullRequest
	for _, pr := range sum.PullReqs {
		r := PullRequest{
			Project: id, Repo: pr.Repo, Number: pr.Number, URL: pr.URL, HeadSHA: pr.HeadSHA, State: pr.State,
			Watching: p.WatchingPR && pr.State != "merged" && pr.State != "closed",
			FixTasks: []string{},
		}
		if p.Live != nil && p.Live.ReviewPoll != nil {
			rp := *p.Live.ReviewPoll
			r.ReviewPoll = &rp
		}
		for _, ts := range p.Tasks {
			tf, _, err := m.d.apiFiles.tasks.get(ts.path)
			if err != nil || tf.Source == nil || tf.Source.PR != pr.Number {
				continue
			}
			if tf.Source.Repo != "" && tf.Source.Repo != pr.Repo {
				continue
			}
			r.FixTasks = append(r.FixTasks, ts.Name)
		}
		redactStrings(&r)
		out = append(out, r)
	}
	return out
}

// ---------------------------------------------------------------------------
// File caches
// ---------------------------------------------------------------------------

// apiFiles holds the parsed files the API reads beside the snapshot model.
type apiFiles struct {
	projects *fileCache[*project.Project]
	tasks    *fileCache[*task.Task]
	intake   *fileCache[string]

	gmu    sync.Mutex
	gcache map[string]cachedGraph // keyed by project path
}

type cachedGraph struct {
	sig string
	g   *taskgraph.Graph
	err error
}

func newAPIFiles() *apiFiles {
	return &apiFiles{
		projects: newFileCache(project.Parse),
		tasks:    newFileCache(task.Parse),
		intake:   newFileCache(func(_ string, data []byte) (string, error) { return string(data), nil }),
		gcache:   map[string]cachedGraph{},
	}
}

// graph loads a project's task graph with taskgraph.Load, cached on the same
// stat signature the snapshot collector uses. A missing project is
// fs.ErrNotExist.
func (f *apiFiles) graph(projectPath string) (*taskgraph.Graph, error) {
	tasksDir := filepath.Join(filepath.Dir(projectPath), "tasks")
	sig := projectSignature(projectPath, tasksDir)
	f.gmu.Lock()
	defer f.gmu.Unlock()
	if c, ok := f.gcache[projectPath]; ok && c.sig == sig {
		return c.g, c.err
	}
	if _, err := os.Stat(projectPath); errors.Is(err, fs.ErrNotExist) {
		delete(f.gcache, projectPath)
		return nil, err
	}
	g, err := taskgraph.Load(tasksDir)
	f.gcache[projectPath] = cachedGraph{sig: sig, g: g, err: err}
	return g, err
}
