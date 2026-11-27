package control

import (
	"errors"

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
// confirmation. With [ReportNoop] it is reported instead; opts may also add a
// [Check].
func Hide(projectPath string, opts ...Option) (string, error) {
	return setHide(projectPath, true, opts)
}

// Show clears `hide:`, putting the work stream back in the gallery. Like Hide
// it writes as `daemon`, and showing a stream that is not hidden is refused
// (or, with [ReportNoop], reported).
func Show(projectPath string, opts ...Option) (string, error) {
	return setHide(projectPath, false, opts)
}

// setHide is the shared body of Hide/Show: load, refuse the no-op, flip the
// flag, save.
func setHide(projectPath string, hide bool, opts []Option) (string, error) {
	if projectPath == "" {
		return "", errors.New("no work stream selected")
	}
	name := DisplayName(projectPath)
	o := collect(opts)
	var refusal error
	err := o.update(projectPath, project.WriterDaemon, func(p *project.Project) bool {
		if p.Hide == hide {
			if o.noopNow() {
				return false
			}
			if hide {
				refusal = refuse(ErrState, "%s is already hidden", name)
			} else {
				refusal = refuse(ErrState, "%s is not hidden", name)
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
