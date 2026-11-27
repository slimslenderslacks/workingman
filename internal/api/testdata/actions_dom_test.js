// A smoke test of the GUI's project actions and inbox answers
// (internal/api/web/projects.js, inbox.js, P.4): it loads the real bundle
// into a fake browser (fakedom.js) over a fake API and checks that, for a
// session with the write and answer scopes, the project header's Stop sends
// POST …/actions/stop with the project's etag as If-Match, CSRF and an
// Idempotency-Key, and redraws from the response (Stop becomes Start); that
// a 412 shows "changed since it was loaded" and takes the current project;
// that Wolf… asks first and sends the typed reason; that Cleanup and Hide
// need no confirm; that archive is never offered; that the inbox answers a
// step-up request with an option (its CHANGE line drawn verbatim, no
// "approve all"), replies in free text, dismisses a notification, and shows
// the winner when another channel answered first; and that a session
// without the scopes gets every control disabled and sends nothing. Run by
// TestProjectViewModel (web_test.go):
//
//   node internal/api/testdata/actions_dom_test.js internal/api/web
"use strict";

const assert = require("node:assert/strict");
const path = require("node:path");
const { boot, buttons, label, SCRIPTS } = require("./fakedom.js");

const webDir = path.resolve(process.argv[2] || path.join(__dirname, "..", "web"));

const CHANGE = "CHANGE: sandbox_network_allow gui-api-session-relay registry.npmjs.org";

function fixture(scopes) {
  const project = {
    id: "p_0.gui-api", work_stream: "gui-api", status: "working", branch: "gui-api", max_parallel_tasks: 1, etag: '"pj-1111"',
    flags: {}, commit_queue: [], task_counts: {}, task_total: 0, pull_requests: [], live: { sessions: [], running_tasks: [] },
  };
  const now = Date.now();
  const iso = (ms) => new Date(now + ms).toISOString();
  const messages = [
    { id: "msg_A", kind: "request", from: { agent: "step-up", sandbox: "step-up" }, recipient: "operator", subject: "allow registry.npmjs.org?", body: "Task session-relay asks for npm.\n" + CHANGE, options: ["approve", "deny"], created_at: iso(-60000), expires_at: iso(3600000), state: "open", expired: false, resolution: null },
    { id: "msg_B", kind: "request", from: { agent: "task", sandbox: "gui-api-auth", task: "auth-token", project: "/Users/me/orch/gui-api/project.yaml" }, recipient: "operator", subject: "which bucket?", body: "Which bucket should the token go to?", options: [], created_at: iso(-120000), expires_at: iso(3600000), state: "open", expired: false, resolution: null },
    { id: "msg_C", kind: "notification", from: { agent: "workingman", sandbox: "workingman-agent" }, recipient: "operator", subject: "gui-api is blocked", body: "gui-api is blocked: auth-token failed", options: [], created_at: iso(-180000), expires_at: iso(3600000), state: "open", expired: false, resolution: null },
    { id: "msg_D", kind: "request", from: { agent: "step-up", sandbox: "step-up" }, recipient: "operator", subject: "allow pypi?", body: "CHANGE: sandbox_network_allow gui-api-x pypi.org", options: ["approve", "deny"], created_at: iso(-240000), expires_at: iso(3600000), state: "open", expired: false, resolution: null },
  ];
  const state = { project, messages, stale: false, slackWon: false };
  const closed = (m, res) => Object.assign({}, m, { state: "closed", resolution: Object.assign({ id: m.id, at: new Date().toISOString() }, res) });
  const routes = {
    "/api/v1/session": () => ({ auth: "cookie", csrf: "csrf-1", scopes }),
    "/api/v1/projects": () => ({ items: [project], cursor: 10 }),
    "/api/v1/projects/p_0.gui-api": () => project,
    "/api/v1/projects/p_0.gui-api/tasks": () => ({ items: [], cursor: 10 }),
    "/api/v1/projects/p_0.gui-api/graph": () => ({ nodes: [], edges: [], commit_queue: [], errors: [], max_attempts: 3 }),
    "/api/v1/sessions": () => ({ items: [], cursor: 10 }),
    "/api/v1/messages": () => ({ items: messages.slice(), cursor: 10 }),
  };
  for (const a of ["stop", "start", "wolf", "review", "cleanup", "hide", "show", "catchup"]) {
    routes["/api/v1/projects/p_0.gui-api/actions/" + a] = (u, init) => {
      if (state.stale) {
        state.stale = false;
        project.status = "blocked";
        project.etag = '"pj-2222"';
        throw Object.assign(new Error("project.yaml changed since you loaded it (now \"pj-2222\")"), { status: 412, code: "precondition_failed", current: project });
      }
      if (a === "stop") Object.assign(project, { status: "stopped", stopped_from: "working", etag: '"pj-3333"' });
      if (a === "start") Object.assign(project, { status: "working", stopped_from: "", etag: '"pj-4444"' });
      if (a === "wolf") Object.assign(project, { status: "blocked", blocked_reason: JSON.parse(init.body).reason, etag: '"pj-5555"' });
      if (a === "cleanup") Object.assign(project, { flags: Object.assign({}, project.flags, { cleanup: true }), etag: '"pj-6666"' });
      if (a === "hide") Object.assign(project, { flags: Object.assign({}, project.flags, { hidden: true }), etag: '"pj-7777"' });
      return { project: Object.assign({}, project), write: { path: "/Users/me/orch/gui-api/project.yaml" }, expect: ["project.status"] };
    };
  }
  for (const m of messages) {
    const answer = (u, init) => {
      if (m.id === "msg_D" && state.slackWon) {
        const cur = closed(m, { outcome: "answered", option: "deny", response: "deny", responder: { kind: "operator", via: "slack" } });
        messages[messages.indexOf(m)] = cur;
        throw Object.assign(new Error("message msg_D is already answered via slack"), { status: 409, code: "conflict", current: cur });
      }
      const b = JSON.parse(init.body);
      const res = u.pathname.endsWith("/dismiss")
        ? { outcome: "dismissed", responder: { kind: "operator", via: "gui" } }
        : { outcome: "answered", option: b.option || "", response: b.option || b.response, responder: { kind: "operator", via: "gui" } };
      const c = closed(m, res);
      messages[messages.indexOf(m)] = c;
      return { message: c, write: { path: "/po/outbox/" + m.id + ".json" }, expect: ["message.closed"] };
    };
    routes["/api/v1/messages/" + m.id + "/answer"] = answer;
    routes["/api/v1/messages/" + m.id + "/dismiss"] = answer;
  }
  return { state, routes };
}

const writes = (requests) => requests.filter((r) => r.method !== "GET" && r.method !== "HEAD");
const byLabel = (main, l) => buttons(main).find((b) => label(b) === l);
const input = (main, aria) => main.find((e) => e.getAttribute("aria-label") === aria);

function page(scopes, url) {
  const { state, routes } = fixture(scopes);
  const p = boot({ webDir, scripts: SCRIPTS, url, routes });
  return Object.assign(p, { state });
}

(async () => {
  // ---- project actions, with the write scope ----
  const pg = page(["read", "write", "answer"], "http://127.0.0.1:7078/projects/p_0.gui-api");
  const { main, requests, settle, state } = pg;
  pg.start();
  await settle();
  pg.es.source.send({ seq: 10, type: "hello", data: { cursor: 10, epoch: "1" } });
  await settle();

  for (const l of ["Stop", "Wolf…", "Cleanup", "Hide"]) assert.ok(byLabel(main, l) && !byLabel(main, l).disabled, l + " is not enabled");
  // Review refuses on a working project, with the TUI's words.
  assert.ok(byLabel(main, "Review").disabled);
  assert.match(byLabel(main, "Review").getAttribute("title"), /^review starts on an idle work stream/);
  assert.ok(byLabel(main, "Open shell ▾").disabled, "the shells are enabled");
  assert.ok(!buttons(main).some((b) => /archive/i.test(label(b))), "archive is offered");

  byLabel(main, "Stop").dispatch("click");
  await settle();
  let w = writes(requests);
  assert.equal(w.length, 1);
  assert.equal(w[0].method, "POST");
  assert.equal(w[0].path, "/api/v1/projects/p_0.gui-api/actions/stop");
  assert.equal(w[0].headers["If-Match"], '"pj-1111"');
  assert.equal(w[0].headers["X-WM-CSRF"], "csrf-1");
  assert.equal(w[0].headers["Content-Type"], "application/json");
  assert.match(w[0].headers["Idempotency-Key"], /^[0-9a-f]{32}$/);
  assert.deepEqual(w[0].body, {});
  assert.match(main.textContent, /stop: stopped: the daemon closes the project's sessions · watch for project\.status/);
  assert.ok(byLabel(main, "Start") && !byLabel(main, "Start").disabled, "Start is not offered after stop");

  // Start uses the etag the stop returned.
  byLabel(main, "Start").dispatch("click");
  await settle();
  w = writes(requests);
  assert.equal(w[1].path, "/api/v1/projects/p_0.gui-api/actions/start");
  assert.equal(w[1].headers["If-Match"], '"pj-3333"');

  // A concurrent write: 412, the project as it is now replaces the stale one.
  state.stale = true;
  byLabel(main, "Cleanup").dispatch("click");
  await settle();
  assert.match(main.textContent, /cleanup: the project changed since it was loaded/);
  assert.ok(main.find((e) => e.className && e.className.startsWith("status status-blocked")), "the 412's current project is not drawn");
  state.project.status = "working";

  // Wolf… asks first; Cancel sends nothing; Summon sends the reason.
  const before = writes(requests).length;
  byLabel(main, "Wolf…").dispatch("click");
  await settle();
  assert.match(main.textContent, /Summon the wolf: blocks gui-api/);
  assert.equal(writes(requests).length, before, "Wolf… sent before the confirm");
  byLabel(main, "Cancel").dispatch("click");
  await settle();
  assert.ok(!byLabel(main, "Summon wolf"), "the confirm stayed open");
  byLabel(main, "Wolf…").dispatch("click");
  await settle();
  input(main, "wolf reason").value = "  the API tests hang  ";
  byLabel(main, "Summon wolf").dispatch("click");
  await settle();
  w = writes(requests);
  const wolf = w[w.length - 1];
  assert.equal(wolf.path, "/api/v1/projects/p_0.gui-api/actions/wolf");
  assert.deepEqual(wolf.body, { reason: "the API tests hang" });
  assert.equal(wolf.headers["If-Match"], '"pj-2222"', "wolf did not use the etag from the 412's current");
  assert.match(main.textContent, /wolf: blocked: the daemon launches the wolf/);

  // ---- the inbox ----
  await pg.navigate("/inbox");
  let text = main.textContent;
  assert.ok(!/null|undefined|\[object|NaN/.test(text), "stray value: " + text);
  assert.match(text, /allow registry\.npmjs\.org\?/);
  // The CHANGE line verbatim in a monospace block, flagged as step-up.
  const pre = main.findAll((e) => e.tagName === "PRE").map((e) => e.textContent);
  assert.ok(pre.some((t) => t.includes(CHANGE)), "the CHANGE line is not drawn verbatim");
  assert.match(text, /Step-up request: approving lets Step-Up apply exactly the CHANGE line below/);
  assert.ok(!buttons(main).some((b) => /approve all/i.test(label(b))), "approve all is offered");
  assert.match(text, /task auth-token \(gui-api\)/);

  const itemOf = (subject) => main.find((e) => e.tagName === "SECTION" && e.className.includes("inbox-item") && e.textContent.includes(subject));
  const btnIn = (el, l) => el.findAll((e) => e.tagName === "BUTTON").find((b) => b.textContent === l);

  const n0 = writes(requests).length;
  btnIn(itemOf("allow registry.npmjs.org?"), "approve").dispatch("click");
  await settle();
  w = writes(requests);
  assert.equal(w.length, n0 + 1);
  assert.equal(w[n0].path, "/api/v1/messages/msg_A/answer");
  assert.deepEqual(w[n0].body, { option: "approve" });
  assert.equal(w[n0].headers["X-WM-CSRF"], "csrf-1");
  assert.ok(w[n0].headers["Idempotency-Key"]);
  assert.equal(w[n0].headers["If-Match"], undefined);
  assert.match(itemOf("allow registry.npmjs.org?").textContent, /answered via gui: approve · post-office delivers it/);

  // Free text: blank is refused locally, then sent.
  const b = itemOf("which bucket?");
  btnIn(b, "Reply").dispatch("click");
  await settle();
  assert.equal(writes(requests).length, n0 + 1, "a blank reply was sent");
  input(main, "reply to msg_B").value = "the staging bucket";
  btnIn(itemOf("which bucket?"), "Reply").dispatch("click");
  await settle();
  w = writes(requests);
  assert.equal(w[w.length - 1].path, "/api/v1/messages/msg_B/answer");
  assert.deepEqual(w[w.length - 1].body, { response: "the staging bucket" });

  // A notification is dismissed, with no options to pick.
  const c = itemOf("gui-api is blocked");
  assert.ok(!btnIn(c, "Reply"), "a notification offers a reply");
  btnIn(c, "Dismiss").dispatch("click");
  await settle();
  w = writes(requests);
  assert.equal(w[w.length - 1].path, "/api/v1/messages/msg_C/dismiss");

  // Answered on Slack first: the row says who won.
  state.slackWon = true;
  btnIn(itemOf("allow pypi?"), "approve").dispatch("click");
  await settle();
  assert.match(itemOf("allow pypi?").textContent, /already closed elsewhere: answered via slack: deny/);
  assert.ok(!btnIn(itemOf("allow pypi?"), "approve"), "a closed request still offers its options");

  // ---- without the scopes: everything disabled, nothing sent ----
  const ro = page(["read"], "http://127.0.0.1:7078/projects/p_0.gui-api");
  ro.start();
  await ro.settle();
  for (const l of ["Stop", "Wolf…", "Cleanup", "Hide"]) {
    const btn = byLabel(ro.main, l);
    assert.ok(btn && btn.disabled, "read-only: " + l + " is enabled");
    btn.dispatch("click");
  }
  await ro.navigate("/inbox");
  const roButtons = buttons(ro.main);
  assert.ok(roButtons.length >= 4, "read-only inbox draws its controls");
  for (const btn of roButtons) assert.ok(btn.disabled, "read-only inbox: " + label(btn) + " is enabled");
  assert.match(ro.main.textContent, /lacks the answer scope/);
  assert.equal(writes(ro.requests).length, 0, "a read-only session sent a write");

  // write without answer: project actions yes, inbox no.
  const wo = page(["read", "write"], "http://127.0.0.1:7078/inbox");
  wo.start();
  await wo.settle();
  for (const btn of buttons(wo.main)) assert.ok(btn.disabled, "write-only inbox: " + label(btn) + " is enabled");

  console.log("ok   project actions and inbox answers write from the GUI with If-Match, CSRF and Idempotency-Key; 412 and 409 show what changed; sessions without the scopes send nothing");
})().catch((e) => {
  console.log("FAIL " + (e && e.stack ? e.stack : e));
  process.exit(1);
});
