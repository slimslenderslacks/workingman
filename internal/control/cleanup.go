package control

import (
	"errors"
	"fmt"

	"github.com/slimslenderslacks/work/internal/project"
)

// Cleanup sets the `cleanup: true` request flag on the project identified by
// projectPath (the path of its project.yaml) so the daemon launches the
// archive agent for it. It returns the project's display name for the
// confirmation message.
//
// The write must be made as `agent`, because the daemon drops fsnotify events
// carrying its own `updated_by: daemon` marker and would never see a request
// it wrote itself. See project.Project.Cleanup for the full front end → daemon
// → agent contract.
//
// A project already carrying `archive: true` has been cleaned up, so the
// request is refused rather than issued: re-running the agent on it would at
// best find nothing to commit, and the user's next step is `:archive`, not
// another cleanup. Re-requesting a cleanup that is already in flight is
// harmless — the flag is simply still set, and the daemon dedups the dispatch.
func Cleanup(projectPath string) (string, error) {
	if projectPath == "" {
		return "", errors.New("no work stream selected")
	}
	name := DisplayName(projectPath)
	var refusal error
	err := project.Update(projectPath, project.WriterAgent, func(p *project.Project) bool {
		if p.Archive {
			refusal = fmt.Errorf("%s is already cleaned up — run :archive", name)
			return false
		}
		p.Cleanup = true
		return true
	})
	if err != nil {
		return "", err
	}
	if refusal != nil {
		return "", refusal
	}
	return name, nil
}
