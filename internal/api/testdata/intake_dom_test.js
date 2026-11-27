// A smoke test of the GUI's first writes (internal/api/web/intake.js, P.4):
// it loads the real bundle into a fake browser (fakedom.js) over a fake API
// and checks that, for a session with the write scope, the project view's
// "Queue intake" and the rail's "+ New project" lead to their forms; that
// the intake form posts the request with its CSRF header, an
// Idempotency-Key and a JSON body, keeps the key for a resend after a
// failure, and refuses a bad name before sending; that Withdraw sends
// DELETE with If-Match and reports a 412; that a stopped project offers no
// queue; that the new-project form posts to /projects; and that a
// read-only session gets every control disabled and sends nothing. Run by
// TestProjectViewModel (web_test.go):
//
//   node internal/api/testdata/intake_dom_test.js internal/api/web
"use strict";

const assert = require("node:assert/strict");
const path = require("node:path");
const { boot, buttons, label, SCRIPTS } = require("./fakedom.js");

const webDir = path.resolve(process.argv[2] || path.join(__dirname, "..", "web"));

function fixture(scopes) {
  const project = {
    id: "p_0.gui-api", work_stream: "gui-api", status: "working", branch: "gui-api", max_parallel_tasks: 1,
    flags: {}, commit_queue: [], task_counts: {}, task_total: 0, pull_requests: [], live: { sessions: [], running_tasks: [] },
  };
  const intake = [
    { project: "p_0.gui-api", name: "add-card", file: "add-card.md", state: "pending", created_at: "2026-11-26T10:00:00Z", etag: '"in-aaaa"' },
    { project: "p_0.gui-api", name: "blank", file: "blank.md", state: "empty", created_at: "2026-11-26T09:00:00Z", etag: '"in-bbbb"' },
    { project: "p_0.gui-api", name: "old", file: "old.processed", state: "processed", created_at: "2026-11-25T10:00:00Z", etag: '"in-cccc"' },
  ];
  const state = { project, intake, failNextPost: false, withdraw412: false };
  const routes = {
    "/api/v1/session": () => ({ auth: "cookie", csrf: "csrf-1", scopes }),
    "/api/v1/status": () => ({ daemon: { roots: ["/Users/me/orch"] }, cursor: 10 }),
    "/api/v1/projects": (u, init) => {
      if (init.method === "POST") {
        const b = JSON.parse(init.body);
        return { id: "p_0." + b.name, path: "/Users/me/orch/" + b.name + "/project.md", state: "seeded", write: { path: "/Users/me/orch/" + b.name + "/project.md" }, expect: [] };
      }
      return { items: [project], cursor: 10 };
    },
    "/api/v1/projects/p_0.gui-api": () => project,
    "/api/v1/projects/p_0.gui-api/tasks": () => ({ items: [], cursor: 10 }),
    "/api/v1/projects/p_0.gui-api/graph": () => ({ nodes: [], edges: [], commit_queue: [], errors: [], max_attempts: 3 }),
    "/api/v1/sessions": () => ({ items: [], cursor: 10 }),
    "/api/v1/messages": () => ({ items: [], cursor: 10 }),
    "/api/v1/projects/p_0.gui-api/intake": (u, init) => {
      if (init.method === "POST") {
        if (state.failNextPost) {
          state.failNextPost = false;
          throw Object.assign(new Error("daemon busy"), { status: 503, code: "unavailable" });
        }
        const b = JSON.parse(init.body);
        const r = { project: "p_0.gui-api", name: b.name, file: b.name + ".md", state: "pending", created_at: "2026-11-26T11:00:00Z", etag: '"in-dddd"' };
        intake.push(r);
        return { intake: r, write: { path: "/x" }, expect: ["intake.created"] };
      }
      return { items: intake.slice(), cursor: 10 };
    },
    "/api/v1/projects/p_0.gui-api/intake/add-card": (u, init) => {
      if (init.method !== "DELETE") return intake[0];
      if (state.withdraw412) {
        state.withdraw412 = false;
        throw Object.assign(new Error("intake request \"add-card\" changed since you loaded it"), { status: 412, code: "precondition_failed" });
      }
      intake.splice(intake.findIndex((r) => r.name === "add-card"), 1);
      return { intake: { name: "add-card", file: "add-card.withdrawn", state: "withdrawn" }, write: { path: "/x" }, expect: [] };
    },
  };
  return { state, routes };
}

const writes = (requests) => requests.filter((r) => r.method !== "GET" && r.method !== "HEAD");
const byLabel = (main, l) => buttons(main).find((b) => label(b) === l);
const linkTo = (main, href) => main.find((e) => e.tagName === "A" && e.getAttribute("href") === href);
const input = (main, aria) => main.find((e) => e.getAttribute("aria-label") === aria);

(async () => {
  // ---- a session with the write scope ----
  const { state, routes } = fixture(["read", "write", "answer"]);
  const page = boot({ webDir, scripts: SCRIPTS, url: "http://127.0.0.1:7078/projects/p_0.gui-api", routes });
  const { main, requests, settle } = page;
  page.start();
  await settle();
  page.es.source.send({ seq: 10, type: "hello", data: { cursor: 10, epoch: "1" } });
  await settle();

  // The project view: Queue intake and + New project are links to their
  // forms; the project actions are buttons (actions_dom_test.js drives
  // them), and so are the host tabs.
  const qi = linkTo(main, "/projects/p_0.gui-api/intake");
  assert.ok(qi && qi.textContent === "Queue intake", "Queue intake is not a link to the intake form");
  assert.ok(linkTo(main, "/new-project"), "+ New project is not a link");
  assert.ok(byLabel(main, "Shell: orch dir") && !byLabel(main, "Shell: orch dir").disabled, "Shell: orch dir is not enabled");
  assert.ok(byLabel(main, "Stop") && !byLabel(main, "Stop").disabled, "Stop is not enabled with the write scope");

  // ---- the intake pane ----
  await page.navigate("/projects/p_0.gui-api/intake");
  let text = main.textContent;
  assert.ok(!/null|undefined|\[object|NaN/.test(text), "stray value: " + text);
  assert.match(text, /Intake · gui-api/);
  assert.match(text, /a planner runs alongside its tasks/);
  assert.match(text, /Pending \(2\)/);
  assert.match(text, /add-card\.md/);
  assert.match(text, /Processed \(1\)/);
  assert.match(text, /old\.processed/);
  // A blank request cannot be withdrawn (there is nothing for the planner
  // to take), a pending one can.
  assert.deepEqual(buttons(main).filter((b) => label(b) === "Withdraw").length, 1);
  const send = byLabel(main, "Queue intake");
  assert.ok(send && !send.disabled, "the queue button is disabled");

  // A bad name is refused before anything is sent.
  input(main, "request name").value = "add-card.md";
  input(main, "request").value = "Add a list too";
  send.dispatch("click");
  await settle();
  assert.equal(writes(requests).length, 0, "a bad name was sent");
  assert.match(main.textContent, /leave off \.md/);

  // A failed send keeps its Idempotency-Key for the resend.
  input(main, "request name").value = "add-list";
  state.failNextPost = true;
  send.dispatch("click");
  await settle();
  assert.match(main.textContent, /503 unavailable: daemon busy/);
  send.dispatch("click");
  await settle();
  const posts = writes(requests).filter((r) => r.method === "POST");
  assert.equal(posts.length, 2);
  for (const p of posts) {
    assert.equal(p.path, "/api/v1/projects/p_0.gui-api/intake");
    assert.equal(p.headers["X-WM-CSRF"], "csrf-1");
    assert.equal(p.headers["Content-Type"], "application/json");
    assert.match(p.headers["Idempotency-Key"], /^[0-9a-f]{32}$/);
    assert.deepEqual(p.body, { name: "add-list", body: "Add a list too" });
  }
  assert.equal(posts[0].headers["Idempotency-Key"], posts[1].headers["Idempotency-Key"], "the resend has a new key");
  assert.match(main.textContent, /queued intake\/add-list\.md/);
  assert.equal(input(main, "request").value, "", "the form was not cleared");
  assert.match(main.textContent, /Pending \(3\)/);

  // Withdraw: If-Match is the request's etag; a 412 reloads and says so.
  state.withdraw412 = true;
  const withdrawOf = (file) => buttons(main).find((b) => label(b) === "Withdraw" && b.parentNode.textContent.includes(file));
  let wd = withdrawOf("add-card.md");
  wd.dispatch("click");
  await settle();
  let del = writes(requests).filter((r) => r.method === "DELETE");
  assert.equal(del.length, 1);
  assert.equal(del[0].path, "/api/v1/projects/p_0.gui-api/intake/add-card");
  assert.equal(del[0].headers["If-Match"], '"in-aaaa"');
  assert.equal(del[0].headers["X-WM-CSRF"], "csrf-1");
  assert.equal(del[0].body, undefined, "a DELETE sends no body");
  assert.match(main.textContent, /changed since it was loaded/);
  wd = withdrawOf("add-card.md");
  wd.dispatch("click");
  await settle();
  assert.match(main.textContent, /Pending \(2\)/);
  const rows = main.findAll((e) => e.tagName === "LI" && e.className.includes("intake-row")).map((e) => e.textContent);
  assert.ok(!rows.some((t) => t.includes("add-card.md")), "the withdrawn request is still listed: " + rows);
  assert.match(main.textContent, /withdrew add-card\.md \(now add-card\.withdrawn\)/);

  // The project stops: the pane stops offering the queue.
  state.project.status = "stopped";
  page.es.source.send({ seq: 11, type: "project.status", project: "p_0.gui-api", data: { project: state.project } });
  await settle();
  assert.ok(byLabel(main, "Queue intake").disabled, "queue offered on a stopped project");
  assert.match(main.textContent, /start it first/);
  state.project.status = "working";

  // ---- new project ----
  await page.navigate("/new-project");
  assert.match(main.textContent, /New project/);
  const create = byLabel(main, "Create project");
  input(main, "project name").value = "gui-api";
  input(main, "description").value = "Draw wireframes";
  create.dispatch("click");
  await settle();
  assert.match(main.textContent, /gui-api already exists/);
  assert.equal(writes(requests).filter((r) => r.path === "/api/v1/projects").length, 0);
  input(main, "project name").value = "gui-wireframes";
  create.dispatch("click");
  await settle();
  const np = writes(requests).filter((r) => r.path === "/api/v1/projects");
  assert.equal(np.length, 1);
  assert.deepEqual(np[0].body, { root: 0, name: "gui-wireframes", description: "Draw wireframes" });
  assert.ok(np[0].headers["Idempotency-Key"]);
  assert.match(main.textContent, /seeded \/Users\/me\/orch\/gui-wireframes\/project\.md/);
  assert.ok(linkTo(main, "/projects/p_0.gui-wireframes"), "no link to the new project");

  // ---- a read-only session: everything disabled, nothing sent ----
  const ro = fixture(["read"]);
  const page2 = boot({ webDir, scripts: SCRIPTS, url: "http://127.0.0.1:7078/projects/p_0.gui-api/intake", routes: ro.routes });
  page2.start();
  await page2.settle();
  for (const b of buttons(page2.main)) assert.ok(b.disabled, "read-only: " + label(b) + " is enabled");
  assert.match(byLabel(page2.main, "Queue intake").getAttribute("title"), /lacks the write scope/);
  byLabel(page2.main, "Queue intake").dispatch("click");
  await page2.navigate("/projects/p_0.gui-api");
  assert.ok(!linkTo(page2.main, "/projects/p_0.gui-api/intake"), "read-only: Queue intake is a link");
  assert.ok(byLabel(page2.main, "Queue intake").disabled);
  assert.ok(byLabel(page2.main, "+ New project").disabled);
  await page2.navigate("/new-project");
  assert.ok(byLabel(page2.main, "Create project").disabled);
  assert.equal(writes(page2.requests).length, 0, "a read-only session sent a write");

  console.log("ok   intake and new project write from the GUI with CSRF, Idempotency-Key and If-Match; read-only sessions send nothing");
})().catch((e) => {
  console.log("FAIL " + (e && e.stack ? e.stack : e));
  process.exit(1);
});
