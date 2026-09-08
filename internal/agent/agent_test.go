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
		{ReviewAgent, "review"},
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
		{ReviewAgent, 6},
	}
	for _, tc := range cases {
		if int(tc.kind) != tc.want {
			t.Errorf("%s = %d, want %d", tc.kind, int(tc.kind), tc.want)
		}
	}
}

// ParseKind is the inverse of String() and must round-trip for every kind —
// the daemon recovers a session's Kind from the on-disk string it wrote, so a
// missing case would silently drop that agent kind on restart reconciliation.
func TestParseKindRoundTrip(t *testing.T) {
	for _, k := range []Kind{ProjectAgent, PlanningAgent, TaskAgent, WolfAgent, CommitAgent, ArchiveAgent, ReviewAgent} {
		got, ok := ParseKind(k.String())
		if !ok || got != k {
			t.Errorf("ParseKind(%q) = (%v, %v), want (%v, true)", k.String(), got, ok, k)
		}
	}
	if _, ok := ParseKind("nonsense"); ok {
		t.Errorf("ParseKind(nonsense) should report ok=false")
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
		// The review agent is autonomous: it runs under ACP with `--print` so it
		// can carry --static-mcp github, like planning.
		{ReviewAgent, false},
	}
	for _, tc := range cases {
		if got := tc.kind.Interactive(); got != tc.want {
			t.Errorf("%s.Interactive() = %v, want %v", tc.kind, got, tc.want)
		}
	}
}
