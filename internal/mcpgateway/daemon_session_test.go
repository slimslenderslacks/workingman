package mcpgateway

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/slimslenderslacks/work/internal/audit"
)

// staticGateway is a GatewaySource that always hands out url-<gen>, bumping
// gen on Invalidate.
type staticGateway struct {
	mu  sync.Mutex
	gen int
}

func (g *staticGateway) URL(context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return fmt.Sprintf("http://gw/%d", g.gen), nil
}

func (g *staticGateway) Invalidate(context.Context, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.gen++
}

type echoConn struct {
	id     string
	lost   bool
	calls  int
	closed int
}

func (c *echoConn) ID() string { return c.id }

func (c *echoConn) CallTool(_ context.Context, name string, _ map[string]any) (ToolResult, error) {
	c.calls++
	if c.lost {
		return ToolResult{}, fmt.Errorf("tools/call: %w", ErrSessionLost)
	}
	return ToolResult{Content: []Content{{Type: "text", Text: c.id + ":" + name}}}, nil
}

func (c *echoConn) Close(context.Context) error { c.closed++; return nil }

// TestDaemonSessionToolCaller checks the generic path a non-review consumer
// uses: CallTool through the ToolCaller interface, events and log lines
// named after Name, a lost session recreated and the call retried on it.
func TestDaemonSessionToolCaller(t *testing.T) {
	buf := &safeBuf{}
	var logs []string
	var conns []*echoConn
	m := &DaemonSession{
		Gateway: &staticGateway{},
		Name:    "slack",
		Audit:   audit.New(buf).Log,
		Logf:    func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
		Open: func(_ context.Context, url string) (SessionConn, error) {
			c := &echoConn{id: fmt.Sprintf("s%d", len(conns)+1)}
			conns = append(conns, c)
			return c, nil
		},
	}
	var tc ToolCaller = m
	res, err := tc.CallTool(context.Background(), "send", nil)
	if err != nil || res.Text() != "s1:send" {
		t.Fatalf("CallTool = %q, %v", res.Text(), err)
	}
	conns[0].lost = true
	res, err = tc.CallTool(context.Background(), "send", nil)
	if err != nil || res.Text() != "s2:send" {
		t.Fatalf("CallTool after loss = %q, %v", res.Text(), err)
	}
	m.Close()
	if conns[1].closed != 1 {
		t.Errorf("session closed %d times, want 1", conns[1].closed)
	}
	out := buf.String()
	for _, ev := range []string{"slack_mcp_session_opened", "slack_mcp_session_lost", "slack_mcp_session_recreated", "slack_mcp_session_closed"} {
		if !strings.Contains(out, ev) {
			t.Errorf("audit missing %s:\n%s", ev, out)
		}
	}
	if len(logs) == 0 || !strings.HasPrefix(logs[0], "slack MCP: ") {
		t.Errorf("log lines = %q, want a \"slack MCP: \" prefix", logs)
	}
	if _, err := tc.CallTool(context.Background(), "send", nil); err == nil {
		t.Error("CallTool succeeded after Close")
	}
}

type listConn struct {
	echoConn
	lists int
}

func (c *listConn) ListTools(context.Context) ([]Tool, error) {
	c.lists++
	if c.lost {
		return nil, fmt.Errorf("tools/list: %w", ErrSessionLost)
	}
	return []Tool{{Name: "x-" + c.id}}, nil
}

// TestDaemonSessionListTools checks tools/list goes through the shared
// session, recreating a lost one, and fails cleanly on a connection that
// cannot list.
func TestDaemonSessionListTools(t *testing.T) {
	var conns []*listConn
	m := &DaemonSession{
		Gateway: &staticGateway{},
		Name:    "slack",
		Logf:    func(string, ...any) {},
		Open: func(context.Context, string) (SessionConn, error) {
			c := &listConn{echoConn: echoConn{id: fmt.Sprintf("s%d", len(conns)+1)}}
			conns = append(conns, c)
			return c, nil
		},
	}
	var l ToolLister = m
	tools, err := l.ListTools(context.Background())
	if err != nil || len(tools) != 1 || tools[0].Name != "x-s1" {
		t.Fatalf("ListTools = %v, %v", tools, err)
	}
	conns[0].lost = true
	tools, err = l.ListTools(context.Background())
	if err != nil || len(tools) != 1 || tools[0].Name != "x-s2" {
		t.Fatalf("ListTools after loss = %v, %v", tools, err)
	}

	plain := &DaemonSession{
		Gateway: &staticGateway{},
		Name:    "slack",
		Logf:    func(string, ...any) {},
		Open:    func(context.Context, string) (SessionConn, error) { return &echoConn{id: "p"}, nil },
	}
	if _, err := plain.ListTools(context.Background()); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("ListTools on a non-lister = %v, want not supported", err)
	}
}
