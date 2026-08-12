package tui

import (
	"errors"
	"fmt"

	"github.com/slimslenderslacks/work/internal/project"
)

// requestCleanup flips the `cleanup: true` request flag on the project
// identified by projectPath (the path of its .project.yaml) so the daemon
// launches the archive agent for it. It returns the project's display name for
// the confirmation message.
//
// Like summonWolf, the TUI has no direct handle on the runner, so the project
// file is the channel: the write must be made as `agent`, because the daemon
// drops fsnotify events carrying its own `updated_by: daemon` marker and would
// never see a request it wrote itself. See project.Project.Cleanup for the full
// TUI → daemon → agent contract.
//
// A project already carrying `archive: true` has been cleaned up, so the
// request is refused rather than issued: re-running the agent on it would at
// best find nothing to commit, and the user's next step is `:archive`, not
// another cleanup. Re-requesting a cleanup that is already in flight is
// harmless — the flag is simply still set, and the daemon dedups the dispatch.
func requestCleanup(projectPath string) (string, error) {
	if projectPath == "" {
		return "", errors.New("no work stream selected")
	}
	p, err := project.Load(projectPath)
	if err != nil {
		return "", err
	}
	name := projectDisplayName(projectPath)
	if p.Archive {
		return "", fmt.Errorf("%s is already cleaned up — run :archive", name)
	}
	p.Cleanup = true
	if err := project.SaveAs(projectPath, p, project.WriterAgent); err != nil {
		return "", err
	}
	return name, nil
}
