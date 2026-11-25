package slack

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/mcpgateway"
)

var update = flag.Bool("update", false, "rewrite the testdata/*.golden files")

// TestParseGolden parses every testdata/read_*.json tool result (the live
// samples from docs/research/slack-mcp.md §3, plus synthetic edge cases) and
// compares the parsed page with its .golden file. Run with -update after a
// deliberate parser change.
func TestParseGolden(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "read_*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no testdata: %v", err)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			res := mcpgateway.ToolResult{Content: []mcpgateway.Content{{Type: "text", Text: string(raw)}}}
			text, pagination := readResult(res)
			var page any
			if strings.HasPrefix(filepath.Base(f), "read_thread") {
				page = ParseThread(text, pagination)
			} else {
				page = ParseChannel(text, pagination)
			}
			got, err := json.MarshalIndent(page, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			golden := strings.TrimSuffix(f, ".json") + ".golden"
			if *update {
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run go test -update to create it)", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("parsed %s differs from %s:\n got: %s\nwant: %s", f, golden, got, want)
			}
		})
	}
}

func TestReadResultForms(t *testing.T) {
	sc := mcpgateway.ToolResult{StructuredContent: json.RawMessage(`{"messages":"m","pagination_info":"p"}`)}
	if m, p := readResult(sc); m != "m" || p != "p" {
		t.Errorf("structured = %q, %q", m, p)
	}
	plain := mcpgateway.ToolResult{Content: []mcpgateway.Content{{Type: "text", Text: "just text"}}}
	if m, p := readResult(plain); m != "just text" || p != "" {
		t.Errorf("plain = %q, %q", m, p)
	}
}

func TestSplitFrom(t *testing.T) {
	for in, want := range map[string][2]string{
		"Jim Clark <jim.clark@okta.com> (U0CC1K1MFNX)": {"U0CC1K1MFNX", "Jim Clark"},
		"Jim Clark (U0CC1K1MFNX)":                      {"U0CC1K1MFNX", "Jim Clark"},
		"Jim Clark":                                    {"", "Jim Clark"},
	} {
		if id, name := splitFrom(in); id != want[0] || name != want[1] {
			t.Errorf("splitFrom(%q) = %q, %q; want %q, %q", in, id, name, want[0], want[1])
		}
	}
}

func TestParseCursor(t *testing.T) {
	if got := parseCursor(`Use cursor: "abc=" to get more.`); got != "abc=" {
		t.Errorf("cursor = %q", got)
	}
	if got := parseCursor("There are no more messages available.\n"); got != "" {
		t.Errorf("last page cursor = %q", got)
	}
}
