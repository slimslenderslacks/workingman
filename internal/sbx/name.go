package sbx

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// MaxNameLen is the longest name `sbx create` accepts. A sandbox name becomes a
// Docker container name — an RFC 1123 DNS label — so sbx rejects anything over
// 63 characters outright:
//
//	error: sandbox name cannot exceed 63 characters: <name>
//
// This package owns the constant because it owns the sbx CLI's rules. Every
// producer of a sandbox name (runner.SandboxNameFor, acpwrapper's
// "acp-<session-id>" default, the daemon's interactive windows) funnels through
// SanitizeName below so the limit is enforced in exactly one place rather than
// re-derived — and re-missed — at each call site.
const MaxNameLen = 63

// nameHashLen is how many hex characters of the full name's digest FitNameTo
// appends when it has to shorten. Seven hex chars (28 bits) makes an accidental
// collision between two shortened names from the same work stream vanishingly
// unlikely, and reads like a git short sha, which is the familiar shape for
// "this identifier was elided".
const nameHashLen = 7

// SanitizeName turns an intended sandbox name into one sbx will actually
// accept: underscores become hyphens (sbx rejects underscores) and the result
// is shortened to MaxNameLen if needed. It is the function every caller that
// hands a name to `sbx` should use, so the name we ask for, the name we log,
// and the name sbx registers are always the same string.
func SanitizeName(name string) string {
	return FitName(strings.ReplaceAll(name, "_", "-"))
}

// FitName shortens name to MaxNameLen. See FitNameTo.
func FitName(name string) string { return FitNameTo(name, MaxNameLen) }

// FitNameTo returns name unchanged when it already fits in max characters, and
// otherwise a deterministically shortened form: as much of the leading text as
// fits, then "-<7 hex of sha256(name)>".
//
// Two properties matter more than prettiness here:
//
//   - Deterministic. The sandbox name is the task's identity across daemon
//     restarts — ensureSandbox looks up an existing sandbox *by this name* to
//     decide reuse vs. recreate — so the same input must always shorten to the
//     same output. No counters, no timestamps, no map iteration order.
//   - Injective in practice. Two long names that share a prefix (the common
//     case: "fix-foo-in-very-long-package-a" / "...-b") must not collapse onto
//     one sandbox. The digest is of the FULL name, so the divergent tail still
//     separates them.
//
// The result is a valid RFC 1123 DNS label whenever the input was: the kept
// prefix has any trailing hyphens trimmed before the hash is appended, and hex
// digits are legal label characters.
//
// max <= 0 yields "" — there is no room for a name at all, which callers treat
// as "no sandbox name could be derived" rather than guessing one.
func FitNameTo(name string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(name) <= max {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	h := hex.EncodeToString(sum[:])[:nameHashLen]
	// Budget too small for the digest itself? Keep as much of it as fits —
	// still deterministic, still distinct per input, just weaker.
	if max < nameHashLen {
		return h[:max]
	}
	// Room for "<kept>-<digest>"? Keep the leading words so the name stays
	// recognizable. Otherwise the digest alone is the whole name.
	keep := max - nameHashLen - 1
	if keep <= 0 {
		return h
	}
	kept := strings.TrimRight(name[:keep], "-")
	if kept == "" {
		return h
	}
	return kept + "-" + h
}

// NameBudget returns how many characters are left for the suffix of a sandbox
// name shaped "<prefix>-<suffix>" — the shape every per-task sandbox uses,
// where prefix is the work stream (the basename of the project's control dir)
// and suffix is the task name.
//
// Callers use it two ways: to tell an agent, in its prompt, how long a task
// name it may write (see internal/prompts), and to repair one that came back
// too long anyway (see taskgraph.Load). A prefix at or over MaxNameLen-1
// leaves nothing and returns 0.
func NameBudget(prefix string) int {
	n := MaxNameLen - len(prefix) - 1
	if n < 0 {
		return 0
	}
	return n
}

// CheckName reports whether name is within MaxNameLen, returning an error
// describing the overage if it is not. It exists so the few places that want to
// *fail loudly* on an over-long name (rather than silently shorten it) phrase
// the complaint identically.
func CheckName(name string) error {
	if len(name) > MaxNameLen {
		return fmt.Errorf("sandbox name %q is %d chars, exceeds the sbx limit of %d", name, len(name), MaxNameLen)
	}
	return nil
}
