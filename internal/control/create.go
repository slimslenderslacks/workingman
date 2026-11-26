package control

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/slimslenderslacks/work/internal/pathlock"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/sbx"
	"github.com/slimslenderslacks/work/internal/worktree"
)

// The create-only actions (docs/proposals/workingman-gui.md §A.4.3) do not
// edit a file the daemon or an agent also writes: they add a file the daemon
// reacts to, as a human dropping it in place would. A new project is
// <root>/<name>/project.md; an intake request is <project>/intake/<name>.md.
// Each file is written to a temp name the daemon's handlers ignore and then
// hard-linked to its final name, so the daemon sees it once and complete, and
// a name that already exists is refused atomically instead of overwritten.

// Kinds of create refusal. A refusal's text is meant for the user; errors.Is
// against these tells a caller (the API) which status to answer with.
var (
	// ErrInvalid is a name or body the action cannot use.
	ErrInvalid = errors.New("invalid")
	// ErrExists is a name that is already taken.
	ErrExists = errors.New("exists")
	// ErrNotFound is a project, root or intake request that is not there.
	ErrNotFound = errors.New("not found")
	// ErrProcessed is an intake request the planner has already taken.
	ErrProcessed = errors.New("processed")
	// ErrState is a project whose status refuses the action.
	ErrState = errors.New("invalid state")
)

// refusal is a user-facing error of one of the kinds above.
type refusal struct {
	kind error
	msg  string
}

func (r *refusal) Error() string { return r.msg }
func (r *refusal) Unwrap() error { return r.kind }

func refuse(kind error, format string, args ...any) error {
	return &refusal{kind: kind, msg: fmt.Sprintf(format, args...)}
}

// IntakeDir is the directory of a project's intake requests, beside its
// project.yaml at projectPath.
func IntakeDir(projectPath string) string {
	return filepath.Join(filepath.Dir(projectPath), "intake")
}

// Intake file extensions: a pending request, one the planner has taken, and
// one withdrawn before it was.
const (
	IntakePendingExt   = ".md"
	IntakeProcessedExt = ".processed"
	IntakeWithdrawnExt = ".withdrawn"
)

// MaxIntakeNameLen bounds an intake request's name (without its extension).
const MaxIntakeNameLen = 100

var kebabRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ValidProjectName checks a new work stream's name: one lowercase kebab-case
// path segment, at most sbx.MaxNameLen characters, since it prefixes every
// sandbox name of the work stream. Kebab-case already rules out ".", "..",
// separators and the .worktrees directory.
func ValidProjectName(name string) error {
	if name == "" {
		return refuse(ErrInvalid, "a project needs a name")
	}
	if !kebabRE.MatchString(name) || name == worktree.DirName {
		return refuse(ErrInvalid, "project name %q must be lowercase kebab-case (letters, digits and single hyphens)", name)
	}
	if len(name) > sbx.MaxNameLen {
		return refuse(ErrInvalid, "project name %q is %d characters; the limit is %d (it prefixes the sandbox names)", name, len(name), sbx.MaxNameLen)
	}
	return nil
}

// minTaskNameBudget is the task-name room below which a work-stream name is
// worth a warning: task names must fit in what the work stream leaves of a
// sandbox name.
const minTaskNameBudget = 20

// ProjectNameWarning says how a valid work-stream name will be shortened in
// its sandbox names, or "" when it fits comfortably.
func ProjectNameWarning(name string) string {
	if want := name + "-project"; sbx.SanitizeName(want) != want {
		return fmt.Sprintf("the project agent's sandbox name %q exceeds %d characters and will be shortened", want, sbx.MaxNameLen)
	}
	if n := sbx.NameBudget(name); n < minTaskNameBudget {
		return fmt.Sprintf("%q leaves only %d characters for task names in sandbox names", name, n)
	}
	return ""
}

// NewProject seeds a work stream: it creates <root>/<name>/ and writes
// project.md there with description. The daemon does the rest, as for a
// hand-made seed: it writes project.yaml with the description and launches
// the project agent. It returns project.md's path.
//
// root must be a directory the daemon watches. A name that exists under it
// already, as a project or anything else, is refused (ErrExists).
func NewProject(root, name, description string) (string, error) {
	if err := ValidProjectName(name); err != nil {
		return "", err
	}
	description = strings.TrimSpace(description)
	if description == "" {
		return "", refuse(ErrInvalid, "a project needs a description: it is what the project agent plans from")
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return "", refuse(ErrNotFound, "root %s is not a directory", root)
	}
	dir := filepath.Join(root, name)
	if rel, err := filepath.Rel(root, dir); err != nil || rel != name {
		return "", refuse(ErrInvalid, "project name %q is not a single path segment", name)
	}
	// Mkdir is the atomic claim on the name: of two concurrent creates, one
	// gets ErrExist.
	if err := os.Mkdir(dir, 0o755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return "", refuse(ErrExists, "%s already exists in %s", name, root)
		}
		return "", err
	}
	path := filepath.Join(dir, "project.md")
	if err := createFile(path, []byte(description+"\n")); err != nil {
		// Leave no empty directory behind: a later retry would be refused.
		os.Remove(dir)
		return "", err
	}
	return path, nil
}

// ValidIntakeName checks an intake request's name: one lowercase kebab-case
// segment of at most MaxIntakeNameLen characters, without its .md.
func ValidIntakeName(name string) error {
	if name == "" {
		return refuse(ErrInvalid, "an intake request needs a name")
	}
	if !kebabRE.MatchString(name) {
		return refuse(ErrInvalid, "intake name %q must be lowercase kebab-case (letters, digits and single hyphens), without .md", name)
	}
	if len(name) > MaxIntakeNameLen {
		return refuse(ErrInvalid, "intake name %q is %d characters; the limit is %d", name, len(name), MaxIntakeNameLen)
	}
	return nil
}

// DefaultIntakeName is the name for a request queued without one:
// "<prefix>-<UTC time>-<4 hex>", for example gui-20261126-215700-3fa9.
func DefaultIntakeName(prefix string, now time.Time) string {
	b := make([]byte, 2)
	if _, err := rand.Read(b); err != nil {
		panic("control: crypto/rand: " + err.Error())
	}
	return prefix + "-" + now.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

// IntakePaths are the files an intake request named name may be, in the
// order pending, processed, withdrawn.
func IntakePaths(projectPath, name string) (pending, processed, withdrawn string) {
	dir := IntakeDir(projectPath)
	return filepath.Join(dir, name+IntakePendingExt), filepath.Join(dir, name+IntakeProcessedExt), filepath.Join(dir, name+IntakeWithdrawnExt)
}

// QueueIntake writes <project>/intake/<name>.md with body, for the planner.
// projectPath is the project's project.yaml. The daemon reacts as it does to
// a hand-dropped file (handleIntakeFile): it plans alongside a working
// project and flips any other to ready. A name used before, pending,
// processed or withdrawn, is refused (ErrExists). It returns the file's path.
//
// Two statuses are refused (ErrState), because flipping them to ready would
// do more than queue a request: a stopped project would be un-stopped (its
// stopped_from left behind), and an unpopulated one would skip its project
// agent. A hand-dropped file still does both; the refusal only keeps a front
// end from doing it by accident.
func QueueIntake(projectPath, name, body string) (string, error) {
	if err := ValidIntakeName(name); err != nil {
		return "", err
	}
	if strings.TrimSpace(body) == "" {
		return "", refuse(ErrInvalid, "an intake request needs a body: the daemon ignores a blank one")
	}
	p, err := project.Load(projectPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", refuse(ErrNotFound, "%s has no project.yaml", DisplayName(projectPath))
		}
		return "", err
	}
	switch {
	case p.Status == project.StatusStopped:
		return "", refuse(ErrState, "%s is stopped, and queuing intake would flip it to ready; :start it first", DisplayName(projectPath))
	case p.Unpopulated():
		return "", refuse(ErrState, "%s has no status yet: the project agent has not filled it in", DisplayName(projectPath))
	}
	pending, processed, withdrawn := IntakePaths(projectPath, name)
	defer pathlock.Lock(pending)()
	for _, p := range []string{processed, withdrawn} {
		if _, err := os.Lstat(p); err == nil {
			return "", refuse(ErrExists, "intake request %q was used before (%s); pick another name", name, filepath.Base(p))
		}
	}
	if err := os.MkdirAll(IntakeDir(projectPath), 0o755); err != nil {
		return "", err
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	if err := createFile(pending, []byte(body)); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return "", refuse(ErrExists, "intake request %q is already pending", name)
		}
		return "", err
	}
	return pending, nil
}

// WithdrawIntake withdraws a pending intake request by renaming
// <name>.md to <name>.withdrawn, which the daemon and the planner ignore.
// It races the planner, which renames the file to .processed when it takes
// it, so it is best-effort: a request already taken is refused
// (ErrProcessed). check, when set, runs on the request's bytes before the
// rename and can refuse it (the API's If-Match). It returns the withdrawn
// file's path; noop reports a request that was already withdrawn.
func WithdrawIntake(projectPath, name string, check func(body []byte) error) (path string, noop bool, err error) {
	if err := ValidIntakeName(name); err != nil {
		return "", false, refuse(ErrNotFound, "no intake request %q", name)
	}
	pending, processed, withdrawn := IntakePaths(projectPath, name)
	defer pathlock.Lock(pending)()
	body, err := os.ReadFile(pending)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return "", false, err
		}
		if _, err := os.Lstat(processed); err == nil {
			return "", false, refuse(ErrProcessed, "intake request %q was already taken by the planner", name)
		}
		if _, err := os.Lstat(withdrawn); err == nil {
			return withdrawn, true, nil
		}
		return "", false, refuse(ErrNotFound, "%s has no intake request %q", DisplayName(projectPath), name)
	}
	if check != nil {
		if err := check(body); err != nil {
			return "", false, err
		}
	}
	// A .withdrawn left by a hand edit is not replaced. The lock keeps other
	// in-process writers out between this check and the rename.
	if _, err := os.Lstat(withdrawn); err == nil {
		return "", false, refuse(ErrExists, "intake request %q has both a pending and a withdrawn file; remove one by hand", name)
	}
	if err := os.Rename(pending, withdrawn); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, refuse(ErrProcessed, "intake request %q was taken by the planner while withdrawing it", name)
		}
		return "", false, err
	}
	return withdrawn, false, nil
}

// createFile writes data to path, which must not exist (fs.ErrExist if it
// does). The bytes go to a hidden temp file in the same directory first, one
// no daemon handler matches (it does not end in .md), and are then
// hard-linked to path: the daemon's watcher sees path appear once, complete,
// and an existing path is never replaced.
func createFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmpName, 0o644)
	}
	if err == nil {
		err = os.Link(tmpName, path)
	}
	return err
}
