package taskgraph

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/slimslenderslacks/work/internal/task"
)

// areaRE is the shape of an areas tag: lowercase kebab-case, like a task name.
var areaRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// badHint returns why a task's independence hints can't be read, or "" when
// they can (H1). A glob is bad when it is blank, absolute, climbs out of the
// workspace with a `..` segment, or has a segment path.Match rejects; a tag is bad when
// it is not kebab-case. Load treats a task with any bad hint as exclusive.
func badHint(t *task.Task) string {
	if t.Touches != nil {
		for _, g := range *t.Touches {
			if why := badGlob(g); why != "" {
				return fmt.Sprintf("touches glob %q %s", g, why)
			}
		}
	}
	for _, a := range t.Areas {
		if !areaRE.MatchString(a) {
			return fmt.Sprintf("areas tag %q is not kebab-case", a)
		}
	}
	return ""
}

func badGlob(g string) string {
	g = strings.TrimSpace(g)
	switch {
	case g == "":
		return "is blank"
	case strings.HasPrefix(g, "/"):
		return "is absolute"
	}
	for _, s := range strings.Split(g, "/") {
		if s == ".." {
			return "climbs out with .."
		}
	}
	segs := globSegments(g)
	if len(segs) == 0 {
		return "names no path"
	}
	for _, s := range segs {
		if _, err := path.Match(s, ""); err != nil {
			return "is malformed"
		}
	}
	return ""
}

// effectiveHints works out, for every task whose hints for overlap purposes
// differ from what its file declares, the copy Runnable and Graph.Overlap
// compare instead (H1, H3):
//
//   - a task with a bad glob or tag is exclusive, and the repair is reported
//     as a warning;
//   - a task carrying an integration report with conflicting files (a re-run
//     after a conflict, until it lands) has those files added to its touches,
//     each as "<repo>/<file>", so it can't run beside another task declaring
//     them. An exclusive task stays exclusive.
//
// The task files are not rewritten; the copies live only in the Graph.
func (g *Graph) effectiveHints() {
	g.effective = map[string]*task.Task{}
	for _, t := range g.Tasks() {
		name := t.Name
		if why := badHint(t); why != "" {
			e := *t
			e.Touches, e.Areas = nil, nil
			g.effective[name] = &e
			g.warnings = append(g.warnings, fmt.Sprintf(
				"taskgraph: task %q has an invalid hint (%s); scheduling it as exclusive", name, why))
			continue
		}
		if t.Exclusive() || t.Integration == nil || len(t.Integration.Files) == 0 {
			continue
		}
		repo := t.Integration.Repo
		if repo == "" {
			repo = "*"
		}
		var touches []string
		if t.Touches != nil {
			touches = append(touches, *t.Touches...)
		}
		for _, f := range t.Integration.Files {
			if f = strings.Trim(strings.TrimSpace(f), "/"); f != "" {
				touches = append(touches, repo+"/"+f)
			}
		}
		e := *t
		e.Touches = &touches
		g.effective[name] = &e
	}
}

// hinted returns the copy of t whose hints scheduling compares: its effective
// hints when they differ from the file's, otherwise t itself. Lookup is by
// name, so any copy of a graph task resolves the same way.
func (g *Graph) hinted(t *task.Task) *task.Task {
	if e, ok := g.effective[t.Name]; ok {
		return e
	}
	return t
}

// Overlap is the package-level Overlap over the two tasks' effective hints:
// a task with a bad hint is exclusive, and a re-run after a conflict also
// touches the files it conflicted in (H3).
func (g *Graph) Overlap(a, b *task.Task) bool {
	return Overlap(g.hinted(a), g.hinted(b))
}

// EffectiveTouches returns the touches globs scheduling uses for t (see
// Graph.Overlap): nil for an exclusive task.
func (g *Graph) EffectiveTouches(t *task.Task) *[]string { return g.hinted(t).Touches }

// countDependents records, for every task, how many tasks depend on it
// directly or transitively: the work its integration can unblock (H3).
func (g *Graph) countDependents() {
	rdeps := map[string][]string{}
	for name, deps := range g.deps {
		for _, dep := range deps {
			rdeps[dep] = append(rdeps[dep], name)
		}
	}
	g.dependents = make(map[string]int, len(g.tasks))
	for name := range g.tasks {
		seen := map[string]bool{}
		stack := append([]string(nil), rdeps[name]...)
		for len(stack) > 0 {
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[n] {
				continue
			}
			seen[n] = true
			stack = append(stack, rdeps[n]...)
		}
		g.dependents[name] = len(seen)
	}
}

// Dependents returns how many tasks depend on the named task, directly or
// transitively.
func (g *Graph) Dependents(name string) int { return g.dependents[name] }

// byPriority sorts candidates into H3's admission order: re-runs (attempts >
// 0) first, then the most transitive dependents, then name.
func (g *Graph) byPriority(ts []*task.Task) {
	sort.SliceStable(ts, func(i, j int) bool {
		a, b := ts[i], ts[j]
		if ra, rb := a.Attempts > 0, b.Attempts > 0; ra != rb {
			return ra
		}
		if da, db := g.dependents[a.Name], g.dependents[b.Name]; da != db {
			return da > db
		}
		return a.Name < b.Name
	})
}

// standardAreas maps the tags the planning prompt suggests (H4) to the files
// they stand for, so the hint audit can tell a change covered by an area from
// one outside every hint. Each matcher gets a repo-relative path. A tag not
// listed here covers nothing in the audit (it still serializes tasks).
var standardAreas = map[string]func(rel string) bool{
	"go-deps": baseIn("go.mod", "go.sum", "go.work", "go.work.sum"),
	"npm-deps": baseIn("package.json", "package-lock.json", "npm-shrinkwrap.json",
		"yarn.lock", "pnpm-lock.yaml"),
	"ci": func(rel string) bool {
		return strings.HasPrefix(rel, ".github/") || strings.HasPrefix(rel, ".circleci/") ||
			baseIn(".gitlab-ci.yml", "Jenkinsfile")(rel)
	},
	"db-schema": func(rel string) bool {
		return strings.EqualFold(path.Ext(rel), ".sql") || hasSegment(rel, "migrations")
	},
	"generated-docs": func(rel string) bool { return rel == "docs/packages.md" },
	"changelog": func(rel string) bool {
		return strings.HasPrefix(strings.ToUpper(path.Base(rel)), "CHANGELOG")
	},
	"release-version": baseIn("VERSION", "version.txt"),
}

func baseIn(names ...string) func(string) bool {
	return func(rel string) bool {
		b := path.Base(rel)
		for _, n := range names {
			if b == n {
				return true
			}
		}
		return false
	}
}

func hasSegment(rel, seg string) bool {
	for _, s := range strings.Split(rel, "/") {
		if s == seg {
			return true
		}
	}
	return false
}

// Uncovered returns the changed paths (each "<repo>/<repo-relative path>")
// that t's declared hints do not cover, sorted: the hint audit after an
// integration (H3, touches_exceeded). A path is covered when some touches glob
// matches it (PathCovered) or one of t's areas is a standard tag that stands
// for it. An exclusive task declared nothing and ran alone, so nothing is
// reported for it.
func Uncovered(t *task.Task, paths []string) []string {
	if t.Exclusive() {
		return nil
	}
	var out []string
	for _, p := range paths {
		if !covered(t, p) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func covered(t *task.Task, p string) bool {
	if t.Touches != nil {
		for _, g := range *t.Touches {
			if PathCovered(g, p) {
				return true
			}
		}
	}
	rel := p
	if i := strings.Index(p, "/"); i >= 0 {
		rel = p[i+1:]
	}
	for _, a := range t.Areas {
		if m := standardAreas[a]; m != nil && m(rel) {
			return true
		}
	}
	return false
}

// PathCovered reports whether the touches glob covers the literal path p
// (H1): segment by segment, a wildcard segment must match (path.Match), `**`
// matches the rest, and a glob that runs out first covers p when its last
// segment reads as a directory (no wildcard, no extension).
func PathCovered(glob, p string) bool {
	gs, ps := globSegments(glob), globSegments(p)
	if len(gs) == 0 || len(ps) == 0 {
		return false
	}
	for i, s := range gs {
		if s == "**" {
			return true
		}
		if i >= len(ps) {
			return false
		}
		if hasWildcard(s) {
			if ok, err := path.Match(s, ps[i]); err != nil || !ok {
				return false
			}
		} else if s != ps[i] {
			return false
		}
	}
	return len(gs) == len(ps) || namesDir(gs[len(gs)-1])
}
