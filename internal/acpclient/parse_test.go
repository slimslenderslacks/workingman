package acpclient

import "testing"

// TestParseStreamFrame covers the log-replay decoder a reconnecting TUI uses to
// rebuild scrollback from a session's recorded ACP stream: assistant message and
// thought chunks become StateStreaming text Events, and every other frame shape
// is skipped (ok=false) rather than mis-rendered as reply text.
func TestParseStreamFrame(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantOK   bool
		wantText string
	}{
		{
			name:     "agent message chunk",
			line:     `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello"}}}}`,
			wantOK:   true,
			wantText: "hello",
		},
		{
			name:     "agent thought chunk",
			line:     `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"thinking"}}}}`,
			wantOK:   true,
			wantText: "thinking",
		},
		{
			name:   "tool call carries no reply text",
			line:   `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call"}}}`,
			wantOK: false,
		},
		{
			name:   "empty text chunk is skipped",
			line:   `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":""}}}}`,
			wantOK: false,
		},
		{
			name:   "response frame is not a stream update",
			line:   `{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}`,
			wantOK: false,
		},
		{
			name:   "request frame is not a stream update",
			line:   `{"jsonrpc":"2.0","id":2,"method":"session/update","params":{}}`,
			wantOK: false,
		},
		{
			name:   "malformed line",
			line:   `not json at all`,
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev, ok := ParseStreamFrame([]byte(tt.line))
			if ok != tt.wantOK {
				t.Fatalf("ParseStreamFrame ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if ev.State != StateStreaming {
				t.Errorf("State = %q, want %q", ev.State, StateStreaming)
			}
			if ev.Text != tt.wantText {
				t.Errorf("Text = %q, want %q", ev.Text, tt.wantText)
			}
		})
	}
}
