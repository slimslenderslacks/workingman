package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseRole(t *testing.T) {
	cases := map[string]string{
		"Package x does y.\n\nRole: daemon\n\nMore.": "daemon",
		"Package x does y.\n\nRole: TUI\n":           "tui",
		"Package x does y.\nRole:   wrapper — acp":   "wrapper",
		"Package x does y.\n\nRole: client\n":        todo,
		"Package x does y.\n\nIts role: shared\n":    todo,
		"": todo,
	}
	for text, want := range cases {
		if got := parseRole(text); got != want {
			t.Errorf("parseRole(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestRenderGroupsByRole(t *testing.T) {
	pkgs := []*Package{
		{Rel: "cmd/orch", Entry: true, Imports: []string{"internal/a"}},
		{Rel: "internal/a", Role: "daemon", Synopsis: "Package a | b.", HasDocGo: true, Imports: []string{"internal/b-c"}},
		{Rel: "internal/b-c", Role: todo, Synopsis: todo},
	}
	var buf bytes.Buffer
	render(&buf, pkgs)
	out := buf.String()
	for _, want := range []string{
		"| `internal/a` | daemon | Package a \\| b. | — | — |",
		"| `internal/b-c` | TODO | TODO | — | — |",
		`subgraph grp_daemon["daemon"]`,
		`internal_b_c["b-c"]`,
		"cmd_orch --> internal_a",
		"internal_a --> internal_b_c",
		"class internal_b_c TODO",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
	index, _, _ := strings.Cut(out, "## Dependency graph")
	if strings.Contains(index, "cmd/orch") {
		t.Errorf("cmd/orch should not appear in the package index")
	}
}
