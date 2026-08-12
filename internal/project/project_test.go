package project

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestLoadExample(t *testing.T) {
	p, err := Load("../../examples/.project.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Branch != "feat/healthz-probe" {
		t.Errorf("Branch = %q, want feat/healthz-probe", p.Branch)
	}
	if p.Status != StatusReady {
		t.Errorf("Status = %q, want ready", p.Status)
	}
	if p.UpdatedBy != WriterAgent {
		t.Errorf("UpdatedBy = %q, want agent", p.UpdatedBy)
	}
	if len(p.Repos) != 2 || p.Repos[0].Name != "gateway" {
		t.Errorf("Repos = %+v, want [gateway, deploy-manifests]", p.Repos)
	}
	if p.Cron != "*/15 * * * *" {
		t.Errorf("Cron = %q", p.Cron)
	}
}

func TestSaveForcesDaemonWriter(t *testing.T) {
	src, err := Load("../../examples/.project.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if src.UpdatedBy != WriterAgent {
		t.Fatalf("precondition: example must be writer=agent, got %q", src.UpdatedBy)
	}
	dst := filepath.Join(t.TempDir(), ".project.yaml")
	if err := Save(dst, src); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded, err := Load(dst)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.UpdatedBy != WriterDaemon {
		t.Errorf("UpdatedBy after Save = %q, want daemon", reloaded.UpdatedBy)
	}
	// Save must not mutate the source struct.
	if src.UpdatedBy != WriterAgent {
		t.Errorf("Save mutated source UpdatedBy: %q", src.UpdatedBy)
	}
	reloaded.UpdatedBy = src.UpdatedBy
	if !reflect.DeepEqual(src, reloaded) {
		t.Errorf("round-trip mismatch:\n src=%+v\n got=%+v", src, reloaded)
	}
}

func TestSaveAsAgent(t *testing.T) {
	src, _ := Load("../../examples/.project.yaml")
	dst := filepath.Join(t.TempDir(), ".project.yaml")
	if err := SaveAs(dst, src, WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	reloaded, _ := Load(dst)
	if reloaded.UpdatedBy != WriterAgent {
		t.Errorf("UpdatedBy = %q, want agent", reloaded.UpdatedBy)
	}
}

func TestCreatedAtRoundTripAndOmitEmpty(t *testing.T) {
	// Zero/missing CreatedAt must not produce a `created_at:` line so the
	// daemon's "stamp the first time we see it set" gate stays meaningful.
	noStamp := filepath.Join(t.TempDir(), "a.yaml")
	if err := SaveAs(noStamp, &Project{
		Description: "x", Branch: "feat/y", Status: StatusReady,
	}, WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	raw, _ := os.ReadFile(noStamp)
	if strings.Contains(string(raw), "created_at") {
		t.Errorf("nil CreatedAt should not emit created_at line; got:\n%s", string(raw))
	}

	// Setting it round-trips: stored, loaded, equal to the second.
	withStamp := filepath.Join(t.TempDir(), "b.yaml")
	now := time.Date(2026, 6, 10, 14, 0, 0, 0, time.UTC)
	if err := SaveAs(withStamp, &Project{
		Description: "x", Branch: "feat/y", Status: StatusReady, CreatedAt: &now,
	}, WriterDaemon); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	reloaded, err := Load(withStamp)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reloaded.CreatedAt == nil || !reloaded.CreatedAt.Equal(now) {
		t.Errorf("CreatedAt = %v, want %v", reloaded.CreatedAt, now)
	}
}

func TestBaseBranchRoundTrip(t *testing.T) {
	dst := filepath.Join(t.TempDir(), ".project.yaml")
	src := &Project{
		Description: "x",
		Branch:      "feat/y",
		Status:      StatusReady,
		Repos: []Repo{
			{Org: "docker", Name: "mcpruntime", BaseBranch: "mcp-kit-hooks"},
			{Org: "docker", Name: "sandboxes"}, // no BaseBranch → omitted
		},
	}
	if err := SaveAs(dst, src, WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	reloaded, err := Load(dst)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(reloaded.Repos) != 2 {
		t.Fatalf("want 2 repos, got %d", len(reloaded.Repos))
	}
	if reloaded.Repos[0].BaseBranch != "mcp-kit-hooks" {
		t.Errorf("repo[0].BaseBranch = %q, want mcp-kit-hooks", reloaded.Repos[0].BaseBranch)
	}
	if reloaded.Repos[1].BaseBranch != "" {
		t.Errorf("repo[1].BaseBranch = %q, want empty", reloaded.Repos[1].BaseBranch)
	}
}

func TestEmptyFile(t *testing.T) {
	dst := filepath.Join(t.TempDir(), ".project.yaml")
	if err := Save(dst, &Project{}); err != nil {
		// Save of an empty Project writes a non-empty YAML; create the
		// true-empty case by truncating.
		t.Fatalf("Save empty: %v", err)
	}
	// Now create a truly empty file.
	empty := filepath.Join(t.TempDir(), ".project.yaml")
	if err := writeEmpty(empty); err != nil {
		t.Fatalf("writeEmpty: %v", err)
	}
	p, err := Load(empty)
	if err != nil {
		t.Fatalf("Load empty: %v", err)
	}
	if !p.Empty() {
		t.Errorf("Empty() = false for zero-byte file: %+v", p)
	}
}

func TestRepoShorthandUnmarshal(t *testing.T) {
	cases := []struct {
		name           string
		yaml           string
		wantOrg, wantN string
		wantBase       string
		wantErr        bool
	}{
		{"string shorthand", "repos:\n  - docker/desktop\nstatus: ready\n", "docker", "desktop", "", false},
		{"shorthand with base", "repos:\n  - docker/desktop@integration\nstatus: ready\n", "docker", "desktop", "integration", false},
		{"mapping form", "repos:\n  - org: docker\n    name: desktop\nstatus: ready\n", "docker", "desktop", "", false},
		{"missing slash", "repos:\n  - justname\nstatus: ready\n", "", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var p Project
			err := yaml.Unmarshal([]byte(tc.yaml), &p)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got repos=%+v", p.Repos)
				}
				return
			}
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(p.Repos) != 1 {
				t.Fatalf("want 1 repo, got %d", len(p.Repos))
			}
			r := p.Repos[0]
			if r.Org != tc.wantOrg || r.Name != tc.wantN || r.BaseBranch != tc.wantBase {
				t.Errorf("got %+v, want {Org:%s Name:%s Base:%s}", r, tc.wantOrg, tc.wantN, tc.wantBase)
			}
		})
	}
}

func TestNewReposParsing(t *testing.T) {
	src := "" +
		"description: build a thing\n" +
		"repos:\n" +
		"  - docker/desktop\n" +
		"new_repos:\n" +
		"  - slimslenderslacks/weather-tui-madness\n" +
		"  - org: acme\n" +
		"    name: gizmo\n" +
		"    visibility: public\n" +
		"branch: main\n" +
		"status: ready\n"
	var p Project
	if err := yaml.Unmarshal([]byte(src), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(p.Repos) != 1 || p.Repos[0].Org != "docker" || p.Repos[0].Name != "desktop" {
		t.Errorf("repos = %+v", p.Repos)
	}
	if len(p.NewRepos) != 2 {
		t.Fatalf("want 2 new_repos, got %d: %+v", len(p.NewRepos), p.NewRepos)
	}
	// Shorthand form, default (empty) visibility.
	if p.NewRepos[0].Org != "slimslenderslacks" || p.NewRepos[0].Name != "weather-tui-madness" ||
		p.NewRepos[0].Visibility != "" {
		t.Errorf("new_repos[0] = %+v", p.NewRepos[0])
	}
	// Mapping form with explicit visibility.
	if p.NewRepos[1].Org != "acme" || p.NewRepos[1].Name != "gizmo" ||
		p.NewRepos[1].Visibility != "public" {
		t.Errorf("new_repos[1] = %+v", p.NewRepos[1])
	}
}

func TestArchiveFlagLoad(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want bool
	}{
		{
			name: "archive true",
			yaml: "description: x\nbranch: feat/y\nstatus: working\narchive: true\n",
			want: true,
		},
		{
			name: "archive false",
			yaml: "description: x\nbranch: feat/y\nstatus: working\narchive: false\n",
			want: false,
		},
		{
			// The common case: a project file written before the flag existed.
			name: "key absent",
			yaml: "description: x\nbranch: feat/y\nstatus: working\n",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), ".project.yaml")
			if err := os.WriteFile(dst, []byte(tc.yaml), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			p, err := Load(dst)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if p.Archive != tc.want {
				t.Errorf("Archive = %v, want %v", p.Archive, tc.want)
			}
			// The other fields must survive alongside it.
			if p.Description != "x" || p.Branch != "feat/y" || p.Status != StatusWorking {
				t.Errorf("other fields disturbed: %+v", p)
			}
		})
	}
}

func TestArchiveOmitEmpty(t *testing.T) {
	// A project without the flag must re-save without adding an `archive:`
	// key, so files predating the flag stay byte-identical.
	dst := filepath.Join(t.TempDir(), ".project.yaml")
	if err := SaveAs(dst, &Project{
		Description: "x", Branch: "feat/y", Status: StatusWorking,
	}, WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	raw, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(raw), "archive") {
		t.Errorf("false Archive should not emit an archive line; got:\n%s", string(raw))
	}
}

func TestArchiveRoundTrip(t *testing.T) {
	dst := filepath.Join(t.TempDir(), ".project.yaml")
	src := &Project{
		Description: "x",
		Branch:      "feat/y",
		Status:      StatusWorking,
		Repos:       []Repo{{Org: "slimslenderslacks", Name: "workingman"}},
		Archive:     true,
	}
	if err := SaveAs(dst, src, WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	raw, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), "archive: true") {
		t.Errorf("want `archive: true` on disk; got:\n%s", string(raw))
	}
	reloaded, err := Load(dst)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reloaded.Archive {
		t.Errorf("Archive = false after round-trip")
	}
	// Nothing else may shift: the flag is independent of Status.
	if reloaded.Status != StatusWorking {
		t.Errorf("Status = %q, want working", reloaded.Status)
	}
	// SaveAs stamps the writer on the copy it writes, not on src.
	reloaded.UpdatedBy = src.UpdatedBy
	if !reflect.DeepEqual(src, reloaded) {
		t.Errorf("round-trip mismatch:\n src=%+v\n got=%+v", src, reloaded)
	}
}

func TestEmptyIgnoresNewReposPresence(t *testing.T) {
	// A project that carries only new_repos is NOT empty — it's a populated
	// intent the daemon should act on.
	p := Project{NewRepos: []Repo{{Org: "x", Name: "y"}}}
	if p.Empty() {
		t.Errorf("project with new_repos should not be Empty()")
	}
}
