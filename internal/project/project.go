package project

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Writer string

const (
	WriterAgent  Writer = "agent"
	WriterDaemon Writer = "daemon"
)

func (w Writer) Valid() bool {
	return w == WriterAgent || w == WriterDaemon
}

type Repo struct {
	Org  string `yaml:"org"`
	Name string `yaml:"name"`
	// BaseBranch is the branch the workspace's feature branch should start
	// from when the workspace is first created. Defaults to the repo's
	// default branch (typically `main`) when empty. Only applied on first
	// workspace creation — once the workspace exists, the agent's commits
	// own the branch's HEAD.
	BaseBranch string `yaml:"base_branch,omitempty"`
	// Visibility is only meaningful for entries under `new_repos:` — it sets
	// the visibility of the repo the orchestrator creates ("private" or
	// "public"). Empty defaults to private. Ignored for existing `repos:`.
	Visibility string `yaml:"visibility,omitempty"`
}

// UnmarshalYAML accepts a repo written either as the canonical mapping
//
//   - org: docker
//     name: desktop
//     base_branch: integration   # optional
//
// or as the "org/name" string shorthand agents tend to write naturally
// (optionally "org/name@base_branch"). Supporting the shorthand keeps a
// single malformed-looking entry from failing the whole file's parse — which
// otherwise makes the project silently vanish from the TUI.
func (r *Repo) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		spec := value.Value
		base := ""
		if at := strings.IndexByte(spec, '@'); at >= 0 {
			spec, base = spec[:at], spec[at+1:]
		}
		parts := strings.SplitN(spec, "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return fmt.Errorf("invalid repo %q: want \"org/name\" or an {org, name} mapping", value.Value)
		}
		r.Org, r.Name, r.BaseBranch = parts[0], parts[1], base
		return nil
	}
	// Mapping form. Decode via an alias type so we don't recurse into this
	// method.
	type rawRepo Repo
	var raw rawRepo
	if err := value.Decode(&raw); err != nil {
		return err
	}
	*r = Repo(raw)
	return nil
}

type Project struct {
	Description string `yaml:"description"`
	Repos       []Repo `yaml:"repos"`
	// NewRepos are repositories that do NOT yet exist and must be created
	// (empty) on the remote before the workspace is provisioned — for a
	// project whose description asks to start a brand-new repo. They're
	// created at workspace-provisioning time (the planning dispatch) and
	// included in the workspace alongside `Repos`. Same shape as Repos; the
	// per-entry `visibility` applies here (default private).
	NewRepos []Repo `yaml:"new_repos,omitempty"`
	Branch   string `yaml:"branch"`
	// Status omits itself when empty so a `:new` seed (description only, no
	// status yet) doesn't write `status: ""` — which the Status unmarshaler
	// would otherwise have to special-case on every read.
	Status Status `yaml:"status,omitempty"`
	Cron   string `yaml:"cron,omitempty"`
	// BlockedReason is set by the daemon when transitioning a project to
	// `status: blocked` so the cause survives a daemon restart and is
	// visible to both humans reading the file and the wolf agent. Left
	// empty for any non-blocked state. Cleared by whichever agent moves
	// the project back out of blocked (planning, wolf).
	BlockedReason string `yaml:"blocked_reason,omitempty"`
	// Archive means the project has been cleaned up (final commit pushed) and
	// is now safe to archive. It is written by the cleanup/archive agent on
	// success; `:archive` refuses to archive a project without it, and the TUI
	// renders such a project with a blue border. It is an independent flag,
	// not a Status. `omitempty` keeps existing project files byte-identical
	// until the flag is actually set.
	Archive bool `yaml:"archive,omitempty"`
	// CreatedAt is stamped by the daemon the first time it observes a
	// populated .project.yaml (i.e. just after the project agent fills in
	// description/branch/status). Used by the TUI to order work streams
	// most-recent-first. Pointer + omitempty so the field stays out of
	// the YAML on disk until the daemon writes it.
	CreatedAt *time.Time `yaml:"created_at,omitempty"`
	UpdatedBy Writer     `yaml:"updated_by"`
}

// Empty reports whether the file is the unpopulated placeholder the project
// agent is meant to fill in. An empty file on disk is the canonical signal,
// but we also treat a parsed-but-fieldless document as empty.
func (p *Project) Empty() bool {
	return p.Description == "" && len(p.Repos) == 0 && len(p.NewRepos) == 0 &&
		p.Branch == "" && p.Status == ""
}

// Unpopulated reports whether the project still needs the project agent to
// fill it in. A populated project always carries a status (the project agent
// sets `ready` when it finishes); a truly empty file *and* a description-only
// seed (written by `:new`) both have an empty status. This is the broader
// signal the daemon routes on — Empty() only catches the fully blank file,
// which would miss a seed that already has a description.
func (p *Project) Unpopulated() bool {
	return p.Status == ""
}

func Load(path string) (*Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return &Project{}, nil
	}
	var p Project
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &p, nil
}

// Save writes the project file with UpdatedBy forced to daemon. The daemon
// uses this marker to ignore fsnotify events triggered by its own writes.
// Agents that need to write the file should use SaveAs(WriterAgent).
func Save(path string, p *Project) error {
	return SaveAs(path, p, WriterDaemon)
}

func SaveAs(path string, p *Project, by Writer) error {
	if !by.Valid() {
		return fmt.Errorf("invalid writer %q", by)
	}
	out := *p
	out.UpdatedBy = by
	data, err := yaml.Marshal(&out)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
