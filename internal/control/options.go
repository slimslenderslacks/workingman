package control

import "github.com/slimslenderslacks/work/internal/project"

// Option adjusts how a project-file action runs. The TUI passes none; the
// API passes [Check] for If-Match, [ReportNoop] so an action already in
// effect answers "nothing to do" instead of rewriting or refusing, and
// [WolfReason] for the reason a GUI user typed.
type Option func(*options)

type options struct {
	check  func(data []byte) error
	noop   *bool
	reason string
}

// Check runs check on project.yaml's bytes under the write lock, before the
// action reads them; an error from it is returned as is and nothing is
// written. The API's If-Match is such a check (docs/proposals/workingman-gui.md
// §A.4.1).
func Check(check func(data []byte) error) Option {
	return func(o *options) { o.check = check }
}

// ReportNoop makes [Cleanup], [Hide] and [Show] leave a project that already
// has the state they ask for untouched, set *noop, and return no error. Without
// it Hide and Show refuse that case and Cleanup rewrites the flag (a rewrite
// re-dispatches the project, which retries an archive launch that failed).
func ReportNoop(noop *bool) Option {
	return func(o *options) { o.noop = noop }
}

// WolfReason is the blocked_reason [SummonWolf] writes when the project has
// none, in place of [SummonReason].
func WolfReason(reason string) Option {
	return func(o *options) { o.reason = reason }
}

func collect(opts []Option) options {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// update is project.Update with the options' check.
func (o options) update(path string, by project.Writer, fn func(*project.Project) bool) error {
	return project.UpdateIf(path, by, o.check, fn)
}

// noopNow reports whether the caller asked for no-ops to be reported, and
// records one.
func (o options) noopNow() bool {
	if o.noop == nil {
		return false
	}
	*o.noop = true
	return true
}
