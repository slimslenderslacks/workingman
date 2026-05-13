package daemon

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// addTree walks root and registers a watch on every directory it finds.
// fsnotify does not recurse on its own, so new directories created later are
// picked up in handle() when we see a Create event for a directory.
func (d *Daemon) addTree(root string) error {
	return filepath.WalkDir(root, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return d.watcher.Add(p)
		}
		return nil
	})
}

func (d *Daemon) maybeWatchNewDir(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	if err := d.watcher.Add(path); err != nil {
		d.audit.Log("watch_add_error", "path", path, "err", err.Error())
		return true
	}
	d.audit.Log("watch_added", "path", path)
	return true
}

func (d *Daemon) handlerFor(path string) eventHandler {
	base := filepath.Base(path)
	if base == ".project.yaml" {
		return d.handleProject
	}
	// tasks/*.yaml files are observed for audit only — lifecycle reactions
	// happen on session-end. Match the structural pattern "*/tasks/<name>.yaml"
	// so unrelated YAML files in a project root are ignored.
	if strings.HasSuffix(base, ".yaml") && filepath.Base(filepath.Dir(path)) == "tasks" {
		return d.handleTask
	}
	return nil
}

type eventHandler func(path string)
