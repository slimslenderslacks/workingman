package sbx

import (
	"regexp"
	"strings"
	"testing"
)

// dnsLabel is the shape Docker (and therefore sbx) requires of a container
// name component: lowercase alphanumerics separated by single hyphens.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func TestFitNameLeavesShortNamesAlone(t *testing.T) {
	for _, name := range []string{
		"a",
		"proj-add-healthz-handler",
		strings.Repeat("a", MaxNameLen),
	} {
		if got := FitName(name); got != name {
			t.Errorf("FitName(%q) = %q, want it unchanged", name, got)
		}
	}
}

// TestFitNameNeverExceedsMax is the invariant the whole package exists for:
// whatever an agent wrote, whatever work stream it lands under, the string we
// hand `sbx create` fits.
func TestFitNameNeverExceedsMax(t *testing.T) {
	for _, name := range []string{
		strings.Repeat("a", MaxNameLen+1),
		strings.Repeat("a", 500),
		"server-namespace-fix-testifylint-go-require-create-mcp-prefix-test-1019",
		strings.Repeat("ab-", 100),
		strings.Repeat("-", 200),
	} {
		got := FitName(name)
		if len(got) > MaxNameLen {
			t.Errorf("FitName(%d chars) = %q (%d chars), want <= %d", len(name), got, len(got), MaxNameLen)
		}
		if !dnsLabel.MatchString(got) {
			t.Errorf("FitName(%q) = %q, which is not a valid DNS label", name, got)
		}
	}
}

// TestFitNameIsDeterministic matters because the sandbox name IS the session's
// identity across daemon restarts: ensureSandbox looks up the existing sandbox
// by name to decide reuse vs. recreate. A name that shortened differently on
// each launch would orphan a sandbox every time.
func TestFitNameIsDeterministic(t *testing.T) {
	name := strings.Repeat("long-task-name-", 10)
	first := FitName(name)
	for i := 0; i < 100; i++ {
		if got := FitName(name); got != first {
			t.Fatalf("FitName is not deterministic: %q then %q", first, got)
		}
	}
	// Idempotent too: fitting an already-fitted name is a no-op, so a name that
	// round-trips through a config file doesn't drift.
	if got := FitName(first); got != first {
		t.Errorf("FitName(FitName(x)) = %q, want %q", got, first)
	}
}

// TestFitNameSeparatesSharedPrefixes is why the shortening appends a digest
// rather than just chopping the tail: the names that overflow are long and
// similar, and two tasks collapsing onto one sandbox name is worse than an ugly
// name.
func TestFitNameSeparatesSharedPrefixes(t *testing.T) {
	prefix := strings.Repeat("a", MaxNameLen)
	a, b := FitName(prefix+"-one"), FitName(prefix+"-two")
	if a == b {
		t.Errorf("distinct names both shortened to %q", a)
	}
}

func TestFitNameToTinyBudgets(t *testing.T) {
	name := strings.Repeat("a", 100)
	for max := 0; max <= 12; max++ {
		got := FitNameTo(name, max)
		if len(got) > max {
			t.Errorf("FitNameTo(_, %d) = %q (%d chars), want <= %d", max, got, len(got), max)
		}
		if max > 0 && got == "" {
			t.Errorf("FitNameTo(_, %d) = \"\", want a usable name", max)
		}
	}
	if got := FitNameTo(name, 0); got != "" {
		t.Errorf("FitNameTo(_, 0) = %q, want \"\"", got)
	}
}

func TestSanitizeNameRewritesUnderscores(t *testing.T) {
	if got := SanitizeName("some_work_stream-some_task"); got != "some-work-stream-some-task" {
		t.Errorf("SanitizeName = %q, want underscores rewritten", got)
	}
	// Underscores are rewritten BEFORE the length check, so the digest is taken
	// over the name sbx would actually have seen.
	long := strings.Repeat("a_b", 100)
	if got := SanitizeName(long); strings.Contains(got, "_") || len(got) > MaxNameLen {
		t.Errorf("SanitizeName(long) = %q, want no underscores and <= %d chars", got, MaxNameLen)
	}
}

func TestNameBudget(t *testing.T) {
	cases := []struct {
		prefix string
		want   int
	}{
		{"proj", MaxNameLen - 5},
		{"server-namespace", 46},
		{"", MaxNameLen - 1},
		{strings.Repeat("a", MaxNameLen), 0},
		{strings.Repeat("a", MaxNameLen*2), 0},
	}
	for _, c := range cases {
		if got := NameBudget(c.prefix); got != c.want {
			t.Errorf("NameBudget(%d chars) = %d, want %d", len(c.prefix), got, c.want)
		}
	}
	// The budget is exactly what it claims: prefix + "-" + a name of budget
	// length is at the limit, never over it.
	for _, prefix := range []string{"proj", "server-namespace", "shared-gateway"} {
		name := strings.Repeat("a", NameBudget(prefix))
		if n := len(prefix + "-" + name); n > MaxNameLen {
			t.Errorf("%q + budget-length name = %d chars, want <= %d", prefix, n, MaxNameLen)
		}
	}
}

func TestCheckName(t *testing.T) {
	if err := CheckName(strings.Repeat("a", MaxNameLen)); err != nil {
		t.Errorf("CheckName at the limit: %v", err)
	}
	if err := CheckName(strings.Repeat("a", MaxNameLen+1)); err == nil {
		t.Error("CheckName over the limit returned nil")
	}
}
