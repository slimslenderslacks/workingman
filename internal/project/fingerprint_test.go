package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func samplePR() PullRequest {
	at := time.Date(2026, 11, 13, 4, 37, 0, 0, time.UTC)
	return PullRequest{
		Repo: "slimslenderslacks/workingman", Number: 26,
		URL:     "https://github.com/slimslenderslacks/workingman/pull/26",
		HeadSHA: "abc123", State: "open",
		Fingerprint: &PRFingerprint{
			LatestCommentID: 1001, LatestReviewID: 2002,
			ReviewThreads: 3, UnresolvedThreads: 1,
			CheckStates: []CheckState{{Name: "build", State: "success"}, {Name: "lint", State: "failure"}},
			ObservedAt:  &at,
		},
	}
}

// TestFingerprintRoundTrip pins that the fingerprint block survives Save/Load
// unchanged and leaves the surrounding pull_requests fields intact.
func TestFingerprintRoundTrip(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "project.yaml")
	in := &Project{
		Description: "pr project", Branch: "feat/pr", Status: StatusIdle, Review: true,
		Repos:        []Repo{{Org: "slimslenderslacks", Name: "workingman"}},
		PullRequests: []PullRequest{samplePR(), {Repo: "docker/other", Number: 7, HeadSHA: "def", State: "merged"}},
	}
	if err := SaveAs(dst, in, WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	raw, _ := os.ReadFile(dst)
	for _, want := range []string{"fingerprint:", "latest_comment_id: 1001", "check_states:", "observed_at:"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("saved file missing %q:\n%s", want, raw)
		}
	}
	got, err := Load(dst)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.PullRequests) != 2 {
		t.Fatalf("PullRequests = %+v", got.PullRequests)
	}
	for i := range in.PullRequests {
		if !got.PullRequests[i].Equal(in.PullRequests[i]) {
			t.Errorf("PR %d round-trip: got %+v, want %+v", i, got.PullRequests[i], in.PullRequests[i])
		}
	}
	if got.PullRequests[1].Fingerprint != nil {
		t.Errorf("entry without fingerprint gained one: %+v", got.PullRequests[1].Fingerprint)
	}
	if got.Description != in.Description || got.Branch != in.Branch || !got.Review || got.Status != StatusIdle {
		t.Errorf("other fields disturbed: %+v", got)
	}
}

// TestFingerprintOmitEmpty: an entry without a fingerprint (or with an empty
// one) writes no stray keys.
func TestFingerprintOmitEmpty(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "project.yaml")
	if err := SaveAs(dst, &Project{
		Description: "x", Branch: "b", Status: StatusIdle,
		PullRequests: []PullRequest{{Repo: "o/r", Number: 1, State: "open"}},
	}, WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	raw, _ := os.ReadFile(dst)
	if strings.Contains(string(raw), "fingerprint") {
		t.Errorf("nil fingerprint should be omitted:\n%s", raw)
	}
}

// TestFingerprintLoadsAgentYAML parses the shape the review template tells the
// agent to write.
func TestFingerprintLoadsAgentYAML(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "project.yaml")
	yaml := `description: x
branch: b
status: idle
pull_requests:
  - repo: o/r
    number: 5
    head_sha: abc
    state: open
    fingerprint:
      latest_comment_id: 11
      latest_review_id: 22
      review_threads: 2
      unresolved_threads: 0
      check_states:
        - name: test
          state: success
      observed_at: 2026-11-13T04:37:00Z
updated_by: agent
`
	if err := os.WriteFile(dst, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := Load(dst)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	f := p.PullRequests[0].Fingerprint
	if f == nil || f.LatestCommentID != 11 || f.LatestReviewID != 22 || f.ReviewThreads != 2 ||
		len(f.CheckStates) != 1 || f.CheckStates[0] != (CheckState{"test", "success"}) ||
		f.ObservedAt == nil || !f.ObservedAt.Equal(time.Date(2026, 11, 13, 4, 37, 0, 0, time.UTC)) {
		t.Errorf("fingerprint = %+v", f)
	}
}

func TestCanonicalFingerprint(t *testing.T) {
	base := samplePR()
	want, ok := base.CanonicalFingerprint()
	if !ok || want == "" {
		t.Fatalf("CanonicalFingerprint ok=%v %q", ok, want)
	}

	// Equal: check order, case/whitespace, ObservedAt and URL don't matter.
	same := samplePR()
	later := time.Now()
	same.URL = ""
	same.Repo = " SlimSlenderSlacks/Workingman "
	same.State = "OPEN"
	same.Fingerprint.ObservedAt = &later
	same.Fingerprint.CheckStates = []CheckState{{Name: "lint", State: "FAILURE"}, {Name: "build", State: "success"}}
	if got, _ := same.CanonicalFingerprint(); got != want {
		t.Errorf("equivalent PR fingerprint differs:\n got %s\nwant %s", got, want)
	}

	// Change detection: every signal moves the fingerprint.
	changes := map[string]func(*PullRequest){
		"head":       func(p *PullRequest) { p.HeadSHA = "zzz" },
		"state":      func(p *PullRequest) { p.State = "merged" },
		"comment":    func(p *PullRequest) { p.Fingerprint.LatestCommentID++ },
		"review":     func(p *PullRequest) { p.Fingerprint.LatestReviewID++ },
		"threads":    func(p *PullRequest) { p.Fingerprint.ReviewThreads++ },
		"unresolved": func(p *PullRequest) { p.Fingerprint.UnresolvedThreads = 0 },
		"checkState": func(p *PullRequest) { p.Fingerprint.CheckStates[1].State = "success" },
		"checkAdded": func(p *PullRequest) {
			p.Fingerprint.CheckStates = append(p.Fingerprint.CheckStates, CheckState{"e2e", "pending"})
		},
		"checkRemoved": func(p *PullRequest) { p.Fingerprint.CheckStates = p.Fingerprint.CheckStates[:1] },
		"number":       func(p *PullRequest) { p.Number++ },
	}
	for name, mutate := range changes {
		pr := samplePR()
		mutate(&pr)
		if got, _ := pr.CanonicalFingerprint(); got == want {
			t.Errorf("%s: fingerprint unchanged after change", name)
		}
	}

	// No recorded block → not comparable.
	bare := samplePR()
	bare.Fingerprint = nil
	if _, ok := bare.CanonicalFingerprint(); ok {
		t.Errorf("PR without fingerprint should not be comparable")
	}
}

func TestPRsFingerprint(t *testing.T) {
	a := samplePR()
	b := samplePR()
	b.Repo, b.Number = "docker/other", 9
	h1, ok := PRsFingerprint([]PullRequest{a, b})
	if !ok {
		t.Fatal("PRsFingerprint not ok")
	}
	if h2, _ := PRsFingerprint([]PullRequest{b, a}); h2 != h1 {
		t.Errorf("order-dependent: %s vs %s", h1, h2)
	}
	b.Fingerprint.LatestCommentID++
	if h3, _ := PRsFingerprint([]PullRequest{a, b}); h3 == h1 {
		t.Errorf("change on one PR not detected")
	}
	b.Fingerprint = nil
	if _, ok := PRsFingerprint([]PullRequest{a, b}); ok {
		t.Errorf("partial baseline should not be ok")
	}
	if _, ok := PRsFingerprint(nil); ok {
		t.Errorf("empty list should not be ok")
	}
}

func TestPullRequestEqual(t *testing.T) {
	a, b := samplePR(), samplePR()
	if !a.Equal(b) {
		t.Errorf("identical PRs not Equal")
	}
	b.Fingerprint.CheckStates[0].State = "failure"
	if a.Equal(b) {
		t.Errorf("differing fingerprint reported Equal")
	}
	b.Fingerprint = nil
	if a.Equal(b) || b.Equal(a) {
		t.Errorf("nil vs non-nil fingerprint reported Equal")
	}
}
