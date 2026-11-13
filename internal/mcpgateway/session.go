package mcpgateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// ProtocolVersion is the MCP protocol revision a Session asks for in
// initialize.
const ProtocolVersion = "2025-06-18"

// sessionTimeout bounds one session request when Session.HTTP is nil. A tool
// call proxies to a remote server (GitHub), so it gets more room than the
// /gateways API calls.
const sessionTimeout = 60 * time.Second

// ErrSessionLost marks an error after which the session cannot be used again:
// the gateway no longer knows the session id (HTTP 404, or 400 on a request
// carrying one), or it could not be reached at all (connection refused or
// reset). Callers should open a new session — after re-creating the gateway,
// if that is gone too. Test with errors.Is(err, ErrSessionLost).
var ErrSessionLost = errors.New("mcp session lost")

// ToolError is a tools/call result with isError: true — the tool ran and
// failed. The session is still usable.
type ToolError struct {
	// Tool is the tool name that was called.
	Tool string
	// Text is the result's text content, which carries the tool's message.
	Text string
}

func (e *ToolError) Error() string {
	return fmt.Sprintf("mcp tool %s: %s", e.Tool, e.Text)
}

// RPCError is a JSON-RPC error response (e.g. an unknown tool or invalid
// arguments). The session is still usable.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("mcp: json-rpc error %d: %s", e.Code, e.Message)
}

// Content is one part of a tool result. Only text parts are interpreted;
// other kinds (image, resource, …) are kept with their Type.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// ToolResult is a tools/call result.
type ToolResult struct {
	Content           []Content       `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

// Text joins the result's text parts with newlines.
func (r ToolResult) Text() string {
	var parts []string
	for _, c := range r.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// Session is one MCP session over the streamable HTTP transport, opened
// against a gateway URL (Gateway.URL) by OpenSession. Safe for concurrent use.
type Session struct {
	url    string
	http   *http.Client
	id     string // Mcp-Session-Id; empty if the server did not assign one
	proto  string // negotiated protocol version
	nextID atomic.Int64
}

// SessionOptions configures OpenSession. The zero value is usable.
type SessionOptions struct {
	// HTTP is the client used for requests; nil means one with a 60s timeout.
	HTTP *http.Client
	// ClientName and ClientVersion are sent as initialize's clientInfo;
	// empty means "workingman" and "0".
	ClientName    string
	ClientVersion string
}

// OpenSession initializes an MCP session at url: it sends initialize, keeps
// the Mcp-Session-Id the server answers with, and sends
// notifications/initialized.
func OpenSession(ctx context.Context, url string, opts SessionOptions) (*Session, error) {
	s := &Session{url: strings.TrimSpace(url), http: opts.HTTP}
	if s.http == nil {
		s.http = &http.Client{Timeout: sessionTimeout}
	}
	name, version := opts.ClientName, opts.ClientVersion
	if name == "" {
		name = "workingman"
	}
	if version == "" {
		version = "0"
	}
	params := map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": name, "version": version},
	}
	raw, hdr, err := s.call(ctx, "initialize", params)
	if err != nil {
		return nil, err
	}
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(raw, &init); err != nil {
		return nil, fmt.Errorf("mcp: decode initialize result: %w", err)
	}
	s.id = hdr.Get("Mcp-Session-Id")
	s.proto = init.ProtocolVersion
	if err := s.notify(ctx, "notifications/initialized"); err != nil {
		return nil, err
	}
	return s, nil
}

// ID is the server-assigned session id (empty if it assigned none).
func (s *Session) ID() string { return s.id }

// CallTool calls the named tool and returns its result. A result with
// isError: true is returned along with a *ToolError; a JSON-RPC error is an
// *RPCError; an error wrapping ErrSessionLost means the session is gone.
func (s *Session) CallTool(ctx context.Context, name string, args map[string]any) (ToolResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	raw, _, err := s.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return ToolResult{}, err
	}
	var res ToolResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return ToolResult{}, fmt.Errorf("mcp: decode %s result: %w", name, err)
	}
	if res.IsError {
		return res, &ToolError{Tool: name, Text: res.Text()}
	}
	return res, nil
}

// Close best-effort ends the session with DELETE. A server that does not
// support it (405) or already forgot the session (404) is not an error.
func (s *Session) Close(ctx context.Context) error {
	if s.id == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, s.url, nil)
	if err != nil {
		return fmt.Errorf("mcp: %w", err)
	}
	s.setHeaders(req)
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("mcp: DELETE %s: %w", s.url, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusAccepted, http.StatusNotFound, http.StatusMethodNotAllowed:
		return nil
	}
	return fmt.Errorf("mcp: DELETE %s: %s", s.url, resp.Status)
}

func (s *Session) setHeaders(req *http.Request) {
	if s.id != "" {
		req.Header.Set("Mcp-Session-Id", s.id)
	}
	if s.proto != "" {
		req.Header.Set("Mcp-Protocol-Version", s.proto)
	}
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  any             `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// post sends one JSON-RPC message and returns the response, or an error that
// wraps ErrSessionLost when the session is gone.
func (s *Session) post(ctx context.Context, method string, msg rpcMessage) (*http.Response, error) {
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("mcp: %s: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("mcp: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	s.setHeaders(req)
	resp, err := s.http.Do(req)
	if err != nil {
		if connectionLost(err) {
			return nil, fmt.Errorf("mcp: %s: %w: %v", method, ErrSessionLost, err)
		}
		return nil, fmt.Errorf("mcp: %s: %w", method, err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		detail := strings.TrimSpace(string(data))
		// 404 is the spec's answer to an expired session id (and what a
		// deleted gateway answers); some servers say 400 for an unknown id.
		if resp.StatusCode == http.StatusNotFound || (resp.StatusCode == http.StatusBadRequest && s.id != "") {
			return nil, fmt.Errorf("mcp: %s: %w: %s: %s", method, ErrSessionLost, resp.Status, detail)
		}
		return nil, fmt.Errorf("mcp: %s: %s: %s", method, resp.Status, detail)
	}
	return resp, nil
}

// connectionLost reports whether a transport error means the gateway is not
// there any more (as opposed to, say, a timeout).
func connectionLost(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// call sends a request and returns its result and the response headers.
func (s *Session) call(ctx context.Context, method string, params any) (json.RawMessage, http.Header, error) {
	id := s.nextID.Add(1)
	idRaw := json.RawMessage(fmt.Sprint(id))
	resp, err := s.post(ctx, method, rpcMessage{JSONRPC: "2.0", ID: idRaw, Method: method, Params: params})
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	var msg *rpcMessage
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch ct {
	case "text/event-stream":
		msg, err = readSSEResponse(resp.Body, idRaw)
	case "application/json", "":
		msg, err = readJSONResponse(resp.Body, idRaw)
	default:
		err = fmt.Errorf("unexpected content type %q", ct)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("mcp: %s: %w", method, err)
	}
	if msg.Error != nil {
		return nil, nil, fmt.Errorf("mcp: %s: %w", method, msg.Error)
	}
	if msg.Result == nil {
		return nil, nil, fmt.Errorf("mcp: %s: response has neither result nor error", method)
	}
	return msg.Result, resp.Header, nil
}

// notify sends a notification; the server answers 202 with no body.
func (s *Session) notify(ctx context.Context, method string) error {
	resp, err := s.post(ctx, method, rpcMessage{JSONRPC: "2.0", Method: method})
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.Body.Close()
}

// matches reports whether msg is the response to request id.
func (m *rpcMessage) matches(id json.RawMessage) bool {
	return m.Method == "" && bytes.Equal(bytes.TrimSpace(m.ID), id)
}

// readJSONResponse decodes an application/json body: one message, or a
// batch holding the response among others.
func readJSONResponse(r io.Reader, id json.RawMessage) (*rpcMessage, error) {
	data, err := io.ReadAll(io.LimitReader(r, 16<<20))
	if err != nil {
		return nil, err
	}
	data = bytes.TrimSpace(data)
	var msgs []rpcMessage
	if len(data) > 0 && data[0] == '[' {
		err = json.Unmarshal(data, &msgs)
	} else {
		var m rpcMessage
		err = json.Unmarshal(data, &m)
		msgs = []rpcMessage{m}
	}
	if err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	for i := range msgs {
		if msgs[i].matches(id) {
			return &msgs[i], nil
		}
	}
	return nil, fmt.Errorf("no response with id %s", id)
}

// readSSEResponse reads a text/event-stream body until the event carrying
// the response to id, skipping server notifications and requests on the way.
func readSSEResponse(r io.Reader, id json.RawMessage) (*rpcMessage, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	var data []string
	dispatch := func() (*rpcMessage, error) {
		if len(data) == 0 {
			return nil, nil
		}
		payload := strings.Join(data, "\n")
		data = data[:0]
		var m rpcMessage
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			return nil, fmt.Errorf("decode event: %w", err)
		}
		if m.matches(id) {
			return &m, nil
		}
		return nil, nil
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if m, err := dispatch(); m != nil || err != nil {
				return m, err
			}
			continue
		}
		if v, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimPrefix(v, " "))
		}
		// event:, id:, retry: and comments carry nothing we need.
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if m, err := dispatch(); m != nil || err != nil {
		return m, err
	}
	return nil, fmt.Errorf("event stream ended without a response to id %s: %w", id, io.ErrUnexpectedEOF)
}
