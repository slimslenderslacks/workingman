package acpwrapper

import (
	"path/filepath"
	"testing"

	"github.com/slimslenderslacks/work/internal/task"
)

// usageFrame is a claude-agent-acp-shaped usage_update carrying the concrete
// model in _meta["_claude/model"].
func usageFrame(model string) []byte {
	return []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"usage_update","used":1200,"size":200000,"_meta":{"_claude/model":"` + model + `"}}}}` + "\n")
}

func TestObservedModel(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame []byte
		want  string
	}{
		{"usage update", usageFrame("claude-opus-5-5"), "claude-opus-5-5"},
		{"synthetic", usageFrame("<synthetic>"), ""},
		{"other update kind", []byte(`{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","_meta":{"_claude/model":"x"}}}}`), ""},
		{"response", []byte(`{"jsonrpc":"2.0","id":1,"result":{"_meta":{"_claude/model":"x"}}}`), ""},
		{"no meta", []byte(`{"method":"session/update","params":{"update":{"sessionUpdate":"usage_update","used":1}}}`), ""},
		{"garbage", []byte("not json _claude/model\n"), ""},
	} {
		if got := observedModel(tc.frame); got != tc.want {
			t.Errorf("%s: observedModel = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestConfigHarness(t *testing.T) {
	if got := (Config{}).Harness(); got != StockTemplate {
		t.Errorf("Harness() with no template = %q, want %q", got, StockTemplate)
	}
	if got := (Config{TemplateImage: " img:1 "}).Harness(); got != "img:1" {
		t.Errorf("Harness() = %q, want img:1", got)
	}
}

// The recorder writes the sandbox's template and the first observed model into
// the task file as resolved_harness/resolved_model, keeps the requested model,
// and re-asserts both on flush after the agent has rewritten the file without
// them.
func TestRunRecorderWritesResolvedFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "first.yaml")
	if err := task.Save(path, &task.Task{Name: "first", Status: task.StatusReady, Model: task.ModelDefault}); err != nil {
		t.Fatal(err)
	}
	r := newRunRecorder(Config{TaskPath: path, SessionID: "s"})
	r.setHarness("slimslenderslacks/claude-code-acp:0.90.0")
	r.observeFrame([]byte(`{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk"}}}`))
	r.observeFrame(usageFrame("claude-opus-5-5"))

	got, err := task.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.ResolvedModel != "claude-opus-5-5" || got.ResolvedHarness != "slimslenderslacks/claude-code-acp:0.90.0" {
		t.Errorf("resolved = %q/%q", got.ResolvedModel, got.ResolvedHarness)
	}
	if got.Model != task.ModelDefault {
		t.Errorf("Model = %q, want requested %q kept", got.Model, task.ModelDefault)
	}

	// The agent rewrites its task file (status: success) and drops the fields.
	if err := task.Save(path, &task.Task{Name: "first", Status: task.StatusSuccess, Model: task.ModelDefault}); err != nil {
		t.Fatal(err)
	}
	r.flush()
	got, err = task.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusSuccess {
		t.Errorf("flush clobbered status: %q", got.Status)
	}
	if got.ResolvedModel != "claude-opus-5-5" || got.ResolvedHarness == "" {
		t.Errorf("flush did not re-assert resolved fields: %q/%q", got.ResolvedModel, got.ResolvedHarness)
	}
}

// A session with no task (planning, wolf, ...) records nothing.
func TestRunRecorderNoTaskIsNoop(t *testing.T) {
	r := newRunRecorder(Config{SessionID: "s"})
	r.setHarness("claude")
	r.observeFrame(usageFrame("claude-opus-5-5"))
	r.flush()
	var nilRec *runRecorder
	nilRec.observeFrame(usageFrame("x"))
	nilRec.flush()
}
