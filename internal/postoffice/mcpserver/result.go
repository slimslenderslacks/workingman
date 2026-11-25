package mcpserver

import (
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// result is a tool result: ordered "key: value" fields whose first is
// status (§4.4). The text form puts each field on its own line so the model
// can branch on the first line even when a gateway or code mode flattens
// structured content; a multi-line field (response, body) is written last as
// "key:" followed by its text, so it needs no escaping. The same fields are
// the structuredContent.
type result struct {
	keys   []string
	values map[string]any
	block  string // the multi-line field, rendered last
	lines  []string
}

func newResult(status string) *result {
	r := &result{values: map[string]any{}}
	return r.set("status", status)
}

func rejected(reason string) *result { return newResult("rejected").set("reason", reason) }

// set adds or replaces a single-line field. Empty strings and nil lists are
// skipped, so optional fields need no guard at the call site.
func (r *result) set(key string, v any) *result {
	switch x := v.(type) {
	case string:
		if x == "" {
			return r
		}
	case []string:
		if len(x) == 0 {
			return r
		}
	}
	if _, ok := r.values[key]; !ok {
		r.keys = append(r.keys, key)
	}
	r.values[key] = v
	return r
}

// setBlock sets the field rendered last, after every other field, on the
// lines following "key:". It is set even when empty, so a reader always
// finds it.
func (r *result) setBlock(key, text string) *result {
	r.block = key
	r.values[key] = text
	return r
}

// addLine appends a free-form line after the fields (list_requests rows).
func (r *result) addLine(line string) *result {
	r.lines = append(r.lines, line)
	return r
}

func (r *result) text() string {
	var b strings.Builder
	for _, k := range r.keys {
		b.WriteString(k)
		b.WriteString(": ")
		switch v := r.values[k].(type) {
		case string:
			// Single-line fields come from post-office or are capped
			// one-liners; fold any stray newline so line parsing holds.
			b.WriteString(strings.ReplaceAll(v, "\n", " "))
		case []string:
			b.WriteString(strings.Join(v, ", "))
		default:
			b.WriteString(toString(v))
		}
		b.WriteByte('\n')
	}
	for _, l := range r.lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	if r.block != "" {
		b.WriteString(r.block)
		b.WriteString(":\n")
		b.WriteString(r.values[r.block].(string))
		return b.String()
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func toString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	}
	return strings.TrimSpace(strings.ReplaceAll(jsonString(v), "\n", " "))
}

// toolResult renders r as a CallToolResult.
func (r *result) toolResult() *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: r.text()}},
		StructuredContent: r.values,
	}
}
