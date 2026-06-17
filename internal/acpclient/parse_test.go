package acpclient

import "testing"

// TestParseStreamFrame covers the log-replay decoder a reconnecting TUI uses to
// rebuild scrollback from a session's recorded ACP stream: assistant message and
// thought chunks become StateStreaming text Events, and non-text frames
// (responses, requests, malformed lines, empty chunks) are skipped (ok=false).
// The richer kinds — tool calls and plans — and the message/thought Kind
// distinction are asserted in TestParseStreamFrameKinds.
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
			name:   "empty thought chunk is skipped",
			line:   `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":""}}}}`,
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

// TestParseStreamFrameKinds asserts the Event.Kind discriminator and the
// kind-specific fields the renderer relies on: message vs thought chunks, a
// tool_call and its tool_call_update (including flattened output), and a plan.
func TestParseStreamFrameKinds(t *testing.T) {
	msg, ok := ParseStreamFrame([]byte(`{"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hi"}}}}`))
	if !ok || msg.Kind != EventStream {
		t.Errorf("message chunk: ok=%v kind=%v, want true/EventStream", ok, msg.Kind)
	}

	thought, ok := ParseStreamFrame([]byte(`{"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"hmm"}}}}`))
	if !ok || thought.Kind != EventThought {
		t.Errorf("thought chunk: ok=%v kind=%v, want true/EventThought", ok, thought.Kind)
	}

	call, ok := ParseStreamFrame([]byte(`{"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call","toolCallId":"c1","title":"Run tests","kind":"execute","status":"in_progress"}}}`))
	if !ok || call.Kind != EventToolCall {
		t.Fatalf("tool_call: ok=%v kind=%v, want true/EventToolCall", ok, call.Kind)
	}
	if call.ToolCallID != "c1" || call.ToolTitle != "Run tests" || call.ToolKind != "execute" || call.ToolStatus != "in_progress" {
		t.Errorf("tool_call fields = %+v, want id=c1 title=\"Run tests\" kind=execute status=in_progress", call)
	}

	upd, ok := ParseStreamFrame([]byte(`{"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call_update","toolCallId":"c1","status":"completed","content":[{"type":"content","content":{"type":"text","text":"ok\n"}}]}}}`))
	if !ok || upd.Kind != EventToolCall {
		t.Fatalf("tool_call_update: ok=%v kind=%v, want true/EventToolCall", ok, upd.Kind)
	}
	if upd.ToolStatus != "completed" || upd.Text != "ok\n" {
		t.Errorf("tool_call_update status=%q text=%q, want completed/\"ok\\n\"", upd.ToolStatus, upd.Text)
	}

	plan, ok := ParseStreamFrame([]byte(`{"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"plan","entries":[{"content":"step one","status":"completed"},{"content":"step two","status":"in_progress"}]}}}`))
	if !ok || plan.Kind != EventPlan {
		t.Fatalf("plan: ok=%v kind=%v, want true/EventPlan", ok, plan.Kind)
	}
	if len(plan.Plan) != 2 || plan.Plan[0].Content != "step one" || plan.Plan[1].Status != "in_progress" {
		t.Errorf("plan entries = %+v, want [{step one completed} {step two in_progress}]", plan.Plan)
	}

	// An unrendered kind still decodes to ok=false and is dropped.
	if _, ok := ParseStreamFrame([]byte(`{"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"current_mode_update","currentModeId":"bypassPermissions"}}}`)); ok {
		t.Error("current_mode_update should be skipped (ok=false)")
	}
}
