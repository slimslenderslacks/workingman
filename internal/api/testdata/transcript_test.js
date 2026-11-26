// Tests for the transcript model, internal/api/web/transcript.js. Run by
// TestProjectViewModel (web_test.go) with node, or by hand:
//
//   node internal/api/testdata/transcript_test.js internal/api/web/transcript.js
"use strict";

const assert = require("node:assert/strict");
const path = require("node:path");

const tx = require(path.resolve(process.argv[2] || path.join(__dirname, "..", "web", "transcript.js")));

const tests = [];
const test = (name, fn) => tests.push({ name, fn });

const upd = (u) => ({ jsonrpc: "2.0", method: "session/update", params: { sessionId: "s1", update: u } });

// A one-shot task session's stream.log, as the wrapper tees it.
const frames = [
  { jsonrpc: "2.0", id: 1, result: { protocolVersion: 1 } },
  { jsonrpc: "2.0", id: 2, result: { sessionId: "s1" } },
  upd({ sessionUpdate: "agent_thought_chunk", content: { type: "text", text: "Reading the " } }),
  upd({ sessionUpdate: "agent_thought_chunk", content: { type: "text", text: "instructions." } }),
  upd({ sessionUpdate: "tool_call", toolCallId: "t1", title: "Read .orch/instructions.md", kind: "read", status: "pending" }),
  upd({ sessionUpdate: "tool_call_update", toolCallId: "t1", status: "completed", content: [{ type: "content", content: { type: "text", text: "You are a task agent." } }] }),
  upd({ sessionUpdate: "agent_message_chunk", content: { type: "text", text: "I'll add " } }),
  upd({ sessionUpdate: "agent_message_chunk", content: { type: "text", text: "the relay." } }),
  upd({ sessionUpdate: "plan", entries: [{ content: "write relay.go", status: "completed" }, { content: "test it", status: "pending" }] }),
  upd({ sessionUpdate: "tool_call", toolCallId: "t2", title: "go test ./...", kind: "execute", status: "in_progress" }),
  upd({ sessionUpdate: "tool_call_update", toolCallId: "t2", status: "failed", content: [{ type: "content", content: { type: "text", text: "FAIL token=[REDACTED]" } }] }),
  { jsonrpc: "2.0", id: 9, method: "session/request_permission", params: { toolCall: { title: "Write internal/api/relay.go" } } },
  upd({ sessionUpdate: "usage_update" }),
  { jsonrpc: "2.0", id: 3, result: { stopReason: "end_turn" } },
];

test("entries: chunks joined, tool calls updated in place, plan, turn end", () => {
  const es = tx.entries(frames, { prompt_count: 1 });
  assert.deepEqual(es.map((e) => e.kind), ["prompt", "thought", "tool", "text", "plan", "tool", "ask", "turn"]);
  assert.equal(es[0].text, tx.OPENING_PROMPT);
  assert.equal(es[1].text, "Reading the instructions.");
  assert.deepEqual([es[2].title, es[2].status, es[2].tool, es[2].output], ["Read .orch/instructions.md", "completed", "read", "You are a task agent."]);
  assert.equal(es[3].text, "I'll add the relay.");
  assert.deepEqual(es[4].items, [{ text: "write relay.go", status: "completed" }, { text: "test it", status: "pending" }]);
  assert.equal(es[5].status, "failed");
  assert.match(es[5].output, /\[REDACTED\]/);
  assert.equal(es[6].text, "asked permission: Write internal/api/relay.go");
  assert.equal(es[7].text, "turn ended: end_turn");
});

test("entries: no opening prompt before the session is primed", () => {
  assert.equal(tx.entries(frames, { prompt_count: 0 })[0].kind, "thought");
  assert.equal(tx.entries([], null).length, 0);
  // Frames that are not objects, or carry nothing to show, are skipped.
  assert.equal(tx.entries([null, 3, "x", upd({ sessionUpdate: "agent_message_chunk", content: {} })], null).length, 0);
});

test("parseNDJSON skips lines that are not JSON", () => {
  assert.equal(tx.parseNDJSON('{"a":1}\nnot json\n\n{"b":2}\n').length, 2);
});

test("asText: what Save transcript writes", () => {
  const text = tx.asText(tx.entries(frames, { prompt_count: 1 }), "workingman transcript: session task-x");
  assert.match(text, /^workingman transcript: session task-x\n\n> Read \.orch\/instructions\.md/);
  assert.match(text, /\(thinking\) Reading the instructions\./);
  assert.match(text, /▸ Read \.orch\/instructions\.md ✓\n {4}You are a task agent\./);
  assert.match(text, /▸ go test \.\/\.\.\. ✗/);
  assert.match(text, /plan:\n {2}1\. write relay\.go \[completed\]/);
  assert.match(text, /-- turn ended: end_turn/);
});

test("label: live, lost when the session ends, gone", () => {
  const live = tx.label("live", { kind: "task", persistent: false });
  assert.equal(live.text, "live — not kept after the session ends");
  assert.match(live.detail, /one-shot task session/);
  assert.match(tx.label("live", { kind: "wolf", persistent: true }).detail, /persistent wolf session/);
  const lost = tx.label("lost", null);
  assert.equal(lost.cls, "lost");
  assert.equal(lost.text, "session ended — transcript lost");
  assert.match(lost.detail, /deleted the session directory/);
  assert.equal(tx.label("gone", null).text, "transcript lost");
});

test("fileName is safe and stamped", () => {
  assert.equal(tx.fileName("task-a/b c", new Date(2026, 10, 25, 9, 5).getTime()), "task-a_b_c-20261125-0905.transcript.txt");
  assert.equal(tx.fileName("", NaN), "session.transcript.txt");
});

let failed = 0;
for (const t of tests) {
  try {
    t.fn();
    console.log("ok   " + t.name);
  } catch (e) {
    failed++;
    console.log("FAIL " + t.name + "\n" + (e && e.stack ? e.stack : e));
  }
}
console.log(`${tests.length - failed}/${tests.length} passed`);
process.exit(failed ? 1 : 0);
