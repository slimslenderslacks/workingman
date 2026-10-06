package signal

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// Port of hermes-agent's gateway/platforms/signal_format.py: Markdown becomes
// plain text plus signal-cli text styles ("start:length:STYLE", positions in
// UTF-16 code units). Supported styles: BOLD, ITALIC, STRIKETHROUGH, MONOSPACE.
// Unlike the Python original, pipe tables are left as plain text.

var (
	codeBlockRE = regexp.MustCompile("(?s)```[a-zA-Z0-9_+-]*\n?(.*?)```")
	headingRE   = regexp.MustCompile(`(?m)^#{1,6}[ \t]+`)
	bulletRE    = regexp.MustCompile(`(?m)^([ \t]{0,3})[-*+][ \t]+`)
	blankRunsRE = regexp.MustCompile(`\n{3,}`)

	boldStarRE   = regexp.MustCompile(`(?s)\*\*(.+?)\*\*`)
	boldUnderRE  = regexp.MustCompile(`(?s)__(.+?)__`)
	strikeRE     = regexp.MustCompile(`~~(.+?)~~`)
	monospaceRE  = regexp.MustCompile("`(.+?)`")
	fenceSplitRE = regexp.MustCompile("(?s)```.*?```")
)

// span is a styled byte range [start, end) of some text.
type span struct {
	start, end int
	style      string
}

// match is one inline marker pair: outer [start, end), inner [gs, ge).
type match struct {
	start, end, gs, ge int
	style              string
}

// MarkdownToSignal converts Markdown to plain text plus Signal text styles.
func MarkdownToSignal(text string) (string, []string) {
	text = strings.TrimSpace(blankRunsRE.ReplaceAllString(text, "\n\n"))
	text = normalizeBullets(text)

	var out strings.Builder
	var spans []span
	last := 0
	for _, loc := range codeBlockRE.FindAllStringSubmatchIndex(text, -1) {
		appendPlain(&out, &spans, text[last:loc[0]])
		inner := strings.TrimRight(text[loc[2]:loc[3]], "\n")
		if inner != "" {
			spans = append(spans, span{out.Len(), out.Len() + len(inner), "MONOSPACE"})
		}
		out.WriteString(inner)
		last = loc[1]
	}
	appendPlain(&out, &spans, text[last:])

	final := out.String()
	sort.SliceStable(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		return spans[i].end < spans[j].end
	})
	var styles []string
	for _, s := range spans {
		if s.start < 0 || s.end > len(final) || s.end <= s.start {
			continue
		}
		styles = append(styles, fmt.Sprintf("%d:%d:%s", utf16Len(final[:s.start]), utf16Len(final[s.start:s.end]), s.style))
	}
	return final, styles
}

// normalizeBullets turns "- item" into "• item" outside code fences, since
// Signal renders the Markdown marker literally.
func normalizeBullets(s string) string {
	var b strings.Builder
	last := 0
	for _, loc := range fenceSplitRE.FindAllStringIndex(s, -1) {
		b.WriteString(bulletRE.ReplaceAllString(s[last:loc[0]], "${1}• "))
		b.WriteString(s[loc[0]:loc[1]])
		last = loc[1]
	}
	b.WriteString(bulletRE.ReplaceAllString(s[last:], "${1}• "))
	return b.String()
}

// appendPlain handles a non-code segment: headings become bold lines, inline
// markers are stripped and recorded. Spans are appended in out's coordinates.
func appendPlain(out *strings.Builder, spans *[]span, seg string) {
	if seg == "" {
		return
	}
	// Headings: drop the "## " and bold the line.
	var stripped strings.Builder
	var heads []span // in stripped coordinates
	last := 0
	for _, loc := range headingRE.FindAllStringIndex(seg, -1) {
		stripped.WriteString(seg[last:loc[0]])
		eol := strings.IndexByte(seg[loc[1]:], '\n')
		if eol < 0 {
			eol = len(seg)
		} else {
			eol += loc[1]
		}
		start := stripped.Len()
		stripped.WriteString(seg[loc[1]:eol])
		heads = append(heads, span{start, stripped.Len(), "BOLD"})
		last = eol
	}
	stripped.WriteString(seg[last:])
	text := stripped.String()

	// Inline markers: the first pattern to claim a span wins.
	var matches []match
	claimed := func(s, e int) bool {
		for _, m := range matches {
			if s < m.end && e > m.start {
				return true
			}
		}
		return false
	}
	for _, p := range []struct {
		re    *regexp.Regexp
		style string
	}{{boldStarRE, "BOLD"}, {boldUnderRE, "BOLD"}, {strikeRE, "STRIKETHROUGH"}, {monospaceRE, "MONOSPACE"}} {
		for _, loc := range p.re.FindAllStringSubmatchIndex(text, -1) {
			if !claimed(loc[0], loc[1]) {
				matches = append(matches, match{loc[0], loc[1], loc[2], loc[3], p.style})
			}
		}
	}
	for _, m := range italicMatches(text) {
		if !claimed(m.start, m.end) {
			matches = append(matches, m)
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].start < matches[j].start })

	// Strip the markers, recording removals so heading spans can be shifted.
	base := out.Len()
	var removals [][2]int // (pos, len) in stripped coordinates
	pos := 0
	for _, m := range matches {
		if m.gs > m.start {
			removals = append(removals, [2]int{m.start, m.gs - m.start})
		}
		if m.end > m.ge {
			removals = append(removals, [2]int{m.ge, m.end - m.ge})
		}
		out.WriteString(text[pos:m.start])
		*spans = append(*spans, span{out.Len(), out.Len() + m.ge - m.gs, m.style})
		out.WriteString(text[m.gs:m.ge])
		pos = m.end
	}
	out.WriteString(text[pos:])

	adjust := func(p int) int {
		shift := 0
		for _, r := range removals {
			if r[0] >= p {
				break
			}
			shift += min(r[1], p-r[0])
		}
		return p - shift
	}
	for _, h := range heads {
		if s, e := adjust(h.start), adjust(h.end); e > s {
			*spans = append(*spans, span{base + s, base + e, h.style})
		}
	}
}

// italicMatches finds *x* and _x_ spans. Go's regexp has no lookaround, so the
// boundary rules of the Python patterns are checked by hand:
//
//	*x*  : the opening * is not part of ** and not followed by a space; the
//	       closing * is not part of **.
//	_x_  : neither underscore touches a word character on its outside, and
//	       neither is part of __.
func italicMatches(text string) []match {
	var out []match
	for i := 0; i < len(text); {
		c := text[i]
		if c != '*' && c != '_' {
			i++
			continue
		}
		end, ok := italicAt(text, i, c)
		if !ok {
			i++
			continue
		}
		out = append(out, match{i, end, i + 1, end - 1, "ITALIC"})
		i = end
	}
	return out
}

func italicAt(text string, i int, c byte) (end int, ok bool) {
	prev, _ := utf8.DecodeLastRuneInString(text[:i])
	if i+1 >= len(text) || text[i+1] == c || text[i+1] == '\n' {
		return 0, false
	}
	if i > 0 && text[i-1] == c {
		return 0, false
	}
	if c == '*' && text[i+1] == ' ' {
		return 0, false
	}
	if c == '_' && isWordRune(prev) && i > 0 {
		return 0, false
	}
	for j := i + 2; j < len(text); j++ {
		if text[j] == '\n' {
			return 0, false // `.` does not cross lines
		}
		if text[j] != c || text[j-1] == c {
			continue
		}
		if j+1 < len(text) && text[j+1] == c {
			continue
		}
		if c == '_' {
			if next, _ := utf8.DecodeRuneInString(text[j+1:]); j+1 < len(text) && isWordRune(next) {
				continue
			}
		}
		return j + 1, true
	}
	return 0, false
}

func isWordRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

// utf16Len is the length of s in UTF-16 code units, which is what Signal
// text-style ranges are measured in.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += len(utf16.Encode([]rune{r}))
	}
	return n
}
