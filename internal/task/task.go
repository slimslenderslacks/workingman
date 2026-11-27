package task

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/slimslenderslacks/work/internal/pathlock"
	"github.com/slimslenderslacks/work/internal/policy"
	"github.com/slimslenderslacks/work/internal/sbx"
	"gopkg.in/yaml.v3"
)

// Status is a task's lifecycle state: ready → running (task agent) →
// success | failed | blocked, and success → committed (commit agent). Any other
// value is rejected on load.
type Status string

const (
	StatusReady     Status = "ready"
	StatusRunning   Status = "running"
	StatusSuccess   Status = "success"
	StatusFailed    Status = "failed"
	StatusBlocked   Status = "blocked"
	StatusCommitted Status = "committed"
)

// MaxAttempts is the retry budget the daemon enforces against Task.Attempts:
// a task that fails (or conflicts at integration) with Attempts already at
// MaxAttempts is not run again, and its project blocks. The TUI shows it as
// the denominator of "attempt n/MaxAttempts".
const MaxAttempts = 3

// ModelDefault is the *requested* model every task carries today: "let claude
// pick its own default". The planning agent writes this verbatim; Load()
// backfills it for tasks on disk that pre-date the field. It is never the model
// a task actually ran under — that concrete id is recorded separately in
// Task.ResolvedModel once the agent starts.
const ModelDefault = "default"

// HarnessDefault is the name of the harness every task runs under unless its
// `harness:` field names another: the claude ACP kit layered onto the
// daemon's configured sbx template (--acp-kit / --acp-image). An empty
// Task.Harness means this one.
const HarnessDefault = "claude-acp"

// MaxNameLen is the ABSOLUTE maximum length of a task name — the ceiling no
// name may exceed regardless of project. A task name flows into the task's sbx
// sandbox name ("<work-stream>-<task-name>"), which sbx passes to Docker as a
// container name: an RFC 1123 DNS label capped at sbx.MaxNameLen characters.
//
// Note what this constant can NOT express: the work stream contributes
// len(work-stream)+1 characters to that same budget, so a name of exactly
// MaxNameLen is always too long in practice. The per-project limit is
// MaxNameLenFor(workStream), and that — not this — is what Load/validation
// should check whenever the work stream is known. MaxNameLen survives as the
// context-free bound for the places that see a task in isolation (Save) and for
// the regex-level ValidName.
const MaxNameLen = sbx.MaxNameLen

// MaxNameLenFor returns the longest task name that still leaves room for the
// work stream in the sandbox name "<workStream>-<task-name>". This is the real
// limit a planning or review agent must respect, and the one taskgraph.Load
// repairs against; the prompts quote this same number back to the agent.
func MaxNameLenFor(workStream string) int { return sbx.NameBudget(workStream) }

var nameRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ValidName reports whether name is a well-formed task name: non-empty,
// lowercase kebab-case (letters, digits, and single hyphens between them,
// no leading/trailing/doubled hyphen), and no longer than MaxNameLen. Task
// names become Docker container-name components and taskgraph/depends_on
// keys, so anything else must be repaired before it reaches the graph.
func ValidName(name string) bool {
	return name != "" && len(name) <= MaxNameLen && nameRE.MatchString(name)
}

// ValidNameFor is ValidName with the project's work stream taken into account:
// the name must additionally fit MaxNameLenFor(workStream), so that
// "<workStream>-<name>" is a legal sandbox name. Callers that know which
// project a task belongs to (taskgraph.Load does) should prefer this —
// ValidName alone accepts names that are individually well-formed but produce a
// sandbox name sbx will refuse to create.
func ValidNameFor(name, workStream string) bool {
	return ValidName(name) && len(name) <= MaxNameLenFor(workStream)
}

// Slugify derives a valid task name from free-form text such as a
// description or a filename stem: lowercased, with runs of characters
// outside [a-z0-9] collapsed to single hyphens, leading/trailing hyphens
// trimmed, and the result capped to MaxNameLen. Returns "task" if nothing
// usable remains (e.g. the input was empty or all punctuation).
func Slugify(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > MaxNameLen {
		slug = strings.Trim(slug[:MaxNameLen], "-")
	}
	if slug == "" {
		return "task"
	}
	return slug
}

// SlugifyFor is Slugify capped to the project's budget instead of the absolute
// MaxNameLen: the result is guaranteed to satisfy ValidNameFor(_, workStream),
// so "<workStream>-<result>" always fits a sandbox name.
//
// When the slug has to be shortened it keeps the leading words and appends a
// short digest of the full slug (see sbx.FitNameTo) rather than just chopping
// the tail. That matters because the names that overflow are usually long and
// similar — "fix-x-in-some-very-long-package" vs. "fix-y-in-some-very-long-..."
// — and a plain chop would collapse them onto one name, turning a cosmetic
// repair into two tasks fighting over one identity. The digest is of the whole
// slug, so the tail that distinguished them still does.
func SlugifyFor(s, workStream string) string {
	slug := sbx.FitNameTo(Slugify(s), MaxNameLenFor(workStream))
	if slug == "" {
		// The work stream alone consumes the entire budget; there is no legal
		// task name under it. Return the unqualified slug so the caller still
		// has a usable task id — the sandbox layer (sbx.SanitizeName) remains
		// the backstop that keeps `sbx create` from ever seeing it over-long.
		return Slugify(s)
	}
	return slug
}

// Valid reports whether s is one of the defined statuses.
func (s Status) Valid() bool {
	switch s {
	case StatusReady, StatusRunning, StatusSuccess, StatusFailed, StatusBlocked, StatusCommitted:
		return true
	}
	return false
}

// UnmarshalYAML rejects any value that is not a defined Status.
func (s *Status) UnmarshalYAML(unmarshal func(any) error) error {
	var raw string
	if err := unmarshal(&raw); err != nil {
		return err
	}
	candidate := Status(raw)
	if !candidate.Valid() {
		return fmt.Errorf("invalid task status %q", raw)
	}
	*s = candidate
	return nil
}

// Task is the parsed contents of one tasks/*.yaml file: the unit of work the
// planning agent creates, a task agent carries out and the commit agent
// lands. A blank Name with a description is a pending seed waiting for the
// planning agent to name it (see taskgraph.Load).
type Task struct {
	Name          string   `yaml:"name"`
	Description   string   `yaml:"description"`
	DependsOn     []string `yaml:"depends_on"`
	Status        Status   `yaml:"status"`
	Attempts      int      `yaml:"attempts"`
	FailureReason string   `yaml:"failure_reason"`
	BlockedReason string   `yaml:"blocked_reason"`

	// Summary is a short prose record of what was done, written by the
	// commit agent when it transitions the task to `committed`. Empty on a
	// task that hasn't been committed yet.
	Summary string `yaml:"summary,omitempty"`

	// Commits records the git commits the commit agent produced — one
	// entry per repo that had modifications. Empty when the work didn't
	// require any commits (e.g. the task was a no-op or its only output
	// landed in CreatedFiles).
	Commits []Commit `yaml:"commits,omitempty"`

	// CreatedFiles lists files the agent created outside of any commit:
	// untracked files left in repo working trees, scratch notes in the
	// workspace root, etc. Useful for surfacing artifacts that don't show
	// up in `git log`.
	CreatedFiles []string `yaml:"created_files,omitempty"`

	// CompletedAt is when the daemon observed this task reach `committed` —
	// i.e. when its work was fully landed. It is stamped by the daemon in its
	// commit-session callback, AFTER the commit agent has exited, so an agent
	// rewriting the task file can never clobber it. A pointer so it omits
	// cleanly from YAML until set; nil on tasks that haven't committed (or that
	// committed before this field existed). The TUI sorts the Tasks pane by it
	// to show tasks in the order they actually ran/completed.
	CompletedAt *time.Time `yaml:"completed_at,omitempty"`

	// StaticMCPs are the MCP server names this task's sandbox should reach.
	// Populated by the planning agent when a task needs an MCP server inside
	// its sandbox (web search, github, etc.). They are no longer passed to
	// `sbx create --static-mcp`: when workingman runs with --mcp-gateway-url,
	// they are the catalog server names of the sandbox's per-sandbox
	// standalone-codemode gateway (POST /gateways {"servers": StaticMCPs}),
	// handed to the sandbox as MCP_URL. Empty means the configured default
	// set (--mcp-gateway-servers). See internal/mcpgateway.
	StaticMCPs []string `yaml:"static_mcps,omitempty"`

	// Policies are per-task sandbox policy rules applied with `sbx policy`
	// after the sandbox is created and before any `sbx exec` runs in it.
	// Each rule is an allow/deny decision over a network or filesystem
	// resource pattern. The default sandbox policy is "balanced" — many
	// common resources are already permitted — so rules here typically
	// tighten the network surface (deny all + allow a known host) or
	// loosen filesystem access for a task that needs more.
	Policies []policy.Rule `yaml:"policies,omitempty"`

	// SaveSandbox, when true, tells the acp-wrapper to leave this task's
	// sandbox in place when its agent exits instead of tearing it down with
	// `sbx rm --force`. It defaults to false so per-task sandboxes are cleaned
	// up as tasks complete and don't accumulate across a project's run. The
	// daemon flips it to true when a task fails terminally (retries exhausted,
	// or the agent reported blocked) so the retained sandbox is available for
	// the wolf agent to inspect; a human or the planner may also set it to
	// preserve in-sandbox state that should remain acquirable. omitempty so it
	// stays out of the YAML for the common (false) case.
	SaveSandbox bool `yaml:"save_sandbox,omitempty"`

	// Model is the claude model the task agent should run under. For now
	// every task uses "default", which lets claude pick its own current
	// default; the field is reserved so a future planner can choose a
	// cheaper/heavier model per task (e.g. "haiku" for trivial tasks).
	// Load() backfills empty values to "default" so older tasks on disk
	// continue to work without a planner rewrite.
	Model string `yaml:"model"`

	// ResolvedModel is the concrete model id the task's agent actually ran
	// under (e.g. "claude-opus-5-5") — what Model resolved to once claude
	// picked it. Written by the acp-wrapper as soon as the agent reports it
	// (claude-agent-acp tags each usage_update with the model), so a
	// requested "default" or alias ("opus") is made explicit. Empty until the
	// task's agent has started; Model remains the requested value.
	ResolvedModel string `yaml:"resolved_model,omitempty"`

	// Harness optionally names the agent harness (sandbox template + kit) the
	// task should run under. Empty means the daemon's configured default
	// (HarnessDefault: the claude ACP kit on --acp-image), which is the only
	// harness that works today; the field exists so a planner can later pick a
	// different harness per task. An unknown name fails the launch.
	Harness string `yaml:"harness,omitempty"`

	// ResolvedHarness records the sbx template the task's sandbox was
	// actually created from (the `sbx create -t` image, or "claude" for the
	// stock template). Written by the acp-wrapper once the sandbox exists.
	ResolvedHarness string `yaml:"resolved_harness,omitempty"`

	// Source records the external signal this task exists to address, set by the
	// review agent when it turns a PR review comment or a failed check into a
	// task. It lets a later review poll correlate a live PR signal with a task it
	// already created — so an unresolved thread that already has an in-flight task
	// isn't duplicated, and a committed task's thread can be resolved once its fix
	// has landed. The planning agent sets an intake source (SourceKindIntake) on
	// the tasks it creates for an intake request; other planned tasks carry none.
	// See Source for the field meanings.
	Source *Source `yaml:"source,omitempty"`

	// Touches lists the globs (first segment the repo's workspace dir name)
	// the task expects to create, edit or delete. Written by the planner or the
	// wolf as a scheduling hint (docs/parallel-task-worktrees.md H1). A pointer
	// so absent (nil: no hint, see Exclusive) stays distinct from an explicit
	// `touches: []` ("touches nothing"), which a plain omitempty slice would
	// drop on the next Save.
	Touches *[]string `yaml:"touches,omitempty"`

	// Areas are kebab-case tags for shared hotspots (go-deps, db-schema, ...).
	// Two tasks with a tag in common overlap whatever their Touches say (H1).
	Areas []string `yaml:"areas,omitempty"`

	// Worktree is the absolute path of the task's worktree root
	// (<shared>/.worktrees/<task>). Daemon-written when the worktree is first
	// cut, kept across retries and re-runs, cleared after cleanup at
	// `committed` (W4, W6). Empty means no worktree: the task runs in the
	// shared tree (the legacy path).
	Worktree string `yaml:"worktree,omitempty"`

	// TaskBranch is the per-task branch, wm/<project-branch>/<task>.
	// Daemon-written and stored rather than recomputed, so a later rename of
	// project.branch cannot orphan it (W3). Empty selects the legacy commit
	// path; see HasTaskBranch.
	TaskBranch string `yaml:"task_branch,omitempty"`

	// Base is the per-repo shared tip the worktree was cut from, re-stamped
	// as the merge base each time the task reaches `success` (W4).
	Base []Commit `yaml:"base,omitempty"`

	// SucceededAt is when the daemon first observed this task at `success`.
	// It orders the commit queue (S5) and is cleared when the task goes back
	// to `ready`.
	SucceededAt *time.Time `yaml:"succeeded_at,omitempty"`

	// Integration is the outcome of the latest attempt to land this task on
	// the shared branch (I4, I5). Cleared on `committed`.
	Integration *Integration `yaml:"integration,omitempty"`

	// TouchesActual lists changed paths outside the declared hints, recorded
	// by the daemon after a successful integration. Informational only (H3).
	TouchesActual []string `yaml:"touches_actual,omitempty"`

	// Path is the absolute file path the task was loaded from. It is set by
	// Load and excluded from YAML so it round-trips cleanly. Callers use it
	// when they need to read or write the task file again (e.g. the daemon
	// reloading after a session ends), so they never have to reconstruct a
	// path from Name — filenames may carry sort prefixes ("00-foo.yaml")
	// that don't match the task's identity.
	Path string `yaml:"-"`
}

// Commit captures a single git commit produced by the commit agent during a
// task's commit phase. Repo is the workspace-relative directory name of the
// repo that was committed in (matches workspace.Repo.DirName()); Hash is
// the full SHA from `git rev-parse HEAD`.
type Commit struct {
	Repo string `yaml:"repo"`
	Hash string `yaml:"hash"`
}

// Integration is the commit agent's report of its latest attempt to land
// this task on the shared branch (see docs/parallel-task-worktrees.md I4).
// Result, Repo, Files, Onto, Output and Published are written by the commit
// agent; Landed and Attempts by the daemon.
type Integration struct {
	Result    string   `yaml:"result"`              // conflict | verify_failed | error | partial
	Repo      string   `yaml:"repo,omitempty"`      // repo that failed
	Files     []string `yaml:"files,omitempty"`     // conflicting paths, repo-relative
	Onto      []Commit `yaml:"onto,omitempty"`      // shared tips it tried to rebase onto
	Output    string   `yaml:"output,omitempty"`    // verify_failed only: tail of the build check's output
	Published []string `yaml:"published,omitempty"` // repos already fast-forwarded (partial only)
	Landed    []string `yaml:"landed,omitempty"`    // tasks integrated between base and onto (daemon)
	Attempts  int      `yaml:"attempts,omitempty"`  // commit-agent retries for this task run (daemon)
}

// Source identifies the external PR signal a review-agent-created task exists to
// address. Kind is "pr-comment" (Ref is the GitHub review-thread node id, e.g.
// "PRRT_kwDO...") or "pr-check" (Ref is the failing check/workflow name); Repo is
// the "org/name" the signal came from and PR its pull request number. Repo
// disambiguates when a project watches a PR in several repos at once (check
// names and even PR numbers can collide across repos). Used by the review agent
// to dedup signals against tasks and to resolve the right thread once a fix
// lands.
//
// Kind may also be "pr-push" (SourceKindPush): a task the review agent inserts,
// depending on a batch of commit-only fix tasks, whose sole job is to publish
// those accumulated commits to one repo's PR branch — Repo names that repo. See
// IsPushTask.
//
// Kind may also be "intake" (SourceKindIntake): a back-reference the planning
// agent records on each task it creates for an intake request, with Ref the
// request's name (see IntakeRefFor). Repo and PR are unused for it, and it is
// not a PR signal: see IsPR.
type Source struct {
	Kind string `yaml:"kind"`
	Ref  string `yaml:"ref"`
	Repo string `yaml:"repo,omitempty"`
	PR   int    `yaml:"pr"`
}

// intakeSource is how an intake Source is written: the same keys without the
// PR fields, so a planner-created task doesn't carry a meaningless `pr: 0`.
// PR sources keep Source's own layout, `pr: 0` included (a push task has no PR
// yet), so their files are byte-for-byte what they always were.
type intakeSource struct {
	Kind string `yaml:"kind"`
	Ref  string `yaml:"ref"`
	Repo string `yaml:"repo,omitempty"`
	PR   int    `yaml:"pr,omitempty"`
}

// MarshalYAML writes an intake source without `pr: 0` and every other source
// unchanged.
func (s Source) MarshalYAML() (any, error) {
	if s.Kind == SourceKindIntake {
		return intakeSource(s), nil
	}
	type plain Source
	return plain(s), nil
}

// UnmarshalYAML decodes a source leniently. The block is written by agents, a
// planner may get it wrong, and nothing depends on it being present, so a
// malformed one (a scalar, a list, a field of the wrong type) must not stop the
// whole task file from loading: the fields that decode are kept, the rest are
// left zero, and Parse drops a source that ends up empty.
func (s *Source) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return nil
	}
	type plain Source
	var p plain
	if err := n.Decode(&p); err == nil {
		*s = Source(p)
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i].Value, n.Content[i+1]
		switch k {
		case "kind":
			_ = v.Decode(&s.Kind)
		case "ref":
			_ = v.Decode(&s.Ref)
		case "repo":
			_ = v.Decode(&s.Repo)
		case "pr":
			_ = v.Decode(&s.PR)
		}
	}
	return nil
}

// DisplayModel is the model to show for the task: the concrete model it ran
// under when known, otherwise the requested one.
func (t *Task) DisplayModel() string {
	if t.ResolvedModel != "" {
		return t.ResolvedModel
	}
	if t.Model != "" {
		return t.Model
	}
	return ModelDefault
}

// DisplayHarness is the harness to show for the task: the sbx template its
// sandbox was created from when known, otherwise the requested harness name
// (HarnessDefault when none was requested).
func (t *Task) DisplayHarness() string {
	if t.ResolvedHarness != "" {
		return t.ResolvedHarness
	}
	if t.Harness != "" {
		return t.Harness
	}
	return HarnessDefault
}

// SourceKindPush is the Source.Kind marking a push task: it carries no code work
// of its own, it publishes the commit-only fixes other review tasks left in the
// worktree. The review agent inserts one (depending on those fixes) to batch a
// set of changes for bulk review rather than pushing every fix commit
// individually. The daemon routes such a task straight to the commit agent in
// push mode, skipping the (work-less) task-agent phase.
const SourceKindPush = "pr-push"

// IsPushTask reports whether this task is a review-agent push task (see
// SourceKindPush).
func (t *Task) IsPushTask() bool {
	return t.Source != nil && t.Source.Kind == SourceKindPush
}

// SourceKindIntake is the Source.Kind the planning agent records on a task it
// created for an intake request: `source: {kind: intake, ref: <name>}`, with
// <name> as IntakeRefFor gives it. Best-effort: the planner may leave it out,
// and nothing in the daemon depends on it.
const SourceKindIntake = "intake"

// IsPR reports whether the source is a PR signal the review agent created the
// task for (pr-comment, pr-check or pr-push), as opposed to an intake
// back-reference or a kind this version doesn't know.
func (s *Source) IsPR() bool {
	return s != nil && strings.HasPrefix(s.Kind, "pr-")
}

// IntakeRef is the name of the intake request this task was planned from, or
// "" when it records none.
func (t *Task) IntakeRef() string {
	if t.Source == nil || t.Source.Kind != SourceKindIntake {
		return ""
	}
	return t.Source.Ref
}

// IntakeRefFor is the Source.Ref for the intake file at path: its base name
// without the extension ("intake/foo.md" and "intake/foo.processed" are both
// "foo"). It is the name the API lists the request under, and it survives the
// planner's .md → .processed rename.
func IntakeRefFor(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// Exclusive reports whether the task declares no independence hints at all —
// neither touches nor areas — and so overlaps every other task (H1). An
// explicit `touches: []` is not exclusive: it means "touches nothing". This is
// the single place the rule lives so taskgraph and the TUI agree on it.
func (t *Task) Exclusive() bool {
	return t.Touches == nil && len(t.Areas) == 0
}

// HasTaskBranch reports whether the task runs on its own per-task branch
// (and worktree). A task without one — every task today, and any task an
// older daemon started — takes the legacy path: it works and commits in the
// shared tree (D7).
func (t *Task) HasTaskBranch() bool {
	return t.TaskBranch != ""
}

// Load reads and parses the task file at path, records path in Task.Path, and
// backfills an empty Model to ModelDefault.
func Load(path string) (*Task, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(path, data)
}

// Parse is Load for a file already read: it parses data as the task file at
// path, records path in Task.Path and backfills an empty Model.
func Parse(path string, data []byte) (*Task, error) {
	var t Task
	if err := yaml.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	t.Path = path
	if t.Source != nil && *t.Source == (Source{}) {
		t.Source = nil // a malformed source block (see Source.UnmarshalYAML)
	}
	if t.Model == "" {
		t.Model = ModelDefault
	}
	return &t, nil
}

// Update loads the task at path, applies fn and, when fn reports that it
// changed something, saves the result back. It is how processes other than the
// task's agent (the acp-wrapper recording the resolved model and harness)
// annotate a task file without clobbering fields they don't own.
//
// The load, fn and the save all run under the in-process write lock for path
// (see internal/pathlock), so two writers in the same process updating one
// task file can't lose each other's change. fn must not save or update the
// same path itself: the lock is not reentrant.
func Update(path string, fn func(*Task) bool) error {
	return UpdateIf(path, nil, fn)
}

// UpdateIf is Update with a check: under the write lock, check runs on the
// file's bytes before they are parsed, and an error from it is returned
// without fn running or anything being written. The API's If-Match on a task
// is such a check (docs/proposals/workingman-gui.md §A.4.1). A nil check is
// Update.
func UpdateIf(path string, check func(data []byte) error, fn func(*Task) bool) error {
	defer pathlock.Lock(path)()
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if check != nil {
		if err := check(data); err != nil {
			return err
		}
	}
	t, err := Parse(path, data)
	if err != nil {
		return err
	}
	if !fn(t) {
		return nil
	}
	return saveLocked(path, t)
}

// Save writes t to path as YAML. It is the single choke point every Go writer
// of a task file passes through, so it asserts the MaxNameLen invariant here
// rather than silently repairing it: a name longer than MaxNameLen is refused
// with an error, and nothing is written. Save deliberately does NOT truncate,
// because it sees one task in isolation — silently shortening a name would
// break every other task's depends_on that referenced the full name, with no
// way to fix those references. Repairing an over-length name (with graph
// context and dedupe) is taskgraph.Load's job, which does it loudly via
// Warnings(). An over-length name reaching Save therefore signals a Go-side
// bug, and this error surfaces it (daemon: audit task_save_error; TUI: returned
// to the caller) instead of corrupting the file. An empty Name is allowed — it
// is the "human-added seed" signal the planning agent fills in later.
//
// The write holds the in-process write lock for path, so it never interleaves
// with another Save or Update of the same file in this process. A caller that
// loads, mutates and saves should use Update so the whole read-modify-write
// runs under the lock.
func Save(path string, t *Task) error {
	defer pathlock.Lock(path)()
	return saveLocked(path, t)
}

// saveLocked is Save for a caller that already holds the write lock for path.
func saveLocked(path string, t *Task) error {
	if len(t.Name) > MaxNameLen {
		return fmt.Errorf("task name %q is %d chars, exceeds MaxNameLen %d", t.Name, len(t.Name), MaxNameLen)
	}
	data, err := yaml.Marshal(t)
	if err != nil {
		return err
	}
	return writeAtomic(path, data)
}

// writeAtomic replaces path with data through a temporary file in the same
// directory and a rename, so a concurrent reader sees the old file or the new
// one, never a half-written one. The daemon reads task files while it writes
// others (its scheduling passes run beside the launches of the tasks they
// admit), and a torn read would miscount the tasks in flight. The temporary
// name does not end in .yaml, so task loaders and the watcher skip it.
func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o644)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}
