// Package workspace owns the multi-repo directory each agent runs in.
//
// The production implementation will shell out to `wsp new`, which creates a
// directory at ~/dev/workspaces/<branch>/ holding clones of the requested
// repos all on the same feature branch. The interface defined here mirrors
// the wsp surface so that swapping the stub for the real driver is a one-line
// change inside the daemon wiring.
package workspace

import (
	"context"
	"path"
)

// Repo identifies a repository for wsp. Identity is the canonical name wsp
// uses in its registry (e.g. "github.com/docker/gateway"); Shortname is the
// equivalent unqualified alias ("gateway"). Either is accepted by `wsp new`.
//
// BaseBranch, when set, is the branch the feature branch's HEAD should
// start at when the workspace is first created. Empty means use wsp's
// default behavior (start from the repo's default branch).
type Repo struct {
	Identity   string
	Shortname  string
	BaseBranch string
}

// Ref returns the string form wsp prefers — identity if known, otherwise the
// shortname. Callers should populate at least one.
func (r Repo) Ref() string {
	if r.Identity != "" {
		return r.Identity
	}
	return r.Shortname
}

// DirName is the per-repo directory wsp creates inside the workspace —
// always the last path segment of the Ref (e.g. "mcpruntime" for
// "github.com/docker/mcpruntime"). Used by code that needs to reach into
// the repo on disk after `wsp new` succeeds (e.g. to reset the base
// branch).
func (r Repo) DirName() string {
	if r.Shortname != "" {
		return r.Shortname
	}
	return path.Base(r.Identity)
}

type Manager interface {
	// Create provisions a workspace named branch containing repos. The branch
	// name is also used as the workspace name and the per-repo feature branch,
	// per wsp's defaults. Returns the absolute path to the workspace root.
	Create(ctx context.Context, branch string, repos []Repo) (string, error)

	// Path returns the absolute path to an existing workspace. It does not
	// verify that the directory is healthy — just resolves the location.
	Path(branch string) (string, error)

	// Remove tears the workspace down. Safe to call on a non-existent workspace.
	Remove(ctx context.Context, branch string) error
}
