package signal

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestMarkdownToSignal(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		wantText   string
		wantStyles []string
	}{
		{"plain", "hello world", "hello world", nil},
		{"bold stars", "a **bold** b", "a bold b", []string{"2:4:BOLD"}},
		{"bold underscores", "__bold__ x", "bold x", []string{"0:4:BOLD"}},
		{"italic star", "an *it* here", "an it here", []string{"3:2:ITALIC"}},
		{"italic underscore", "an _it_ here", "an it here", []string{"3:2:ITALIC"}},
		{"snake_case is not italic", "use snake_case_names now", "use snake_case_names now", nil},
		{"star list is not italic", "* one\n* two", "• one\n• two", nil},
		{"strikethrough", "~~gone~~", "gone", []string{"0:4:STRIKETHROUGH"}},
		{"inline code", "run `ls -l` now", "run ls -l now", []string{"4:5:MONOSPACE"}},
		{"code block", "```go\nx := 1\n```", "x := 1", []string{"0:6:MONOSPACE"}},
		{"markers inside code block untouched", "```\n**not bold**\n```", "**not bold**", []string{"0:12:MONOSPACE"}},
		{"heading", "## Title\ntext", "Title\ntext", []string{"0:5:BOLD"}},
		{"heading with inline", "# A **b** c\nx", "A b c\nx", []string{"0:5:BOLD", "2:1:BOLD"}},
		{"bullets", "- a\n- b", "• a\n• b", nil},
		{"bullets inside code kept", "```\n- a\n```", "- a", []string{"0:3:MONOSPACE"}},
		{"blank lines collapsed", "a\n\n\n\nb", "a\n\nb", nil},
		{"utf16 offsets for astral chars", "😀 **x**", "😀 x", []string{"3:1:BOLD"}},
		{"bold wins over italic", "**x**", "x", []string{"0:1:BOLD"}},
		{"after code block shifts", "```\nab\n```\n**c**", "ab\nc", []string{"0:2:MONOSPACE", "3:1:BOLD"}},
		{"topic prefix survives", "[wolf] started **now**", "[wolf] started now", []string{"15:3:BOLD"}},
		{"unbalanced marker left alone", "2 * 3 = 6", "2 * 3 = 6", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, styles := MarkdownToSignal(tt.in)
			if text != tt.wantText {
				t.Errorf("text = %q, want %q", text, tt.wantText)
			}
			if !reflect.DeepEqual(styles, tt.wantStyles) {
				t.Errorf("styles = %v, want %v", styles, tt.wantStyles)
			}
		})
	}
}

func TestMarkdownToSignalNeverLosesText(t *testing.T) {
	// Whatever the input, the styled output must not reference past the text.
	for _, in := range []string{"", "***", "**", "`", "```", "_", "__a", "* ", "#", "# ", "a_b_c _d_", "😀*x*😀", strings.Repeat("*a* ", 50)} {
		text, styles := MarkdownToSignal(in)
		max := utf16Len(text)
		for _, s := range styles {
			parts := strings.Split(s, ":")
			if len(parts) != 3 {
				t.Errorf("input %q: malformed style %q", in, s)
				continue
			}
			a, errA := strconv.Atoi(parts[0])
			b, errB := strconv.Atoi(parts[1])
			if errA != nil || errB != nil || a < 0 || b <= 0 || a+b > max {
				t.Errorf("input %q: bad style %q for text %q (utf16 len %d)", in, s, text, max)
			}
		}
	}
}
