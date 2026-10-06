package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIdleStatusValidAndRoundTrips pins the collapsed status: the YAML loader
// enum-checks against Valid(), so a project written with status:idle must
// load back cleanly rather than being rejected. Whether it's also being
// watched for PR review is carried by WatchingPR, not a separate status
// value — see TestWatchingPR.
func TestIdleStatusValidAndRoundTrips(t *testing.T) {
	if !StatusIdle.Valid() {
		t.Fatalf("StatusIdle must be Valid()")
	}
	dst := filepath.Join(t.TempDir(), ".project.yaml")
	if err := SaveAs(dst, &Project{
		Description: "pr project",
		Branch:      "feat/pr",
		Status:      StatusIdle,
		Repos:       []Repo{{Org: "docker", Name: "gateway"}},
	}, WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	got, err := Load(dst)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Status != StatusIdle {
		t.Errorf("Status = %q, want idle", got.Status)
	}
}

// TestLegacyStatusAliasesLoadAsIdle is the regression for the live breakage
// the collapse caused: every project file already on disk from before this
// change carries status: done (or, for one mid-PR-review, status:
// reviewing) — both values this code no longer writes, but must still load
// as StatusIdle rather than erroring "invalid project status", or every
// existing project in ~/orch starts failing to parse the moment the daemon
// is rebuilt.
func TestLegacyStatusAliasesLoadAsIdle(t *testing.T) {
	for _, legacy := range []string{"done", "reviewing"} {
		t.Run(legacy, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), ".project.yaml")
			body := "description: p\nbranch: b\nstatus: " + legacy + "\nupdated_by: agent\n"
			if err := os.WriteFile(dst, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := Load(dst)
			if err != nil {
				t.Fatalf("Load(status: %s) errored: %v", legacy, err)
			}
			if got.Status != StatusIdle {
				t.Errorf("Status = %q, want idle (legacy %q normalized)", got.Status, legacy)
			}
		})
	}
}

// TestLegacyStatusAliasPreservesWatchingData confirms a project that was
// mid-PR-review under the old status:reviewing value keeps reading as
// watched: WatchingPR() is derived from review/pull_requests, which the
// legacy alias must not disturb.
func TestLegacyStatusAliasPreservesWatchingData(t *testing.T) {
	dst := filepath.Join(t.TempDir(), ".project.yaml")
	body := "description: p\nbranch: b\nstatus: reviewing\n" +
		"pull_requests:\n  - repo: docker/gateway\n    number: 1\n    state: open\n" +
		"updated_by: agent\n"
	if err := os.WriteFile(dst, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dst)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Status != StatusIdle {
		t.Errorf("Status = %q, want idle", got.Status)
	}
	if !got.WatchingPR() {
		t.Errorf("WatchingPR() = false, want true — the open PR must still read as watched")
	}
}

// TestWatchingPR pins the predicate that replaced status:reviewing: true when
// either Review declares PR-shaped intent (even with no PR recorded yet) or
// at least one recorded PullRequests entry hasn't reached a terminal state;
// false only once every signal says there's nothing left to watch.
func TestWatchingPR(t *testing.T) {
	cases := []struct {
		name string
		p    Project
		want bool
	}{
		{"neither set", Project{}, false},
		{"review intent, no PR yet", Project{Review: true}, true},
		{"open PR, no review intent", Project{
			PullRequests: []PullRequest{{Repo: "docker/gateway", Number: 1, State: "open"}},
		}, true},
		{"blank-state PR treated as open", Project{
			PullRequests: []PullRequest{{Repo: "docker/gateway", Number: 1}},
		}, true},
		{"every PR resolved, no review intent", Project{
			PullRequests: []PullRequest{
				{Repo: "docker/gateway", Number: 1, State: "merged"},
				{Repo: "docker/cli", Number: 2, State: "closed"},
			},
		}, false},
		{"one resolved, one still open", Project{
			PullRequests: []PullRequest{
				{Repo: "docker/gateway", Number: 1, State: "merged"},
				{Repo: "docker/cli", Number: 2, State: "open"},
			},
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.WatchingPR(); got != tc.want {
				t.Errorf("WatchingPR() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestReviewFieldsRoundTrip checks Review and PullRequests survive a save/load
// (including several PRs, one per repo) and that both stay out of the YAML when
// unset (omitempty keeps existing files byte-identical).
func TestReviewFieldsRoundTrip(t *testing.T) {
	dst := filepath.Join(t.TempDir(), ".project.yaml")
	if err := SaveAs(dst, &Project{
		Description: "pr project",
		Branch:      "feat/pr",
		Status:      StatusIdle,
		Repos:       []Repo{{Org: "docker", Name: "gateway"}},
		Review:      true,
		PullRequests: []PullRequest{
			{Repo: "docker/gateway", Number: 42, URL: "https://github.com/docker/gateway/pull/42", HeadSHA: "abc123", State: "open"},
			{Repo: "docker/mcpruntime", Number: 7, State: "merged"},
		},
	}, WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	got, err := Load(dst)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.Review {
		t.Errorf("Review = false, want true")
	}
	if len(got.PullRequests) != 2 {
		t.Fatalf("PullRequests = %+v, want 2 entries", got.PullRequests)
	}
	if got.PullRequests[0].Number != 42 || got.PullRequests[0].State != "open" {
		t.Errorf("PR[0] = %+v, want number 42 / state open", got.PullRequests[0])
	}
	if got.PullRequests[1].Repo != "docker/mcpruntime" || got.PullRequests[1].State != "merged" {
		t.Errorf("PR[1] = %+v, want mcpruntime / merged", got.PullRequests[1])
	}

	// Unset case: neither key appears on disk.
	bare := filepath.Join(t.TempDir(), ".project.yaml")
	if err := SaveAs(bare, &Project{Description: "x", Branch: "b", Status: StatusIdle}, WriterAgent); err != nil {
		t.Fatalf("SaveAs bare: %v", err)
	}
	data, err := os.ReadFile(bare)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, key := range []string{"review:", "pull_requests:"} {
		if strings.Contains(string(data), key) {
			t.Errorf("unset field leaked into YAML: %q\n%s", key, data)
		}
	}
}

func TestMergedStatusRoundTripAndNotWatched(t *testing.T) {
	dst := filepath.Join(t.TempDir(), ".project.yaml")
	in := &Project{
		Description: "p", Branch: "b", Status: StatusMerged, Review: true,
		PullRequests: []PullRequest{{Repo: "docker/gateway", Number: 1, State: "merged"}},
	}
	if err := SaveAs(dst, in, WriterDaemon); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	got, err := Load(dst)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Status != StatusMerged || !got.Status.Valid() {
		t.Errorf("Status = %q, want merged", got.Status)
	}
	if got.WatchingPR() {
		t.Errorf("WatchingPR() = true for a merged project, want false even with review set")
	}
}

func TestAllBranchesMerged(t *testing.T) {
	m := func(repo, state string) PullRequest { return PullRequest{Repo: repo, Number: 1, State: state} }
	cases := []struct {
		name    string
		prs     []PullRequest
		commits []string
		want    bool
	}{
		{"no PRs", nil, nil, false},
		{"one merged", []PullRequest{m("a/x", "merged")}, nil, true},
		{"open", []PullRequest{m("a/x", "open")}, nil, false},
		{"unknown state", []PullRequest{m("a/x", "")}, nil, false},
		{"all merged two repos", []PullRequest{m("a/x", "merged"), m("a/y", "merged")}, nil, true},
		{"one open one merged", []PullRequest{m("a/x", "merged"), m("a/y", "open")}, nil, false},
		{"closed only", []PullRequest{m("a/x", "closed")}, nil, false},
		{"merged plus closed other repo", []PullRequest{m("a/x", "merged"), m("a/y", "closed")}, nil, false},
		{"merged plus closed duplicate", []PullRequest{m("a/x", "merged"), m("a/x", "closed")}, nil, true},
		{"commits without PR", []PullRequest{m("a/x", "merged")}, []string{"a/y"}, false},
		{"commits repo covered by merged PR", []PullRequest{m("A/X", "merged")}, []string{"a/x"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Project{PullRequests: tc.prs}
			if got := p.AllBranchesMerged(tc.commits); got != tc.want {
				t.Errorf("AllBranchesMerged = %v, want %v", got, tc.want)
			}
		})
	}
}
