package control

import "path/filepath"

// DisplayName is the human-facing name for the project whose project.yaml
// lives at path — the basename of its control directory, which is what the
// daemon and sandbox naming also key off. Empty path yields a placeholder so a
// caller never renders a bare "in ".
func DisplayName(path string) string {
	if path == "" {
		return "(none)"
	}
	return filepath.Base(filepath.Dir(path))
}
