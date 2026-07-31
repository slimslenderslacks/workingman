package workspace

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- Pure JSON-parsing tests ----

func TestParseNewResultSuccess(t *testing.T) {
	// Captured from `wsp new --json --empty test-orch-probe`.
	out := []byte(`Creating workspace "test-orch-probe" with 0 repos...
{
  "ok": true,
  "message": "Workspace created: /Users/slim/dev/workspaces/test-orch-probe",
  "duration_ms": 10,
  "workspace": "test-orch-probe",
  "path": "/Users/slim/dev/workspaces/test-orch-probe",
  "branch": "test-orch-probe"
}`)
	path, errMsg, err := parseNewResult(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if errMsg != "" {
		t.Errorf("errMsg = %q, want empty", errMsg)
	}
	if path != "/Users/slim/dev/workspaces/test-orch-probe" {
		t.Errorf("path = %q", path)
	}
}

func TestParseNewResultAlreadyExists(t *testing.T) {
	out := []byte(`{
  "error": "workspace \"test-orch-probe\" already exists"
}`)
	path, errMsg, err := parseNewResult(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if path != "" {
		t.Errorf("path = %q, want empty on error", path)
	}
	if !isAlreadyExists(errMsg) {
		t.Errorf("isAlreadyExists(%q) = false", errMsg)
	}
}

func TestParseLsResult(t *testing.T) {
	out := []byte(`{
  "workspaces": [
    {"name": "a", "path": "/x/a"},
    {"name": "b", "path": "/x/b"}
  ]
}`)
	entries, err := parseLsResult(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(entries) != 2 || entries[0].Name != "a" || entries[1].Path != "/x/b" {
		t.Errorf("entries = %+v", entries)
	}
}

func TestParseRmResultSuccess(t *testing.T) {
	out := []byte(`Removing workspace "x"...
{"ok": true, "message": "Workspace removed."}`)
	ok, errMsg, err := parseRmResult(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !ok || errMsg != "" {
		t.Errorf("ok=%v errMsg=%q", ok, errMsg)
	}
}

func TestParseRmResultNotFound(t *testing.T) {
	out := []byte(`{"error": "reading workspace metadata: opening /…/.wsp.yaml"}`)
	ok, errMsg, err := parseRmResult(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ok {
		t.Error("ok should be false")
	}
	if !isNotFound(errMsg) {
		t.Errorf("isNotFound(%q) = false", errMsg)
	}
}

func TestSSHURLFromIdentity(t *testing.T) {
	cases := []struct {
		identity, want string
	}{
		{"github.com/foo/bar", "git@github.com:foo/bar.git"},
		{"github.com/slimslenderslacks/workingman", "git@github.com:slimslenderslacks/workingman.git"},
		{"gitlab.example.com/team/repo", "git@gitlab.example.com:team/repo.git"},
		{"", ""},
		{"foo", ""},
		{"foo/", ""},
		{"foo/bar", ""},
		{"github.com/onlyone", ""},
		{"/leading-slash/x", ""},
	}
	for _, c := range cases {
		if got := sshURLFromIdentity(c.identity); got != c.want {
			t.Errorf("sshURLFromIdentity(%q) = %q, want %q", c.identity, got, c.want)
		}
	}
}

func TestParseRegistryListResult(t *testing.T) {
	out := []byte(`{
  "repos": [
    {
      "identity": "github.com/docker/dash",
      "shortname": "dash",
      "url": "git@github.com:docker/dash.git"
    },
    {
      "identity": "github.com/docker/gantry",
      "shortname": "gantry",
      "url": "https://github.com/docker/gantry.git"
    }
  ]
}`)
	entries, err := parseRegistryListResult(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	if entries[0].Identity != "github.com/docker/dash" || entries[1].URL != "https://github.com/docker/gantry.git" {
		t.Errorf("entries = %+v", entries)
	}
}

func TestParseRegistryAddResultSuccess(t *testing.T) {
	out := []byte(`{"ok": true, "message": "added"}`)
	ok, errMsg, err := parseRegistryAddResult(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !ok || errMsg != "" {
		t.Errorf("ok=%v errMsg=%q", ok, errMsg)
	}
}

func TestParseRegistryAddResultError(t *testing.T) {
	out := []byte(`{"error": "could not clone: Permission denied (publickey)"}`)
	ok, errMsg, err := parseRegistryAddResult(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ok || errMsg == "" {
		t.Errorf("expected error: ok=%v errMsg=%q", ok, errMsg)
	}
}

func TestParseNewRejectsNonJSON(t *testing.T) {
	out := []byte("totally not json")
	if _, _, err := parseNewResult(out); err == nil {
		t.Error("expected parse error")
	}
}

// ---- Integration tests against real wsp ----

// These tests run the real wsp CLI to verify the shell-out wiring. They
// create and tear down a uniquely-named empty workspace so they cannot
// interfere with the user's real workspaces. Skipped when wsp is not
// installed (CI without wsp).

func TestWspManagerCreateAndPath_Integration(t *testing.T) {
	if _, err := exec.LookPath("wsp"); err != nil {
		t.Skip("wsp not on PATH")
	}
	m := NewWsp()
	name := fmt.Sprintf("orch-test-create-%d", time.Now().UnixNano())
	ctx := context.Background()
	t.Cleanup(func() { _ = m.Remove(ctx, name) })

	path, err := m.Create(ctx, name, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !filepath.IsAbs(path) {
		t.Errorf("path not absolute: %s", path)
	}
	if !strings.Contains(path, name) {
		t.Errorf("path %q does not contain name %q", path, name)
	}

	// Path() resolves the same location.
	got, err := m.Path(name)
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if got != path {
		t.Errorf("Path = %q, want %q", got, path)
	}
}

func TestWspManagerCreateIsIdempotent_Integration(t *testing.T) {
	if _, err := exec.LookPath("wsp"); err != nil {
		t.Skip("wsp not on PATH")
	}
	m := NewWsp()
	name := fmt.Sprintf("orch-test-idem-%d", time.Now().UnixNano())
	ctx := context.Background()
	t.Cleanup(func() { _ = m.Remove(ctx, name) })

	first, err := m.Create(ctx, name, nil)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	second, err := m.Create(ctx, name, nil)
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if first != second {
		t.Errorf("Create returned different paths on re-create: %q vs %q", first, second)
	}
}

func TestWspManagerRemoveIsIdempotent_Integration(t *testing.T) {
	if _, err := exec.LookPath("wsp"); err != nil {
		t.Skip("wsp not on PATH")
	}
	m := NewWsp()
	name := fmt.Sprintf("orch-test-rm-%d", time.Now().UnixNano())
	ctx := context.Background()

	if _, err := m.Create(ctx, name, nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.Remove(ctx, name); err != nil {
		t.Fatalf("first Remove: %v", err)
	}
	if err := m.Remove(ctx, name); err != nil {
		t.Errorf("second Remove (idempotent): %v", err)
	}
}

func TestGhOwnerName(t *testing.T) {
	cases := []struct {
		identity string
		want     string
		wantErr  bool
	}{
		{"github.com/slimslenderslacks/weather-tui-madness", "slimslenderslacks/weather-tui-madness", false},
		{"github.com/docker/desktop", "docker/desktop", false},
		{"gitlab.com/org/repo", "", true},       // non-github
		{"github.com/onlyowner", "", true},      // no name
		{"github.com/org/name/extra", "", true}, // too many segments
		{"github.com/org/", "", true},           // trailing slash
	}
	for _, tc := range cases {
		got, err := ghOwnerName(tc.identity)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ghOwnerName(%q) = %q, want error", tc.identity, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ghOwnerName(%q) = (%q, %v), want (%q, nil)", tc.identity, got, err, tc.want)
		}
	}
}

func TestGhCreateArgs(t *testing.T) {
	priv := ghCreateArgs("acme/gizmo", "")
	if strings.Join(priv, " ") != "repo create acme/gizmo --private --add-readme" {
		t.Errorf("default visibility args = %v", priv)
	}
	pub := ghCreateArgs("acme/gizmo", "public")
	if strings.Join(pub, " ") != "repo create acme/gizmo --public --add-readme" {
		t.Errorf("public visibility args = %v", pub)
	}
	// Any non-"public" value falls back to private.
	if got := ghCreateArgs("a/b", "weird"); got[3] != "--private" {
		t.Errorf("unknown visibility should be private, got %v", got)
	}
}
