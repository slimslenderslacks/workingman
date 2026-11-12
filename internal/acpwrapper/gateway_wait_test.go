package acpwrapper

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/mcpgateway"
	"github.com/slimslenderslacks/work/internal/session"
)

// A configured MCP gateway that never answers fails the launch — before any
// sandbox is touched — and records why in session.json, rather than starting
// an agent that has no okta-gateway.
func TestRunFailsWhenMCPGatewayNeverReady(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	down := "http://" + l.Addr().String()
	_ = l.Close()

	root := t.TempDir()
	c := Config{
		SessionID:    "wolf-x",
		SessionsRoot: root,
		KitPath:      "/kits/acp-kit",
		SbxPath:      "/nonexistent/sbx", // must never be reached
		Workspaces:   []string{t.TempDir()},
		Persistent:   true,
		MCPGateway: mcpgateway.Manager{
			Client:    mcpgateway.Client{BaseURL: down},
			Servers:   []string{"github"},
			Log:       &strings.Builder{},
			RetryBase: 5 * time.Millisecond,
		},
		MCPGatewayReadyTimeout: 50 * time.Millisecond,
	}
	err = Run(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "MCP gateway (okta-gateway) unavailable") {
		t.Fatalf("Run err = %v, want the gateway-unavailable failure", err)
	}
	rec, rerr := (session.Store{Root: root}).Read("wolf-x")
	if rerr != nil {
		t.Fatal(rerr)
	}
	if rec.Status != session.StatusFailed || !strings.Contains(rec.Error, "okta-gateway") {
		t.Errorf("session.json = %+v, want failed with the gateway reason", rec)
	}
}
