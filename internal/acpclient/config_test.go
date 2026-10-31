package acpclient

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// modelOption builds a claude-agent-acp-shaped "model" select option.
func modelOption(current string) map[string]any {
	return map[string]any{
		"id": ConfigIDModel, "name": "Model", "category": "model", "type": "select",
		"currentValue": current,
		"options": []map[string]any{
			{"value": "default", "name": "Default"},
			{"name": "Other", "options": []map[string]any{
				{"value": "opus", "name": "Opus"},
				{"value": "haiku", "name": "Haiku"},
			}},
		},
	}
}

func TestSetModelSendsSetConfigOptionAndTracksResult(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got := make(chan setConfigOptionParams, 1)
	c := newTestClient(t, func(a *agentConn) {
		init := a.readRequest()
		a.result(init.ID, map[string]any{"protocolVersion": 1})
		newReq := a.readRequest()
		a.result(newReq.ID, map[string]any{
			"sessionId":     "s1",
			"configOptions": []any{modelOption("default"), map[string]any{"id": "fast", "type": "boolean", "currentValue": false}},
		})
		setMode := a.readRequest()
		a.result(setMode.ID, map[string]any{})

		req := a.readRequest()
		if req.Method != methodSetConfigOption {
			a.t.Errorf("method = %q, want %q", req.Method, methodSetConfigOption)
		}
		var p setConfigOptionParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			a.t.Errorf("decode params: %v", err)
		}
		got <- p
		a.result(req.ID, map[string]any{"configOptions": []any{modelOption("opus")}})
		a.holdOpen()
	})

	if err := c.Connect(ctx, "/work"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if m := c.Model(); m != "default" {
		t.Errorf("Model after Connect = %q, want default", m)
	}
	opts := c.ConfigOptions()
	if len(opts) != 2 {
		t.Fatalf("ConfigOptions = %d, want 2", len(opts))
	}
	if opts[1].Current() != "" {
		t.Errorf("boolean option Current = %q, want empty", opts[1].Current())
	}
	var vals []string
	for _, v := range opts[0].Values() {
		vals = append(vals, v.Value)
	}
	if len(vals) != 3 || vals[0] != "default" || vals[1] != "opus" || vals[2] != "haiku" {
		t.Errorf("Values = %v, want [default opus haiku] (groups flattened)", vals)
	}

	if err := c.SetModel(ctx, "opus"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	p := <-got
	if p.SessionID != "s1" || p.ConfigID != ConfigIDModel || p.Value != "opus" {
		t.Errorf("params = %+v, want {s1 model opus}", p)
	}
	if m := c.Model(); m != "opus" {
		t.Errorf("Model after SetModel = %q, want opus", m)
	}
}

func TestSetModelAgentErrorPropagates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := newTestClient(t, func(a *agentConn) {
		req := a.readRequest()
		a.rpcErr(req.ID, -32602, "Invalid value for config option model: gpt")
		a.holdOpen()
	})
	if err := c.Adopt("s1"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetModel(ctx, "gpt"); err == nil {
		t.Fatal("SetModel error = nil, want the agent's error")
	}
	if m := c.Model(); m != "" {
		t.Errorf("Model = %q, want empty", m)
	}
}

func TestSetModelBeforeSession(t *testing.T) {
	c := newTestClient(t, func(a *agentConn) { a.holdOpen() })
	if err := c.SetModel(context.Background(), "opus"); err == nil {
		t.Fatal("SetModel before Connect/Adopt = nil error")
	}
}

// A config_option_update — e.g. another client's switch on a shared session —
// updates Model for the session it names, and only that session.
func TestConfigOptionUpdateTracksModel(t *testing.T) {
	c := newTestClient(t, func(a *agentConn) {
		for _, sid := range []string{"other", "s1"} {
			model := "haiku"
			if sid == "other" {
				model = "opus"
			}
			a.write(map[string]any{"jsonrpc": "2.0", "method": methodUpdate, "params": map[string]any{
				"sessionId": sid,
				"update":    map[string]any{"sessionUpdate": updateConfigOption, "configOptions": []any{modelOption(model)}},
			}})
		}
		a.holdOpen()
	})
	if err := c.Adopt("s1"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for c.Model() != "haiku" {
		if time.Now().After(deadline) {
			t.Fatalf("Model = %q, want haiku", c.Model())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
