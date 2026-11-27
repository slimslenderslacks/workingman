// Tests for the timeline, pull-request and intake model,
// internal/api/web/secondary.js. Run by TestProjectViewModel (web_test.go)
// with node, or by hand:
//
//   node internal/api/testdata/secondary_test.js internal/api/web/secondary.js
"use strict";

const assert = require("node:assert/strict");
const path = require("node:path");

const sv = require(path.resolve(process.argv[2] || path.join(__dirname, "..", "web", "secondary.js")));

const tests = [];
const test = (name, fn) => tests.push({ name, fn });

const P = "/Users/me/orch/gui-api/project.yaml";
// The wireframe's timeline (docs/proposals/workingman-gui/secondary-views.svg).
const events = [
  { seq: 40, time: "2026-11-25T13:47:12Z", event: "api_login", fields: { session: "gui-session-3" } },
  { seq: 41, time: "2026-11-25T13:59:00Z", event: "step_up_op", fields: { id: "op_1", op: "sandbox_network_allow", target: "gui-api-session-relay", resource: "registry.npmjs.org", status: "applied", detail: "approved" } },
  { seq: 42, time: "2026-11-25T14:02:10Z", event: "intake_queued", project: "p_0.gui-api", fields: { path: "/Users/me/orch/gui-api/intake/add-sandbox-card.md", project: P } },
  { seq: 43, time: "2026-11-25T14:12:30Z", event: "integration_succeeded", project: "p_0.gui-api", fields: { path: P, task: "snapshot-model", commits: "workingman=8be04d1", fast_forward: "true" } },
  { seq: 44, time: "2026-11-25T14:18:44Z", event: "task_retry", project: "p_0.gui-api", fields: { path: P, task: "auth-token", attempts: "1", cause: "failure", failure_reason: "go test: token file mode 0644, want 0600" } },
  { seq: 45, time: "2026-11-25T14:31:05Z", event: "acp_session_started", fields: { kind: "commit", session_id: "commit-api-read-endpoints", sandbox: "gui-api-api-read-endpoints-commit" } },
  { seq: 46, time: "2026-11-25T14:31:05Z", event: "integration_started", project: "p_0.gui-api", fields: { path: P, task: "api-read-endpoints", queue_depth: "1" } },
  { seq: 47, time: "2026-11-25T14:40:00Z", event: "project_blocked", project: "p_0.docs", fields: { path: "/Users/me/orch/docs/project.yaml", reason: "wolf needed" } },
  { seq: 48, time: "2026-11-25T14:41:00Z", event: "task_running", project: "p_0.gui-api", fields: { task: "session-relay" } },
];

test("families follow survey §13", () => {
  const f = (n) => sv.family(n);
  assert.equal(f("integration_started"), "integration");
  assert.equal(f("worktree_created"), "integration");
  assert.equal(f("touches_exceeded"), "integration");
  assert.equal(f("task_retry_exhausted"), "task");
  assert.equal(f("sandbox_retained"), "task");
  assert.equal(f("session_reaped"), "session");
  assert.equal(f("acp_session_started"), "session");
  assert.equal(f("step_up_op"), "session");
  assert.equal(f("review_gateway_started"), "review");
  assert.equal(f("workingman_mcp_error"), "session");
  assert.equal(f("intake_queued"), "intake");
  assert.equal(f("cron_fired"), "cron");
  assert.equal(f("project_blocked"), "project");
  assert.equal(f("api_login"), "api");
  assert.equal(f("daemon_start"), "process");
  assert.equal(f("custom_event"), "other");
});

test("rows: highlight, summary, detail and links to what each names", () => {
  const rows = new Map(events.map((e) => [e.event, sv.row(e)]));
  const step = rows.get("step_up_op");
  assert.ok(step.highlight);
  assert.deepEqual(step.links, [{ kind: "sandbox", label: "gui-api-session-relay", href: "/sandboxes/gui-api-session-relay" }]);
  assert.match(step.summary, /resource=registry\.npmjs\.org/);
  assert.equal(step.detail, "detail: approved");
  const retry = rows.get("task_retry");
  assert.ok(!retry.highlight);
  assert.match(retry.summary, /^auth-token attempts=1 cause=failure$/);
  assert.equal(retry.detail, "failure_reason: go test: token file mode 0644, want 0600");
  assert.deepEqual(retry.links.map((l) => l.href), ["/projects/p_0.gui-api", "/projects/p_0.gui-api?task=auth-token"]);
  assert.ok(rows.get("project_blocked").highlight);
  // A session the event names links to its watch page.
  assert.deepEqual(rows.get("acp_session_started").links.map((l) => l.kind), ["sandbox", "session"]);
  assert.equal(rows.get("acp_session_started").links[1].href, "/sessions/commit-api-read-endpoints");
  // The task of a tasks/<name>.yaml path.
  assert.equal(sv.taskOf({ path: "/Users/me/orch/gui-api/tasks/build.yaml" }), "build");
  assert.equal(sv.taskOf({ path: P }), "");
  // Paths are links, not summary text.
  assert.ok(!/\/Users\/me/.test(rows.get("intake_queued").summary));
});

test("timeline: newest first, filters, no duplicates from the stream", () => {
  const tl = new sv.Timeline(100);
  tl.load(events.slice(0, 6).reverse());
  assert.equal(tl.rows()[0].seq, 45);
  assert.ok(!tl.add(events[3]), "an event the list already has is not added twice");
  assert.ok(tl.add(events[6]));
  assert.ok(!tl.add({ seq: 0 }));
  assert.deepEqual(tl.range(), { first: 40, last: 46 });
  assert.deepEqual(tl.rows({ project: "p_0.gui-api" }).map((r) => r.seq), [46, 44, 43, 42]);
  assert.deepEqual(tl.rows({ family: "integration" }).map((r) => r.event), ["integration_started", "integration_succeeded"]);
  assert.deepEqual(tl.rows({ highlighted: true }).map((r) => r.event), ["step_up_op"]);
  assert.deepEqual(tl.rows({ q: "0644" }).map((r) => r.event), ["task_retry"]);
  const small = new sv.Timeline(3);
  small.load(events);
  assert.deepEqual(small.events.map((e) => e.seq), [46, 47, 48]);
});

const LADDER = ["@every 2m", "@every 4m", "@every 8m", "@every 16m", "@every 32m", "@every 1h"];
const now = Date.parse("2026-11-25T14:00:00Z");
const pr = {
  project: "p_0.workingman-tui", repo: "slimslenderslacks/workingman", number: 34, url: "https://github.com/slimslenderslacks/workingman/pull/34",
  head_sha: "c97c47ec1234", state: "open", watching: true,
  review_poll: { schedule: "@every 8m", backoff_index: 2, has_baseline: true, baseline_source: "mcp" },
  next_poll_at: "2026-11-25T14:03:00Z", poll_ladder: LADDER,
  budgets: { review_fix_cycles: 1, max_review_fix_cycles: 10, review_errors: 0, max_review_errors: 5 },
  fix_tasks: ["fix-pr-34-comment-1201", "push-pr-34", "gone"],
};
const tasks = new Map([
  ["fix-pr-34-comment-1201", { name: "fix-pr-34-comment-1201", status: "committed", source: { kind: "pr-comment", pr: 34 } }],
  ["push-pr-34", { name: "push-pr-34", status: "success", queue_position: 1, source: { kind: "pr-push", pr: 34 } }],
]);

test("PR card: the wireframe's PR from live resources", () => {
  const c = sv.pullCard(pr, tasks, now);
  assert.equal(c.title, "slimslenderslacks/workingman #34");
  assert.equal(c.state, "open");
  assert.equal(c.url, pr.url);
  assert.deepEqual(c.facts, ["head c97c47ec", "watching"]);
  assert.equal(c.ladder.map((r) => (r.current ? "[" + r.label + "]" : r.label)).join("→"), "2m→4m→[8m]→16m→32m→1h");
  assert.equal(c.poll, "next poll in 3m · baseline mcp");
  assert.deepEqual(c.fixTasks.map((f) => [f.name, f.kind, f.text]), [
    ["fix-pr-34-comment-1201", "pr-comment", "committed ✓"],
    ["push-pr-34", "pr-push", "in commit queue #1"],
    ["gone", "", "task file gone"],
  ]);
  assert.deepEqual(c.budgets.map((b) => b.text), ["review_fix_cycles 1/10", "review_errors 0/5"]);
  assert.ok(!c.budgets.some((b) => b.warn));
});

test("PR card: no poll, a merged PR, a near budget, an unsafe URL", () => {
  const c = sv.pullCard(Object.assign({}, pr, { state: "merged", watching: false, review_poll: null, next_poll_at: null, url: "javascript:alert(1)", budgets: { review_fix_cycles: 9, max_review_fix_cycles: 10, review_errors: 0, max_review_errors: 5 } }), tasks, now);
  assert.equal(c.poll, "no poll armed");
  assert.deepEqual(c.facts, ["head c97c47ec", "not watched (merged)"]);
  assert.ok(!c.ladder.some((r) => r.current));
  assert.equal(c.url, "", "a javascript: URL is never a link");
  assert.ok(c.budgets[0].warn);
  assert.equal(sv.until("2026-11-25T13:00:00Z", now), "due");
  assert.equal(sv.until("2026-11-25T15:30:00Z", now), "in 1h30");
  const nb = sv.pullCard(Object.assign({}, pr, { review_poll: { schedule: "@every 2m", backoff_index: 0, has_baseline: false }, next_poll_at: null }), tasks, now);
  assert.match(nb.poll, /^poll armed \(2m\) · no baseline yet/);
  assert.match(sv.pullsNote({ flags: { review: false } }), /review: false/);
  assert.match(sv.pullsNote({ flags: { review: true } }), /review agent opens the PR/);
});

test("intake: hints follow the daemon's routing; names are checked as the server does", () => {
  const hint = (status, sessions) => sv.intakeHint({ status, live: { sessions: sessions || [] } });
  assert.match(hint("working").text, /planner runs alongside its tasks/);
  assert.match(hint("idle").text, /flips it to ready and plans this/);
  assert.ok(hint("blocked").warn && /clearing blocked_reason/.test(hint("blocked").text));
  assert.match(hint("stopped").refuse, /start it first/);
  assert.match(hint("").refuse, /project agent has not filled in/);
  assert.match(hint("working", ["task", "planning"]).text, /follow-up planning run/);
  assert.equal(sv.intakeNameError(""), "");
  assert.equal(sv.intakeNameError("add-card"), "");
  assert.match(sv.intakeNameError("add-card.md"), /leave off \.md/);
  assert.match(sv.intakeNameError("Add Card"), /lowercase/);
  assert.match(sv.intakeNameError("../x"), /lowercase/);
  assert.match(sv.projectNameError(""), /required/);
  assert.match(sv.projectNameError("gui-api", ["gui-api"]), /already exists/);
  assert.match(sv.projectNameError("a".repeat(64)), /63/);
  assert.equal(sv.projectNameError("gui-wireframes", ["gui-api"]), "");
  const g = sv.intakeGroups([
    { name: "a", state: "pending", created_at: "2026-11-26T09:00:00Z" },
    { name: "b", state: "processed", created_at: "2026-11-25T09:00:00Z" },
    { name: "c", state: "empty", created_at: "2026-11-26T10:00:00Z" },
  ]);
  assert.deepEqual(g.pending.map((r) => r.name), ["c", "a"]);
  assert.deepEqual(g.processed.map((r) => r.name), ["b"]);
});

test("inbox: senders as the TUI shows them, closings, order", () => {
  assert.equal(sv.sender({ agent: "task", task: "fix-ci", sandbox: "x", project: "/o/workingman-messaging/project.yaml" }), "task fix-ci (workingman-messaging)");
  assert.equal(sv.sender({ agent: "step-up", sandbox: "step-up" }), "step-up");
  assert.equal(sv.sender({ agent: "workingman", sandbox: "workingman-agent" }), "workingman agent");
  assert.equal(sv.sender({ sandbox: "orphan" }), "orphan");
  assert.equal(sv.closedText({ outcome: "answered", option: "deny", response: "deny", responder: { via: "slack" } }), "answered via slack: deny");
  assert.equal(sv.closedText({ outcome: "answered", response: "use staging", responder: { via: "gui" } }), "answered via gui: use staging");
  assert.equal(sv.closedText({ outcome: "cancelled", reason: "not needed" }), "cancelled — not needed");
  assert.equal(sv.closedText({ outcome: "expired" }), "expired");
  assert.equal(sv.closedText(null), "");

  const now = Date.parse("2026-11-26T12:00:00Z");
  const req = { id: "m1", kind: "request", state: "open", from: { agent: "step-up", sandbox: "step-up" }, subject: "allow?", body: "CHANGE: x", options: ["approve", "deny"], expires_at: "2026-11-26T13:00:00Z", created_at: "2026-11-26T11:00:00Z" };
  let it = sv.inboxItem(req, now);
  assert.ok(it.open && it.canReply && !it.canDismiss && it.stepUp);
  assert.deepEqual(it.options, ["approve", "deny"]);
  assert.equal(it.expires, "in 1h00");
  it = sv.inboxItem(Object.assign({}, req, { expires_at: "2026-11-26T11:59:00Z" }), now);
  assert.ok(!it.open);
  assert.equal(it.refusal, "this message has expired");
  it = sv.inboxItem({ id: "m2", kind: "notification", state: "open", from: { agent: "workingman" }, options: [] }, now);
  assert.ok(it.open && it.canDismiss && !it.canReply && !it.stepUp);
  it = sv.inboxItem(Object.assign({}, req, { state: "closed", resolution: { outcome: "answered", option: "approve", responder: { via: "tui" } } }), now);
  assert.ok(!it.open);
  assert.equal(it.refusal, "closed: answered via tui: approve");

  const order = sv.inboxOrder([
    { id: "old-closed", state: "closed", created_at: "2026-11-26T09:00:00Z" },
    { id: "old-open", state: "open", created_at: "2026-11-26T08:00:00Z" },
    { id: "new-open", state: "open", created_at: "2026-11-26T10:00:00Z" },
  ]);
  assert.deepEqual(order.map((m) => m.id), ["new-open", "old-open", "old-closed"]);
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
