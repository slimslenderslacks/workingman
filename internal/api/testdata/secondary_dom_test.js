// A smoke test of the timeline, pull-request and transcript screens
// (internal/api/web/timeline.js, pulls.js, watch.js): it loads the real
// bundle into a fake browser (fakedom.js) over a fake API, renders the
// timeline and follows an audit event, renders a project's PRs, the
// transcript in the project view's strip, on the sandbox card and on the
// session watch page, and checks that a transcript is labelled lost when
// its session ends and that no write is sent. Run by TestProjectViewModel
// (web_test.go):
//
//   node internal/api/testdata/secondary_dom_test.js internal/api/web
"use strict";

const assert = require("node:assert/strict");
const path = require("node:path");
const { boot, buttons, localButtons, label, SCRIPTS } = require("./fakedom.js");

const webDir = path.resolve(process.argv[2] || path.join(__dirname, "..", "web"));

const P = "/Users/me/orch/gui-api/project.yaml";
const task = (name, status, extra) =>
  Object.assign(
    {
      project: "p_0.gui-api", name, status, attempts: 0, failure_reason: "", blocked_reason: "",
      depends_on: [], touches: null, areas: [], exclusive: false,
      capabilities: { model: "default", resolved_model: "claude-opus-5-5", harness: "claude-acp", static_mcps: ["github"], policies: [], save_sandbox: false },
      source: null, base: [], succeeded_at: null, completed_at: null, queue_position: 0, waiting_on: [], integration: null,
      output: { summary: "", commits: [], created_files: [], touches_actual: [] },
      task_branch: "wm/gui-api/" + name, sandbox: "gui-api-" + name, commit_sandbox: "gui-api-" + name + "-commit", session: "",
    },
    extra
  );
const tasks = [
  task("session-relay", "running", { session: "task-session-relay" }),
  task("fix-pr-34-comment-1201", "committed", { source: { kind: "pr-comment", pr: 34 } }),
  task("push-pr-34", "success", { queue_position: 1, source: { kind: "pr-push", pr: 34 } }),
];
const project = {
  id: "p_0.gui-api", work_stream: "gui-api", status: "working", branch: "gui-api", max_parallel_tasks: 1,
  flags: { review: true, watching_pr: true }, commit_queue: ["push-pr-34"], task_counts: {}, task_total: 3,
  pull_requests: [{ repo: "slimslenderslacks/workingman", number: 34, state: "open" }],
  live: { integrating: "", running_tasks: ["session-relay"] },
};
let sessions = [{
  id: "task-session-relay", kind: "task", project: "p_0.gui-api", task: "session-relay", started_at: new Date(Date.now() - 60000).toISOString(),
  prompt_count: 1, persistent: false, status: "running", acp: { attachable: true },
  sandbox: { name: "gui-api-session-relay", live: true, image: "img:1", workspaces: [] }, gateway: { requested: ["github"], servers: ["github"], dynamic: false },
}];
const audit = [
  { seq: 41, time: "2026-11-25T13:59:00Z", event: "step_up_op", fields: { op: "sandbox_network_allow", target: "gui-api-session-relay", resource: "registry.npmjs.org", status: "applied" } },
  { seq: 43, time: "2026-11-25T14:12:30Z", event: "integration_succeeded", project: "p_0.gui-api", fields: { path: P, task: "snapshot-model", commits: "workingman=8be04d1" } },
  { seq: 44, time: "2026-11-25T14:18:44Z", event: "task_retry", project: "p_0.gui-api", fields: { path: P, task: "auth-token", attempts: "1", failure_reason: "go test: token file mode 0644, want 0600" } },
];
const pulls = [{
  project: "p_0.gui-api", repo: "slimslenderslacks/workingman", number: 34, url: "https://github.com/slimslenderslacks/workingman/pull/34",
  head_sha: "c97c47ec1234", state: "open", watching: true,
  review_poll: { schedule: "@every 8m", backoff_index: 2, has_baseline: true, baseline_source: "mcp" },
  next_poll_at: new Date(Date.now() + 3 * 60000 + 5000).toISOString(),
  poll_ladder: ["@every 2m", "@every 4m", "@every 8m", "@every 16m", "@every 32m", "@every 1h"],
  budgets: { review_fix_cycles: 1, max_review_fix_cycles: 10, review_errors: 0, max_review_errors: 5 },
  fix_tasks: ["fix-pr-34-comment-1201", "push-pr-34"],
}];

const routes = {
  "/api/v1/session": () => ({ auth: "cookie", csrf: "c", scopes: ["read"] }),
  "/api/v1/status": () => ({ daemon: { roots: ["/Users/me/orch"] }, cursor: 10 }),
  "/api/v1/projects": () => ({ items: [project], cursor: 10 }),
  "/api/v1/projects/p_0.gui-api": () => project,
  "/api/v1/projects/p_0.gui-api/tasks": () => ({ items: tasks, cursor: 10 }),
  "/api/v1/projects/p_0.gui-api/tasks/session-relay": () => tasks[0],
  "/api/v1/projects/p_0.gui-api/graph": () => ({ nodes: tasks.map((t) => ({ name: t.name, status: t.status })), edges: [], commit_queue: ["push-pr-34"], errors: [], max_attempts: 3 }),
  "/api/v1/projects/p_0.gui-api/pulls": () => ({ items: pulls, cursor: 10 }),
  "/api/v1/sessions": () => ({ items: sessions, cursor: 10 }),
  "/api/v1/sessions/task-session-relay": () => {
    if (!sessions.length) throw Object.assign(new Error("no session"), { status: 404, code: "not_found" });
    return sessions[0];
  },
  "/api/v1/sessions/task-session-relay/prompt": () => ({ instructions_md: "You are a task agent.\n", context_yaml: "kind: task\n" }),
  "/api/v1/sandboxes": () => ({ items: [], cursor: 10 }),
  "/api/v1/audit": () => ({ items: audit, cursor: 10 }),
  "/api/v1/messages": () => ({ items: [], cursor: 10 }),
};

// The stream log, which the wrapper removes when the session ends.
const upd = (u) => JSON.stringify({ jsonrpc: "2.0", method: "session/update", params: { sessionId: "s1", update: u } });
let log =
  upd({ sessionUpdate: "tool_call", toolCallId: "t1", title: "Read internal/acpwrapper/bridge.go", kind: "read", status: "completed" }) + "\n" +
  upd({ sessionUpdate: "agent_message_chunk", content: { type: "text", text: "ok  internal/api  0.41s" } }) + "\n";
let logGone = false;
const transcriptReqs = [];
const raw = (u) => {
  if (!u.pathname.endsWith("/transcript")) return null;
  transcriptReqs.push(u.search);
  if (logGone) return { ok: false, status: 404, headers: new Map(), text: async () => '{"error":{"code":"not_found","message":"session has no stream log"}}' };
  const after = Number(u.searchParams.get("after") || 0);
  const body = u.searchParams.get("limit") === "1" ? log.slice(0, log.indexOf("\n") + 1) : log.slice(after);
  const next = u.searchParams.get("limit") === "1" ? body.length : log.length;
  return { ok: true, status: 200, headers: new Map([["X-Transcript-Size", String(log.length)], ["X-Transcript-Next", String(next)]]), text: async () => body };
};

const page = boot({
  webDir,
  scripts: SCRIPTS,
  url: "http://127.0.0.1:7078/timeline",
  routes,
  raw,
  // Re-polls (the transcript every 2 s) would loop.
  skip: (fn) => String(fn).includes("pollTranscript(tr)"),
});
const { main, requests, settle } = page;
const send = (ev) => page.es.source.send(ev);

function noStray(where) {
  const text = main.textContent;
  assert.ok(!/null|undefined|\[object|NaN/.test(text), where + ": stray value rendered: " + text);
  return text;
}

function checkButtons(where) {
  for (const b of buttons(main)) {
    assert.ok(b.disabled, where + ": " + b.textContent + " is not disabled");
    assert.match(b.getAttribute("title"), /in the TUI: /, where + ": " + b.textContent);
  }
  return buttons(main).map(label);
}

const hrefs = () => main.findAll((e) => e.tagName === "A").map((a) => a.getAttribute("href"));

(async () => {
  page.start();
  await settle();
  // The one stream asks for audit events too.
  assert.match(page.es.source.url, /types=.*audit/);
  send({ seq: 10, type: "hello", data: { cursor: 10, epoch: "1" } });
  await settle();

  // ---- the timeline (§G.5) ----
  let text = noStray("timeline");
  assert.match(text, /Timeline/);
  assert.match(text, /step_up_op/);
  assert.match(text, /failure_reason: go test: token file mode 0644, want 0600/);
  assert.match(text, /audit seq 41–44/);
  let rows = main.findAll((e) => e.tagName === "TR" && e.className.includes("tl-row"));
  assert.equal(rows.length, 3);
  assert.equal(rows[0].textContent.includes("task_retry"), true, "newest first");
  assert.ok(rows.find((r) => r.textContent.includes("step_up_op")).className.includes("hl"), "step_up_op is highlighted");
  assert.ok(hrefs().includes("/projects/p_0.gui-api?task=auth-token"));
  assert.ok(hrefs().includes("/sandboxes/gui-api-session-relay"));
  // An audit event from the stream joins the top; a repeat does not.
  const blocked = { seq: 45, time: "2026-11-25T14:40:00Z", event: "project_blocked", project: "p_0.gui-api", fields: { path: P, reason: "wolf needed" } };
  send({ seq: 11, type: "audit", project: "p_0.gui-api", data: blocked });
  send({ seq: 12, type: "audit", project: "p_0.gui-api", data: blocked });
  await settle();
  rows = main.findAll((e) => e.tagName === "TR" && e.className.includes("tl-row"));
  assert.equal(rows.length, 4);
  assert.match(rows[0].textContent, /project_blocked/);
  assert.ok(rows[0].className.includes("hl"));
  // Filters: highlighted only.
  const hi = main.find((e) => e.tagName === "INPUT" && e.getAttribute("type") === "checkbox");
  hi.checked = true;
  hi.dispatch("change");
  rows = main.findAll((e) => e.tagName === "TR" && e.className.includes("tl-row"));
  assert.deepEqual(rows.map((r) => r.childNodes[1].childNodes[0].textContent), ["project_blocked", "step_up_op"]);

  // ---- pull requests (§G.5) ----
  await page.navigate("/projects/p_0.gui-api/pulls");
  text = noStray("pulls");
  assert.match(text, /Pull requests · gui-api/);
  assert.match(text, /slimslenderslacks\/workingman #34/);
  assert.match(text, /head c97c47ec · watching · poll ladder 2m→4m→\[8m\]→16m→32m→1h/);
  assert.match(text, /next poll in 3m · baseline mcp/);
  assert.match(text, /fix-pr-34-comment-1201 pr-comment · committed ✓/);
  assert.match(text, /push-pr-34 pr-push · in commit queue #1/);
  assert.match(text, /review_fix_cycles 1\/10 · review_errors 0\/5/);
  assert.match(text, /never calls GitHub itself/);
  assert.ok(hrefs().includes("https://github.com/slimslenderslacks/workingman/pull/34"));
  assert.ok(hrefs().includes("/projects/p_0.gui-api?task=push-pr-34"));
  let labels = checkButtons("pulls");
  assert.deepEqual(labels, ["Poll now"]);
  assert.match(buttons(main)[0].getAttribute("title"), /^review starts on an idle work stream/);

  // ---- the project view's strip: a transcript preview ----
  await page.navigate("/projects/p_0.gui-api?task=session-relay");
  text = noStray("project view");
  assert.match(text, /▸ Read internal\/acpwrapper\/bridge\.go/);
  assert.match(text, /live — not kept after the session ends/);
  assert.ok(hrefs().includes("/sessions/task-session-relay"), "the strip links to the full transcript");
  assert.ok(hrefs().includes("/projects/p_0.gui-api/pulls"), "the header links to the PRs");
  assert.ok(hrefs().includes("/timeline?project=p_0.gui-api"));
  assert.deepEqual(localButtons(main).map(label), ["Save transcript"]);
  checkButtons("project view");

  // ---- the sandbox card: the same transcript ----
  await page.navigate("/projects/p_0.gui-api/tasks/session-relay");
  text = noStray("card");
  assert.match(text, /Transcript/);
  assert.match(text, /▸ Read internal\/acpwrapper\/bridge\.go/);
  assert.match(text, /live — not kept after the session ends/);
  checkButtons("card");

  // ---- the session watch page: the whole transcript ----
  await page.navigate("/sessions/task-session-relay");
  text = noStray("watch");
  assert.match(text, /Session task-session-relay/);
  assert.match(text, /opening prompt/);
  assert.match(text, /Read \.orch\/instructions\.md and \.orch\/context\.yaml, then follow the instructions\./);
  assert.match(text, /▸ Read internal\/acpwrapper\/bridge\.go ✓/);
  assert.match(text, /ok {2}internal\/api {2}0\.41s/);
  assert.match(text, /one-shot task session · read-only \(observe\)/);
  assert.ok(transcriptReqs.some((s) => s.includes("after=0")), "the full view reads from the start");
  labels = checkButtons("watch");
  assert.ok(labels.includes("Watch (ACP)") && labels.includes("Prompt (persistent only)"));
  assert.ok(hrefs().includes("/projects/p_0.gui-api/tasks/session-relay"), "the watch page links to the sandbox card");

  // More output arrives, then the session ends: the wrapper deletes the
  // directory. The view keeps what it loaded and says the transcript is lost.
  log += upd({ sessionUpdate: "agent_message_chunk", content: { type: "text", text: "Done: relay landed." } }) + "\n";
  logGone = true;
  sessions = [];
  send({ seq: 13, type: "session.ended", project: "p_0.gui-api", data: { id: "task-session-relay", kind: "task", outcome: "ended" } });
  await settle();
  text = noStray("watch after end");
  assert.match(text, /session ended — transcript lost/);
  assert.match(text, /▸ Read internal\/acpwrapper\/bridge\.go/, "what was loaded stays");
  assert.match(text, /Save transcript/);

  // A session that is gone when the page opens.
  await page.navigate("/sessions/task-session-relay");
  text = noStray("watch, gone");
  assert.match(text, /the daemon no longer tracks this session/);
  assert.match(text, /transcript lost/);

  assert.ok(!requests.some((r) => r.method !== "GET" && r.method !== "HEAD"), "a screen sent a write");
  console.log("ok   timeline, pull requests and transcripts render from the API; a transcript is labelled lost when its session ends");
})().catch((e) => {
  console.log("FAIL " + (e && e.stack ? e.stack : e));
  process.exit(1);
});
