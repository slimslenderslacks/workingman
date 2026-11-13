package project

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// fingerprintVersion prefixes every canonical fingerprint string so a change to
// the encoding can never compare equal to a baseline written by an older one.
const fingerprintVersion = "v1"

// PRFingerprint is the PR activity the review agent observed at the end of a
// poll, recorded under a pull_requests: entry's `fingerprint:` key. The head
// commit is not repeated here — it is the entry's own head_sha.
//
// It is also the canonical shape of the daemon's own (MCP-probed) fingerprint:
// the daemon fills a PullRequest{Repo, Number, HeadSHA, State, Fingerprint}
// from the same GitHub data and calls CanonicalFingerprint, so the two sources
// are directly comparable.
type PRFingerprint struct {
	// LatestCommentID is the highest id among the PR's review comments
	// (inline code comments) and issue comments (conversation).
	LatestCommentID int64 `yaml:"latest_comment_id,omitempty"`
	// LatestReviewID is the highest id among the PR's submitted reviews.
	LatestReviewID int64 `yaml:"latest_review_id,omitempty"`
	// ReviewThreads is the number of review threads on the PR, and
	// UnresolvedThreads how many of those are not resolved — a thread being
	// resolved or reopened changes no ids, only these counts.
	ReviewThreads     int `yaml:"review_threads,omitempty"`
	UnresolvedThreads int `yaml:"unresolved_threads,omitempty"`
	// CheckStates is every check run and commit status at the head SHA.
	CheckStates []CheckState `yaml:"check_states,omitempty"`
	// ObservedAt is when the fingerprint was taken. Informational only: it is
	// excluded from CanonicalFingerprint.
	ObservedAt *time.Time `yaml:"observed_at,omitempty"`
}

// CheckState is one check at the PR's head: the check run (or commit status
// context) name and its state — the conclusion once completed (success,
// failure, cancelled, timed_out, skipped, neutral, ...), otherwise the status
// (queued, in_progress, pending).
type CheckState struct {
	Name  string `yaml:"name"`
	State string `yaml:"state"`
}

// Equal reports whether f and o carry the same recorded values, including
// ObservedAt. For change detection use CanonicalFingerprint instead.
func (f PRFingerprint) Equal(o PRFingerprint) bool {
	if f.LatestCommentID != o.LatestCommentID || f.LatestReviewID != o.LatestReviewID ||
		f.ReviewThreads != o.ReviewThreads || f.UnresolvedThreads != o.UnresolvedThreads ||
		len(f.CheckStates) != len(o.CheckStates) {
		return false
	}
	for i := range f.CheckStates {
		if f.CheckStates[i] != o.CheckStates[i] {
			return false
		}
	}
	if f.ObservedAt == nil || o.ObservedAt == nil {
		return f.ObservedAt == nil && o.ObservedAt == nil
	}
	return f.ObservedAt.Equal(*o.ObservedAt)
}

// CanonicalFingerprint returns the canonical, comparable encoding of the PR's
// change-detection signals: repo, number, state, head SHA and the Fingerprint
// block (minus ObservedAt). Repo, state and check states are normalised
// (trimmed, lower-cased) and check states are sorted, so the order GitHub or
// the agent listed them in doesn't matter. ok is false when no Fingerprint
// block was recorded — a bare head_sha is not enough to gate on.
func (pr PullRequest) CanonicalFingerprint() (string, bool) {
	if pr.Fingerprint == nil {
		return "", false
	}
	f := pr.Fingerprint
	checks := make([]string, 0, len(f.CheckStates))
	for _, c := range f.CheckStates {
		checks = append(checks, fmt.Sprintf("%q=%q", strings.TrimSpace(c.Name), norm(c.State)))
	}
	sort.Strings(checks)
	return fmt.Sprintf("%s|repo=%s|pr=%d|state=%s|head=%s|comment=%d|review=%d|threads=%d|unresolved=%d|checks=[%s]",
		fingerprintVersion, norm(pr.Repo), pr.Number, norm(pr.State), norm(pr.HeadSHA),
		f.LatestCommentID, f.LatestReviewID, f.ReviewThreads, f.UnresolvedThreads,
		strings.Join(checks, ",")), true
}

// PRsFingerprint hashes the canonical fingerprints of every pull request into
// one project-level value (hex SHA-256), independent of list order. ok is false
// when prs is empty or any entry lacks a recorded Fingerprint, since a partial
// baseline could hide a change on the unrecorded PR.
func PRsFingerprint(prs []PullRequest) (string, bool) {
	if len(prs) == 0 {
		return "", false
	}
	parts := make([]string, 0, len(prs))
	for _, pr := range prs {
		c, ok := pr.CanonicalFingerprint()
		if !ok {
			return "", false
		}
		parts = append(parts, c)
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:]), true
}

func norm(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
