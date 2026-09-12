package task

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/slimslenderslacks/work/internal/policy"
	"gopkg.in/yaml.v3"
)

type Status string

const (
	StatusReady     Status = "ready"
	StatusRunning   Status = "running"
	StatusSuccess   Status = "success"
	StatusFailed    Status = "failed"
	StatusBlocked   Status = "blocked"
	StatusCommitted Status = "committed"
)

// ModelDefault is the placeholder value every task carries today. The
// planning agent writes this verbatim; Load() backfills it for tasks on
// disk that pre-date the field.
const ModelDefault = "default"

// MaxNameLen is the maximum length of a task name. A task name flows into the
// task's sbx sandbox name ("<work-stream>-<task-name>"), which sbx passes to
// Docker as a container name — an RFC 1123 DNS label capped at 63 characters.
// taskgraph.Load repairs (rather than rejects) a name that violates this, so
// this constant is the single source of truth both packages consult.
const MaxNameLen = 63

var nameRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ValidName reports whether name is a well-formed task name: non-empty,
// lowercase kebab-case (letters, digits, and single hyphens between them,
// no leading/trailing/doubled hyphen), and no longer than MaxNameLen. Task
// names become Docker container-name components and taskgraph/depends_on
// keys, so anything else must be repaired before it reaches the graph.
func ValidName(name string) bool {
	return name != "" && len(name) <= MaxNameLen && nameRE.MatchString(name)
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

func (s Status) Valid() bool {
	switch s {
	case StatusReady, StatusRunning, StatusSuccess, StatusFailed, StatusBlocked, StatusCommitted:
		return true
	}
	return false
}

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

	// StaticMCPs are the sbx static-MCP names this task's sandbox should be
	// created with: each entry becomes a `--static-mcp <name>` flag on
	// `sbx create`. Populated by the planning agent when a task needs an MCP
	// server inside its sandbox (web search, github, etc.). Empty means no
	// MCPs and a vanilla sandbox.
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

	// Source records the external signal this task exists to address, set by the
	// review agent when it turns a PR review comment or a failed check into a
	// task. It lets a later review poll correlate a live PR signal with a task it
	// already created — so an unresolved thread that already has an in-flight task
	// isn't duplicated, and a committed task's thread can be resolved once its fix
	// has landed. Nil for ordinary planning-created tasks, which carry no external
	// source. See Source for the field meanings.
	Source *Source `yaml:"source,omitempty"`

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
type Source struct {
	Kind string `yaml:"kind"`
	Ref  string `yaml:"ref"`
	Repo string `yaml:"repo,omitempty"`
	PR   int    `yaml:"pr"`
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

func Load(path string) (*Task, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t Task
	if err := yaml.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	t.Path = path
	if t.Model == "" {
		t.Model = ModelDefault
	}
	return &t, nil
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
func Save(path string, t *Task) error {
	if len(t.Name) > MaxNameLen {
		return fmt.Errorf("task name %q is %d chars, exceeds MaxNameLen %d", t.Name, len(t.Name), MaxNameLen)
	}
	data, err := yaml.Marshal(t)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
