// Tests for the project view's model, internal/api/web/graph.js. Run by
// TestProjectViewModel (web_test.go) with node, or by hand:
//
//   node internal/api/testdata/graph_test.js internal/api/web/graph.js
"use strict";

const assert = require("node:assert/strict");
const path = require("node:path");

const pv = require(path.resolve(process.argv[2] || path.join(__dirname, "..", "web", "graph.js")));

const tests = [];
const test = (name, fn) => tests.push({ name, fn });

// The wireframe's project (docs/proposals/workingman-gui/project-graph.svg).
function fixture() {
  const t = (name, status, extra) =>
    Object.assign(
      {
        project: "p_0.gui-api", name, status, attempts: 0, failure_reason: "", blocked_reason: "",
        depends_on: [], touches: null, areas: [], exclusive: false,
        capabilities: { model: "default", resolved_model: "claude-opus-5-5", harness: "claude-acp", static_mcps: ["github"], policies: [], save_sandbox: false },
        base: [], succeeded_at: null, completed_at: null, queue_position: 0, waiting_on: [], integration: null,
        output: { summary: "", commits: [], created_files: [], touches_actual: [] },
        task_branch: "wm/gui-api/" + name, sandbox: "gui-api-" + name,
      },
      extra
    );
  const tasks = [
    t("control-package", "committed", { areas: ["internal/control"], completed_at: "2026-11-25T08:47:00Z", output: { commits: [{ repo: "workingman", hash: "3f1a9c2aaaa" }] }, base: [{ repo: "workingman", hash: "950a530bbbb" }] }),
    t("snapshot-model", "committed", { completed_at: "2026-11-25T09:12:00Z", output: { commits: [{ repo: "workingman", hash: "8be04d1cccc" }] }, base: [{ repo: "workingman", hash: "3f1a9c2aaaa" }] }),
    t("auth-token", "failed", { attempts: 1, failure_reason: "go test failed", capabilities: { save_sandbox: true, policies: [] } }),
    t("api-read-endpoints", "success", { depends_on: ["control-package"], queue_position: 1, succeeded_at: "2026-11-25T09:20:00Z" }),
    t("sse-events", "success", { depends_on: ["snapshot-model"], queue_position: 2, succeeded_at: "2026-11-25T09:31:05Z" }),
    t("session-relay", "running", { depends_on: ["snapshot-model"], session: "task-session-relay", touches: ["internal/api/relay.go"] }),
    t("gui-shell", "ready", { depends_on: ["api-read-endpoints", "sse-events"], waiting_on: ["api-read-endpoints", "sse-events"] }),
    t("browser-login", "ready", { depends_on: ["auth-token"] }),
  ];
  const graph = {
    project: "p_0.gui-api",
    nodes: tasks.map((x) => ({ name: x.name, status: x.status, exclusive: x.exclusive, queue_position: x.queue_position || undefined })),
    edges: tasks.flatMap((x) => x.depends_on.map((d) => ({ from: d, to: x.name }))),
    ready: [], runnable: [], deferred: null,
    commit_queue: ["api-read-endpoints", "sse-events"],
    errors: [], max_attempts: 3,
  };
  const project = {
    id: "p_0.gui-api", work_stream: "gui-api", status: "working", branch: "gui-api", max_parallel_tasks: 2,
    flags: { watching_pr: false }, commit_queue: graph.commit_queue,
    task_counts: { committed: 2, success: 2, running: 1, ready: 2, failed: 1, blocked: 0 }, task_total: 8,
    live: { integrating: "api-read-endpoints", commit_reserved: true, running_tasks: ["session-relay"], wolf_in_flight: false },
  };
  const sessions = [{ id: "task-session-relay", kind: "task", project: "p_0.gui-api", task: "session-relay", started_at: "2026-11-25T09:25:00Z", prompt_count: 1, persistent: false, acp: { attachable: true }, sandbox: { name: "gui-api-session-relay", live: true } }];
  return { tasks, graph, project, sessions };
}

function ctxOf(f, now) {
  const st = pv.newState(f.project, f.tasks, f.graph, f.sessions, 10);
  return { st, ctx: { graph: st.graph, project: st.project, tasks: st.tasks, sessions: st.sessions, now: now || Date.parse("2026-11-25T09:31:00Z") } };
}

test("depths follow depends_on and survive cycles and dangling edges", () => {
  const f = fixture();
  const d = pv.depths(f.graph.nodes, f.graph.edges);
  assert.equal(d.get("control-package"), 0);
  assert.equal(d.get("api-read-endpoints"), 1);
  assert.equal(d.get("gui-shell"), 2);
  assert.equal(d.get("browser-login"), 1);
  const cyc = pv.depths([{ name: "a" }, { name: "b" }], [{ from: "a", to: "b" }, { from: "b", to: "a" }, { from: "ghost", to: "a" }]);
  assert.ok(cyc.has("a") && cyc.has("b"));
});

test("layout places columns left to right with no overlapping nodes", () => {
  const f = fixture();
  const L = pv.layout(f.graph);
  assert.equal(L.columns, 3);
  assert.equal(L.nodes.length, 8);
  const by = new Map(L.nodes.map((n) => [n.name, n]));
  assert.ok(by.get("control-package").x < by.get("api-read-endpoints").x);
  assert.ok(by.get("api-read-endpoints").x < by.get("gui-shell").x);
  const seen = new Set();
  for (const n of L.nodes) {
    const key = n.x + "," + n.y;
    assert.ok(!seen.has(key), "two nodes at " + key);
    seen.add(key);
    assert.ok(n.x + L.nodeW <= L.width && n.y + L.nodeH <= L.height, n.name + " is outside the canvas");
  }
  assert.equal(L.edges.length, f.graph.edges.length);
  // A dependent sits in the row of its dependency when it can.
  assert.equal(by.get("api-read-endpoints").row, by.get("control-package").row);
  assert.deepEqual(pv.layout({ nodes: [], edges: [] }).nodes, []);
});

test("node labels: success is queued for integration, not done", () => {
  const { ctx } = ctxOf(fixture());
  const head = pv.nodeFacts("api-read-endpoints", ctx);
  assert.match(head.line, /^⧗ success · integrating/);
  assert.ok(head.classes.includes("integrating"));
  const second = pv.nodeFacts("sse-events", ctx);
  assert.equal(second.line, "⧗ success · queue #2");
  assert.ok(!/done|committed/.test(second.line));
  assert.equal(pv.nodeFacts("control-package", ctx).line, "✓ committed · 3f1a9c2");
  assert.equal(pv.nodeFacts("control-package", ctx).detail, "areas: internal/control");
});

test("node labels: attempts, retained sandbox, running time, waiting_on", () => {
  const { ctx } = ctxOf(fixture());
  const failed = pv.nodeFacts("auth-token", ctx);
  assert.equal(failed.line, "✗ failed · attempt 2/4");
  assert.equal(failed.detail, "sandbox retained");
  const running = pv.nodeFacts("session-relay", ctx);
  assert.equal(running.line, "● running · 6m · turn 1");
  assert.equal(running.detail, "sandbox gui-api-session-relay");
  const shell = pv.nodeFacts("gui-shell", ctx);
  assert.match(shell.detail, /api-read-endpoints \(integrating\), sse-events/);
  const login = pv.nodeFacts("browser-login", ctx);
  assert.equal(login.line, "○ ready · waiting_on");
  assert.equal(login.detail, "auth-token (failed)");
  assert.ok(pv.failedDep("failed") && pv.failedDep("blocked") && !pv.failedDep("success"));
});

test("node labels: conflict re-run and deferred", () => {
  const f = fixture();
  const relay = f.tasks.find((t) => t.name === "session-relay");
  relay.status = "ready";
  relay.attempts = 1;
  relay.integration = { result: "conflict", files: ["internal/api/relay.go"] };
  f.graph.deferred = { overlap: ["session-relay"] };
  const { ctx } = ctxOf(f);
  const n = pv.nodeFacts("session-relay", ctx);
  assert.ok(n.classes.includes("rerun") && n.classes.includes("deferred"));
  assert.equal(n.detail, "conflict in internal/api/relay.go (attempt 2/4)");
  assert.match(n.title, /^deferred: overlaps/);
});

test("lane: queue head first, landed newest first, base, failures", () => {
  const f = fixture();
  const verify = f.tasks.find((t) => t.name === "auth-token");
  verify.status = "ready";
  verify.integration = { result: "verify_failed", repo: "workingman", output: "l1\nl2\nFAIL internal/api\n" };
  const { st } = ctxOf(f);
  const ln = pv.lane(st.project, st.tasks, st.graph);
  assert.deepEqual(ln.queued.map((q) => [q.position, q.name, q.integrating]), [[1, "api-read-endpoints", true], [2, "sse-events", false]]);
  assert.deepEqual(ln.landed.map((c) => c.hash.slice(0, 7)), ["8be04d1", "3f1a9c2"]);
  assert.equal(ln.base[0].hash, "950a530bbbb");
  assert.equal(ln.failures.length, 1);
  assert.equal(ln.failures[0].text, "build check failed in workingman");
  assert.equal(ln.failures[0].outcome, "task back to ready");
  assert.match(ln.failures[0].output, /FAIL internal\/api$/);
  assert.equal(ln.integrating, "api-read-endpoints");
  assert.equal(ln.branch, "gui-api");
});

test("actions: all labelled with their TUI command; refusals match the TUI", () => {
  const { st } = ctxOf(fixture());
  const acts = pv.projectActions(st.project);
  assert.deepEqual(acts.map((a) => a.label), ["Stop", "Wolf…", "Review", "Queue intake", "Open shell ▾"]);
  for (const a of acts) assert.ok(a.command, a.label + " has no command");
  assert.equal(acts[0].command, ":stop");
  const review = acts.find((a) => a.command === ":review");
  assert.equal(review.refusal, 'review starts on an idle work stream or kicks one already watching a PR (this one is "working")');
  const stopped = pv.projectActions(Object.assign({}, st.project, { status: "stopped" }));
  assert.equal(stopped[0].label, "Start");
  assert.equal(stopped[0].command, ":start");
  const idle = pv.projectActions(Object.assign({}, st.project, { status: "idle" }));
  assert.equal(idle.find((a) => a.command === ":review").refusal, "");

  const relay = st.tasks.get("session-relay");
  const ta = pv.taskActions(relay, st.sessions.get("task-session-relay"));
  assert.equal(ta[0].label, "Watch (ACP)");
  assert.equal(ta[0].refusal, "");
  assert.ok(ta.find((a) => a.label === "Prompt (persistent only)").refusal.startsWith("persistent sessions only"));
  assert.ok(!ta.some((a) => a.label === "Retry"));
  const failed = pv.taskActions(st.tasks.get("auth-token"), null);
  assert.ok(failed.some((a) => a.label === "Retry"));
  assert.equal(failed[0].refusal, "no live ACP session for this task");
});

test("apply patches state from stream events", () => {
  const { st } = ctxOf(fixture());
  // Replayed events the lists already hold change nothing.
  assert.equal(pv.apply(st, { seq: 9, type: "task.status", project: "p_0.gui-api", data: { name: "gui-shell", to: "running" } }).changed, false);
  // Another project's events change nothing.
  assert.equal(pv.apply(st, { seq: 11, type: "task.status", project: "p_0.other", data: { name: "gui-shell", to: "running" } }).changed, false);

  let r = pv.apply(st, { seq: 12, type: "task.status", project: "p_0.gui-api", data: { name: "session-relay", from: "running", to: "success", attempts: 0 } });
  assert.ok(r.changed && r.graph);
  assert.equal(st.tasks.get("session-relay").status, "success");
  r = pv.apply(st, { seq: 13, type: "queue.updated", project: "p_0.gui-api", data: { commit_queue: ["sse-events", "session-relay"], integrating: "sse-events", commit_reserved: true } });
  assert.ok(r.project);
  assert.deepEqual(st.graph.commit_queue, ["sse-events", "session-relay"]);
  assert.equal(st.project.live.integrating, "sse-events");
  assert.deepEqual(st.project.live.running_tasks, ["session-relay"], "queue.updated keeps the rest of live");
  pv.apply(st, { seq: 14, type: "task.created", project: "p_0.gui-api", data: { name: "new-one", status: "ready", depends_on: [] } });
  assert.ok(st.tasks.has("new-one"));
  pv.apply(st, { seq: 15, type: "task.removed", project: "p_0.gui-api", data: { name: "new-one" } });
  assert.ok(!st.tasks.has("new-one"));
  pv.apply(st, { seq: 16, type: "session.ended", project: "p_0.gui-api", data: { id: "task-session-relay" } });
  assert.equal(st.sessions.size, 0);
  pv.apply(st, { seq: 17, type: "project.status", project: "p_0.gui-api", data: { from: "working", to: "blocked", blocked_reason: "x" } });
  assert.equal(st.project.status, "blocked");
  assert.equal(st.cursor, 17);
  assert.ok(pv.apply(st, { type: "reset", data: { cursor: 99 } }).reset);
  pv.apply(st, { seq: 18, type: "project.removed", project: "p_0.gui-api", data: { id: "p_0.gui-api" } });
  assert.ok(st.removed);
});

test("transcript preview: tool calls, text and plan from ACP frames", () => {
  const upd = (u) => ({ jsonrpc: "2.0", method: "session/update", params: { sessionId: "s", update: u } });
  const frames = [
    { jsonrpc: "2.0", id: 1, result: {} },
    upd({ sessionUpdate: "tool_call", title: "Read .orch/instructions.md", kind: "read" }),
    upd({ sessionUpdate: "agent_message_chunk", content: { type: "text", text: "Plan the " } }),
    upd({ sessionUpdate: "agent_message_chunk", content: { type: "text", text: "relay.\nThen test." } }),
    upd({ sessionUpdate: "plan", entries: [{ content: "relay type" }, { content: "tests" }] }),
    upd({ sessionUpdate: "tool_call_update", status: "failed", title: "Bash go test" }),
    upd({ sessionUpdate: "usage_update", used: 1 }),
  ];
  const lines = pv.transcriptLines(frames, 10, 200);
  assert.deepEqual(lines.map((l) => l.text), [
    "▸ Read .orch/instructions.md",
    "  Plan the relay.",
    "  Then test.",
    "  plan: 1 relay type  2 tests",
    "  ✗ Bash go test",
  ]);
  assert.equal(pv.transcriptLines(frames, 2, 200).length, 2);
  assert.ok(pv.transcriptLines(frames, 10, 12)[0].text.endsWith("…"));
});

test("rail facts: counts, progress bar and badges", () => {
  const { st } = ctxOf(fixture());
  const f = pv.railFacts(st.project);
  assert.equal(f.summary, "8 tasks · 2 queued · 1 running · 1 failed");
  assert.equal(f.bar.reduce((a, s) => a + s.fraction, 0), 1);
  assert.equal(f.bar[0].status, "committed");
  const blocked = pv.railFacts({ status: "blocked", task_total: 0, task_counts: {}, flags: { watching_pr: true, archive: true }, cron_active: true, cron: "@daily", pull_requests: [{ repo: "o/r", number: 34, state: "open" }], live: { wolf_in_flight: true } });
  assert.equal(blocked.note, "wolf in flight · join ▸");
  assert.deepEqual(blocked.badges.map((b) => b.text), ["cron", "PR #34", "archive-ready"]);
  assert.deepEqual(blocked.bar, []);
});

test("policy summary starts from deny **", () => {
  assert.equal(pv.policySummary([]), "deny **");
  assert.equal(pv.policySummary([{ action: "allow", type: "network", resource: "api.github.com" }, { action: "allow", type: "filesystem", resource: "/x" }]), "deny ** · allow api.github.com");
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
