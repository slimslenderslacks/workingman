package tui

import "github.com/slimslenderslacks/work/internal/control"

// The project-file writes behind :stop, :start, :wolf, :review, :cleanup,
// :hide, :show and :archive live in internal/control, together with their
// preconditions and refusal text, so every front end refuses the same actions
// with the same words. These wrappers keep the TUI's names for them.

func projectDisplayName(path string) string { return control.DisplayName(path) }

func requestStop(projectPath string) (string, error)  { return control.Stop(projectPath) }
func requestStart(projectPath string) (string, error) { return control.Start(projectPath) }
func summonWolf(projectPath string) (string, error)   { return control.SummonWolf(projectPath) }
func requestCleanup(projectPath string) (string, error) {
	return control.Cleanup(projectPath)
}
func requestHide(projectPath string) (string, error) { return control.Hide(projectPath) }
func requestShow(projectPath string) (string, error) { return control.Show(projectPath) }

func requestReview(projectPath string) (name string, kicked bool, err error) {
	return control.Review(projectPath)
}

func archiveProject(root, projectPath string, wsp control.WorkspaceRemover) error {
	return control.Archive(root, projectPath, wsp)
}
