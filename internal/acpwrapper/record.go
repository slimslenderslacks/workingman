package acpwrapper

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/slimslenderslacks/work/internal/task"
)

// StockTemplate is what Harness reports for a sandbox created from sbx's own
// claude template (no --template): the `sbx create claude` agent name.
const StockTemplate = "claude"

// Harness is the sbx template this session's sandbox is created from: the
// TemplateImage passed as `sbx create -t`, or StockTemplate when none is set.
// It is what the wrapper records as the task's resolved_harness.
func (c Config) Harness() string {
	if img := strings.TrimSpace(c.TemplateImage); img != "" {
		return img
	}
	return StockTemplate
}

// claudeModelMetaKey is the _meta key claude-agent-acp attaches to every
// usage_update with the concrete model id the API reported for the turn (the
// assistant message's `model`, e.g. "claude-opus-5-5"). ACP's usage_update has
// no model field of its own, so this is the one place the adapter tells a
// client which model a "default" (or an alias) actually resolved to.
const claudeModelMetaKey = "_claude/model"

// observedModel extracts the concrete model from one agent stdout frame, or ""
// when the frame is not a session/update usage_update carrying one. It is
// deliberately forgiving — any frame it cannot decode is just not a model
// report — because it runs on every frame the agent emits.
func observedModel(frame []byte) string {
	if !strings.Contains(string(frame), claudeModelMetaKey) {
		return ""
	}
	var msg struct {
		Method string `json:"method"`
		Params struct {
			Update struct {
				SessionUpdate string         `json:"sessionUpdate"`
				Meta          map[string]any `json:"_meta"`
			} `json:"update"`
		} `json:"params"`
	}
	if json.Unmarshal(frame, &msg) != nil || msg.Method != "session/update" ||
		msg.Params.Update.SessionUpdate != "usage_update" {
		return ""
	}
	m, _ := msg.Params.Update.Meta[claudeModelMetaKey].(string)
	m = strings.TrimSpace(m)
	if m == "<synthetic>" {
		return ""
	}
	return m
}

// runRecorder writes what a task's session actually ran under — the concrete
// model (from the agent's usage reports) and the harness (the sandbox's sbx
// template) — into the task file as resolved_model / resolved_harness, so the
// TUI can show them instead of the requested "default". A zero taskPath (a
// session with no task: planning, wolf, ...) makes every method a no-op.
//
// Writes go through task.Update, which only saves when a value changed, so the
// steady stream of usage_updates costs one decode per frame and no disk I/O.
// The model is first written on the turn's first usage report — before the
// agent has done any work, so before it edits its own task file — and both
// values are re-asserted by flush once the agent has exited, in case the
// agent rewrote the file without them.
type runRecorder struct {
	taskPath  string
	sessionID string

	mu      sync.Mutex
	model   string
	harness string
}

func newRunRecorder(c Config) *runRecorder {
	return &runRecorder{taskPath: c.TaskPath, sessionID: c.SessionID}
}

// setHarness records the sandbox's harness and writes it to the task file.
func (r *runRecorder) setHarness(h string) {
	if r == nil || r.taskPath == "" || h == "" {
		return
	}
	r.mu.Lock()
	r.harness = h
	r.mu.Unlock()
	r.write()
}

// observeFrame inspects one agent frame and, the first time it reports a model
// (or when the model changes), records it.
func (r *runRecorder) observeFrame(frame []byte) {
	if r == nil || r.taskPath == "" {
		return
	}
	m := observedModel(frame)
	if m == "" {
		return
	}
	r.mu.Lock()
	changed := m != r.model
	r.model = m
	r.mu.Unlock()
	if changed {
		r.write()
	}
}

// Model is the last concrete model observed ("" before the first report).
func (r *runRecorder) Model() string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.model
}

// flush re-asserts the recorded values on the task file. Called after the
// agent has exited and before anything (the sandbox-retention check, the
// daemon's session-end handling) re-reads the task.
func (r *runRecorder) flush() {
	if r == nil || r.taskPath == "" {
		return
	}
	r.write()
}

func (r *runRecorder) write() {
	r.mu.Lock()
	model, harness := r.model, r.harness
	r.mu.Unlock()
	if model == "" && harness == "" {
		return
	}
	err := task.Update(r.taskPath, func(t *task.Task) bool {
		changed := false
		if model != "" && t.ResolvedModel != model {
			t.ResolvedModel = model
			changed = true
		}
		if harness != "" && t.ResolvedHarness != harness {
			t.ResolvedHarness = harness
			changed = true
		}
		return changed
	})
	if err != nil {
		// Display metadata only: never fail or stall the session over it.
		fmt.Fprintf(os.Stderr, "acp-wrapper: session %s: record model/harness in %s: %v\n", r.sessionID, r.taskPath, err)
	}
}
