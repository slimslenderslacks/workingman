package daemon

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
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

// startupScan re-evaluates every .project.yaml under each watched root.
// fsnotify only fires on changes-from-now, so without this call a daemon
// restart strands any project whose file is already on disk in a
// non-terminal state (e.g. status=ready that the previous daemon hadn't
// gotten around to dispatching). Running handleProject for each file is
// safe — it's the same code path fsnotify events drive, including the
// daemon-writer self-filter and session dedup.
func (d *Daemon) startupScan() {
	for _, root := range d.roots {
		var found int
		_ = filepath.WalkDir(root, func(p string, entry fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if entry.IsDir() {
				return nil
			}
			if filepath.Base(p) != ".project.yaml" {
				return nil
			}
			found++
			d.handleProject(p)
			return nil
		})
		d.audit.Log("startup_scan", "root", root, "projects", strconv.Itoa(found))
	}
}

func (d *Daemon) maybeWatchNewDir(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	// Walk the new subtree: install watches on every directory and dispatch
	// any handler-matching files found within. Callers (the TUI's :new
	// command, an agent scaffolding a project) commonly do mkdir+write
	// back-to-back, so by the time we see the directory's Create event the
	// .project.yaml inside has often already landed. Without this scan the
	// file's own Create event is missed (the watch wasn't installed yet)
	// and the project never gets dispatched.
	_ = filepath.WalkDir(path, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if err := d.watcher.Add(p); err != nil {
				d.audit.Log("watch_add_error", "path", p, "err", err.Error())
				return nil
			}
			d.audit.Log("watch_added", "path", p)
			return nil
		}
		if h := d.handlerFor(p); h != nil {
			h(p)
		}
		return nil
	})
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
