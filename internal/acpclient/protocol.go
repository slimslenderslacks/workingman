package acpclient

import "encoding/json"

// This file defines just enough of the Agent Client Protocol (ACP) wire format
// for the TUI to drive a non-interactive Claude session over agent.sock. ACP is
// JSON-RPC 2.0 framed as newline-delimited JSON (one '\n'-terminated object per
// message) — the exact framing the acp-wrapper bridge preserves end to end (see
// internal/acpwrapper/bridge.go). We model only the messages this client sends
// and the notifications it consumes; unknown fields are ignored on decode so the
// agent's schema can grow without breaking us.

// jsonrpcVersion is the only JSON-RPC version ACP uses. It is written on every
// outgoing request and not validated on inbound frames (the bridge only ever
// carries the agent's own well-formed JSON-RPC).
const jsonrpcVersion = "2.0"

// protocolVersion is the ACP protocol version this client negotiates in
// initialize. The acp-kit bridge answers initialize with protocolVersion: 1, so
// we offer the same integer; a mismatch would surface as the agent's own error.
const protocolVersion = 1

// ACP method names this client uses. They are the three client→agent requests
// of the minimal prompt sequence plus the one agent→client notification we
// decode (session/update); see acp-kit's README "ACP message mapping".
const (
	methodInitialize     = "initialize"
	methodSessionNew     = "session/new"
	methodSessionSetMode = "session/set_mode"
	methodPrompt         = "session/prompt"
	methodUpdate         = "session/update"
)

// ModeBypassPermissions is the ACP mode id that disables tool-call permission
// prompts. Non-interactive orch agents run in this mode because the workingman
// ACP client doesn't implement session/request_permission — without bypass, the
// bridge issues a permission request on the first escalated tool call, our
// "method not found" reply fails the call, and the agent gives up.
const ModeBypassPermissions = "bypassPermissions"

// request is an outgoing JSON-RPC 2.0 request. ID is unique per client so the
// read loop can match the response back to the blocked caller; Params is
// pre-marshalled by the typed param builders below.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// frame is the permissive shape every inbound line is first decoded into so the
// read loop can classify it. A line is:
//
//   - a response   when ID is present and Method is empty (Result or Error set),
//   - a request    when both Method and ID are present (agent→client call), or
//   - a notification when Method is present and ID is absent.
//
// ID is a pointer so "absent" (notification) is distinguishable from id 0.
type frame struct {
	ID     *int            `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
	Params json.RawMessage `json:"params"`
}

// rpcError is a JSON-RPC error object. The bridge surfaces the agent's errors
// verbatim; we wrap Message into a Go error for the blocked caller.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return e.Message }

// initializeParams is the initialize handshake payload. clientCapabilities is
// intentionally empty: this client neither serves files nor handles permission
// prompts (the sandbox runs with IS_SANDBOX so tool calls don't stall), so we
// advertise no capabilities and the agent never calls back into us.
type initializeParams struct {
	ProtocolVersion    int            `json:"protocolVersion"`
	ClientCapabilities map[string]any `json:"clientCapabilities"`
}

// initializeResult is the agent's handshake reply. We keep only the fields worth
// surfacing for diagnostics; the negotiated ProtocolVersion lets a caller notice
// a downgrade.
type initializeResult struct {
	ProtocolVersion int `json:"protocolVersion"`
	AgentInfo       struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"agentInfo"`
}

// newSessionParams creates a fresh non-interactive session. Cwd must exist
// inside the sandbox — Docker Sandboxes bind-mounts the workspace at its native
// host path, so the caller passes that path (see acp-kit README's "cwd gotcha").
// McpServers is required by the schema and is always an empty (non-nil) slice.
type newSessionParams struct {
	Cwd        string `json:"cwd"`
	McpServers []any  `json:"mcpServers"`
}

// newSessionResult carries the agent-assigned session id that every subsequent
// prompt is scoped to.
type newSessionResult struct {
	SessionID string `json:"sessionId"`
}

// setModeParams switches the session into one of the modes the agent advertised
// in session/new's result.modes.availableModes. Connect uses it to flip into
// bypassPermissions right after session/new (see ModeBypassPermissions).
type setModeParams struct {
	SessionID string `json:"sessionId"`
	ModeID    string `json:"modeId"`
}

// promptParams sends one user turn. Prompt is a content block array; this client
// sends a single text block (the TUI's prompt text).
type promptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []contentBlock `json:"prompt"`
}

// promptResult is the turn's terminal result. StopReason (e.g. "end_turn") tells
// the caller the turn finished and why.
type promptResult struct {
	StopReason string `json:"stopReason"`
}

// contentBlock is an ACP content block. Only the text variant is produced or
// consumed here; Type is always "text".
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// textBlock builds the single text content block a prompt turn carries.
func textBlock(text string) contentBlock {
	return contentBlock{Type: "text", Text: text}
}

// updateParams is the payload of a session/update notification: which session it
// concerns and the tagged update union. Update is kept raw so the decoder can
// peek the kind discriminator and then unmarshal only the variant it renders.
// Decoding lazily is also what lets a tool_call's array-shaped "content" coexist
// with a message chunk's object-shaped "content": the two kinds share the field
// name but not the JSON type, so a single struct embedding both would fail to
// decode one of them.
type updateParams struct {
	SessionID string          `json:"sessionId"`
	Update    json.RawMessage `json:"update"`
}

// updateKind peeks just the discriminator out of an update body so the decoder
// can branch to the matching variant struct below.
type updateKind struct {
	SessionUpdate string `json:"sessionUpdate"`
}

// messageUpdate decodes the *_message_chunk / *_thought_chunk kinds, whose
// content is a single text block.
type messageUpdate struct {
	Content *contentBlock `json:"content"`
}

// toolCallUpdate decodes both tool_call (a tool invocation appearing) and
// tool_call_update (its status/output changing). The two share a shape;
// tool_call_update simply omits the fields that have not changed. Content is an
// array of output blocks — a different JSON type than a message chunk's
// object-shaped content, which is why updateParams.Update is decoded lazily.
type toolCallUpdate struct {
	ToolCallID string            `json:"toolCallId"`
	Title      string            `json:"title"`
	Kind       string            `json:"kind"`   // read | edit | execute | search | ...
	Status     string            `json:"status"` // pending | in_progress | completed | failed
	Content    []toolCallContent `json:"content"`
}

// toolCallContent is one block of a tool call's output. The common variant is
// {"type":"content","content":{...text...}}; some agents inline a bare "text".
// We read whichever is present and ignore the diff/terminal variants.
type toolCallContent struct {
	Type    string        `json:"type"`
	Content *contentBlock `json:"content"`
	Text    string        `json:"text"`
}

// planUpdate decodes the plan kind: the agent's current task list, sent in full
// on every change (so a renderer replaces the prior plan rather than appending).
type planUpdate struct {
	Entries []planEntryWire `json:"entries"`
}

// planEntryWire is one task in a plan update.
type planEntryWire struct {
	Content  string `json:"content"`
	Status   string `json:"status"` // pending | in_progress | completed
	Priority string `json:"priority"`
}

// Update kind discriminators we decode. The *_chunk kinds carry streamed model
// text; the tool_call kinds carry tool activity; plan carries the agent's task
// list. Other kinds (current_mode_update, available_commands_update,
// user_message_chunk) are recognised by absence — they fall through the decode
// and are not rendered.
const (
	updateAgentMessageChunk = "agent_message_chunk"
	updateAgentThoughtChunk = "agent_thought_chunk"
	updateToolCall          = "tool_call"
	updateToolCallUpdate    = "tool_call_update"
	updatePlan              = "plan"
)
