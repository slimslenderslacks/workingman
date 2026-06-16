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
// concerns and the tagged update union. We decode Update lazily so we only pay
// for the kinds we render.
type updateParams struct {
	SessionID string          `json:"sessionId"`
	Update    sessionUpdate   `json:"update"`
	Raw       json.RawMessage `json:"-"`
}

// sessionUpdate is the discriminated update body. SessionUpdate is the kind tag;
// Content carries the text for the message/thought chunk kinds. Tool-call and
// other kinds decode with a zero Content and are surfaced as non-text activity.
type sessionUpdate struct {
	// SessionUpdate is the kind discriminator: "agent_message_chunk",
	// "agent_thought_chunk", "tool_call", "tool_call_update", etc.
	SessionUpdate string `json:"sessionUpdate"`
	// Content is the chunk body for the *_message_chunk / *_thought_chunk kinds.
	// Pointer so an absent content (tool-call kinds) is distinguishable from an
	// empty-text chunk.
	Content *contentBlock `json:"content"`
}

// Update kind discriminators we special-case. agentMessageChunk is the streamed
// assistant text the TUI renders incrementally; the others are recognised so we
// can classify a notification as "activity" without rendering it as reply text.
const (
	updateAgentMessageChunk = "agent_message_chunk"
	updateAgentThoughtChunk = "agent_thought_chunk"
)
