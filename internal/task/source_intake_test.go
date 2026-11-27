package task

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntakeSourceRoundTrip pins the intake back-reference (R.4 Q12): it saves
// and loads intact, reads back through IntakeRef, and is written without the
// PR fields a planned task has no use for.
func TestIntakeSourceRoundTrip(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "add-healthz.yaml")
	if err := Save(dst, &Task{
		Name:   "add-healthz",
		Status: StatusReady,
		Source: &Source{Kind: SourceKindIntake, Ref: "add-healthz"},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if want := "source:\n    kind: intake\n    ref: add-healthz\n"; !strings.Contains(string(data), want) {
		t.Errorf("YAML lacks %q:\n%s", want, data)
	}
	if strings.Contains(string(data), "pr:") {
		t.Errorf("an intake source wrote a pr field:\n%s", data)
	}
	got, err := Load(dst)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Source == nil || *got.Source != (Source{Kind: SourceKindIntake, Ref: "add-healthz"}) {
		t.Errorf("Source = %+v, want {intake, add-healthz}", got.Source)
	}
	if got.IntakeRef() != "add-healthz" {
		t.Errorf("IntakeRef() = %q, want add-healthz", got.IntakeRef())
	}
	if got.IsPushTask() || got.Source.IsPR() {
		t.Errorf("intake source taken for a PR source: IsPushTask=%v IsPR=%v", got.IsPushTask(), got.Source.IsPR())
	}
}

// TestPRSourceYAMLUnchanged guards the other side of the intake encoding: PR
// and push sources keep their exact layout, `pr: 0` included for a push task
// that has no PR yet.
func TestPRSourceYAMLUnchanged(t *testing.T) {
	cases := []struct {
		src  Source
		want string
	}{
		{Source{Kind: "pr-comment", Ref: "PRRT_x", Repo: "o/r", PR: 7}, "source:\n    kind: pr-comment\n    ref: PRRT_x\n    repo: o/r\n    pr: 7\n"},
		{Source{Kind: "pr-check", Ref: "build", PR: 3}, "source:\n    kind: pr-check\n    ref: build\n    pr: 3\n"},
		{Source{Kind: SourceKindPush, Repo: "o/r"}, "source:\n    kind: pr-push\n    ref: \"\"\n    repo: o/r\n    pr: 0\n"},
	}
	for _, tc := range cases {
		dst := filepath.Join(t.TempDir(), "t.yaml")
		src := tc.src
		if err := Save(dst, &Task{Name: "t", Status: StatusReady, Source: &src}); err != nil {
			t.Fatalf("Save: %v", err)
		}
		data, err := os.ReadFile(dst)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if !strings.Contains(string(data), tc.want) {
			t.Errorf("%s: YAML lacks %q:\n%s", tc.src.Kind, tc.want, data)
		}
		got, err := Load(dst)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got.Source == nil || *got.Source != tc.src {
			t.Errorf("%s: Source = %+v, want %+v", tc.src.Kind, got.Source, tc.src)
		}
		if !got.Source.IsPR() || got.IntakeRef() != "" {
			t.Errorf("%s: IsPR=%v IntakeRef=%q, want a PR source", tc.src.Kind, got.Source.IsPR(), got.IntakeRef())
		}
	}
}

// TestParseToleratesBadSource: the source block is agent-written and
// best-effort, so neither its absence nor a malformed or unknown one may stop
// a task file from loading.
func TestParseToleratesBadSource(t *testing.T) {
	cases := []struct {
		name, yaml string
		want       *Source
	}{
		{"no source", "name: t\nstatus: ready\n", nil},
		{"scalar", "name: t\nstatus: ready\nsource: intake/foo.md\n", nil},
		{"list", "name: t\nstatus: ready\nsource: [intake, foo]\n", nil},
		{"empty map", "name: t\nstatus: ready\nsource: {}\n", nil},
		{"bad pr", "name: t\nstatus: ready\nsource: {kind: intake, ref: foo, pr: none}\n", &Source{Kind: SourceKindIntake, Ref: "foo"}},
		{"nested ref", "name: t\nstatus: ready\nsource: {kind: intake, ref: {file: foo}}\n", &Source{Kind: SourceKindIntake}},
		{"unknown kind", "name: t\nstatus: ready\nsource: {kind: jira, ref: ABC-1, extra: x}\n", &Source{Kind: "jira", Ref: "ABC-1"}},
	}
	for _, tc := range cases {
		got, err := Parse("t.yaml", []byte(tc.yaml))
		if err != nil {
			t.Errorf("%s: Parse: %v", tc.name, err)
			continue
		}
		switch {
		case tc.want == nil && got.Source != nil:
			t.Errorf("%s: Source = %+v, want nil", tc.name, got.Source)
		case tc.want != nil && (got.Source == nil || *got.Source != *tc.want):
			t.Errorf("%s: Source = %+v, want %+v", tc.name, got.Source, tc.want)
		}
		if got.Name != "t" || got.Status != StatusReady {
			t.Errorf("%s: rest of the task lost: %+v", tc.name, got)
		}
		if got.IsPushTask() || got.Source.IsPR() {
			t.Errorf("%s: taken for a PR source", tc.name)
		}
	}
}

func TestIntakeRefFor(t *testing.T) {
	for in, want := range map[string]string{
		"/orch/p/intake/add-healthz.md":        "add-healthz",
		"/orch/p/intake/add-healthz.processed": "add-healthz",
		"intake/v1.2-notes.md":                 "v1.2-notes",
		"foo":                                  "foo",
	} {
		if got := IntakeRefFor(in); got != want {
			t.Errorf("IntakeRefFor(%q) = %q, want %q", in, got, want)
		}
	}
}
