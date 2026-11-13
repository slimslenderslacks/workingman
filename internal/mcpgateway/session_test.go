package mcpgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeMCP is a streamable-HTTP MCP server for tests. Tool calls are answered
// by tool; sse switches responses to text/event-stream.
type fakeMCP struct {
	t   *testing.T
	sse bool

	mu        sync.Mutex
	sessionID string
	methods   []string
	initArgs  map[string]any
	deleted   bool
	expired   bool // answer 404 to every request carrying the session id
	staleCode int  // status for an expired session; 0 means 404
	tool      func(name string, args map[string]any) map[string]any
}

func (f *fakeMCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodDelete {
		if r.Header.Get("Mcp-Session-Id") != f.sessionID {
			http.Error(w, "unknown session", http.StatusNotFound)
			return
		}
		f.deleted = true
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		f.t.Errorf("Accept = %q", r.Header.Get("Accept"))
	}
	raw, _ := io.ReadAll(r.Body)
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params map[string]any  `json:"params"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		f.t.Errorf("bad request body %s: %v", raw, err)
		return
	}
	f.methods = append(f.methods, msg.Method)
	if msg.Method != "initialize" {
		if got := r.Header.Get("Mcp-Session-Id"); got != f.sessionID {
			f.t.Errorf("%s: Mcp-Session-Id = %q, want %q", msg.Method, got, f.sessionID)
		}
		if f.expired {
			code := f.staleCode
			if code == 0 {
				code = http.StatusNotFound
			}
			http.Error(w, "session not found", code)
			return
		}
	}
	var result any
	switch msg.Method {
	case "initialize":
		f.initArgs = msg.Params
		f.sessionID = "sess-1"
		w.Header().Set("Mcp-Session-Id", f.sessionID)
		result = map[string]any{"protocolVersion": ProtocolVersion, "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "fake"}}
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
		return
	case "tools/call":
		args, _ := msg.Params["arguments"].(map[string]any)
		res := f.tool(msg.Params["name"].(string), args)
		if res == nil {
			f.reply(w, msg.ID, nil, map[string]any{"code": -32602, "message": "unknown tool"})
			return
		}
		result = res
	default:
		f.t.Errorf("unexpected method %q", msg.Method)
		return
	}
	f.reply(w, msg.ID, result, nil)
}

func (f *fakeMCP) reply(w http.ResponseWriter, id json.RawMessage, result, rpcErr any) {
	resp := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		resp["error"] = rpcErr
	} else {
		resp["result"] = result
	}
	data, _ := json.Marshal(resp)
	if !f.sse {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	// A server notification and a comment before the response, as a real
	// server may interleave them.
	fmt.Fprint(w, ": keepalive\n\n")
	fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
	fmt.Fprintf(w, "event: message\nid: 1\ndata: %s\n\n", data)
}

func newFakeMCP(t *testing.T, sse bool) (*fakeMCP, *httptest.Server) {
	f := &fakeMCP{t: t, sse: sse, tool: func(name string, args map[string]any) map[string]any {
		switch name {
		case "get_pull_request":
			return map[string]any{"content": []any{
				map[string]any{"type": "text", "text": fmt.Sprintf("PR %v", args["pullNumber"])},
				map[string]any{"type": "text", "text": "head abc123"},
			}}
		case "broken":
			return map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "404 Not Found"}}}
		}
		return nil
	}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func TestSessionJSONAndSSE(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sse=%v", sse), func(t *testing.T) {
			f, srv := newFakeMCP(t, sse)
			ctx := context.Background()
			s, err := OpenSession(ctx, srv.URL, SessionOptions{ClientName: "orch", ClientVersion: "1"})
			if err != nil {
				t.Fatal(err)
			}
			if s.ID() != "sess-1" {
				t.Errorf("ID = %q", s.ID())
			}
			if f.initArgs["protocolVersion"] != ProtocolVersion {
				t.Errorf("initialize protocolVersion = %v", f.initArgs["protocolVersion"])
			}
			if ci, _ := f.initArgs["clientInfo"].(map[string]any); ci["name"] != "orch" || ci["version"] != "1" {
				t.Errorf("clientInfo = %v", f.initArgs["clientInfo"])
			}

			res, err := s.CallTool(ctx, "get_pull_request", map[string]any{"owner": "o", "repo": "r", "pullNumber": 7})
			if err != nil {
				t.Fatal(err)
			}
			if got, want := res.Text(), "PR 7\nhead abc123"; got != want {
				t.Errorf("Text = %q, want %q", got, want)
			}

			_, err = s.CallTool(ctx, "nope", nil)
			var rpcErr *RPCError
			if !errors.As(err, &rpcErr) || rpcErr.Code != -32602 || errors.Is(err, ErrSessionLost) {
				t.Errorf("unknown tool: err = %v", err)
			}

			if err := s.Close(ctx); err != nil || !f.deleted {
				t.Errorf("Close: %v, deleted=%v", err, f.deleted)
			}
			want := []string{"initialize", "notifications/initialized", "tools/call", "tools/call"}
			if strings.Join(f.methods, ",") != strings.Join(want, ",") {
				t.Errorf("methods = %v, want %v", f.methods, want)
			}
		})
	}
}

func TestSessionToolIsError(t *testing.T) {
	_, srv := newFakeMCP(t, false)
	ctx := context.Background()
	s, err := OpenSession(ctx, srv.URL, SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.CallTool(ctx, "broken", nil)
	var toolErr *ToolError
	if !errors.As(err, &toolErr) || toolErr.Tool != "broken" || toolErr.Text != "404 Not Found" {
		t.Fatalf("err = %v, want a ToolError", err)
	}
	if errors.Is(err, ErrSessionLost) {
		t.Error("a tool error must not count as a lost session")
	}
	if !res.IsError {
		t.Error("result should still be returned with IsError")
	}
}

func TestSessionLost(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusBadRequest} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			f, srv := newFakeMCP(t, false)
			ctx := context.Background()
			s, err := OpenSession(ctx, srv.URL, SessionOptions{})
			if err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			f.expired, f.staleCode = true, code
			f.mu.Unlock()
			if _, err := s.CallTool(ctx, "get_pull_request", nil); !errors.Is(err, ErrSessionLost) {
				t.Errorf("expired session (%d): err = %v, want ErrSessionLost", code, err)
			}
		})
	}

	t.Run("other status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		defer srv.Close()
		_, err := OpenSession(context.Background(), srv.URL, SessionOptions{})
		if err == nil || errors.Is(err, ErrSessionLost) {
			t.Errorf("500: err = %v, want an ordinary error", err)
		}
	})

	t.Run("connection refused", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		if _, err := OpenSession(context.Background(), url, SessionOptions{}); !errors.Is(err, ErrSessionLost) {
			t.Errorf("refused: err = %v, want ErrSessionLost", err)
		}
	})

	t.Run("gateway deleted", func(t *testing.T) {
		// A gateway URL whose gateway is gone answers 404 even to initialize.
		srv := httptest.NewServer(http.NotFoundHandler())
		defer srv.Close()
		if _, err := OpenSession(context.Background(), srv.URL+"/gateways/x", SessionOptions{}); !errors.Is(err, ErrSessionLost) {
			t.Errorf("404 on initialize: err = %v, want ErrSessionLost", err)
		}
	})
}

func TestReadSSEResponseErrors(t *testing.T) {
	id := json.RawMessage("3")
	if _, err := readSSEResponse(strings.NewReader("data: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{}}\n\n"), id); err == nil {
		t.Error("stream without our id: want an error")
	}
	m, err := readSSEResponse(strings.NewReader("data: {\"jsonrpc\":\"2.0\",\n"+"data: \"id\":3,\"result\":{\"ok\":true}}"), id)
	if err != nil || string(m.Result) != `{"ok":true}` {
		t.Errorf("multi-line data without trailing blank line: %v, %v", m, err)
	}
}
