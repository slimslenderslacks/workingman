package control

import (
	"errors"
	"fmt"

	"github.com/slimslenderslacks/work/internal/project"
)

// Hide sets `hide: true` on the work stream identified by projectPath (the
// path of its project.yaml) so the gallery stops drawing a card for it, and
// returns the project's display name for the confirmation message.
//
// Unlike every other action here, this writes as `daemon` rather than
// `agent`. The request flags (`cleanup:`, `review_now:`) are written as the
// agent precisely so the daemon's fsnotify handler acts on them; hiding must
// do the opposite — it changes nothing about how the work stream runs, so the
// write must not be routed at all. Written as the agent it would land in
// dispatchProject like any other project change and, on a `blocked` stream,
// re-summon the wolf for the sake of a display toggle. See
// project.Project.Hide.
//
// Hiding an already-hidden stream is refused rather than rewritten: it would
// be a no-op file write whose only effect is a misleading "hid X"
// confirmation.
func Hide(projectPath string) (string, error) {
	return setHide(projectPath, true)
}

// Show clears `hide:`, putting the work stream back in the gallery. Like Hide
// it writes as `daemon`, and showing a stream that is not hidden is refused.
func Show(projectPath string) (string, error) {
	return setHide(projectPath, false)
}

// setHide is the shared body of Hide/Show: load, refuse the no-op, flip the
// flag, save.
func setHide(projectPath string, hide bool) (string, error) {
	if projectPath == "" {
		return "", errors.New("no work stream selected")
	}
	name := DisplayName(projectPath)
	var refusal error
	err := project.Update(projectPath, project.WriterDaemon, func(p *project.Project) bool {
		if p.Hide == hide {
			if hide {
				refusal = fmt.Errorf("%s is already hidden", name)
			} else {
				refusal = fmt.Errorf("%s is not hidden", name)
			}
			return false
		}
		p.Hide = hide
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
