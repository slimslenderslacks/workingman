package taskgraph

import (
	"path"
	"strings"

	"github.com/slimslenderslacks/work/internal/task"
)

// Overlap reports whether a and b may not run at the same time, going by their
// independence hints (docs/parallel-task-worktrees.md H2). They overlap when
// either is exclusive (declares neither touches nor areas), when they share an
// areas tag, or when some glob in one's touches could match a path some glob
// in the other's could. The glob check is conservative: a false positive only
// costs throughput, a false negative costs an integration conflict.
func Overlap(a, b *task.Task) bool {
	if a.Exclusive() || b.Exclusive() {
		return true
	}
	for _, x := range a.Areas {
		for _, y := range b.Areas {
			if x == y {
				return true
			}
		}
	}
	if a.Touches == nil || b.Touches == nil {
		return false
	}
	for _, x := range *a.Touches {
		for _, y := range *b.Touches {
			if GlobsOverlap(x, y) {
				return true
			}
		}
	}
	return false
}

// GlobsOverlap reports whether two touches globs could match a common path
// (H2). Both are split into segments and compared from the left: two literal
// segments must be equal, a wildcard segment must accept the other literal
// (path.Match), two wildcard segments are assumed to match, and `**` on either
// side matches the rest. When one glob runs out first, they overlap if the
// shorter one names a directory (no wildcard and no extension in its last
// segment, read as dir/**) or the longer one has only `**` left. A malformed
// pattern counts as overlapping, as does an empty glob.
func GlobsOverlap(a, b string) bool {
	as, bs := globSegments(a), globSegments(b)
	if len(as) == 0 || len(bs) == 0 {
		return true
	}
	for i := 0; i < len(as) && i < len(bs); i++ {
		x, y := as[i], bs[i]
		if x == "**" || y == "**" {
			return true
		}
		if !segmentsMatch(x, y) {
			return false
		}
	}
	if len(as) == len(bs) {
		return true
	}
	short, long := as, bs
	if len(short) > len(long) {
		short, long = long, short
	}
	if namesDir(short[len(short)-1]) {
		return true
	}
	for _, s := range long[len(short):] {
		if s != "**" {
			return false
		}
	}
	return true
}

func globSegments(g string) []string {
	g = strings.Trim(path.Clean(strings.TrimSpace(g)), "/")
	if g == "" || g == "." {
		return nil
	}
	return strings.Split(g, "/")
}

func hasWildcard(s string) bool { return strings.ContainsAny(s, "*?[") }

func segmentsMatch(x, y string) bool {
	xw, yw := hasWildcard(x), hasWildcard(y)
	switch {
	case xw && yw:
		return true
	case xw:
		return matchOrBad(x, y)
	case yw:
		return matchOrBad(y, x)
	}
	return x == y
}

// matchOrBad is path.Match, except that a malformed pattern matches: the
// conservative answer when a hint can't be read.
func matchOrBad(pattern, name string) bool {
	ok, err := path.Match(pattern, name)
	return ok || err != nil
}

// namesDir reports whether a glob's last segment reads as a directory prefix.
func namesDir(last string) bool {
	return !hasWildcard(last) && path.Ext(last) == ""
}

// overlapping returns the names of the tasks in set that overlap t, in set
// order, skipping t itself.
func overlapping(t *task.Task, set []*task.Task) []string {
	var out []string
	for _, o := range set {
		if o.Name != t.Name && Overlap(t, o) {
			out = append(out, o.Name)
		}
	}
	return out
}
