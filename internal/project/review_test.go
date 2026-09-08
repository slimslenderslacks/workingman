package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReviewingStatusValidAndRoundTrips pins the new status: the YAML loader
// enum-checks against Valid(), so a project written with status:reviewing must
// load back cleanly rather than being rejected.
func TestReviewingStatusValidAndRoundTrips(t *testing.T) {
	if !StatusReviewing.Valid() {
		t.Fatalf("StatusReviewing must be Valid()")
	}
	dst := filepath.Join(t.TempDir(), ".project.yaml")
	if err := SaveAs(dst, &Project{
		Description: "pr project",
		Branch:      "feat/pr",
		Status:      StatusReviewing,
		Repos:       []Repo{{Org: "docker", Name: "gateway"}},
	}, WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	got, err := Load(dst)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Status != StatusReviewing {
		t.Errorf("Status = %q, want reviewing", got.Status)
	}
}

// TestReviewFieldsRoundTrip checks Review and PullRequest survive a save/load
// and that both stay out of the YAML when unset (omitempty keeps existing files
// byte-identical).
func TestReviewFieldsRoundTrip(t *testing.T) {
	dst := filepath.Join(t.TempDir(), ".project.yaml")
	if err := SaveAs(dst, &Project{
		Description: "pr project",
		Branch:      "feat/pr",
		Status:      StatusReviewing,
		Repos:       []Repo{{Org: "docker", Name: "gateway"}},
		Review:      true,
		PullRequest: &PullRequest{Repo: "docker/gateway", Number: 42, URL: "https://github.com/docker/gateway/pull/42", HeadSHA: "abc123", State: "open"},
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
	if got.PullRequest == nil || got.PullRequest.Number != 42 || got.PullRequest.State != "open" {
		t.Errorf("PullRequest = %+v, want number 42 / state open", got.PullRequest)
	}

	// Unset case: neither key appears on disk.
	bare := filepath.Join(t.TempDir(), ".project.yaml")
	if err := SaveAs(bare, &Project{Description: "x", Branch: "b", Status: StatusDone}, WriterAgent); err != nil {
		t.Fatalf("SaveAs bare: %v", err)
	}
	data, err := os.ReadFile(bare)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, key := range []string{"review:", "pull_request:"} {
		if strings.Contains(string(data), key) {
			t.Errorf("unset field leaked into YAML: %q\n%s", key, data)
		}
	}
}

func TestHasRepos(t *testing.T) {
	cases := []struct {
		name string
		p    Project
		want bool
	}{
		{"none", Project{}, false},
		{"repos", Project{Repos: []Repo{{Org: "docker", Name: "gateway"}}}, true},
		{"new_repos", Project{NewRepos: []Repo{{Org: "me", Name: "new"}}}, true},
	}
	for _, tc := range cases {
		if got := tc.p.HasRepos(); got != tc.want {
			t.Errorf("%s: HasRepos() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
