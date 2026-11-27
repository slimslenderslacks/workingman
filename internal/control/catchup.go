package control

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/slimslenderslacks/work/internal/project"
)

// CatchupMarker is the file, created beside a project.yaml, that asks the
// daemon to fire that project's cron callback once immediately (`orch
// catchup`, the sleepy-head agent and the API's catchup action all write it).
const CatchupMarker = "catchup-request"

// CatchupPath is the catch-up marker of the project whose project.yaml is at
// projectPath.
func CatchupPath(projectPath string) string {
	return filepath.Join(filepath.Dir(projectPath), CatchupMarker)
}

// RequestCatchup asks the daemon to fire the cron callback of the project at
// projectPath (its project.yaml) once, by creating its catch-up marker. The
// daemon consumes the marker and fires the callback only while the project
// is still behind (Daemon.catchUpProject). It returns the marker's path;
// noop reports a marker that is already there, waiting for the daemon.
//
// A project without a cron is refused (ErrState): it has no schedule to catch
// up on. The marker is created, never rewritten, so the daemon sees it once.
func RequestCatchup(projectPath string) (path string, noop bool, err error) {
	if filepath.Base(projectPath) != project.FileName {
		return "", false, refuse(ErrInvalid, "%q is not a %s", projectPath, project.FileName)
	}
	p, err := project.Load(projectPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, refuse(ErrNotFound, "%s has no project.yaml", DisplayName(projectPath))
		}
		return "", false, err
	}
	if p.Cron == "" {
		return "", false, refuse(ErrState, "%s has no cron schedule to catch up on", DisplayName(projectPath))
	}
	path = CatchupPath(projectPath)
	if err := createFile(path, []byte("catchup\n")); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return path, true, nil
		}
		return "", false, fmt.Errorf("catch-up marker: %w", err)
	}
	return path, false, nil
}
