package agent

import "testing"

// The Kind strings are not cosmetic: prompts.Render looks up its template by
// `kind.String()+".tmpl"`, and the daemon/TUI label sessions with them. A typo
// here silently breaks template lookup, so pin every value.
func TestKindString(t *testing.T) {
	cases := []struct {
		kind Kind
		want string
	}{
		{ProjectAgent, "project"},
		{PlanningAgent, "planning"},
		{TaskAgent, "task"},
		{WolfAgent, "wolf"},
		{CommitAgent, "commit"},
		{ArchiveAgent, "archive"},
		{Kind(99), "unknown"},
	}
	for _, tc := range cases {
		if got := tc.kind.String(); got != tc.want {
			t.Errorf("Kind(%d).String() = %q, want %q", int(tc.kind), got, tc.want)
		}
	}
}

// New kinds are appended to the iota block; if someone inserts one in the
// middle, every previously recorded value shifts underneath persisted session
// records. Pin the ordinals.
func TestKindOrdinals(t *testing.T) {
	cases := []struct {
		kind Kind
		want int
	}{
		{ProjectAgent, 0},
		{PlanningAgent, 1},
		{TaskAgent, 2},
		{WolfAgent, 3},
		{CommitAgent, 4},
		{ArchiveAgent, 5},
	}
	for _, tc := range cases {
		if int(tc.kind) != tc.want {
			t.Errorf("%s = %d, want %d", tc.kind, int(tc.kind), tc.want)
		}
	}
}

// Interactive drives the --print flag in runner.DefaultCommandBuilder and the
// ACP-vs-tmux routing in Runner.Start, so it has to be exact: the archive agent
// may need a human to approve a .gitignore change, which makes it interactive
// like the wolf.
func TestKindInteractive(t *testing.T) {
	cases := []struct {
		kind Kind
		want bool
	}{
		{ProjectAgent, false},
		{PlanningAgent, false},
		{TaskAgent, false},
		{CommitAgent, false},
		{WolfAgent, true},
		{ArchiveAgent, true},
	}
	for _, tc := range cases {
		if got := tc.kind.Interactive(); got != tc.want {
			t.Errorf("%s.Interactive() = %v, want %v", tc.kind, got, tc.want)
		}
	}
}
