package whatsapp

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFormatMessage(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"bold", "this is **bold** text", "this is *bold* text"},
		{"underscore bold", "this is __bold__ text", "this is *bold* text"},
		{"italic", "this is *italic* text", "this is _italic_ text"},
		{"bold and italic together", "**bold** and *it*", "*bold* and _it_"},
		{"strike", "~~gone~~", "~gone~"},
		{"headings", "# One\n## Two\n###### Six", "*One*\n*Two*\n*Six*"},
		{"heading with bold inside", "# **Title**", "*Title*"},
		{"not a heading", "#hashtag", "#hashtag"},
		{"link", "see [docs](https://x.test/a?b=1)", "see docs (https://x.test/a?b=1)"},
		{"bullets untouched", "* one\n* two", "* one\n* two"},
		{"italic does not span lines", "*a\nb*", "*a\nb*"},
		{"lone asterisk", "2 * 3 = 6", "2 * 3 = 6"},
		{"fence preserved", "```go\n**x** # y *z*\n```", "```go\n**x** # y *z*\n```"},
		{"inline code preserved", "run `**x**` now **y**", "run `**x**` now *y*"},
		{"fence and text", "**a**\n```\n**b**\n```\n**c**", "*a*\n```\n**b**\n```\n*c*"},
		{"zero width stripped", "a\u200bb\u2060c\ufeffd", "abcd"},
		{"odd space normalized", "a\u00a0b\u202fc", "a b c"},
		{"emoji joiner kept", "👨\u200d👩", "👨\u200d👩"},
		{"nul in input", "a\x00FENCE0\x00b", "aFENCE0b"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormatMessage(tc.in); got != tc.want {
				t.Errorf("FormatMessage(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestChunkMessage_ShortIsOneUnmarkedChunk(t *testing.T) {
	got := ChunkMessage("hello", 4096)
	if len(got) != 1 || got[0] != "hello" {
		t.Fatalf("got %q", got)
	}
	exact := strings.Repeat("a", 4096)
	if got := ChunkMessage(exact, 4096); len(got) != 1 || got[0] != exact {
		t.Fatalf("a message of exactly the limit must not be split (got %d chunks)", len(got))
	}
}

func TestChunkMessage_Boundaries(t *testing.T) {
	const limit = 100
	para := func(c byte) string { return strings.Repeat(string(c), 40) }

	t.Run("prefers paragraph breaks", func(t *testing.T) {
		// Three 40-char paragraphs: two fit in a chunk, so the cut is at a "\n\n".
		text := para('a') + "\n\n" + para('b') + "\n\n" + para('c')
		got := ChunkMessage(text, limit)
		if len(got) != 2 {
			t.Fatalf("got %d chunks: %q", len(got), got)
		}
		if want := para('a') + "\n\n" + para('b') + " (1/2)"; got[0] != want {
			t.Errorf("chunk 1 = %q, want %q", got[0], want)
		}
		if want := para('c') + " (2/2)"; got[1] != want {
			t.Errorf("chunk 2 = %q, want %q", got[1], want)
		}
	})

	t.Run("falls back to line breaks", func(t *testing.T) {
		line := strings.Repeat("x", 30)
		text := strings.Join([]string{line, line, line, line, line}, "\n")
		for i, c := range ChunkMessage(text, limit) {
			body := c[:strings.LastIndex(c, " (")]
			for _, l := range strings.Split(body, "\n") {
				if l != line {
					t.Errorf("chunk %d broke a line: %q", i+1, l)
				}
			}
		}
	})

	t.Run("falls back to words", func(t *testing.T) {
		text := strings.TrimSpace(strings.Repeat("word ", 60))
		for i, c := range ChunkMessage(text, limit) {
			body := c[:strings.LastIndex(c, " (")]
			for _, w := range strings.Fields(body) {
				if w != "word" {
					t.Errorf("chunk %d broke a word: %q", i+1, w)
				}
			}
		}
	})

	t.Run("hard cuts unbroken text", func(t *testing.T) {
		got := ChunkMessage(strings.Repeat("z", 250), limit)
		if len(got) < 3 {
			t.Fatalf("got %d chunks", len(got))
		}
		var total int
		for _, c := range got {
			total += strings.Count(c, "z")
		}
		if total != 250 {
			t.Errorf("lost characters: %d of 250 survived", total)
		}
	})
}

func TestChunkMessage_NeverExceedsLimit(t *testing.T) {
	inputs := map[string]string{
		"unbroken":   strings.Repeat("q", 20000),
		"multibyte":  strings.Repeat("héllo wörld 🌍 ", 1500),
		"paragraphs": strings.Repeat("A paragraph of some words.\n\n", 800),
		"fenced":     "```go\n" + strings.Repeat("fmt.Println(\"hi\")\n", 1500) + "```\n",
	}
	for name, in := range inputs {
		t.Run(name, func(t *testing.T) {
			chunks := ChunkMessage(in, MaxMessageLength)
			if len(chunks) < 2 {
				t.Fatalf("expected a split, got %d chunk(s)", len(chunks))
			}
			for i, c := range chunks {
				if n := utf8.RuneCountInString(c); n > MaxMessageLength {
					t.Errorf("chunk %d is %d chars, over %d", i+1, n, MaxMessageLength)
				}
				if !utf8.ValidString(c) {
					t.Errorf("chunk %d is not valid UTF-8", i+1)
				}
			}
		})
	}
}

func TestChunkMessage_PageMarkers(t *testing.T) {
	chunks := ChunkMessage(strings.Repeat("word ", 400), 500)
	for i, c := range chunks {
		want := " (" + string(rune('1'+i)) + "/" + string(rune('0'+len(chunks))) + ")"
		if !strings.HasSuffix(c, want) {
			t.Errorf("chunk %d = …%q, want suffix %q", i+1, c[max(0, len(c)-12):], want)
		}
	}
}

func TestChunkMessage_FencesStayBalanced(t *testing.T) {
	code := "```python\n" + strings.Repeat("print('hello')\n", 60) + "```"
	chunks := ChunkMessage("intro\n"+code+"\noutro", 300)
	if len(chunks) < 3 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		body := c[:strings.LastIndex(c, " (")]
		if n := strings.Count(body, "```"); n%2 != 0 {
			t.Errorf("chunk %d has an unclosed fence (%d markers):\n%s", i+1, n, body)
		}
		if i > 0 && i < len(chunks)-1 && !strings.HasPrefix(body, "```python\n") {
			t.Errorf("middle chunk %d did not reopen the fence with its language: %q", i+1, body[:min(20, len(body))])
		}
	}
}

func TestChunkMessage_AvoidsSplittingInlineCode(t *testing.T) {
	// The cut would land inside `…` unless it backs off to before the span.
	text := strings.Repeat("a ", 20) + "`" + strings.Repeat("c", 60) + "` tail words here for padding to force a split"
	chunks := ChunkMessage(text, 100)
	for i, c := range chunks {
		body := c[:strings.LastIndex(c, " (")]
		if strings.Count(body, "`")%2 != 0 {
			t.Errorf("chunk %d splits an inline code span: %q", i+1, body)
		}
	}
}

func TestChunkMessage_KeepsCodeIndentationOnContinuation(t *testing.T) {
	code := "```\n" + strings.Repeat("x\n", 80) + "    indented\n" + strings.Repeat("y\n", 80) + "```"
	joined := strings.Join(ChunkMessage(code, 120), "")
	if !strings.Contains(joined, "    indented") {
		t.Error("indentation inside a code block was lost")
	}
}
