package tui

import (
	"errors"
	"fmt"

	"github.com/slimslenderslacks/work/internal/project"
)

// requestHide sets `hide: true` on the work stream identified by projectPath
// (the path of its .project.yaml) so the gallery stops drawing a card for it,
// and returns the project's display name for the confirmation message.
//
// Unlike every other TUI command, this writes as `daemon` rather than `agent`.
// The request flags (`cleanup:`, `review_now:`) are written as the agent
// precisely so the daemon's fsnotify handler acts on them; hiding must do the
// opposite — it changes nothing about how the work stream runs, so the write
// must not be routed at all. Written as the agent it would land in
// dispatchProject like any other project change and, on a `blocked` stream,
// re-summon the wolf for the sake of a display toggle. See
// project.Project.Hide.
//
// Hiding an already-hidden stream is refused rather than rewritten: it would
// be a no-op file write whose only effect is a misleading "hid X" on the
// status line.
func requestHide(projectPath string) (string, error) {
	return setProjectHide(projectPath, true)
}

// requestShow clears `hide:`, putting the work stream back in the gallery
// unconditionally. Reaching a hidden stream to run this on takes the `tp`
// toggle first (see model.showHidden) — otherwise it has no card to select.
func requestShow(projectPath string) (string, error) {
	return setProjectHide(projectPath, false)
}

// setProjectHide is the shared body of requestHide/requestShow: load, refuse
// the no-op, flip the flag, save.
func setProjectHide(projectPath string, hide bool) (string, error) {
	if projectPath == "" {
		return "", errors.New("no work stream selected")
	}
	p, err := project.Load(projectPath)
	if err != nil {
		return "", err
	}
	name := projectDisplayName(projectPath)
	if p.Hide == hide {
		if hide {
			return "", fmt.Errorf("%s is already hidden", name)
		}
		return "", fmt.Errorf("%s is not hidden", name)
	}
	p.Hide = hide
	if err := project.SaveAs(projectPath, p, project.WriterDaemon); err != nil {
		return "", err
	}
	return name, nil
}
