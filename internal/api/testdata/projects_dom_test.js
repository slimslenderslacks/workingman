// A smoke test of the project view (internal/api/web/projects.js): it loads
// the real bundle (app.js, graph.js, projects.js) into a node vm with a
// minimal fake DOM (fakedom.js), a fake fetch serving one project, and a fake
// EventSource, renders /projects/p_0.gui-api, then feeds a stream event and
// checks the view follows it. Run by TestProjectViewModel (web_test.go):
//
//   node internal/api/testdata/projects_dom_test.js internal/api/web
"use strict";

const assert = require("node:assert/strict");
const path = require("node:path");
const { byClass, boot } = require("./fakedom.js");

const webDir = path.resolve(process.argv[2] || path.join(__dirname, "..", "web"));

const t = (name, status, extra) =>
  Object.assign(
    {
      project: "p_0.gui-api", name, status, attempts: 0, failure_reason: "", blocked_reason: "",
      depends_on: [], touches: null, areas: [], exclusive: false,
      capabilities: { model: "default", resolved_model: "claude-opus-5-5", harness: "claude-acp", resolved_harness: "slimslenderslacks/claude-code-acp:0.90.0", static_mcps: ["github"], policies: [{ action: "allow", type: "network", resource: "api.github.com" }], save_sandbox: false },
      base: [], succeeded_at: null, completed_at: null, queue_position: 0, waiting_on: [], integration: null,
      output: { summary: "", commits: [], created_files: [], touches_actual: [] },
      task_branch: "wm/gui-api/" + name, sandbox: "gui-api-" + name,
    },
    extra
  );
const tasks = [
  t("control-package", "committed", { completed_at: "2026-11-25T08:47:00Z", output: { commits: [{ repo: "workingman", hash: "3f1a9c2aaaa" }] }, base: [{ repo: "workingman", hash: "950a530bbbb" }] }),
  t("api-read-endpoints", "success", { depends_on: ["control-package"], queue_position: 1, succeeded_at: "2026-11-25T09:20:00Z" }),
  t("auth-token", "failed", { attempts: 1, failure_reason: "go test failed" }),
  t("session-relay", "running", { depends_on: ["control-package"], session: "task-session-relay" }),
  t("browser-login", "ready", { depends_on: ["auth-token"] }),
];
const project = {
  id: "p_0.gui-api", work_stream: "gui-api", status: "working", branch: "gui-api", max_parallel_tasks: 2, verify: "go build ./...",
  repos: [{ org: "slimslenderslacks", name: "workingman" }],
  flags: { watching_pr: false }, commit_queue: ["api-read-endpoints"],
  task_counts: { committed: 1, success: 1, running: 1, ready: 1, failed: 1, blocked: 0 }, task_total: 5,
  live: { integrating: "api-read-endpoints", commit_reserved: true, running_tasks: ["session-relay"] },
};
const graph = {
  project: "p_0.gui-api",
  nodes: tasks.map((x) => ({ name: x.name, status: x.status })),
  edges: tasks.flatMap((x) => x.depends_on.map((d) => ({ from: d, to: x.name }))),
  ready: [], runnable: [], deferred: null, commit_queue: ["api-read-endpoints"], errors: [], max_attempts: 3,
};
const sessions = [{ id: "task-session-relay", kind: "task", project: "p_0.gui-api", task: "session-relay", started_at: new Date(Date.now() - 6 * 60000).toISOString(), prompt_count: 1, persistent: false, acp: { attachable: true }, sandbox: { name: "gui-api-session-relay", live: true, image: "slimslenderslacks/claude-code-acp:0.90.0" }, gateway: { requested: ["github"], servers: ["github"], dynamic: true } }];
const transcript =
  JSON.stringify({ method: "session/update", params: { update: { sessionUpdate: "tool_call", title: "Read internal/acpwrapper/bridge.go" } } }) + "\n" +
  JSON.stringify({ method: "session/update", params: { update: { sessionUpdate: "agent_message_chunk", content: { type: "text", text: "ok  internal/api  0.41s" } } } }) + "\n";

const routes = {
  "/api/v1/session": () => ({ auth: "cookie", csrf: "c", scopes: ["read"] }),
  "/api/v1/projects": () => ({ items: [project, { id: "p_0.docs", work_stream: "docs-refresh", status: "blocked", task_total: 0, task_counts: {}, flags: {}, live: { wolf_in_flight: true } }], cursor: 10 }),
  "/api/v1/projects/p_0.gui-api": () => project,
  "/api/v1/projects/p_0.gui-api/tasks": () => ({ items: tasks, cursor: 10 }),
  "/api/v1/projects/p_0.gui-api/graph": () => graph,
  "/api/v1/sessions": () => ({ items: sessions, cursor: 10 }),
  "/api/v1/messages": () => ({ items: [], cursor: 10 }),
};
const transcriptRaw = (u) =>
  u.pathname.endsWith("/transcript")
    ? { ok: true, status: 200, headers: new Map([["X-Transcript-Size", String(transcript.length)], ["X-Transcript-Next", String(transcript.length)]]), text: async () => transcript }
    : null;

const page = boot({
  webDir,
  scripts: ["app.js", "graph.js", "projects.js", "workload.js", "sandboxes.js", "agents.js"],
  url: "http://127.0.0.1:7078/projects/p_0.gui-api?task=session-relay",
  routes,
  raw: transcriptRaw,
  // The 2 s transcript re-poll would loop.
  skip: (fn) => String(fn).includes("pollTranscript(tr)"),
});
const { main, requests, settle } = page;
const send = (ev) => page.es.source.send(ev);

(async () => {
  page.start();
  await settle();
  send({ seq: 10, type: "hello", data: { cursor: 10, epoch: "1" } });
  await settle();

  const text = main.textContent;
  assert.ok(!/null|undefined|\[object/.test(text), "stray value rendered: " + text);
  // Header and the disabled actions with their TUI commands.
  assert.match(text, /gui-api/);
  assert.match(text, /max_parallel_tasks 2/);
  const buttons = main.findAll((e) => e.tagName === "BUTTON");
  assert.ok(buttons.length >= 8, "buttons: " + buttons.length);
  for (const b of buttons) {
    assert.ok(b.disabled, b.textContent + " is not disabled");
    assert.match(b.getAttribute("title"), /in the TUI: /, b.textContent);
  }
  const labels = buttons.map((b) => b.childNodes[0].textContent);
  for (const l of ["Stop", "Wolf…", "Review", "Queue intake", "Open shell ▾", "Watch (ACP)", "Open in my terminal", "Prompt (persistent only)"]) {
    assert.ok(labels.includes(l), "missing button " + l + " in " + labels.join(", "));
  }
  // The strip links to the task's full sandbox card (§G.3).
  const cardLink = main.find((e) => e.tagName === "A" && e.textContent === "Full sandbox card ▸");
  assert.ok(cardLink, "no link to the sandbox card");
  assert.equal(cardLink.getAttribute("href"), "/projects/p_0.gui-api/tasks/session-relay");
  assert.match(buttons.find((b) => b.childNodes[0].textContent === "Review").getAttribute("title"), /^review starts on an idle work stream/);
  // Rail.
  assert.match(text, /docs-refresh/);
  assert.match(text, /wolf in flight · join ▸/);
  // Graph.
  const svg = main.find((e) => e.tagName === "SVG");
  assert.ok(svg, "no graph");
  const nodes = svg.findAll((e) => e.tagName === "G" && byClass("node")(e));
  assert.equal(nodes.length, 5);
  const label = (name) => nodes.find((n) => n.getAttribute("aria-label").startsWith(name + ":")).getAttribute("aria-label");
  assert.match(label("api-read-endpoints"), /success · integrating/);
  assert.match(label("auth-token"), /failed · attempt 2\/4/);
  const edges = svg.findAll((e) => e.tagName === "PATH" && byClass("edge")(e));
  assert.equal(edges.length, 3);
  assert.equal(edges.filter(byClass("bad")).length, 1, "the edge out of the failed task is dashed red");
  assert.ok(nodes.find((n) => n.getAttribute("aria-label").startsWith("session-relay:")).className.includes("selected"));
  // Lane.
  assert.match(text, /Commit queue → project branch/);
  assert.match(text, /#1 api-read-endpoints/);
  assert.match(text, /integrating: rebase → verify → ff-only/);
  assert.match(text, /3f1a9c2 control-package/);
  assert.match(text, /base 950a530/);
  // Selected-task strip, from ?task=.
  assert.match(text, /Selected: session-relay/);
  assert.match(text, /default → claude-opus-5-5/);
  assert.match(text, /dynamic gateway/);
  assert.match(text, /deny \*\* · allow api\.github\.com/);
  assert.match(text, /▸ Read internal\/acpwrapper\/bridge\.go/);
  assert.ok(requests.some((r) => r.path.startsWith("/api/v1/sessions/task-session-relay/transcript")));
  assert.ok(!requests.some((r) => r.method !== "GET" && r.method !== "HEAD"), "the view sent a write");

  // Live: the queue head lands, the next task joins the queue.
  tasks[1] = Object.assign({}, tasks[1], { status: "committed", queue_position: 0, completed_at: "2026-11-25T09:40:00Z", output: { commits: [{ repo: "workingman", hash: "77aa11bcccc" }] } });
  tasks[3] = Object.assign({}, tasks[3], { status: "success", queue_position: 1, session: "" });
  graph.nodes = tasks.map((x) => ({ name: x.name, status: x.status }));
  graph.commit_queue = ["session-relay"];
  project.live = { integrating: "", commit_reserved: false };
  send({ seq: 11, type: "task.updated", project: "p_0.gui-api", data: { name: "api-read-endpoints", changed: ["status"], task: tasks[1] } });
  send({ seq: 12, type: "task.updated", project: "p_0.gui-api", data: { name: "session-relay", changed: ["status"], task: tasks[3] } });
  send({ seq: 13, type: "queue.updated", project: "p_0.gui-api", data: { commit_queue: ["session-relay"], integrating: "", commit_reserved: false } });
  send({ seq: 14, type: "session.ended", project: "p_0.gui-api", data: { id: "task-session-relay" } });
  await settle();
  const after = main.textContent;
  assert.ok(!/null|undefined|\[object/.test(after), "stray value rendered: " + after);
  assert.match(after, /#1 session-relay/);
  assert.match(after, /77aa11b api-read-endpoints/);
  assert.match(after, /no live session/);
  const svg2 = main.find((e) => e.tagName === "SVG");
  const n2 = svg2.findAll((e) => e.tagName === "G" && byClass("node")(e));
  assert.match(n2.find((n) => n.getAttribute("aria-label").startsWith("api-read-endpoints:")).getAttribute("aria-label"), /✓ committed · 77aa11b/);
  assert.match(n2.find((n) => n.getAttribute("aria-label").startsWith("session-relay:")).getAttribute("aria-label"), /success · queue #1/);

  console.log("ok   project view renders from the API and follows the stream");
})().catch((e) => {
  console.log("FAIL " + (e && e.stack ? e.stack : e));
  process.exit(1);
});
