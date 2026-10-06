package whatsapp

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxMessageLength is the per-message budget for outbound text. WhatsApp's
// protocol allows ~65K characters, but anything past 4096 is unreadable on a
// phone; hermes-agent uses the same practical limit (MAX_MESSAGE_LENGTH in
// whatsapp_common.py) and so do we. Lengths are counted in characters
// (runes), as the Cloud API's own 4096 limit on text.body is.
const MaxMessageLength = 4096

// chunkIndicatorReserve is room for the " (12/34)" page marker appended to
// every chunk of a multi-part message.
const chunkIndicatorReserve = 10

var (
	fenceRE      = regexp.MustCompile("(?s)```.*?```")
	inlineCodeRE = regexp.MustCompile("`[^`\n]+`")
	boldRE       = regexp.MustCompile(`\*\*(.+?)\*\*`)
	underBoldRE  = regexp.MustCompile(`__(.+?)__`)
	strikeRE     = regexp.MustCompile(`~~(.+?)~~`)
	headingRE    = regexp.MustCompile(`(?m)^#{1,6}[ \t]+(.+)$`)
	linkRE       = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)

	// Zero-width format characters and odd unicode spaces render as mojibake
	// prefixes in WhatsApp; emoji joiners (U+200D) are deliberately kept.
	invisibleRE = regexp.MustCompile(`[\x{200b}\x{2060}\x{2063}\x{feff}]`)
	oddSpaceRE  = regexp.MustCompile(`[\x{00a0}\x{1680}\x{180e}\x{2000}-\x{200a}\x{202f}\x{205f}\x{3000}]`)
)

// FormatMessage converts common Markdown to WhatsApp markup, as
// whatsapp_common.format_message does:
//
//	**bold** and __bold__   -> *bold*
//	*italic*                -> _italic_
//	~~strike~~              -> ~strike~
//	# Heading               -> *Heading*
//	[text](url)             -> text (url)
//
// Fenced code blocks and `inline code` pass through untouched (WhatsApp
// renders both in monospace). Zero-width characters and odd unicode spaces are
// stripped first.
func FormatMessage(content string) string {
	if content == "" {
		return content
	}
	content = oddSpaceRE.ReplaceAllString(invisibleRE.ReplaceAllString(content, ""), " ")
	// The placeholders below use NUL as a delimiter; none may pre-exist.
	content = strings.ReplaceAll(content, "\x00", "")

	result, fences := stash(fenceRE, content, "FENCE")
	result, codes := stash(inlineCodeRE, result, "CODE")

	// Italic must run before bold so that **bold** is not mistaken for it.
	result = italicToUnderscore(result)
	result = boldRE.ReplaceAllString(result, "*$1*")
	result = underBoldRE.ReplaceAllString(result, "*$1*")
	result = strikeRE.ReplaceAllString(result, "~$1~")
	result = headingRE.ReplaceAllStringFunc(result, func(m string) string {
		inner := strings.TrimSpace(headingRE.FindStringSubmatch(m)[1])
		// "# **Title**" must not render with literal asterisks.
		for len(inner) > 1 && strings.HasPrefix(inner, "*") && strings.HasSuffix(inner, "*") {
			inner = strings.TrimSpace(inner[1 : len(inner)-1])
		}
		return "*" + inner + "*"
	})
	result = linkRE.ReplaceAllString(result, "$1 ($2)")

	result = unstash(result, "FENCE", fences)
	return unstash(result, "CODE", codes)
}

func stash(re *regexp.Regexp, text, tag string) (string, []string) {
	var saved []string
	out := re.ReplaceAllStringFunc(text, func(m string) string {
		saved = append(saved, m)
		return "\x00" + tag + strconv.Itoa(len(saved)-1) + "\x00"
	})
	return out, saved
}

func unstash(text, tag string, saved []string) string {
	for i, original := range saved {
		text = strings.Replace(text, "\x00"+tag+strconv.Itoa(i)+"\x00", original, 1)
	}
	return text
}

// italicToUnderscore rewrites Markdown *italic* as WhatsApp _italic_. It is
// the hand-rolled equivalent of hermes' lookaround regex
//
//	(?<!\*)\*(?!\s|\*)([^*\n]*?\S[^*\n]*?)\*(?!\*)
//
// (RE2 has no lookarounds): the opening * is not preceded by * and not
// followed by whitespace or *; the body has no * or newline; the closing * is
// not followed by another *. List bullets ("* item") and bold delimiters fail
// those tests and are left alone.
func italicToUnderscore(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '*' && (i == 0 || s[i-1] != '*') {
			if end, ok := italicEnd(s, i); ok {
				b.WriteByte('_')
				b.WriteString(s[i+1 : end])
				b.WriteByte('_')
				i = end + 1
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// italicEnd reports the index of the closing * of an italic span opened at s[open].
func italicEnd(s string, open int) (int, bool) {
	rest := s[open+1:]
	first, _ := utf8.DecodeRuneInString(rest)
	if rest == "" || first == '*' || unicode.IsSpace(first) {
		return 0, false
	}
	for j := 0; j < len(rest); j++ {
		switch rest[j] {
		case '\n':
			return 0, false
		case '*':
			if j+1 < len(rest) && rest[j+1] == '*' {
				return 0, false
			}
			return open + 1 + j, true
		}
	}
	return 0, false
}

// ChunkMessage splits content into pieces of at most limit characters,
// breaking at paragraph, then line, then word boundaries. It ports
// BasePlatformAdapter.truncate_message with these behaviors:
//
//   - A split inside a fenced code block closes the fence at the end of the
//     chunk and reopens it (same language tag) at the start of the next.
//   - A split is moved off an unpaired inline backtick when it can.
//   - Multi-chunk output gets " (1/3)" page markers; the limit includes them.
//
// Content that already fits comes back as a single, unmarked chunk.
func ChunkMessage(content string, limit int) []string {
	if limit <= 0 {
		limit = MaxMessageLength
	}
	if utf8.RuneCountInString(content) <= limit {
		return []string{content}
	}
	const fenceClose = "\n```"
	var chunks []string
	remaining := []rune(content)
	var carry *string // language tag of the fence the previous chunk ended inside
	for len(remaining) > 0 {
		prefix := ""
		if carry != nil {
			prefix = "```" + *carry + "\n"
		}
		prefixLen := utf8.RuneCountInString(prefix)
		headroom := limit - chunkIndicatorReserve - prefixLen - len([]rune(fenceClose))
		if headroom < 1 {
			headroom = max(1, limit/2)
		}
		if prefixLen+len(remaining) <= limit-chunkIndicatorReserve {
			final := prefix + string(remaining)
			if carry != nil {
				if open, _ := fenceStateAfter(string(remaining), true, *carry); open {
					final += fenceClose
				}
			}
			chunks = append(chunks, final)
			break
		}

		splitAt := chunkSplit(remaining[:min(headroom, len(remaining))])
		body := string(remaining[:splitAt])
		remaining = remaining[splitAt:]
		if carry == nil {
			remaining = trimLeftRunes(remaining, unicode.IsSpace)
		} else {
			// Indentation inside a code block is significant.
			remaining = trimLeftRunes(remaining, func(r rune) bool { return r == '\n' || r == '\r' })
		}

		inCode, lang := fenceStateAfter(body, carry != nil, derefOr(carry, ""))
		chunk := prefix + body
		if inCode {
			chunk += fenceClose
			carry = &lang
		} else {
			carry = nil
		}
		chunks = append(chunks, chunk)
	}
	if len(chunks) > 1 {
		for i, c := range chunks {
			chunks[i] = c + " (" + strconv.Itoa(i+1) + "/" + strconv.Itoa(len(chunks)) + ")"
		}
	}
	return chunks
}

// chunkSplit picks where to cut region (the text that fits): the last
// paragraph break, else the last newline, else the last space, falling back to
// a hard cut. A boundary in the first half is only used when nothing later
// exists, so chunks do not come out tiny; an unpaired inline backtick is
// avoided when a clean boundary precedes it.
func chunkSplit(region []rune) int {
	limit := len(region)
	at := lastIndexRunes(region, "\n\n")
	if at < limit/2 {
		at = lastIndexRunes(region, "\n")
	}
	if at < limit/2 {
		at = lastIndexRunes(region, " ")
	}
	if at < 1 {
		at = limit
	}
	candidate := region[:at]
	if countRune(candidate, '`')%2 == 1 {
		lastTick := -1
		for i, r := range candidate {
			if r == '`' {
				lastTick = i
			}
		}
		if lastTick > 0 {
			safe := max(lastIndexRunes(candidate[:lastTick], " "), lastIndexRunes(candidate[:lastTick], "\n"))
			if safe > limit/4 {
				at = safe
			}
		}
	}
	return at
}

// fenceStateAfter walks text line by line, toggling on ``` lines, and returns
// whether a fence is open at the end and its language tag.
func fenceStateAfter(text string, inCode bool, lang string) (bool, string) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "```") {
			continue
		}
		if inCode {
			inCode, lang = false, ""
			continue
		}
		inCode, lang = true, ""
		if tag := strings.Fields(line[3:]); len(tag) > 0 {
			lang = tag[0]
		}
	}
	return inCode, lang
}

func lastIndexRunes(rs []rune, sep string) int {
	pat := []rune(sep)
	for i := len(rs) - len(pat); i >= 0; i-- {
		match := true
		for j, p := range pat {
			if rs[i+j] != p {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func countRune(rs []rune, target rune) int {
	n := 0
	for _, r := range rs {
		if r == target {
			n++
		}
	}
	return n
}

func trimLeftRunes(rs []rune, drop func(rune) bool) []rune {
	for len(rs) > 0 && drop(rs[0]) {
		rs = rs[1:]
	}
	return rs
}

func derefOr(s *string, def string) string {
	if s == nil {
		return def
	}
	return *s
}
