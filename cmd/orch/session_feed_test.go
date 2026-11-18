package main

import (
	"context"
	"reflect"
	"testing"

	"github.com/slimslenderslacks/work/internal/daemon"
)

// adaptSessionFeed copies the session's launch config into the view the TUI's
// agent box renders.
func TestAdaptSessionFeedCopiesLaunchConfig(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := make(chan []daemon.SessionInfo, 1)
	in <- []daemon.SessionInfo{{
		ID:         "p",
		Harness:    "img:1",
		Model:      "claude-opus-5-5",
		Mounts:     []string{"/ws", "/orch:ro"},
		MCPs:       []string{"github"},
		MCPDynamic: true,
	}}
	views := <-adaptSessionFeed(ctx, in)
	if len(views) != 1 {
		t.Fatalf("views = %+v", views)
	}
	v := views[0]
	if v.Harness != "img:1" || v.Model != "claude-opus-5-5" || !v.MCPDynamic ||
		!reflect.DeepEqual(v.Mounts, []string{"/ws", "/orch:ro"}) || !reflect.DeepEqual(v.MCPs, []string{"github"}) {
		t.Errorf("view = %+v", v)
	}
}
